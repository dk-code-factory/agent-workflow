package runtimeexec

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/dk-code-factory/agent-workflow/pkg/containerapi"
)

func fixtureGuardian(t *testing.T, architecture string) GuardianAsset {
	t.Helper()
	dir := t.TempDir()
	data := make([]byte, 64)
	copy(data, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(data[16:], 2)
	machine := uint16(62)
	if architecture == "arm64" {
		machine = 183
	}
	binary.LittleEndian.PutUint16(data[18:], machine)
	binary.LittleEndian.PutUint32(data[20:], 1)
	binary.LittleEndian.PutUint16(data[52:], 64)
	path := filepath.Join(dir, "linux-"+architecture)
	if err := os.WriteFile(path, data, 0755); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	return GuardianAsset{Path: path, SHA256: hex.EncodeToString(hash[:]), Platform: "linux/" + architecture}
}
func TestGuardianSelectionUsesImagePlatformAndVerifiesRetainedBytes(t *testing.T) {
	g := fixtureGuardian(t, "arm64")
	dir := filepath.Dir(g.Path)
	data, _ := json.Marshal(map[string]string{"linux-arm64": g.SHA256})
	if err := os.WriteFile(filepath.Join(dir, "checksums.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := GuardianFor(dir, containerapi.Image{ID: "sha256:" + strings.Repeat("a", 64), OS: "linux", Architecture: "arm64"})
	if err != nil || got != g {
		t.Fatalf("%+v %v", got, err)
	}
	wrong := g
	wrong.Platform = "linux/amd64"
	if err = wrong.Verify(); err == nil {
		t.Fatal("platform swap accepted")
	}
	if err = os.WriteFile(g.Path, []byte("corruption"), 0755); err != nil {
		t.Fatal(err)
	}
	if err = g.Verify(); err == nil {
		t.Fatal("corruption accepted")
	}
}

type apiJournal struct {
	mu          sync.Mutex
	records     []Record
	failCreated bool
}

func (j *apiJournal) AdmitInvocation(_ context.Context, r Record) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.records = append(j.records, r)
	return nil
}
func (j *apiJournal) UpdateInvocation(_ context.Context, r Record) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.records = append(j.records, r)
	if j.failCreated && r.State == "created" {
		return errors.New("lost journal commit")
	}
	return nil
}

func TestAPILaunchJournalsIntentAndIDBeforeWorkloadAndCleansFailures(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("host guardian platform")
	}
	for _, failure := range []string{"", "create-response", "created-commit"} {
		t.Run(failure, func(t *testing.T) {
			journal := &apiJournal{failCreated: failure == "created-commit"}
			var mu sync.Mutex
			var object *containerapi.Object
			var started bool
			socket := filepath.Join(t.TempDir(), "api.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				p := strings.TrimPrefix(r.URL.Path, "/v1.56")
				if p == "/_ping" {
					w.Header().Set("API-Version", "1.56")
					return
				}
				if p == "/info" {
					fmt.Fprint(w, `{"ID":"fixture-daemon"}`)
					return
				}
				if strings.HasPrefix(p, "/images/") {
					fmt.Fprintf(w, `{"Id":%q,"Os":"linux","Architecture":"amd64","Config":{}}`, "sha256:"+strings.Repeat("a", 64))
					return
				}
				mu.Lock()
				defer mu.Unlock()
				switch {
				case p == "/containers/create":
					journal.mu.Lock()
					last := journal.records[len(journal.records)-1]
					journal.mu.Unlock()
					if last.State != "intent" || last.Container == nil || last.Container.ID != "" {
						t.Error("create before durable intent")
					}
					var q struct{ Labels map[string]string }
					if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
						t.Error(err)
					}
					object = &containerapi.Object{ID: strings.Repeat("b", 64), Name: r.URL.Query().Get("name"), Labels: q.Labels}
					if failure == "create-response" {
						http.Error(w, "response lost", 500)
						return
					}
					fmt.Fprintf(w, `{"Id":%q}`, object.ID)
				case r.Method == "DELETE":
					object = nil
					w.WriteHeader(204)
				case strings.HasSuffix(p, "/json"):
					if object == nil {
						http.Error(w, "not found", 404)
						return
					}
					fmt.Fprintf(w, `{"Id":%q,"Name":%q,"Config":{"Labels":`, object.ID, "/"+object.Name)
					_ = json.NewEncoder(w).Encode(object.Labels)
					fmt.Fprint(w, `},"State":{"Running":false}}`)
				case strings.HasSuffix(p, "/start"):
					started = true
					t.Error("workload started during inert creation")
					w.WriteHeader(204)
				default:
					t.Errorf("unexpected %s %s", r.Method, p)
					http.Error(w, "unexpected", 500)
				}
			})}
			go server.Serve(listener)
			defer server.Close()
			dir := t.TempDir()
			state := filepath.Join(dir, "state")
			if err = os.MkdirAll(filepath.Join(state, "home"), 0700); err != nil {
				t.Fatal(err)
			}
			ctx := WithBinding(t.Context(), Binding{Journal: journal, Dir: state, RunID: "run", Generation: 1, Owner: "owner", LogicalID: "step"})
			o := LaunchOptions{Policy: Policy{Mode: "container", Image: "fixture"}, Selection: containerapi.Selection{Transport: containerapi.DockerSDK, Endpoint: "unix://" + socket, Identity: "fixture-daemon"}, Image: containerapi.Image{ID: "sha256:" + strings.Repeat("a", 64), OS: "linux", Architecture: "amd64"}, Guardian: fixtureGuardian(t, "amd64"), Workspace: dir, State: state, Root: dir, Command: []string{"echo", "ready"}}
			session, err := LaunchContainer(ctx, o)
			if failure == "" {
				if err != nil {
					t.Fatal(err)
				}
				if session.Owner.ID == "" {
					t.Fatal("missing retained ID")
				}
				if err = session.Close(); err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("failure accepted")
			}
			mu.Lock()
			defer mu.Unlock()
			if object != nil || started {
				t.Fatal("container leaked or workload launched")
			}
			journal.mu.Lock()
			defer journal.mu.Unlock()
			if journal.records[len(journal.records)-1].State != "cleaned" {
				t.Fatal(journal.records)
			}
		})
	}
}

