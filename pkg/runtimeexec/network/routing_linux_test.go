//go:build linux

package network

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSavedContainerTransportAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, file, body string
		deny             bool
	}{
		{"podman-remote-ssh", ".config/containers/podman-connections.json", `{"Connection":{"Default":"host","Connections":{"host":{"URI":"ssh://operator@host/run/podman.sock"}}}}`, true},
		{"podman-remote-tcp", ".config/containers/podman-connections.json", `{"Connection":{"Connections":{"host":{"URI":"tcp://127.0.0.1:1234"}}}}`, true},
		{"podman-custom-unix", ".config/containers/podman-connections.json", `{"Connection":{"Connections":{"host":{"URI":"unix:///saved/custom.sock"}}}}`, false},
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
			if err := os.WriteFile(path, []byte(`{"URI":"tcp://outside:2375"}`), 0600); err != nil {
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
	if validateRoutingFile(path) == nil {
		t.Fatal("unbounded routing file accepted")
	}
	if validateRoutingFile(dir) == nil {
		t.Fatal("non-file configuration accepted")
	}
}

func TestRoutingAdmissionHelperUsesPrivateRequestEnvironment(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, "containers.conf")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"unix:///custom.sock", "ssh://outside", "tcp://outside:2375"} {
		if err := os.WriteFile(config, []byte("[engine.service_destinations.saved]\nuri='"+endpoint+"'\n"), 0600); err != nil {
			t.Fatal(err)
		}
		file, err := launchFile(memberLaunch{Request: Request{Env: []string{"HOME=" + home, "CONTAINERS_CONF=" + config}}})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "__network_runtime_check_routing_v1", "fixture")
		cmd.ExtraFiles = []*os.File{file}
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/must-not-select-ambient-home"}
		out, err := cmd.CombinedOutput()
		file.Close()
		if (err == nil) != strings.HasPrefix(endpoint, "unix:///") {
			t.Fatalf("%s: %s: %v", endpoint, out, err)
		}
	}
}
