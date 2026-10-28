//go:build linux

package network

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSavedContainerTransportAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, file, body string
		deny             bool
	}{
		{"podman-remote-ssh", ".config/containers/podman-connections.json", `{"Connection":{"Default":"host","Connections":{"host":{"URI":"ssh://operator@host/run/podman.sock"}}}}`, true},
		{"podman-remote-tcp", ".config/containers/podman-connections.json", `{"Connection":{"Connections":{"host":{"URI":"tcp://127.0.0.1:1234"}}}}`, true},
		{"podman-custom-unix", ".config/containers/podman-connections.json", `{"Connection":{"Connections":{"host":{"URI":"unix:///saved/custom.sock"}}}}`, false},
		{"podman-engine-env-tcp", ".config/containers/containers.conf", "[engine]\nenv=['CONTAINER_HOST=tcp://outside:2375']\n", true},
		{"podman-engine-env-ssh", ".config/containers/containers.conf", "[engine]\nenv=['CONTAINER_HOST=ssh://outside']\n", true},
		{"podman-engine-env-unix", ".config/containers/containers.conf", "[engine]\nenv=['CONTAINER_HOST=unix:///saved/custom.sock']\n", false},
		{"podman-engine-env-provider-assignments", ".config/containers/containers.conf", "[engine]\nenv=['HTTPS_PROXY=https://proxy', 'GIT_SSH_COMMAND=ssh provider', 'PROVIDER_URL=https://provider?key=value']\n", false},
		{"podman-mixed-case-engine-env-tcp", ".config/containers/containers.conf", "[Engine]\nEnv=['CONTAINER_HOST=tcp://outside:2375']\n", true},
		{"podman-mixed-case-engine-env-unix", ".config/containers/containers.conf", "[eNgInE]\nEnV=['CONTAINER_HOST=unix:///saved/custom.sock', 'PROVIDER_URI=ssh://provider', {append=true}]\n", false},
		{"podman-mixed-case-destination-ssh", ".config/containers/containers.conf", "[Engine.Service_Destinations.saved]\nUrI='ssh://outside'\n", true},
		{"podman-mixed-case-destination-unix", ".config/containers/containers.conf", "[eNgInE.SERVICE_DESTINATIONS.saved]\nuRi='unix:///saved/custom.sock'\n", false},
		{"podman-container-env-is-unrelated", ".config/containers/containers.conf", "[containers]\nenv=['CONTAINER_HOST=tcp://inside-container:2375']\n[engine]\nuri='provider-metadata'\n", false},
		{"podman-connection-metadata-uri", ".config/containers/podman-connections.json", `{"URI":"provider-metadata","Connection":{"Connections":{"saved":{"URI":"unix:///safe.sock","Metadata":{"Uri":"ci-job"}}}},"Farm":{"Uri":"farm-metadata"}}`, false},
		{"podman-connection-mixed-case-uri", ".config/containers/podman-connections.json", `{"connection":{"connections":{"saved":{"uRi":"tcp://outside:2375"}}}}`, true},
		{"docker-http-headers-uri", ".docker/config.json", `{"HttpHeaders":{"Uri":"ci-job-123","Host":"provider-metadata"},"Uri":"unrelated"}`, false},
		{"docker-context-metadata-uri", ".docker/contexts/meta/custom/meta.json", `{"Uri":"ci-job-123","Metadata":{"Uri":"context-metadata"},"Endpoints":{"docker":{"Host":"unix:///safe.sock","Uri":"endpoint-metadata"},"kubernetes":{"Uri":"unrelated","Host":{"native":"metadata"}},"Docker":{"Host":"tcp://unselected-metadata:2375"}}}`, false},
		{"docker-context-unsafe-with-metadata", ".docker/contexts/meta/custom/meta.json", `{"Uri":"ci-job-123","Endpoints":{"docker":{"Host":"ssh://outside"}}}`, true},
		{"podman-local-selection", ".config/containers/containers.conf", "[engine]\nremote=false\n", false},
		{"podman-remote-selection", ".config/containers/containers.conf", "[engine]\nremote=true\nactive_service='host'\n[engine.service_destinations.host]\nuri='unix:///saved/custom.sock'\n", false},
		{"podman-saved-toml-tcp", ".config/containers/containers.conf", "[engine.service_destinations.host]\nuri='tcp://host:1234'\n", true},
		{"podman-dropin", ".config/containers/containers.conf.d/99-remote.conf", "[engine.service_destinations.host]\nuri='ssh://host'\n", true},
		{"docker-native-default", ".docker/config.json", `{"currentContext":"default","credsStore":"native"}`, false},
		{"docker-saved-context-ssh", ".docker/contexts/meta/custom/meta.json", `{"Name":"host","Endpoints":{"docker":{"Host":"ssh://host"}}}`, true},
		{"docker-unselected-context-tcp", ".docker/contexts/meta/custom/meta.json", `{"Name":"host","Endpoints":{"docker":{"Host":"tcp://host:2375"}}}`, true},
		{"docker-custom-unix", ".docker/contexts/meta/custom/meta.json", `{"Name":"host","Endpoints":{"docker":{"Host":"unix:///saved/custom.sock"}}}`, false},
		{"malformed-saved-routing", ".config/containers/podman-connections.json", "{", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, tc.file)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			for _, routing := range [][]string{nil, {"DOCKER_HOST=unix:///selected", "CONTAINER_HOST=unix:///selected"}, {"DOCKER_HOST=unix:///overwritten", "CONTAINER_HOST=unix:///overwritten"}} {
				env := append([]string{"HOME=" + home}, routing...)
				err := validateSavedRoutingWithDefaults(env, home, nil)
				if (err != nil) != tc.deny {
					t.Fatalf("saved route acceptance with %v: %v", routing, err)
				}
			}
		})
	}
}

