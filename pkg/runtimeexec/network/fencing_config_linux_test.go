//go:build linux

package network

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/sys/unix"
)

type routingFixture struct {
	home   string
	system []string
	env    []string
	ref    Reference
}

func newRoutingFixture(t *testing.T) routingFixture {
	t.Helper()
	base := t.TempDir()
	f := routingFixture{home: filepath.Join(base, "home"), system: []string{filepath.Join(base, "usr", "containers"), filepath.Join(base, "etc", "containers")}, ref: Reference{Name: "fixture", Identity: strings.Repeat("a", 32), Generation: 1}}
	f.ref.Endpoint = Endpoint(f.ref.Name, f.ref.Identity)
	f.env = []string{"HOME=" + f.home}
	return f
}
func (f routingFixture) plan() (routingPlan, error) {
	return planContainerRoutingPaths(f.env, f.ref, uint32(os.Geteuid()), f.system)
}
func writeRoutingFixture(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
func routingFileAt(t *testing.T, plan routingPlan, path string) routingFile {
	t.Helper()
	for _, file := range plan.Files {
		if file.Path == path {
			return file
		}
	}
	t.Fatalf("missing projection %s", path)
	return routingFile{}
}
func routingJSONAt(t *testing.T, plan routingPlan, path string) map[string]any {
	t.Helper()
	doc, err := routingJSON(routingFileAt(t, plan, path).Data)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}
func containsRoutingPath(paths []string, path string) bool {
	for _, got := range paths {
		if got == path {
			return true
		}
	}
	return false
}

func TestRoutingProjectionPreservesConfigurationAndAuthentication(t *testing.T) {
	f := newRoutingFixture(t)
	xdg := filepath.Join(t.TempDir(), "xdg")
	docker := filepath.Join(t.TempDir(), "docker")
	conf := filepath.Join(t.TempDir(), "custom.conf")
	override := filepath.Join(t.TempDir(), "override.conf")
	connections := filepath.Join(t.TempDir(), "connections.json")
	f.env = append(f.env, "XDG_CONFIG_HOME="+xdg, "DOCKER_CONFIG="+docker, "CONTAINERS_CONF="+conf, "CONTAINERS_CONF_OVERRIDE="+override, "PODMAN_CONNECTIONS_CONF="+connections, "XDG_RUNTIME_DIR="+filepath.Join(f.home, "run"))
	originalConf := `[containers]
env = ["AUTH_PATH=/same/auth.json"]
[network]
network_backend = "netavark"
[engine]
remote = false
active_service = "chosen"
remote_uri = "unix:///tmp/legacy-podman.sock"
remote_identity = "/same/ssh/key"
cgroup_manager = "systemd"
[engine.service_destinations.chosen]
uri = "ssh://outside/run/podman.sock"
identity = "/same/ssh/key"
is_machine = true
`
	writeRoutingFixture(t, conf, originalConf)
	writeRoutingFixture(t, override, "[engine]\nremote = false\n")
	dropin := filepath.Join(f.system[1], "containers.conf.d", "20-connections.conf")
	writeRoutingFixture(t, dropin, "[engine.service_destinations.host]\nuri = 'unix:///tmp/saved-host.sock'\n")
	module := filepath.Join(xdg, "containers", "containers.conf.modules", "nested", "module")
	writeRoutingFixture(t, module, "[engine]\nremote=false\n[engine.service_destinations.module]\nuri='tcp://outside:1234'\n")
	originalConnections := `{"Connection":{"Default":"saved","Connections":{"saved":{"URI":"unix:///tmp/saved-podman.sock","Identity":"/same/key","IsMachine":true},"ssh":{"URI":"ssh://user@outside/run/podman.sock"}}},"Farm":{"Default":"pool","List":{"pool":["saved","ssh"]}}}`
	writeRoutingFixture(t, connections, originalConnections)
	originalDocker := `{"auths":{"registry.test":{"auth":"credential-material"}},"credsStore":"secretservice","credHelpers":{"other.test":"pass"},"currentContext":"outside","plugins":{"example":{"counter":9007199254740993}},"detachKeys":"ctrl-a,a"}`
	dockerConfig := filepath.Join(docker, "config.json")
	writeRoutingFixture(t, dockerConfig, originalDocker)
	contextID := fmt.Sprintf("%x", sha256.Sum256([]byte("outside")))
	contextPath := filepath.Join(docker, "contexts", "meta", contextID, "meta.json")
	originalContext := `{"Name":"outside","Metadata":{"Description":"keep this"},"Endpoints":{"docker":{"Host":"unix:///tmp/outside-docker.sock","SkipTLSVerify":true},"kubernetes":{"Host":"https://cluster.test"}}}`
	writeRoutingFixture(t, contextPath, originalContext)
	podmanAuth := filepath.Join(xdg, "containers", "auth.json")
	writeRoutingFixture(t, podmanAuth, "podman-auth-preserved")
	plan, err := f.plan()
	if err != nil {
		t.Fatal(err)
	}
	var gotConf map[string]any
	if err := toml.Unmarshal(routingFileAt(t, plan, conf).Data, &gotConf); err != nil {
		t.Fatal(err)
	}
	engine := gotConf["engine"].(map[string]any)
	if engine["remote"] != true || engine["active_service"] != "chosen" || engine["remote_uri"] != f.ref.Endpoint || engine["remote_identity"] != "" || engine["cgroup_manager"] != "systemd" {
		t.Fatalf("engine: %#v", engine)
	}
	if gotConf["network"].(map[string]any)["network_backend"] != "netavark" {
		t.Fatal("network config lost")
	}
	chosen := engine["service_destinations"].(map[string]any)["chosen"].(map[string]any)
	if chosen["uri"] != f.ref.Endpoint || chosen["identity"] != "" || chosen["is_machine"] != false {
		t.Fatalf("chosen: %#v", chosen)
	}
	saved := routingJSONAt(t, plan, connections)
	con := saved["Connection"].(map[string]any)
	if con["Default"] != "saved" {
		t.Fatal(con)
	}
	for _, dst := range con["Connections"].(map[string]any) {
		value := dst.(map[string]any)
		if value["URI"] != f.ref.Endpoint || value["Identity"] != "" || value["IsMachine"] != false {
			t.Fatal(value)
		}
	}
	var beforeSaved map[string]any
	_ = json.Unmarshal([]byte(originalConnections), &beforeSaved)
	if !reflect.DeepEqual(saved["Farm"], beforeSaved["Farm"]) {
		t.Fatal("farm config lost")
	}
	gotDocker := routingJSONAt(t, plan, dockerConfig)
	beforeDocker, err := routingJSON([]byte(originalDocker))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotDocker, beforeDocker) {
		t.Fatalf("Docker non-routing config changed: %#v", gotDocker)
	}
	gotContext := routingJSONAt(t, plan, contextPath)
	dockerEndpoint := gotContext["Endpoints"].(map[string]any)["docker"].(map[string]any)
	if dockerEndpoint["Host"] != f.ref.Endpoint || dockerEndpoint["SkipTLSVerify"] != false {
		t.Fatal(dockerEndpoint)
	}
	if gotContext["Endpoints"].(map[string]any)["kubernetes"].(map[string]any)["Host"] != "https://cluster.test" {
		t.Fatal("unrelated endpoint changed")
	}
	for _, path := range []string{"/tmp/legacy-podman.sock", "/tmp/saved-host.sock", "/tmp/saved-podman.sock", "/tmp/outside-docker.sock", "/run/docker.sock", filepath.Join(f.home, "run", "podman", "podman.sock")} {
		if !containsRoutingPath(plan.Sockets, path) {
			t.Errorf("missing socket %s", path)
		}
	}
	for _, path := range []string{filepath.Join(docker, "contexts", "meta"), filepath.Join(xdg, "containers", "containers.conf.modules"), filepath.Join(f.system[1], "containers.conf.d")} {
		if !containsRoutingPath(plan.ReadOnlyDirs, path) {
			t.Errorf("missing sealed directory %s", path)
		}
	}
	if !containsRoutingPath(plan.EmptyDirs, filepath.Join(docker, "contexts", "tls", contextID, "docker")) {
		t.Fatal("Docker TLS bundle not cleared")
	}
	for path, want := range map[string]string{conf: originalConf, connections: originalConnections, dockerConfig: originalDocker, contextPath: originalContext, podmanAuth: "podman-auth-preserved"} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Errorf("host config changed: %s: %v", path, err)
		}
	}
	for _, file := range plan.Files {
		if file.Path == podmanAuth {
			t.Fatal("Podman auth projected")
		}
		if file.Path == conf && file.Mode != 0600 {
			t.Errorf("existing private configuration mode changed to %o", file.Mode)
		}
	}
	routingFileAt(t, plan, module)
	routingFileAt(t, plan, filepath.Join(f.home, ".docker", "config.json"))
	routingFileAt(t, plan, filepath.Join(f.home, ".config", "containers", "podman-connections.json"))
	routingFileAt(t, plan, filepath.Join(xdg, "containers", "containers.conf"))
}

