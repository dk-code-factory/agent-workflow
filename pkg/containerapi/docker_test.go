package containerapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dk-code-factory/agent-workflow/internal/testutil"
)

// All tests use a disposable Unix socket. No Docker process or image is used.
func fakeDocker(t *testing.T, handler http.HandlerFunc) Selection {
	t.Helper()
	socket := filepath.Join(testutil.SocketDir(t), "docker.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.56")
			w.WriteHeader(200)
			return
		}
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/v1.56")
		if r.URL.Path == "/info" {
			fmt.Fprint(w, `{"ID":"daemon-a"}`)
			return
		}
		handler(w, r)
	})}
	go server.Serve(l)
	t.Cleanup(func() { server.Close() })
	return Selection{Transport: DockerSDK, Endpoint: "unix://" + socket, Identity: "daemon-a"}
}
func TestDockerCreateUsesTypedContainmentAndPinnedImage(t *testing.T) {
	var request map[string]any
	selection := fakeDocker(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/containers/create" || r.URL.Query().Get("name") != "owned" {
			t.Errorf("unexpected call %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		fmt.Fprint(w, `{"Id":"container-a"}`)
	})
	runtime, err := Open(selection)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	q := CreateRequest{Name: "owned", Image: "sha256:" + strings.Repeat("a", 64), User: "1000:1000", PrivatePaths: []string{"/workspace/.agent-workflow"}, Command: []string{"echo", ""}, Labels: map[string]string{"token": "secret"}, Mounts: []Mount{{Source: "/fixture", Target: "/workspace", ReadOnly: true}}}
	id, err := runtime.Create(t.Context(), q)
	if err != nil || id != "container-a" {
		t.Fatalf("%q %v", id, err)
	}
	if request["Image"] != q.Image {
		t.Fatal(request)
	}
	h := request["HostConfig"].(map[string]any)
	if h["ReadonlyRootfs"] != true || h["NetworkMode"] != "none" || h["Memory"] != float64(4<<30) || h["NanoCpus"] != float64(2e9) || h["PidsLimit"] != float64(256) {
		t.Fatal(h)
	}
	if h["Tmpfs"].(map[string]any)["/workspace/.agent-workflow"] != "ro,nosuid,nodev,noexec,size=4096,mode=000" {
		t.Fatal("private state mount missing", h)
	}
	if _, err = runtime.Create(t.Context(), CreateRequest{Image: "ubuntu:24.04", User: "1000"}); err == nil {
		t.Fatal("tag reached create")
	}
}
func TestSelectedCleanupLostCreateResponseAndFailureBoundaries(t *testing.T) {
	for _, mode := range []string{"lost-create", "not-found", "inspect-error", "foreign-label", "foreign-id", "remove-error", "daemon-change"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			exists := mode != "not-found"
			deletes := 0
			selection := fakeDocker(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Method == "DELETE" {
					deletes++
					if mode == "remove-error" {
						http.Error(w, "failed", 500)
						return
					}
					exists = false
					w.WriteHeader(204)
					return
				}
				if mode == "inspect-error" {
					http.Error(w, "runtime unavailable", 500)
					return
				}
				if !exists {
					http.Error(w, `{"message":"No such container"}`, 404)
					return
				}
				token := "ours"
				if mode == "foreign-label" {
					token = "theirs"
				}
				id := "id-a"
				if mode == "foreign-id" {
					id = "id-b"
				}
				fmt.Fprintf(w, `{"Id":%q,"Name":"/owned","Config":{"Labels":{"token":%q}},"State":{"Running":false}}`, id, token)
			})
			if mode == "daemon-change" {
				selection.Identity = "other"
			}
			owner := Owner{Selection: &selection, Name: "owned", Labels: map[string]string{"token": "ours"}}
			if mode != "lost-create" {
				owner.ID = "id-a"
			}
			err := Remove(context.Background(), owner)
			wantErr := mode != "lost-create" && mode != "not-found"
			if (err != nil) != wantErr {
				t.Fatalf("error %v, expected error=%v", err, wantErr)
			}
			mu.Lock()
			defer mu.Unlock()
			if mode == "lost-create" && deletes != 1 {
				t.Fatalf("lost response did not reconcile by name: %d", deletes)
			}
			if mode != "lost-create" && mode != "remove-error" && deletes != 0 {
				t.Fatalf("unsafe delete: %d", deletes)
			}
		})
	}
}
func TestResolveDockerPinsContextAndIgnoresChangedEnvironment(t *testing.T) {
	selection := fakeDocker(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unexpected", 500) })
	t.Setenv("DOCKER_HOST", selection.Endpoint)
	t.Setenv("DOCKER_CONTEXT", "")
	pinned, err := Resolve(t.Context(), "docker", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_HOST", "unix:///missing/new-context.sock")
	runtime, err := Open(pinned)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err = CheckSelection(t.Context(), runtime, pinned); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(Selection{Transport: DockerSDK, Endpoint: "tcp://remote:2375"}); err == nil {
		t.Fatal("unsupported remote endpoint accepted")
	}
}
func TestPodmanSelectionRetainsEnvironmentWithoutCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "podman")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s' '{\"host\":{\"hostname\":\"fixture\"},\"store\":{\"graphRoot\":\"/fixture\"}}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("CONTAINER_HOST", "")
	t.Setenv("CONTAINER_CONNECTION", "")
	t.Setenv("OPENAI_API_KEY", "credential-sentinel")
	s, err := Resolve(t.Context(), "podman", "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Executable != path || strings.Contains(strings.Join(s.Environment, " "), "credential-sentinel") {
		t.Fatal(s)
	}
	t.Setenv("CONTAINER_HOST", "ssh://different")
	runtime, err := Open(s)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if err = CheckSelection(t.Context(), runtime, s); err != nil {
		t.Fatal(err)
	}
}

func TestDockerAPIStreamsStartWaitAndExec(t *testing.T) {
	var mu sync.Mutex
	attached := make(chan struct{})
	started := make(chan struct{})
	var calls []string
	selection := fakeDocker(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/containers/container-a/attach", "/exec/exec-a/start":
			conn, buffer, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			if strings.HasSuffix(r.URL.Path, "/attach") {
				close(attached)
			}
			fmt.Fprint(buffer, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			buffer.Flush()
			if strings.HasSuffix(r.URL.Path, "/attach") {
				<-started
			}
			_, _ = io.Copy(io.Discard, conn)
			// Docker multiplex headers distinguish stdout and stderr.
			_, _ = conn.Write([]byte{1, 0, 0, 0, 0, 0, 0, 5})
			_, _ = conn.Write([]byte("hello"))
			_, _ = conn.Write([]byte{2, 0, 0, 0, 0, 0, 0, 4})
			_, _ = conn.Write([]byte("logs"))
		case "/containers/container-a/start":
			select {
			case <-attached:
			default:
				t.Error("start preceded attach")
			}
			close(started)
			w.WriteHeader(204)
		case "/containers/container-a/wait":
			fmt.Fprint(w, `{"StatusCode":7}`)
		case "/containers/container-a/exec":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request["Privileged"] == true || request["User"] != "1000:1000" || request["WorkingDir"] != "/workspace" {
				t.Error(request)
			}
			fmt.Fprint(w, `{"Id":"exec-a"}`)
		case "/exec/exec-a/json":
			fmt.Fprint(w, `{"ID":"exec-a","ContainerID":"container-a","Running":false,"ExitCode":3}`)
		default:
			t.Errorf("unexpected call %s", r.URL)
			http.Error(w, "unexpected", 500)
		}
	})
	runtime, err := Open(selection)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	attachment, err := runtime.Attach(t.Context(), "container-a")
	if err != nil {
		t.Fatal(err)
	}
	defer attachment.Close()
	var stdout, stderr strings.Builder
	done := make(chan error, 1)
	go func() { done <- attachment.Copy(Streams{Stdout: &stdout, Stderr: &stderr}) }()
	if err = runtime.Start(t.Context(), "container-a"); err != nil {
		t.Fatal(err)
	}
	code, err := runtime.Wait(t.Context(), "container-a")
	if err != nil || code != 7 {
		t.Fatalf("%d %v", code, err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "hello" || stderr.String() != "logs" {
		t.Fatal(stdout.String(), stderr.String())
	}
	id, err := runtime.ExecCreate(t.Context(), "container-a", ExecRequest{Command: []string{"echo", ""}, User: "1000:1000", WorkingDir: "/workspace"})
	if err != nil || id != "exec-a" {
		t.Fatalf("%q %v", id, err)
	}
	a, err := runtime.ExecAttach(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err = a.Copy(Streams{}); err != nil {
		t.Fatal(err)
	}
	status, err := runtime.ExecInspect(t.Context(), id)
	if err != nil || status.Running || status.ExitCode != 3 || status.ContainerID != "container-a" {
		t.Fatalf("%+v %v", status, err)
	}
}

func TestDockerWaitCancellationUsesSDKRequestContext(t *testing.T) {
	selection := fakeDocker(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/owned/wait" {
			http.Error(w, "unexpected", 500)
			return
		}
		<-r.Context().Done()
	})
	runtime, err := Open(selection)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = runtime.Wait(ctx, "owned"); !errors.Is(err, context.Canceled) {
		t.Fatal("wait cancellation was lost", err)
	}
}
