//go:build linux

package clirunner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"golang.org/x/sys/unix"
)

// This executable fixture speaks only the bridge protocol. It has no SDK or
// provider access; its long-lived command is owned by the production guardian.
func TestSDKGuardianInterruptBridgeFixture(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") != "sdk-guardian-cancel-fixture" {
		return
	}
	encoder := json.NewEncoder(os.Stdout)
	write := func(frame map[string]any) {
		frame["version"] = BridgeProtocolVersion
		if encoder.Encode(frame) != nil {
			os.Exit(2)
		}
	}
	write(map[string]any{"type": "hello", "provider_version": "claude-agent-sdk@" + ClaudeSDKVersion})
	scanner := bufio.NewScanner(os.Stdin)
	cooperative := false
	for scanner.Scan() {
		var input map[string]any
		if json.Unmarshal(scanner.Bytes(), &input) != nil {
			os.Exit(2)
		}
		switch input["type"] {
		case "turn_start":
			cooperative = input["model"] == "cooperative"
			write(map[string]any{"type": "session", "session_id": "sdk-interrupt-session"})
			write(map[string]any{"type": "progress", "data": map[string]any{"guardian_pid": os.Getppid(), "bridge_pid": os.Getpid()}})
			write(map[string]any{"type": "usage", "data": map[string]any{"observation_key": "original-usage", "usage": map[string]int{"input_tokens": 10, "output_tokens": 2}}})
			write(map[string]any{"type": "result", "status": "succeeded", "outputs": map[string]any{"text": "original result", "usage": map[string]int{"input_tokens": 10, "output_tokens": 2}}})
		case "interrupt":
			if err := os.WriteFile("interrupt-observed", []byte("yes"), 0600); err != nil {
				os.Exit(2)
			}
			if cooperative {
				os.Exit(0)
			}
		}
	}
	// Match a wrapper that has retained its completed native result but stays
	// alive even after the controller closes stdin during cancellation.
	for {
		time.Sleep(time.Hour)
	}
}

type sdkCancelProcess struct {
	pid   int
	fd    int
	start string
}

func sdkCancelStat(pid int) (parent int, start, state string, err error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, "", "", err
	}
	fields := strings.Fields(string(raw[strings.LastIndexByte(string(raw), ')')+2:]))
	if len(fields) < 20 {
		return 0, "", "", fmt.Errorf("short owned process identity")
	}
	parent, err = strconv.Atoi(fields[1])
	return parent, fields[19], fields[0], err
}

func sdkCancelPin(pid, parent int) (*sdkCancelProcess, error) {
	observed, start, _, err := sdkCancelStat(pid)
	if err != nil || observed != parent {
		return nil, fmt.Errorf("owned process parent mismatch: %d, observed %d, expected %d: %v", pid, observed, parent, err)
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, err
	}
	observed, after, _, err := sdkCancelStat(pid)
	if err != nil || observed != parent || start != after {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("owned process identity changed: %d: %v", pid, err)
	}
	return &sdkCancelProcess{pid: pid, fd: fd, start: start}, nil
}

func (p *sdkCancelProcess) signal(signal unix.Signal) error {
	return unix.PidfdSendSignal(p.fd, signal, nil, 0)
}

func (p *sdkCancelProcess) absent() bool {
	_, start, _, err := sdkCancelStat(p.pid)
	return errors.Is(err, os.ErrNotExist) || err == nil && start != p.start
}

