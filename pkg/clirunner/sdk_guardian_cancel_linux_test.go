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
	"sync"
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
			if err := os.WriteFile("terminal-written", []byte("yes"), 0600); err != nil {
				os.Exit(2)
			}
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

// Ownership publication and permission to pause share the shutdown gate. A
// callback may arrive after cancellation while bridgeTurn drains buffered data.
type sdkCancelOwnership struct {
	mu               sync.Mutex
	closing          bool
	released         bool
	guardian, child  *sdkCancelProcess
	err              error
	latePublications int
	pauses           int
	rejectedPauses   int
}

func (o *sdkCancelOwnership) stopLocked() {
	if o.released {
		return
	}
	for _, item := range []struct {
		process *sdkCancelProcess
		signal  unix.Signal
	}{{o.child, unix.SIGKILL}, {o.guardian, unix.SIGCONT}} {
		if item.process != nil {
			if err := item.process.signal(item.signal); err != nil && !errors.Is(err, unix.ESRCH) {
				o.err = errors.Join(o.err, err)
			}
		}
	}
}

func (o *sdkCancelOwnership) publish(guardian, child int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.guardian != nil || o.released {
		o.err = errors.Join(o.err, fmt.Errorf("duplicate or released fixture ownership"))
		return
	}
	o.guardian, o.err = sdkCancelPin(guardian, os.Getpid())
	if o.err == nil {
		o.child, o.err = sdkCancelPin(child, guardian)
	}
	if o.closing {
		o.latePublications++
		o.stopLocked()
	}
}

func (o *sdkCancelOwnership) pause() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closing {
		o.rejectedPauses++
		return
	}
	if o.err != nil || o.guardian == nil {
		o.err = errors.Join(o.err, fmt.Errorf("fixture has no guardian to pause"))
		return
	}
	o.err = o.guardian.signal(unix.SIGSTOP)
	deadline := time.Now().Add(time.Second)
	for o.err == nil {
		_, start, state, err := sdkCancelStat(o.guardian.pid)
		if err != nil || start != o.guardian.start {
			o.err = fmt.Errorf("guardian changed before controlled pause: %v", err)
			return
		}
		if state == "T" {
			o.pauses++
			return
		}
		if time.Now().After(deadline) {
			o.err = fmt.Errorf("guardian did not enter controlled pause")
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func (o *sdkCancelOwnership) shutdown() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closing = true
	o.stopLocked()
}

// The caller must join RunTurn, including all of its callbacks, before release.
func (o *sdkCancelOwnership) release() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.released {
		for _, process := range []*sdkCancelProcess{o.child, o.guardian} {
			if process != nil {
				o.err = errors.Join(o.err, unix.Close(process.fd))
			}
		}
		o.released = true
	}
	return o.err
}

