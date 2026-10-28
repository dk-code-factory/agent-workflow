// Standalone, model-free fixture for the explicit privileged network acceptance.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("fixture needs a mode")
	}
	switch os.Args[1] {
	case "--version": // cached doctor probe; never invokes a provider.
		ns, _ := os.Readlink("/proc/self/ns/net")
		fmt.Printf("fixture %s %s\n", ns, os.Getenv("DOCKER_HOST"))
		return nil
	case "login":
		if len(os.Args) != 3 || os.Args[2] != "status" || os.Getenv("AGENT_WORKFLOW_NETWORK_RUNTIME") == "" {
			return fmt.Errorf("expected contained cached-status probe")
		}
		return nil
	case "serve":
		return http.ListenAndServe(os.Args[2], http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "runtime-service") }))
	case "h2":
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/" {
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, `<html><body>HTTP/2 fixture</body></html>`)
				return
			}
			if r.ProtoMajor != 2 {
				http.Error(w, "HTTP/2 required", 500)
				return
			}
			delay, _ := strconv.Atoi(r.URL.Query().Get("delay"))
			time.Sleep(time.Duration(delay) * time.Millisecond)
			fmt.Fprint(w, "h2-ok")
		}))
		server.EnableHTTP2 = true
		server.StartTLS()
		defer server.Close()
		fmt.Println(server.URL)
		select {}
	case "probe":
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		addresses, err := net.DefaultResolver.LookupHost(ctx, os.Args[2])
		if err != nil || len(addresses) == 0 {
			return fmt.Errorf("DNS failed: %w", err)
		}
		resp, err := http.Get("http://" + os.Args[2] + ":8080/")
		if err != nil {
			return err
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(data) != "runtime-service" {
			return fmt.Errorf("invalid peer response %s", data)
		}
		client := http.Client{Timeout: 15 * time.Second}
		response, err := client.Get("https://example.com")
		if err != nil {
			return err
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			return fmt.Errorf("outbound HTTPS: %s", response.Status)
		}
		fmt.Println("dns+https-ok")
		return nil
	case "identity":
		ns, _ := os.Readlink("/proc/self/ns/net")
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"uid": os.Getuid(), "gid": os.Getgid(), "netns": ns, "endpoint": os.Getenv("DOCKER_HOST"), "cwd": mustCWD()})
	case "network":
		return networkCycle(os.Args[2])
	case "exec": // fake codex-cli: prompts contain a disposable network name.
		prompt, _ := io.ReadAll(io.LimitReader(os.Stdin, 4096))
		if err := networkCycle(strings.TrimSpace(string(prompt))); err != nil {
			return err
		}
		fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"contained"}}`)
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":0,"output_tokens":0}}`)
		return nil
	default:
		return fmt.Errorf("unknown fixture mode")
	}
}
func mustCWD() string { cwd, _ := os.Getwd(); return cwd }
func networkCycle(name string) error {
	if !strings.HasPrefix(name, "aw21-") {
		return fmt.Errorf("fixture requires owned network name")
	}
	for _, verb := range []string{"create", "rm"} {
		cmd := exec.Command("podman", "network", verb, name)
		if data, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %v: %s", verb, err, data)
		}
	}
	return nil
}
