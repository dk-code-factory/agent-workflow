package runtimeexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec/network"
)

const codexHookMode = "__agent_workflow_codex_hook_v1"
const codexHookHeader = "AGENT_WORKFLOW_CODEX_HOOK_V1\n"
const codexHookLimit = 1 << 20
const codexHookTimeout = 5 * time.Second

// The same internal mode is available to both shipped binaries and programs
// importing the launch adapter. It never launches a command or joins a namespace.
func init() {
	if len(os.Args) != 2 || os.Args[1] != codexHookMode {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexHookTimeout)
	defer cancel()
	out := runCodexHook(ctx, os.Stdin, os.Getenv, func(ctx context.Context, ref network.Reference) (network.Status, error) {
		response, err := network.Call(ctx, ref.Name, network.Request{Operation: "check", Reference: ref})
		return response.Status, err
	}, func() (string, error) { return os.Readlink("/proc/self/ns/net") })
	if _, err := io.WriteString(os.Stdout, codexHookHeader+string(out)+"\n"); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// Dependencies are passed only at this boundary so deterministic tests need no
// root service, namespace privileges, installed provider or production overrides.
func runCodexHook(ctx context.Context, input io.Reader, getenv func(string) string, status func(context.Context, network.Reference) (network.Status, error), namespace func() (string, error)) []byte {
	name, admitted := getenv("AGENT_WORKFLOW_NETWORK_RUNTIME"), getenv(network.ReferenceEnvironment)
	if name == "" && admitted == "" {
		return []byte(`{}`)
	}
	deny := []byte(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"Network runtime routing could not be verified."}}`)
	var ref network.Reference
	if name == "" || len(admitted) > 1024 || decodeHookJSON([]byte(admitted), &ref, true) != nil || ref.Validate() != nil || ref.Name != name {
		return deny
	}
	type readResult struct {
		data []byte
		err  error
	}
	read := make(chan readResult, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(input, codexHookLimit+1))
		read <- readResult{data, err}
	}()
	var data []byte
	select {
	case <-ctx.Done():
		return deny
	case result := <-read:
		if result.err != nil || len(result.data) > codexHookLimit {
			return deny
		}
		data = result.data
	}
	var event struct {
		Event string                     `json:"hook_event_name"`
		Tool  string                     `json:"tool_name"`
		Input map[string]json.RawMessage `json:"tool_input"`
	}
	if decodeHookJSON(data, &event, false) != nil || event.Event != "PreToolUse" || event.Tool != "Bash" {
		return deny
	}
	var command string
	if raw, ok := event.Input["command"]; !ok || json.Unmarshal(raw, &command) != nil || string(raw) == "null" || strings.ContainsRune(command, 0) {
		return deny
	}
	current, err := status(ctx, ref)
	if err != nil || ctx.Err() != nil || !current.Healthy || current.Reference != ref || current.NetworkNamespace == "" {
		return deny
	}
	actual, err := namespace()
	if err != nil || actual != current.NetworkNamespace {
		return deny
	}
	// The native tool executes this in its original POSIX shell, after native
	// environment preparation. A failed export must stop before the command;
	// a successful export leaves the original command's exit status intact.
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	prefix := "export DOCKER_HOST=" + quote(ref.Endpoint) + " CONTAINER_HOST=" + quote(ref.Endpoint) + " AGENT_WORKFLOW_NETWORK_RUNTIME=" + quote(ref.Name) + " || exit 125\n"
	event.Input["command"], err = json.Marshal(prefix + command)
	if err != nil {
		return deny
	}
	out := struct {
		Specific struct {
			Event    string                     `json:"hookEventName"`
			Decision string                     `json:"permissionDecision"`
			Input    map[string]json.RawMessage `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}{}
	out.Specific.Event, out.Specific.Decision, out.Specific.Input = "PreToolUse", "allow", event.Input
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(out) != nil || buffer.Len() > codexHookLimit {
		return deny
	}
	return bytes.TrimSpace(buffer.Bytes())
}

func decodeHookJSON(data []byte, out any, strict bool) error {
	if !utf8.Valid(data) {
		return errors.New("invalid hook encoding")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("unexpected trailing hook input")
	}
	return nil
}