func TestSavedRoutingOverridesAndBounds(t *testing.T) {
	dir := t.TempDir()
	for _, key := range []string{"CONTAINERS_CONF", "CONTAINERS_CONF_OVERRIDE", "PODMAN_CONNECTIONS_CONF", "XDG_CONFIG_HOME", "DOCKER_CONFIG"} {
		t.Run(key, func(t *testing.T) {
			path := filepath.Join(dir, key+".json")
			value := path
			if key == "XDG_CONFIG_HOME" {
				value = filepath.Join(dir, key)
				path = filepath.Join(value, "containers/podman-connections.json")
			}
			if key == "DOCKER_CONFIG" {
				value = filepath.Join(dir, key)
				path = filepath.Join(value, "contexts/meta/custom/meta.json")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			body := `{"Connection":{"Connections":{"saved":{"URI":"tcp://outside:2375"}}}}`
			if key == "DOCKER_CONFIG" {
				body = `{"Endpoints":{"docker":{"Host":"tcp://outside:2375"}}}`
			}
			if key == "CONTAINERS_CONF" || key == "CONTAINERS_CONF_OVERRIDE" {
				body = "[engine.service_destinations.saved]\nuri='tcp://outside:2375'\n"
			}
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if err := validateSavedRoutingWithDefaults([]string{key + "=" + value}, dir, nil); err == nil {
				t.Fatal("unsupported saved routing accepted")
			}
		})
	}
	path := filepath.Join(dir, "large.json")
	if err := os.WriteFile(path, []byte(strings.Repeat(" ", (4<<20)+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateRoutingFile(path, podmanConnections); err == nil {
		t.Fatal("unbounded routing file accepted")
	}
	if _, err := validateRoutingFile(dir, podmanConnections); err == nil {
		t.Fatal("non-file configuration accepted")
	}
}

// Decoding and saved-file admission share this exact production path. Supplying
// known defaults keeps parser fixtures independent of the host account's files.
func routingPayloadForTest(req Request, home string) error {
	file, err := launchFile(memberLaunch{Request: req})
	if err != nil {
		return err
	}
	defer file.Close()
	return checkRoutingPayload(file, func(env []string) error {
		return validateSavedRoutingWithDefaults(env, home, nil)
	})
}

func TestRoutingAdmissionHelperUsesPrivateRequestEnvironment(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, "containers.conf")
	t.Setenv("HOME", "/must-not-select-ambient-home")
	t.Setenv("CONTAINERS_CONF", "/must-not-select-ambient-config")
	for _, endpoint := range []string{"unix:///custom.sock", "ssh://outside", "tcp://outside:2375"} {
		if err := os.WriteFile(config, []byte("[engine.service_destinations.saved]\nuri='"+endpoint+"'\n"), 0600); err != nil {
			t.Fatal(err)
		}
		err := routingPayloadForTest(Request{Env: []string{"HOME=" + home, "CONTAINERS_CONF=" + config}}, home)
		if (err == nil) != strings.HasPrefix(endpoint, "unix:///") {
			t.Fatalf("%s: %v", endpoint, err)
		}
	}
}

func TestRoutingAdmissionHelperRequiresSealedPayload(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(t.TempDir(), "unsealed")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := json.NewEncoder(file).Encode(memberLaunch{}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "__network_runtime_check_routing_v1", "fixture")
	cmd.ExtraFiles = []*os.File{file}
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "invalid sealed launch payload") {
		t.Fatalf("production helper accepted unsealed payload: %s: %v", out, err)
	}
}

func TestSavedRoutingSourceFormats(t *testing.T) {
	for _, key := range []string{"CONTAINERS_CONF", "CONTAINERS_CONF_OVERRIDE", "PODMAN_CONNECTIONS_CONF"} {
		for _, suffix := range []string{"", ".json", ".toml"} {
			for _, endpoint := range []string{"unix:///safe.sock", "tcp://outside:2375", "ssh://outside"} {
				t.Run(key+suffix+"/"+endpoint, func(t *testing.T) {
					home := t.TempDir()
					path := filepath.Join(home, "custom"+suffix)
					body := "[engine.service_destinations.saved]\nuri='" + endpoint + "'\n"
					malformed := "[engine"
					if key == "PODMAN_CONNECTIONS_CONF" {
						body = fmt.Sprintf(`{"Connection":{"Connections":{"saved":{"URI":%q}}}}`, endpoint)
						malformed = "{"
					}
					env := []string{"HOME=" + home, key + "=" + path}
					if err := os.WriteFile(path, []byte(body), 0600); err != nil {
						t.Fatal(err)
					}
					err := validateSavedRoutingWithDefaults(env, home, nil)
					if strings.HasPrefix(endpoint, "unix:///") {
						if err != nil {
							t.Fatalf("native-format safe route rejected: %v", err)
						}
					} else if err == nil || !strings.Contains(err.Error(), "saved non-Unix") {
						t.Fatalf("non-Unix route not decoded in source format: %v", err)
					}
					if err := os.WriteFile(path, []byte(malformed), 0600); err != nil {
						t.Fatal(err)
					}
					if err := validateSavedRoutingWithDefaults(env, home, nil); err == nil || !strings.Contains(err.Error(), "cannot parse") {
						t.Fatalf("malformed source format accepted: %v", err)
					}
				})
			}
		}
	}
}

func TestSavedRoutingFilteredOverridesRevealDefaults(t *testing.T) {
	for _, location := range []string{"system", "system-dropin", "account-containers", "account-dropin", "account-connections", "account-docker", "home-containers", "home-docker"} {
		t.Run(location, func(t *testing.T) {
			root := t.TempDir()
			account := filepath.Join(root, "account")
			home := filepath.Join(root, "home")
			system := filepath.Join(root, "system")
			xdg := filepath.Join(root, "xdg")
			docker := filepath.Join(root, "docker")
			safe := filepath.Join(root, "selected.conf")
			safeJSON := filepath.Join(root, "selected-connections")
			for path, body := range map[string]string{safe: "[engine]\nremote=false\n", safeJSON: `{}`} {
				if err := os.WriteFile(path, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			env := []string{"HOME=" + home, "XDG_CONFIG_HOME=" + xdg, "DOCKER_CONFIG=" + docker, "CONTAINERS_CONF=" + safe, "CONTAINERS_CONF_OVERRIDE=" + safe, "PODMAN_CONNECTIONS_CONF=" + safeJSON}
			// Safe overrides and absent defaults remain compatible.
			if err := validateSavedRoutingWithDefaults(env, account, []string{system}); err != nil {
				t.Fatal(err)
			}
			paths := map[string]string{
				"system":              filepath.Join(system, "containers.conf"),
				"system-dropin":       filepath.Join(system, "containers.conf.d/99-remote.conf"),
				"account-containers":  filepath.Join(account, ".config/containers/containers.conf"),
				"account-dropin":      filepath.Join(account, ".config/containers/containers.conf.d/99-remote.conf"),
				"account-connections": filepath.Join(account, ".config/containers/podman-connections.json"),
				"account-docker":      filepath.Join(account, ".docker/contexts/meta/unselected/meta.json"),
				"home-containers":     filepath.Join(home, ".config/containers/containers.conf"),
				"home-docker":         filepath.Join(home, ".docker/contexts/meta/unselected/meta.json"),
			}
			path := paths[location]
			body := "[engine]\nenv=['CONTAINER_HOST=ssh://outside']\n"
			if location == "account-connections" {
				body = `{"Connection":{"Connections":{"saved":{"URI":"tcp://outside:2375"}}}}`
			} else if location == "account-docker" || location == "home-docker" {
				body = `{"Endpoints":{"docker":{"Host":"tcp://outside:2375"}}}`
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if err := validateSavedRoutingWithDefaults(env, account, []string{system}); err == nil || !strings.Contains(err.Error(), "saved non-Unix") {
				t.Fatalf("filtered override can expose unchecked default: %v", err)
			}
		})
	}
}

func TestSavedRoutingDiscoveryErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission fixture requires unprivileged caller authority")
	}
	for _, relative := range []string{".config/containers/containers.conf.d", ".docker/contexts/meta"} {
		for _, mode := range []os.FileMode{0100, 0000} {
			t.Run(fmt.Sprintf("%s/%o", relative, mode), func(t *testing.T) {
				home := t.TempDir()
				path := filepath.Join(home, relative)
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
				// A selected context remains directly readable through traverse-only
				// metadata parents; enumeration must still inspect unselected peers.
				if strings.HasSuffix(path, "meta") {
					child := filepath.Join(path, "unselected/meta.json")
					if err := os.MkdirAll(filepath.Dir(child), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(child, []byte(`{"Endpoints":{"docker":{"Host":"ssh://outside"}}}`), 0600); err != nil {
						t.Fatal(err)
					}
				}
				t.Cleanup(func() {
					if err := os.Chmod(path, 0700); err != nil {
						t.Error(err)
					}
				})
				if err := os.Chmod(path, mode); err != nil {
					t.Fatal(err)
				}
				if mode == 0100 && strings.HasSuffix(path, "meta") {
					if _, err := os.ReadFile(filepath.Join(path, "unselected/meta.json")); err != nil {
						t.Fatalf("traverse-only context is not natively readable: %v", err)
					}
				}
				err := validateSavedRoutingWithDefaults([]string{"HOME=" + home}, home, nil)
				if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), path) {
					t.Fatalf("discovery hid caller permission error: %v", err)
				}
			})
		}
	}
	// ENOTDIR is an I/O/discovery failure, not an absent optional directory.
	home := t.TempDir()
	path := filepath.Join(home, ".docker/contexts")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateSavedRoutingWithDefaults([]string{"HOME=" + home}, home, nil); !errors.Is(err, unix.ENOTDIR) {
		t.Fatalf("discovery hid non-directory failure: %v", err)
	}
}

func TestRoutingAdmissionNativeAggregateAndMembership(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, "containers.conf")
	args := make([]string, 20)
	for i := range args {
		args[i] = strings.Repeat("x", 60<<10)
	}
	env := []string{"HOME=" + home, "CONTAINERS_CONF=" + config}
	// Prove the aggregate is accepted by the host's native exec limits before
	// checking the larger serialized representation through routing admission.
	cmd := exec.Command("/bin/true", args...)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native aggregate is not executable: %s: %v", out, err)
	}
	req := Request{Path: "/bin/true", Args: args, Env: env}
	file, err := launchFile(memberLaunch{Request: req})
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() <= 1<<20 {
		t.Fatalf("fixture does not cross old decoder limit: %v: %v", info, err)
	}
	for _, endpoint := range []string{"unix:///safe.sock", "ssh://outside"} {
		if err := os.WriteFile(config, []byte("[engine]\nenv=['CONTAINER_HOST="+endpoint+"']\n"), 0600); err != nil {
			t.Fatal(err)
		}
		err := checkRoutingPayload(file, func(env []string) error { return validateSavedRoutingWithDefaults(env, home, nil) })
		if strings.HasPrefix(endpoint, "unix:///") {
			if err != nil {
				t.Fatalf("native-valid aggregate rejected: %v", err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "saved non-Unix") {
			t.Fatalf("aggregate route admission bypassed or failed decoding: %v", err)
		}
	}

	s, socket := fixtureService(t)
	launches := 0
	s.memberCommand = func(string) *exec.Cmd {
		launches++
		return exec.Command("/bin/false")
	}
	checks := 0
	s.checkRouting = func(req Request, _ *unix.Ucred, _ []uint32) error {
		checks++
		return routingPayloadForTest(req, home)
	}
	req.Operation, req.Reference, req.ID = "exec", s.status.Reference, randomID()
	_, out := requestAt(t, socket, req, true)
	if out.Error == "" || !strings.Contains(out.Error, "saved non-Unix") {
		t.Fatalf("service ignored routing rejection: %+v", out)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if checks != 1 || launches != 0 || len(s.status.Members) != 0 {
		t.Fatalf("rejected routing reached membership: checks=%d launches=%d members=%d", checks, launches, len(s.status.Members))
	}
	if _, err := os.Stat(filepath.Join(s.dir, "status.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected routing wrote durable membership: %v", err)
	}
}

func TestSavedRoutingRootlessCallerSources(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless defaults apply to non-root caller")
	}
	for _, relative := range []string{"containers.rootless.conf", "containers.rootless.conf.d/10-caller.conf", fmt.Sprintf("containers.rootless.conf.d/%d/20-caller.conf", os.Getuid())} {
		for _, endpoint := range []string{"unix:///caller.sock", "tcp://outside:2375", "ssh://outside"} {
			t.Run(relative+"/"+endpoint, func(t *testing.T) {
				home := t.TempDir()
				system := t.TempDir()
				path := filepath.Join(system, relative)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("[Engine.Service_Destinations.saved]\nUri='"+endpoint+"'\n"), 0600); err != nil {
					t.Fatal(err)
				}
				// An unsafe file for another UID is unrelated to this caller.
				other := filepath.Join(system, fmt.Sprintf("containers.rootless.conf.d/%d/99-other.conf", os.Getuid()+1))
				if err := os.MkdirAll(filepath.Dir(other), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(other, []byte("[engine]\nenv=['CONTAINER_HOST=ssh://other-caller']\n"), 0600); err != nil {
					t.Fatal(err)
				}
				err := validateSavedRoutingWithDefaults([]string{"HOME=" + home}, home, []string{system})
				if strings.HasPrefix(endpoint, "unix:///") {
					if err != nil {
						t.Fatalf("safe caller source rejected: %v", err)
					}
				} else if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "saved non-Unix") {
					t.Fatalf("caller source escaped validation: %v", err)
				}
			})
		}
	}

	for _, relative := range []string{"containers.rootless.conf.d", fmt.Sprintf("containers.rootless.conf.d/%d", os.Getuid())} {
		t.Run("unreadable/"+relative, func(t *testing.T) {
			home, system := t.TempDir(), t.TempDir()
			path := filepath.Join(system, relative)
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Chmod(path, 0700); err != nil {
					t.Error(err)
				}
			})
			if err := os.Chmod(path, 0100); err != nil {
				t.Fatal(err)
			}
			if err := validateSavedRoutingWithDefaults(nil, home, []string{system}); !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), path) {
				t.Fatalf("rootless discovery suppressed caller error: %v", err)
			}
		})
	}
}