func TestSDKCancellationRetainsGuardianCleanupBudget(t *testing.T) {
	for _, mode := range []string{"cooperative", "guardian-delayed"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("SDK_GUARDIAN_FIXTURE_KEY", "sdk-guardian-cancel-fixture")
			runner := &Runner{Workspace: root, Config: Config{Agents: map[string]Agent{"sdk": {Provider: "claude-sdk", Command: self, Bridge: "-test.run=^TestSDKGuardianInterruptBridgeFixture$", Model: mode, Authentication: Authentication{Mode: "gateway", Endpoint: "http://127.0.0.1:1", CredentialEnv: "SDK_GUARDIAN_FIXTURE_KEY"}}}}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var guardian, child *sdkCancelProcess
			var identityErr error
			var observations []workflows.UsageUpdate
			cancelled := make(chan struct{})
			type outcome struct {
				result workflows.SessionResult
				err    error
			}
			finished := make(chan outcome, 1)
			joined := false
			defer func() {
				cancel()
				// Pinning makes cleanup safe even if the numeric PID has been reused.
				if child != nil {
					_ = child.signal(unix.SIGKILL)
				}
				if guardian != nil {
					_ = guardian.signal(unix.SIGCONT)
				}
				if !joined {
					select {
					case <-finished:
					case <-time.After(12 * time.Second):
						t.Error("owned RunTurn did not join during failure cleanup")
					}
				}
				if child != nil {
					_ = unix.Close(child.fd)
				}
				if guardian != nil {
					_ = unix.Close(guardian.fd)
				}
			}()
			go func() {
				result, runErr := runner.RunTurn(ctx, workflows.SessionRequest{AgentRequest: workflows.AgentRequest{AgentID: "sdk", Prompt: "original task", ObserveUsage: func(update workflows.UsageUpdate) error {
					observations = append(observations, update)
					return nil
				}}}, func(event workflows.ProviderEvent) {
					if event.Type == "progress" {
						guardian, identityErr = sdkCancelPin(int(event.Data["guardian_pid"].(float64)), os.Getpid())
						if identityErr == nil {
							child, identityErr = sdkCancelPin(int(event.Data["bridge_pid"].(float64)), guardian.pid)
						}
					}
					if event.Type != "usage" {
						return
					}
					if identityErr == nil && mode == "guardian-delayed" {
						identityErr = guardian.signal(unix.SIGSTOP)
						deadline := time.Now().Add(time.Second)
						for identityErr == nil {
							_, start, state, statErr := sdkCancelStat(guardian.pid)
							if statErr != nil || start != guardian.start {
								identityErr = fmt.Errorf("guardian changed before controlled pause: %v", statErr)
								break
							}
							if state == "T" {
								break
							}
							if time.Now().After(deadline) {
								identityErr = fmt.Errorf("guardian did not enter controlled pause")
								break
							}
							time.Sleep(time.Millisecond)
						}
					}
					cancel()
					close(cancelled)
				})
				finished <- outcome{result, runErr}
			}()
			select {
			case <-cancelled:
			case result := <-finished:
				joined = true
				t.Fatalf("fixture ended before cancellation: %+v %v", result.result, result.err)
			case <-time.After(5 * time.Second):
				t.Fatal("fixture did not initialize")
			}
			if identityErr != nil || guardian == nil || child == nil {
				t.Fatal("fixture ownership not established", identityErr)
			}
			if mode == "guardian-delayed" {
				// The old five-second os/exec deadline fires while this guardian is
				// paused. The repaired deadline retains its original cleanup budget
				// after the SDK's five-second interruption grace.
				time.Sleep(6 * time.Second)
				if err := guardian.signal(unix.SIGCONT); err != nil {
					t.Fatalf("guardian killed before receiving its cleanup budget: %v", err)
				}
			}
			var result outcome
			select {
			case result = <-finished:
				joined = true
			case <-time.After(5 * time.Second):
				t.Fatal("owned cancellation did not finish within cleanup budget")
			}
			if !errors.Is(result.err, context.Canceled) || result.result.SessionID != "sdk-interrupt-session" || result.result.Outputs["text"] != "original result" {
				t.Fatalf("cancellation lost original result: %+v %v", result.result, result.err)
			}
			usage, ok := result.result.Outputs["usage"].(map[string]any)
			if !ok || usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(2) || len(observations) != 1 || observations[0].Key != "original-usage" || observations[0].SessionID != "sdk-interrupt-session" {
				t.Fatalf("cancellation lost original accounting: %+v %+v", result.result, observations)
			}
			if !guardian.absent() || !child.absent() {
				t.Fatalf("owned guardian or child survived cancellation: guardian=%d child=%d", guardian.pid, child.pid)
			}
			if _, err := os.Stat(filepath.Join(root, "interrupt-observed")); err != nil {
				t.Fatal("SDK interruption was not requested", err)
			}
		})
	}
}
