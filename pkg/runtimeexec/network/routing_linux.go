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
	var launch memberLaunch
	if err := json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&launch); err != nil {
		return err
	}
	return validateSavedRouting(launch.Request.Env)
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
	if env["HOME"] == "" {
		env["HOME"] = userHome
	}
	config := env["XDG_CONFIG_HOME"]
	if config == "" && env["HOME"] != "" {
		config = filepath.Join(env["HOME"], ".config")
	}
	var files, patterns []string
	for _, dir := range system {
		files = append(files, filepath.Join(dir, "containers.conf"))
		patterns = append(patterns, filepath.Join(dir, "containers.conf.d/*.conf"))
	}
	if config != "" {
		files = append(files, filepath.Join(config, "containers/containers.conf"), filepath.Join(config, "containers/podman-connections.json"))
		patterns = append(patterns, filepath.Join(config, "containers/containers.conf.d/*.conf"))
	}
	if override := env["CONTAINERS_CONF"]; override != "" {
		files = []string{override}
		patterns = nil
		if config != "" {
			files = append(files, filepath.Join(config, "containers/podman-connections.json"))
		}
	}
	for _, key := range []string{"CONTAINERS_CONF", "CONTAINERS_CONF_OVERRIDE", "PODMAN_CONNECTIONS_CONF"} {
		if env[key] != "" {
			files = append(files, env[key])
		}
	}
	docker := env["DOCKER_CONFIG"]
	if docker == "" && env["HOME"] != "" {
		docker = filepath.Join(env["HOME"], ".docker")
	}
	if docker != "" {
		files = append(files, filepath.Join(docker, "config.json"))
		patterns = append(patterns, filepath.Join(docker, "contexts/meta/*/meta.json"))
	}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return err
		}
		files = append(files, matches...)
	}
	for _, path := range files {
		if err := validateRoutingFile(path); err != nil {
			return fmt.Errorf("saved container routing %s: %w", path, err)
		}
	}
	return nil
}

func validateRoutingFile(path string) error {
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
	if strings.HasSuffix(path, ".json") {
		err = json.Unmarshal(data, &document)
	} else {
		err = toml.Unmarshal(data, &document)
	}
	if err != nil {
		return errors.New("cannot parse routing configuration")
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