func TestRoutingProjectionDefaultsAndDeterminism(t *testing.T) {
	f := newRoutingFixture(t)
	first, err := f.plan()
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.plan()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("nondeterministic plan")
	}
	for _, file := range first.Files {
		if strings.HasSuffix(file.Path, "containers.conf") {
			if file.Mode != 0644 {
				t.Errorf("missing TOML must remain caller readable, got %o", file.Mode)
			}
			var conf map[string]any
			if err := toml.Unmarshal(file.Data, &conf); err != nil {
				t.Fatal(err)
			}
			engine := conf["engine"].(map[string]any)
			if engine["remote"] != true || engine["remote_uri"] != f.ref.Endpoint {
				t.Fatal(engine)
			}
		}
	}
	saved := routingJSONAt(t, first, filepath.Join(f.home, ".config", "containers", "podman-connections.json"))
	con := saved["Connection"].(map[string]any)
	if con["Default"] != runtimeConnectionName || con["Connections"].(map[string]any)[runtimeConnectionName].(map[string]any)["URI"] != f.ref.Endpoint {
		t.Fatal(con)
	}
	if _, err := os.Stat(f.home); !os.IsNotExist(err) {
		t.Fatal("planner created a missing host directory")
	}
}

func TestRoutingProjectionRejectsMalformedAndUnsupportedSelectors(t *testing.T) {
	for _, tc := range []struct{ name, kind, contents string }{
		{"malformed TOML", "toml", "[engine\n"},
		{"wrong table", "toml", "engine = 'remote'"},
		{"wrong remote", "toml", "[engine]\nremote='false'"},
		{"wrong legacy identity", "toml", "[engine]\nremote_identity=false"},
		{"unknown destination selector", "toml", "[engine.service_destinations.test]\nuri='unix:///tmp/x'\ntls=true"},
		{"relative Unix destination", "toml", "[engine.service_destinations.test]\nuri='unix://relative/x'"},
		{"empty JSON", "docker", ""},
		{"malformed JSON", "docker", "{"},
		{"duplicate auth keys", "docker", `{"auths":{},"auths":{}}`},
		{"case alias ambiguity", "docker", `{"currentContext":"one","CurrentContext":"two"}`},
		{"trailing JSON", "docker", `{} {}`},
		{"null JSON", "docker", `null`},
		{"bad current context", "docker", `{"currentContext":5}`},
		{"bad connection URI", "connections", `{"Connection":{"Connections":{"x":{"URI":3}}}}`},
		{"bad connection Identity", "connections", `{"Connection":{"Connections":{"x":{"Identity":false}}}}`},
		{"unknown saved selector", "connections", `{"Connection":{"Connections":{"x":{"URI":"ssh://host/x","TLS":true}}}}`},
		{"ambiguous saved selector", "connections", `{"Connection":{"Connections":{"x":{"URI":"ssh://one/x","uri":"ssh://two/x"}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRoutingFixture(t)
			path := filepath.Join(f.home, ".config", "containers", "containers.conf")
			if tc.kind == "docker" {
				path = filepath.Join(f.home, ".docker", "config.json")
			}
			if tc.kind == "connections" {
				path = filepath.Join(f.home, ".config", "containers", "podman-connections.json")
			}
			writeRoutingFixture(t, path, tc.contents)
			if _, err := f.plan(); err == nil {
				t.Fatal("accepted unsafe configuration")
			}
		})
	}
	for _, selector := range []string{"HOME=relative", "XDG_CONFIG_HOME=relative", "DOCKER_CONFIG=relative", "CONTAINERS_CONF=relative", "CONTAINERS_CONF_MODULES=module", "PODMAN_CONNECTIONS_CONF=relative", "XDG_RUNTIME_DIR=relative"} {
		t.Run(selector, func(t *testing.T) {
			f := newRoutingFixture(t)
			f.env = append(f.env, selector)
			if _, err := f.plan(); err == nil {
				t.Fatal("accepted unsupported selector")
			}
		})
	}
}

func TestRoutingProjectionRejectsUnsafeFilesystemLayouts(t *testing.T) {
	for _, kind := range []string{"file symlink", "parent symlink", "hardlink", "fifo", "directory", "module symlink", "dropin symlink", "Docker malformed layout", "Docker mismatched context", "Docker TLS symlink"} {
		t.Run(kind, func(t *testing.T) {
			f := newRoutingFixture(t)
			target := filepath.Join(f.home, ".config", "containers", "containers.conf")
			other := filepath.Join(t.TempDir(), "other")
			writeRoutingFixture(t, other, "[engine]\nremote=false\n")
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "file symlink":
				if err := os.Symlink(other, target); err != nil {
					t.Fatal(err)
				}
			case "parent symlink":
				dir := filepath.Join(t.TempDir(), "real")
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(f.home, "alias")
				if err := os.Symlink(dir, path); err != nil {
					t.Fatal(err)
				}
				f.env = append(f.env, "DOCKER_CONFIG="+path)
			case "hardlink":
				if err := os.Link(other, target); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(target, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
			case "module symlink":
				path := filepath.Join(filepath.Dir(target), "containers.conf.modules")
				if err := os.Symlink(filepath.Dir(other), path); err != nil {
					t.Fatal(err)
				}
			case "dropin symlink":
				path := filepath.Join(target+".d", "override.conf")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, path); err != nil {
					t.Fatal(err)
				}
			case "Docker malformed layout":
				writeRoutingFixture(t, filepath.Join(f.home, ".docker", "contexts", "meta", "unexpected"), "ignored?")
			case "Docker mismatched context":
				writeRoutingFixture(t, filepath.Join(f.home, ".docker", "contexts", "meta", strings.Repeat("a", 64), "meta.json"), `{"Name":"test","Endpoints":{"docker":{"Host":"unix:///tmp/test"}}}`)
			case "Docker TLS symlink":
				id := fmt.Sprintf("%x", sha256.Sum256([]byte("test")))
				writeRoutingFixture(t, filepath.Join(f.home, ".docker", "contexts", "meta", id, "meta.json"), `{"Name":"test","Endpoints":{"docker":{"Host":"unix:///tmp/test"}}}`)
				path := filepath.Join(f.home, ".docker", "contexts", "tls", id, "docker")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Dir(other), path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.plan(); err == nil {
				t.Fatal("unsafe layout accepted")
			}
		})
	}
}

func TestRoutingProjectionRejectsWrongCallerAndMissingExplicitConfig(t *testing.T) {
	f := newRoutingFixture(t)
	if _, err := planContainerRoutingPaths(f.env, f.ref, uint32(os.Geteuid())+1, f.system); err == nil {
		t.Fatal("read configuration with wrong authority")
	}
	f.env = append(f.env, "CONTAINERS_CONF="+filepath.Join(f.home, "absent.conf"))
	if _, err := f.plan(); err == nil {
		t.Fatal("missing explicit configuration accepted")
	}
}

func TestRoutingProjectionRerenderIsIdempotent(t *testing.T) {
	f := newRoutingFixture(t)
	conf := filepath.Join(f.home, ".config", "containers", "containers.conf")
	writeRoutingFixture(t, conf, "[engine]\nactive_service='named'\nremote=false\n[engine.service_destinations.named]\nuri='unix:///tmp/original-host.sock'\nidentity='/tmp/key'\n")
	writeRoutingFixture(t, filepath.Join(f.system[1], "containers.conf.d", "10-extra.conf"), "[engine]\nremote_uri='tcp://outside:8888'\n")
	id := fmt.Sprintf("%x", sha256.Sum256([]byte("named")))
	writeRoutingFixture(t, filepath.Join(f.home, ".docker", "config.json"), `{"currentContext":"named","auths":{"registry":{"auth":"retained"}}}`)
	writeRoutingFixture(t, filepath.Join(f.home, ".docker", "contexts", "meta", id, "meta.json"), `{"Name":"named","Endpoints":{"docker":{"Host":"tcp://outside:2376","SkipTLSVerify":true}}}`)
	first, err := f.plan()
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range first.Files {
		if err := os.MkdirAll(filepath.Dir(file.Path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file.Path, file.Data, file.Mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(file.Path, file.Mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range first.EmptyDirs {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	second, err := f.plan()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Files, second.Files) {
		t.Fatal("projected file bytes change on verification pass")
	}
	if !reflect.DeepEqual(first.ReadOnlyDirs, second.ReadOnlyDirs) || !reflect.DeepEqual(first.EmptyDirs, second.EmptyDirs) {
		t.Fatal("projected directory inventory changes on verification pass")
	}
	if !containsRoutingPath(first.Sockets, "/tmp/original-host.sock") || containsRoutingPath(second.Sockets, "/tmp/original-host.sock") {
		t.Fatal("original socket inventory must be retained separately for post-mount verification")
	}
	if !containsRoutingPath(second.Sockets, strings.TrimPrefix(f.ref.Endpoint, "unix://")) {
		t.Fatal("verification does not rediscover the pinned endpoint")
	}
}