func TestSDKCancellationRetainsGuardianCleanupBudget(t *testing.T) {
	for _, mode := range []string{"cooperative", "guardian-delayed", "initialization-timeout"} {
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
			var owner sdkCancelOwnership
			var observations []workflows.UsageUpdate
			cancelled := make(chan struct{})
			publicationPending := make(chan struct{})
			releasePublication := make(chan struct{})
			var releaseOnce sync.Once
			type outcome struct {
				result workflows.SessionResult
				err    error
			}
			finished := make(chan outcome, 1)
			joined := false
			cleaned := false
			var result outcome
			cleanup := func() error {
				if cleaned {
					return nil
				}
				// Close pause admission before cancellation or releasing a blocked
				// publisher. Newly published handles reconcile under the same gate.
				owner.shutdown()
				cancel()
				releaseOnce.Do(func() { close(releasePublication) })
				if !joined {
					select {
					case result = <-finished:
						joined = true
					case <-time.After(12 * time.Second):
						// Keep the pinned authority live if a publisher remains active.
						return fmt.Errorf("owned RunTurn did not join; fixture ownership retained")
					}
				}
				owner.shutdown()
				cleaned = true
				return owner.release()
			}
			defer func() {
				if err := cleanup(); err != nil {
					t.Error(err)
				}
			}()
			go func() {
				result, runErr := runner.RunTurn(ctx, workflows.SessionRequest{AgentRequest: workflows.AgentRequest{AgentID: "sdk", Prompt: "original task", ObserveUsage: func(update workflows.UsageUpdate) error {
					observations = append(observations, update)
					return nil
				}}}, func(event workflows.ProviderEvent) {
					if event.Type == "progress" {
						if mode == "initialization-timeout" {
							close(publicationPending)
							<-releasePublication
						}
						owner.publish(int(event.Data["guardian_pid"].(float64)), int(event.Data["bridge_pid"].(float64)))
					}
					if event.Type != "usage" {
						return
					}
					if mode != "cooperative" {
						owner.pause()
					}
					cancel()
					close(cancelled)
				})
				finished <- outcome{result, runErr}
			}()
			ready := cancelled
			if mode == "initialization-timeout" {
				ready = publicationPending
			}
			select {
			case <-ready:
			case result = <-finished:
				joined = true
				t.Fatalf("fixture ended before cancellation: %+v %v", result.result, result.err)
			case <-time.After(5 * time.Second):
				t.Fatal("fixture did not initialize")
			}
			if mode == "initialization-timeout" {
				// Hold publication until cleanup has closed admission, while the
				// original terminal and later usage callback are already buffered.
				deadline := time.Now().Add(time.Second)
				for {
					if _, err := os.Stat(filepath.Join(root, "terminal-written")); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("fixture did not buffer its original terminal")
					}
					time.Sleep(time.Millisecond)
				}
				if err := cleanup(); err != nil {
					t.Fatal(err)
				}
				if owner.latePublications != 1 || owner.rejectedPauses != 1 || owner.pauses != 0 {
					t.Fatalf("late callback did not exercise closed admission: published=%d rejected=%d pauses=%d", owner.latePublications, owner.rejectedPauses, owner.pauses)
				}
			}
			if mode == "guardian-delayed" {
				owner.mu.Lock()
				guardianReady := owner.err == nil && owner.guardian != nil && owner.child != nil
				owner.mu.Unlock()
				if !guardianReady {
					t.Fatal("fixture ownership not established before controlled pause")
				}
				// The old five-second os/exec deadline fires while this guardian is
				// paused. The repaired deadline retains its original cleanup budget
				// after the SDK's five-second interruption grace.
				time.Sleep(6 * time.Second)
				owner.mu.Lock()
				resumeErr := owner.guardian.signal(unix.SIGCONT)
				owner.mu.Unlock()
				if resumeErr != nil {
					t.Fatalf("guardian killed before receiving its cleanup budget: %v", resumeErr)
				}
			}
			if !joined {
				select {
				case result = <-finished:
					joined = true
				case <-time.After(5 * time.Second):
					t.Fatal("owned cancellation did not finish within cleanup budget")
				}
			}
			if owner.err != nil || owner.guardian == nil || owner.child == nil {
				t.Fatal("fixture ownership not established", owner.err)
			}
			if !errors.Is(result.err, context.Canceled) || result.result.SessionID != "sdk-interrupt-session" || result.result.Outputs["text"] != "original result" {
				t.Fatalf("cancellation lost original result: %+v %v", result.result, result.err)
			}
			usage, ok := result.result.Outputs["usage"].(map[string]any)
			if !ok || usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(2) || len(observations) != 1 || observations[0].Key != "original-usage" || observations[0].SessionID != "sdk-interrupt-session" {
				t.Fatalf("cancellation lost original accounting: %+v %+v", result.result, observations)
			}
			if !owner.guardian.absent() || !owner.child.absent() {
				t.Fatalf("owned guardian or child survived cancellation: guardian=%d child=%d", owner.guardian.pid, owner.child.pid)
			}
			if mode != "initialization-timeout" {
				if _, err := os.Stat(filepath.Join(root, "interrupt-observed")); err != nil {
					t.Fatal("SDK interruption was not requested", err)
				}
			}
		})
	}
}