func TestSavedRoutingEngineEnvironmentConnectionSelectors(t *testing.T) {
	for _, key := range []string{"PODMAN_CONNECTIONS_CONF", "XDG_CONFIG_HOME", "HOME"} {
		for _, endpoint := range []string{"unix:///selected.sock", "tcp://outside:2375", "ssh://outside"} {
			t.Run(key+"/"+endpoint, func(t *testing.T) {
				home := t.TempDir()
				saved := t.TempDir()
				value, path := saved, filepath.Join(saved, "containers/podman-connections.json")
				if key == "HOME" {
					path = filepath.Join(saved, ".config/containers/podman-connections.json")
				} else if key == "PODMAN_CONNECTIONS_CONF" {
					path = filepath.Join(saved, "connections.arbitrary-suffix")
					value = path
				}
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"Connection":{"Default":"saved","Connections":{"saved":{"URI":%q}}}}`, endpoint)), 0600); err != nil {
					t.Fatal(err)
				}
				config := filepath.Join(home, "containers.conf")
				if err := os.WriteFile(config, []byte(fmt.Sprintf("[Engine]\nEnv=[%q]\n", key+"="+value)), 0600); err != nil {
					t.Fatal(err)
				}
				safe := filepath.Join(home, "safe-connections.json")
				if err := os.WriteFile(safe, []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
				for _, masks := range [][]string{nil, {"PODMAN_CONNECTIONS_CONF=" + safe, "XDG_CONFIG_HOME=" + home}} {
					env := append([]string{"HOME=" + home, "CONTAINERS_CONF=" + config}, masks...)
					err := routingPayloadForTest(Request{Env: env}, home)
					if strings.HasPrefix(endpoint, "unix:///") {
						if err != nil {
							t.Fatalf("safe engine-selected connection rejected: %v", err)
						}
					} else if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "saved non-Unix") {
						t.Fatalf("saved connection selector escaped admission: %v", err)
					}
				}
			})
		}
	}
}

func TestRoutingSavedConnectionSelectorPrecedesMembership(t *testing.T) {
	home := t.TempDir()
	selected := filepath.Join(home, "saved-connections")
	if err := os.WriteFile(selected, []byte(`{"Connection":{"Connections":{"saved":{"URI":"ssh://outside"}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, "containers.conf")
	if err := os.WriteFile(config, []byte(fmt.Sprintf("[Engine]\nEnv=[%q]\n", "PODMAN_CONNECTIONS_CONF="+selected)), 0600); err != nil {
		t.Fatal(err)
	}
	s, socket := fixtureService(t)
	launches := 0
	s.memberCommand = func(string) *exec.Cmd {
		launches++
		return exec.Command("/bin/false")
	}
	s.checkRouting = func(req Request, _ *unix.Ucred, _ []uint32) error { return routingPayloadForTest(req, home) }
	_, out := requestAt(t, socket, Request{Operation: "exec", Reference: s.status.Reference, ID: randomID(), Path: "/bin/true", Env: []string{"HOME=" + home, "CONTAINERS_CONF=" + config}}, true)
	if out.Error == "" || !strings.Contains(out.Error, "saved non-Unix") {
		t.Fatalf("engine-selected connection reached execution: %+v", out)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if launches != 0 || len(s.status.Members) != 0 {
		t.Fatalf("engine-selected connection reached membership: launches=%d members=%d", launches, len(s.status.Members))
	}
	if _, err := os.Stat(filepath.Join(s.dir, "status.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected selector wrote durable membership: %v", err)
	}
}