func TestTypedMountPolicyHidesEngineStateAndRejectsWorkspaceRuntimeSockets(t *testing.T) {
	workspace := t.TempDir()
	engine := filepath.Join(workspace, ".agent-workflow")
	if err := os.MkdirAll(engine, 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(engine, "containers/private")
	if err := os.MkdirAll(state, 0700); err != nil {
		t.Fatal(err)
	}
	lifetime := t.TempDir()
	outside := filepath.Join(t.TempDir(), "api.sock")
	o := LaunchOptions{Policy: Policy{Mode: "container", Image: "fixture", Writable: true}, EngineState: engine, Selection: containerapi.Selection{Transport: containerapi.DockerSDK, Endpoint: "unix://" + outside}, Image: containerapi.Image{ID: "sha256:" + strings.Repeat("a", 64), OS: "linux", Architecture: "amd64"}, Guardian: fixtureGuardian(t, "amd64"), Workspace: workspace, State: state}
	owner := containerapi.Owner{Name: "owned", Labels: map[string]string{"agent-workflow.token": "token"}}
	request, err := BuildRequest(o, owner, lifetime)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.PrivatePaths) != 1 || request.PrivatePaths[0] != "/workspace/.agent-workflow" {
		t.Fatal("engine state and management socket were exposed", request.PrivatePaths)
	}
	for _, mount := range request.Mounts {
		if mount.Source == engine {
			t.Fatal("whole engine state was separately mounted")
		}
	}
	nested := filepath.Join(engine, "nested")
	if err = os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	o.EngineState = nested
	request, err = BuildRequest(o, owner, lifetime)
	if err != nil || len(request.PrivatePaths) != 1 || request.PrivatePaths[0] != "/workspace/.agent-workflow" {
		t.Fatal("nested masks were not collapsed", request.PrivatePaths, err)
	}
	o.Selection.Endpoint = "unix://" + filepath.Join(workspace, "runtime.sock")
	if _, err = BuildRequest(o, owner, lifetime); err == nil {
		t.Fatal("runtime socket inside workspace was exposed")
	}
	o.Selection.Endpoint = "unix://" + outside
	o.EngineState = workspace
	if _, err = BuildRequest(o, owner, lifetime); err == nil {
		t.Fatal("state root was accepted as the workspace")
	}
}
