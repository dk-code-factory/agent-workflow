//go:build linux

package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/sys/unix"
)

func (s *service) validateRoutingAsPeer(req Request, cred *unix.Ucred, groups []uint32) error {
	file, err := launchFile(memberLaunch{Request: req})
	if err != nil {
		return err
	}
	defer file.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.binary, "__network_runtime_check_routing_v1", "admission")
	cmd.Dir = req.Dir
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.ExtraFiles = []*os.File{file}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: cred.Uid, Gid: cred.Gid, Groups: groups}}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("container routing admission failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func checkRoutingHelper() error {
	f := os.NewFile(3, "admission")
	defer f.Close()
	return checkRoutingPayload(f, validateSavedRouting)
}

func checkRoutingPayload(f *os.File, validate func([]string) error) error {
	var launch memberLaunch
	if err := decodeSealedPayload(f, &launch); err != nil {
		return err
	}
	return validate(launch.Request.Env)
}

// Saved network transports cannot be fenced by the Unix LSM while retaining
// ordinary provider HTTPS/SSH. Inspect them as the caller before workload exec.
// All saved destinations are checked, including currently unselected contexts,
// because provider filtering can change which selection becomes the default.
func validateSavedRouting(environment []string) error {
	home := ""
	if current, err := user.Current(); err == nil {
		home = current.HomeDir
	}
	return validateSavedRoutingWithDefaults(environment, home, []string{"/usr/share/containers", "/etc/containers"})
}

func validateSavedRoutingWithDefaults(environment []string, userHome string, system []string) error {
	env := map[string]string{}
	for _, entry := range environment {
		k, v, ok := strings.Cut(entry, "=")
		if ok {
			env[k] = v
		}
	}
	// A provider may filter any of these overrides. Check the defaults it can
	// reveal as well as the explicitly selected locations, including account home.
	type routingFile struct {
		path string
		json bool
	}
	var files []routingFile
	seen := map[routingFile]bool{}
	add := func(path string, json bool) {
		file := routingFile{filepath.Clean(path), json}
		if !seen[file] {
			seen[file] = true
			files = append(files, file)
		}
	}
	configs := map[string]bool{}
	dockers := map[string]bool{}
	for _, home := range []string{userHome, env["HOME"]} {
		if home != "" {
			configs[filepath.Join(home, ".config")] = true
			dockers[filepath.Join(home, ".docker")] = true
		}
	}
	if config := env["XDG_CONFIG_HOME"]; config != "" {
		configs[filepath.Clean(config)] = true
	}
	if docker := env["DOCKER_CONFIG"]; docker != "" {
		dockers[filepath.Clean(docker)] = true
	}
	containerDirs := append([]string(nil), system...)
	for config := range configs {
		containerDirs = append(containerDirs, filepath.Join(config, "containers"))
	}
	for _, dir := range containerDirs {
		add(filepath.Join(dir, "containers.conf"), false)
		dropins := filepath.Join(dir, "containers.conf.d")
		entries, err := routingDirectory(dropins)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".conf") {
				add(filepath.Join(dropins, entry.Name()), false)
			}
		}
	}
	for config := range configs {
		add(filepath.Join(config, "containers/podman-connections.json"), true)
	}
	for _, key := range []string{"CONTAINERS_CONF", "CONTAINERS_CONF_OVERRIDE", "PODMAN_CONNECTIONS_CONF"} {
		if env[key] != "" {
			add(env[key], key == "PODMAN_CONNECTIONS_CONF")
		}
	}
	for docker := range dockers {
		add(filepath.Join(docker, "config.json"), true)
		meta := filepath.Join(docker, "contexts/meta")
		entries, err := routingDirectory(meta)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				add(filepath.Join(meta, entry.Name(), "meta.json"), true)
			}
		}
	}
	for _, file := range files {
		if err := validateRoutingFile(file.path, file.json); err != nil {
			return fmt.Errorf("saved container routing %s: %w", file.path, err)
		}
	}
	return nil
}

func routingDirectory(path string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("saved container routing directory %s: %w", path, err)
	}
	return entries, nil
}

func validateRoutingFile(path string, jsonFormat bool) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	const limit = 4 << 20
	if !info.Mode().IsRegular() || info.Size() > limit {
		return errors.New("routing configuration must be a regular file of at most 4 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return err
	}
	if len(data) > limit {
		return errors.New("routing configuration grew beyond limit")
	}
	var document map[string]any
	if jsonFormat {
		err = json.Unmarshal(data, &document)
	} else {
		err = toml.Unmarshal(data, &document)
	}
	if err != nil {
		return errors.New("cannot parse routing configuration")
	}
	if !jsonFormat {
		if engine, ok := document["engine"].(map[string]any); ok {
			if assignments, ok := engine["env"].([]any); ok {
				for _, assignment := range assignments {
					if text, ok := assignment.(string); ok {
						key, endpoint, _ := strings.Cut(text, "=")
						if key == "CONTAINER_HOST" {
							if err := validateDestinations(map[string]any{"uri": endpoint}); err != nil {
								return err
							}
						}
					}
				}
			}
		}
	}
	return validateDestinations(document)
}

func validateDestinations(value any) error {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			switch strings.ToLower(key) {
			case "uri":
				if endpoint, ok := child.(string); ok && endpoint != "" {
					if !strings.HasPrefix(endpoint, "unix:///") || strings.ContainsAny(endpoint, "\x00\n\r") {
						return errors.New("saved non-Unix container API transport is unsupported with socket containment")
					}
				}
			case "endpoints":
				if endpoints, ok := child.(map[string]any); ok {
					for name, endpoint := range endpoints {
						if !strings.EqualFold(name, "docker") {
							continue
						}
						if docker, ok := endpoint.(map[string]any); ok {
							for field, host := range docker {
								if strings.EqualFold(field, "host") {
									if err := validateDestinations(map[string]any{"uri": host}); err != nil {
										return err
									}
								}
							}
						}
					}
				}
			}
			if err := validateDestinations(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range v {
			if err := validateDestinations(child); err != nil {
				return err
			}
		}
	}
	return nil
}
