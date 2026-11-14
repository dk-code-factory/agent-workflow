package workflows

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/internal/testutil"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
)

func TestStructuredContainerNetworkIsPinnedThroughCheckpointValidation(t *testing.T) {
	for _, standalone := range []bool{false, true} {
		for _, mode := range []string{"", "none", "bridge"} {
			name := "shared/"
			if standalone {
				name = "standalone/"
			}
			t.Run(name+"mode="+mode, func(t *testing.T) {
				e, store, d := containerExecutor(t)
				e.CommandExecution = runtimeexec.Policy{Mode: "container", Runtime: "docker", Image: "fixture", NetworkMode: mode}
				var mu sync.Mutex
				var networks []string
				listener, err := net.Listen("unix", filepath.Join(testutil.SocketDir(t), "network.sock"))
				if err != nil {
					t.Fatal(err)
				}
				server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/containers/create") {
						body, readErr := io.ReadAll(r.Body)
						if readErr != nil {
							t.Error(readErr)
							http.Error(w, "read request", 500)
							return
						}
						var request struct{ HostConfig struct{ NetworkMode string } }
						if decodeErr := json.Unmarshal(body, &request); decodeErr != nil {
							t.Error(decodeErr)
							http.Error(w, "decode request", 500)
							return
						}
						mu.Lock()
						networks = append(networks, request.HostConfig.NetworkMode)
						mu.Unlock()
						r.Body = io.NopCloser(bytes.NewReader(body))
					}
					d.serve(w, r)
				})}
				go server.Serve(listener)
				t.Cleanup(func() { server.Close() })
				t.Setenv("DOCKER_HOST", "unix://"+listener.Addr().String())
				source := structuredWorkflow
				if standalone {
					source = strings.ReplaceAll(source, "    container: ubuntu:24.04\n", "")
					source = strings.ReplaceAll(source, "        argv: [helper]\n", "        uses: docker://ubuntu:24.04\n        with: {entrypoint: helper, args: ''}\n")
				}
				installProtocolWorkflow(t, e, d, &protocolFixture{binding: "stable"}, source)
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				defer cancel()
				run, err := e.Run(ctx, RunRequest{Ref: "workflows/test.yml"})
				if err != nil {
					t.Fatal(err)
				}
				_, snapshot, err := store.Load(ctx, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				job := snapshot.Definitions[run.WorkflowRef].Jobs["capture"]
				implementation := job.Steps[0].ContainerImplementation
				if !standalone {
					implementation = job.Container.Implementation
				}
				if implementation == nil || implementation.Policy.NetworkMode != mode {
					t.Fatalf("container network was not admitted: %+v", implementation)
				}
				e.CommandExecution.NetworkMode = "none"
				if _, err = e.Resume(ctx, run.ID, nil, false); err != nil {
					t.Fatalf("current configuration changed retained validation: %v", err)
				}
				want := mode
				if want == "" {
					want = "none"
				}
				mu.Lock()
				defer mu.Unlock()
				if len(networks) < 3 {
					t.Fatalf("expected execute and lifecycle container observations: %q", networks)
				}
				for _, got := range networks {
					if got != want {
						t.Fatalf("runtime network=%q; want admitted %q across execution/resume: %q", got, want, networks)
					}
				}
			})
		}
	}
}
