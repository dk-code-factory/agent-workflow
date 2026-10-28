//go:build linux

package network

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
)

type routingAPIRecorder struct {
	mu        sync.Mutex
	mutations []string
}

func newRoutingAPIRecorder(t *testing.T, path string) *routingAPIRecorder {
	t.Helper()
	r := &routingAPIRecorder{}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer req.Body.Close()
		_, _ = io.Copy(io.Discard, req.Body)
		if req.Method != http.MethodGet {
			r.mu.Lock()
			r.mutations = append(r.mutations, req.Method+" "+req.URL.Path)
			r.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"Id":"recorded","State":{"Running":true}}`)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return r
}
func (r *routingAPIRecorder) changes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.mutations...)
}

// This client runs in an independent process and reads only the saved fallback
// files a CLI would read after its routing environment has been removed. It does
// not use the routing planner, routingJSON, or any emitted-plan lookup helpers.
func TestRoutingSavedClientFixture(t *testing.T) {
	kind := os.Getenv("AW_SAVED_ROUTING_CLIENT")
	if kind == "" {
		return
	}
	if err := routingSavedClient(kind); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(125)
	}
	os.Exit(0)
}

func routingSavedClient(kind string) error {
	for _, key := range []string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "CONTAINER_HOST", "CONTAINER_CONNECTION", "CONTAINERS_CONF", "CONTAINERS_CONF_OVERRIDE", "CONTAINERS_CONF_MODULES", "PODMAN_CONNECTIONS_CONF", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR", ReferenceEnvironment} {
		if _, exists := os.LookupEnv(key); exists {
			return fmt.Errorf("fixture retained routing selector %s", key)
		}
	}
	home := os.Getenv("HOME")
	endpoint := ""
	switch kind {
	case "podman-conf":
		data, err := os.ReadFile(filepath.Join(home, ".config", "containers", "containers.conf"))
		if err != nil {
			return err
		}
		var conf struct {
			Engine struct {
				Remote              bool   `toml:"remote"`
				RemoteURI           string `toml:"remote_uri"`
				ActiveService       string `toml:"active_service"`
				ServiceDestinations map[string]struct {
					URI string `toml:"uri"`
				} `toml:"service_destinations"`
			} `toml:"engine"`
		}
		if err := toml.Unmarshal(data, &conf); err != nil {
			return err
		}
		if !conf.Engine.Remote {
			return fmt.Errorf("saved configuration permits local execution")
		}
		endpoint = conf.Engine.RemoteURI
		if conf.Engine.ActiveService != "" {
			endpoint = conf.Engine.ServiceDestinations[conf.Engine.ActiveService].URI
		}
	case "podman-connections":
		data, err := os.ReadFile(filepath.Join(home, ".config", "containers", "podman-connections.json"))
		if err != nil {
			return err
		}
		var conf struct {
			Connection struct {
				Default     string
				Connections map[string]struct{ URI string }
			}
		}
		if err := json.Unmarshal(data, &conf); err != nil {
			return err
		}
		endpoint = conf.Connection.Connections[conf.Connection.Default].URI
	case "docker-context":
		data, err := os.ReadFile(filepath.Join(home, ".docker", "config.json"))
		if err != nil {
			return err
		}
		var conf struct{ CurrentContext string }
		if err := json.Unmarshal(data, &conf); err != nil {
			return err
		}
		id := fmt.Sprintf("%x", sha256.Sum256([]byte(conf.CurrentContext)))
		data, err = os.ReadFile(filepath.Join(home, ".docker", "contexts", "meta", id, "meta.json"))
		if err != nil {
			return err
		}
		var meta struct {
			Endpoints map[string]struct{ Host string }
		}
		if err := json.Unmarshal(data, &meta); err != nil {
			return err
		}
		endpoint = meta.Endpoints["docker"].Host
	default:
		return fmt.Errorf("unknown saved client %s", kind)
	}
	if !strings.HasPrefix(endpoint, "unix://") {
		return fmt.Errorf("saved client requires a Unix API endpoint: %s", endpoint)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", strings.TrimPrefix(endpoint, "unix://"))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	for _, operation := range []struct{ method, path, body string }{
		{http.MethodPost, "/containers/create?name=saved-routing-fixture", `{"Image":"fixture-only"}`},
		{http.MethodPost, "/containers/recorded/start", ""},
		{http.MethodGet, "/containers/recorded/json", ""},
		{http.MethodDelete, "/containers/recorded?force=true", ""},
	} {
		req, err := http.NewRequest(operation.method, "http://container-api"+operation.path, bytes.NewBufferString(operation.body))
		if err != nil {
			return err
		}
		response, err := client.Do(req)
		if err != nil {
			return err
		}
		_, readErr := io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK {
			return fmt.Errorf("recorded API operation %s failed: status %d, %v", operation.path, response.StatusCode, readErr)
		}
	}
	return nil
}

func TestSavedRoutingClientsMutateOnlySelectedRecorderAfterEnvironmentRemoval(t *testing.T) {
	// Only disposable Unix HTTP fixtures are used; no shared /run socket, host
	// container API, namespace, mount, provider, or installed container CLI.
	sockets, err := os.MkdirTemp("", "routing-api-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockets) })
	selectedPath, outsidePath := filepath.Join(sockets, "selected"), filepath.Join(sockets, "outside")
	selected := newRoutingAPIRecorder(t, selectedPath)
	outside := newRoutingAPIRecorder(t, outsidePath)
	f := newRoutingFixture(t)
	conf := filepath.Join(f.home, ".config", "containers", "containers.conf")
	connections := filepath.Join(f.home, ".config", "containers", "podman-connections.json")
	docker := filepath.Join(f.home, ".docker")
	contextID := fmt.Sprintf("%x", sha256.Sum256([]byte("saved")))
	writeRoutingFixture(t, conf, fmt.Sprintf("[engine]\nremote=false\nremote_uri='unix://%s'\nactive_service='saved'\n[engine.service_destinations.saved]\nuri='unix://%s'\n", outsidePath, outsidePath))
	writeRoutingFixture(t, connections, fmt.Sprintf(`{"Connection":{"Default":"saved","Connections":{"saved":{"URI":"unix://%s"}}}}`, outsidePath))
	writeRoutingFixture(t, filepath.Join(docker, "config.json"), `{"currentContext":"saved","auths":{"registry.fixture":{"auth":"keep"}}}`)
	writeRoutingFixture(t, filepath.Join(docker, "contexts", "meta", contextID, "meta.json"), fmt.Sprintf(`{"Name":"saved","Endpoints":{"docker":{"Host":"unix://%s"}}}`, outsidePath))
	// Per-file production planners accept this disposable transport endpoint;
	// Reference validation deliberately allows only the canonical /run runtime.
	planner := &routingPlanner{files: map[string]string{}, sockets: map[string]bool{}, endpoint: "unix://" + selectedPath}
	if err := planner.containerConfig(conf, true, true); err != nil {
		t.Fatal(err)
	}
	if err := planner.connections(connections); err != nil {
		t.Fatal(err)
	}
	if err := planner.docker(docker); err != nil {
		t.Fatal(err)
	}
	// Materialize projected bytes at their original fixture paths, simulating
	// the private view without performing any mount or touching host config.
	for _, file := range planner.plan.Files {
		if err := os.WriteFile(file.Path, file.Data, file.Mode); err != nil {
			t.Fatal(err)
		}
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"podman-conf", "podman-connections", "docker-context"} {
		t.Run(kind, func(t *testing.T) {
			before := len(selected.changes())
			cmd := exec.Command(binary, "-test.run=^TestRoutingSavedClientFixture$")
			cmd.Env = []string{"HOME=" + f.home, "AW_SAVED_ROUTING_CLIENT=" + kind}
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("saved-file client failed: %v\n%s", err, output)
			}
			got := selected.changes()[before:]
			want := []string{"POST /containers/create", "POST /containers/recorded/start", "DELETE /containers/recorded"}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("selected recorder mutations %q, want %q", got, want)
			}
			if got := outside.changes(); len(got) != 0 {
				t.Fatalf("saved configuration escaped to outside API: %v", got)
			}
		})
	}
}
