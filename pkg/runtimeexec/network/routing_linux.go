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
	"strconv"
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

type routingSource uint8

const (
	containersConfig routingSource = iota
	podmanConnections
	dockerConfig
	dockerContext
)

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
		path   string
		source routingSource
	}
	var files []routingFile
	seen := map[routingFile]bool{}
	add := func(path string, source routingSource) {
		file := routingFile{filepath.Clean(path), source}
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
	addDropins := func(path string) error {
		entries, err := routingDirectory(path)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".conf") {
				add(filepath.Join(path, entry.Name()), containersConfig)
			}
		}
		return nil
	}
	for _, dir := range containerDirs {
		add(filepath.Join(dir, "containers.conf"), containersConfig)
		if err := addDropins(filepath.Join(dir, "containers.conf.d")); err != nil {
			return err
		}
	}
	// system is ordered from distribution defaults to the administrator's
	// directory. Only the latter supplies native rootless-specific overrides.
	if len(system) > 0 {
		uids := map[int]bool{os.Getuid(): os.Getuid() > 0}
		if uid, err := strconv.Atoi(env["_CONTAINERS_ROOTLESS_UID"]); err == nil && uid > 0 {
			uids[uid] = true
		}
		for uid, applicable := range uids {
			if !applicable {
				continue
			}
			path := filepath.Join(system[len(system)-1], "containers.rootless.conf")
			add(path, containersConfig)
			if err := addDropins(path + ".d"); err != nil {
				return err
			}
			if err := addDropins(filepath.Join(path+".d", strconv.Itoa(uid))); err != nil {
				return err
			}
		}
	}
	for config := range configs {
		add(filepath.Join(config, "containers/podman-connections.json"), podmanConnections)
	}
	for _, key := range []string{"CONTAINERS_CONF", "CONTAINERS_CONF_OVERRIDE", "PODMAN_CONNECTIONS_CONF"} {
		if env[key] != "" {
			source := containersConfig
			if key == "PODMAN_CONNECTIONS_CONF" {
				source = podmanConnections
			}
			add(env[key], source)
		}
	}
	for docker := range dockers {
		add(filepath.Join(docker, "config.json"), dockerConfig)
		meta := filepath.Join(docker, "contexts/meta")
		entries, err := routingDirectory(meta)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				add(filepath.Join(meta, entry.Name(), "meta.json"), dockerContext)
			}
		}
	}
	for i := 0; i < len(files); i++ {
		file := files[i]
		assignments, err := validateRoutingFile(file.path, file.source)
		if err != nil {
			return fmt.Errorf("saved container routing %s: %w", file.path, err)
		}
		// setupEnv runs after TOML loading, before connectionsConfigFile.
		// These defaults can therefore select existing connection JSON, but
		// do not cause another TOML configuration to be loaded in this process.
		for _, assignment := range assignments {
			key, value, ok := strings.Cut(assignment, "=")
			if !ok || value == "" {
				continue
			}
			switch key {
			case "PODMAN_CONNECTIONS_CONF":
				add(value, podmanConnections)
			case "XDG_CONFIG_HOME":
				add(filepath.Join(value, "containers/podman-connections.json"), podmanConnections)
			case "HOME":
				add(filepath.Join(value, ".config/containers/podman-connections.json"), podmanConnections)
			}
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

// Return engine.env assignments only for containers.conf sources. JSON sources
// cannot add further selectors, so following saved defaults is finite.
func validateRoutingFile(path string, source routingSource) ([]string, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	const limit = 4 << 20
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("routing configuration must be a regular file of at most 4 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errors.New("routing configuration grew beyond limit")
	}
	switch source {
	case containersConfig:
		var document map[string]any
		if err := toml.Unmarshal(data, &document); err != nil {
			return nil, errors.New("cannot parse routing configuration")
		}
		return validateContainerConfig(document)
	case podmanConnections:
		var document struct {
			Connection struct {
				Connections map[string]struct{ URI string }
			}
		}
		if err := json.Unmarshal(data, &document); err != nil {
			return nil, errors.New("cannot parse routing configuration")
		}
		for _, destination := range document.Connection.Connections {
			if err := validateContainerEndpoint(destination.URI); err != nil {
				return nil, err
			}
		}
	case dockerContext:
		var document struct {
			Endpoints map[string]json.RawMessage
		}
		if err := json.Unmarshal(data, &document); err != nil {
			return nil, errors.New("cannot parse routing configuration")
		}
		if data, ok := document.Endpoints["docker"]; ok {
			var endpoint struct{ Host string }
			if err := json.Unmarshal(data, &endpoint); err != nil {
				return nil, errors.New("cannot parse routing configuration")
			}
			if err := validateContainerEndpoint(endpoint.Host); err != nil {
				return nil, err
			}
		}
	case dockerConfig:
		// Docker's CLI configuration has selection and authentication metadata;
		// API hosts are stored in context Endpoints["docker"].Host instead.
		var document map[string]any
		if err := json.Unmarshal(data, &document); err != nil {
			return nil, errors.New("cannot parse routing configuration")
		}
	}
	return nil, nil
}

func validateContainerConfig(document map[string]any) ([]string, error) {
	var assignments []string
	// Native Podman TOML struct fields are case insensitive. Keep map entry
	// names intact and inspect only Engine.Env and Engine.ServiceDestinations.
	for key, value := range document {
		if !strings.EqualFold(key, "engine") {
			continue
		}
		engine, _ := value.(map[string]any)
		for field, value := range engine {
			switch {
			case strings.EqualFold(field, "env"):
				environment, _ := value.([]any)
				for _, value := range environment {
					assignment, ok := value.(string)
					if !ok {
						continue
					}
					assignments = append(assignments, assignment)
					if key, endpoint, ok := strings.Cut(assignment, "="); ok && key == "CONTAINER_HOST" {
						if err := validateContainerEndpoint(endpoint); err != nil {
							return nil, err
						}
					}
				}
			case strings.EqualFold(field, "service_destinations"):
				destinations, _ := value.(map[string]any)
				for _, value := range destinations {
					destination, _ := value.(map[string]any)
					for field, value := range destination {
						if strings.EqualFold(field, "uri") {
							endpoint, _ := value.(string)
							if err := validateContainerEndpoint(endpoint); err != nil {
								return nil, err
							}
						}
					}
				}
			}
		}
	}
	return assignments, nil
}

func validateContainerEndpoint(endpoint string) error {
	if endpoint != "" && (!strings.HasPrefix(endpoint, "unix:///") || strings.ContainsAny(endpoint, "\x00\n\r")) {
		return errors.New("saved non-Unix container API transport is unsupported with socket containment")
	}
	return nil
}
