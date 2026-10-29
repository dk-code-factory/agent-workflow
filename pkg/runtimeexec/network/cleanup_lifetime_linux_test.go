//go:build linux

package network

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func cleanupClient(t *testing.T, s *service, lifetime time.Duration) (*net.UnixConn, <-chan struct{}) {
	t.Helper()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "proxy"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := listener.AcceptUnix()
		if err == nil {
			defer c.Close()
			s.proxyCleanupUntil(c, time.Now().Add(lifetime))
		}
	}()
	c, err := net.DialUnix("unix", nil, listener.Addr().(*net.UnixAddr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	return c, done
}

func TestCleanupBoundsSilentUpstreamAndReleasesConnections(t *testing.T) {
	for _, body := range []bool{false, true} {
		for _, disconnect := range []bool{false, true} {
			t.Run(fmt.Sprintf("body=%t/disconnect=%t", body, disconnect), func(t *testing.T) {
				s := &service{run: t.TempDir()}
				backend, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(s.run, "api.sock"), Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = backend.Close() })
				accepted := make(chan struct{})
				upstreamDone := make(chan error, 1)
				go func() {
					c, err := backend.AcceptUnix()
					if err != nil {
						upstreamDone <- err
						return
					}
					defer c.Close()
					_ = c.SetDeadline(time.Now().Add(3 * time.Second))
					reader := bufio.NewReader(c)
					req, err := http.ReadRequest(reader)
					if err != nil {
						upstreamDone <- err
						return
					}
					req.Body.Close()
					if body {
						_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nx")
					}
					close(accepted)
					// The backend never completes its headers/body. EOF must come
					// from proxy cancellation, before this fixture's own deadline.
					_, err = io.Copy(io.Discard, reader)
					upstreamDone <- err
				}()
				c, done := cleanupClient(t, s, 200*time.Millisecond)
				if _, err := io.WriteString(c, "GET /info HTTP/1.1\r\nHost: podman\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
				select {
				case <-accepted:
				case <-time.After(time.Second):
					t.Fatal("backend did not receive cleanup request")
				}
				if disconnect {
					_ = c.Close()
				}
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("cleanup handler outlived its bounded lifetime")
				}
				select {
				case err := <-upstreamDone:
					if err != nil {
						t.Fatalf("upstream was not closed by proxy: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("active upstream connection retained")
				}
			})
		}
	}
}

func TestCleanupPreservesAuthorizedRequestsAndWriteHalfClose(t *testing.T) {
	for _, pipelined := range []bool{false, true} {
		t.Run(fmt.Sprintf("pipelined=%t", pipelined), func(t *testing.T) {
			s := &service{run: t.TempDir()}
			backend, err := net.Listen("unix", filepath.Join(s.run, "api.sock"))
			if err != nil {
				t.Fatal(err)
			}
			reached := make(chan string, 2)
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached <- r.URL.Path
				fmt.Fprint(w, r.URL.Path)
			})}
			go server.Serve(backend)
			t.Cleanup(func() { _ = server.Close() })
			c, done := cleanupClient(t, s, 2*time.Second)
			reader := bufio.NewReader(c)
			read := func(want string) {
				t.Helper()
				response, err := http.ReadResponse(reader, nil)
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil || response.StatusCode != 200 || string(data) != want {
					t.Fatalf("cleanup response %q: %v", data, err)
				}
			}
			first := "GET /info HTTP/1.1\r\nHost: podman\r\n\r\n"
			second := "GET /containers/owned/json HTTP/1.1\r\nHost: podman\r\n\r\n"
			if pipelined {
				_, err = io.WriteString(c, first+second)
			} else {
				_, err = io.WriteString(c, first)
				read("/info")
				if err == nil {
					_, err = io.WriteString(c, second)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := c.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			if pipelined {
				read("/info")
			}
			read("/containers/owned/json")
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("handler did not finish after half-close and responses")
			}
			if len(reached) != 2 {
				t.Fatal("authorized requests were consumed by disconnect handling")
			}
		})
	}
}
