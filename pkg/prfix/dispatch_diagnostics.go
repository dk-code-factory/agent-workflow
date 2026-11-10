package prfix

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

type dispatchContextKey struct{}

type DispatchExecution struct {
	Intent     Intent                  `json:"intent"`
	CreatedAt  string                  `json:"created_at"`
	Dispatched string                  `json:"dispatch_intent_at"`
	Action     workflows.ActionContext `json:"action"`
}

func (execution *DispatchExecution) UnmarshalJSON(data []byte) error {
	type record DispatchExecution
	return decodeGitHubJSON(data, (*record)(execution))
}

type DispatchDiagnostic struct {
	Phase          string `json:"phase"`
	At             string `json:"at"`
	ExitStatus     *int   `json:"exit_status"`
	HTTPStatus     *int   `json:"http_status"`
	ResponseHash   string `json:"response_sha256"`
	ResponseLength int    `json:"response_length"`
}

func (g *GitHub) diagnosticPath(i Intent, name string) string {
	return filepath.Join(g.Root, ".agent-workflow", "pr-fix", "dispatch-diagnostics", digest([]string{i.Run, i.ID, i.Fingerprint()}), name+".json")
}

// Persist dispatch intent before sending. Diagnostics retain hashes and numeric
// statuses, never untrusted response bodies, headers or exception text.
func (g *GitHub) dispatchAPI(ctx context.Context, i Intent, method, path string, payload any, out any) error {
	var input []byte
	if payload != nil {
		input = jsonBytes(payload)
	}
	action, _ := ctx.Value(dispatchContextKey{}).(workflows.ActionContext)
	execution := DispatchExecution{Intent: i, CreatedAt: i.CreatedAt, Dispatched: time.Now().UTC().Format(time.RFC3339Nano), Action: action}
	if err := writeImmutable(g.diagnosticPath(i, "execution"), jsonBytes(execution)); err != nil {
		return fmt.Errorf("retain original dispatch intent: %w", err)
	}
	args := []string{"api", "--hostname", i.PR.Host, "--include", "--method", method, path}
	if payload != nil {
		args = append(args, "--input", "-")
	}
	raw, commandErr := runCommand(ctx, g.Root, input, "gh", args...)
	body, httpStatus := responseBody(raw)
	originalBody := body
	exit := 0
	phase := "response_parse"
	if commandErr != nil {
		phase = "command"
		var status *int
		var exited *exec.ExitError
		if errors.As(commandErr, &exited) {
			exit = exited.ExitCode()
			status = &exit
		}
		if httpStatus != nil && *httpStatus >= 400 {
			phase = "http"
		}
		return g.retainDispatchFailure(i, DispatchDiagnostic{phase, time.Now().UTC().Format(time.RFC3339Nano), status, httpStatus, rawHash(body), len(body)})
	}
	if strings.TrimSpace(string(body)) == "" {
		body = []byte("{}")
	}
	if err := decodeGitHubJSON(body, out); err != nil {
		return g.retainDispatchFailure(i, DispatchDiagnostic{phase, time.Now().UTC().Format(time.RFC3339Nano), &exit, httpStatus, rawHash(originalBody), len(originalBody)})
	}
	return nil
}

func responseBody(raw []byte) ([]byte, *int) {
	body := string(raw)
	var status *int
	for strings.HasPrefix(body, "HTTP/") {
		offset, width := strings.Index(body, "\r\n\r\n"), 4
		if offset < 0 {
			offset, width = strings.Index(body, "\n\n"), 2
		}
		if offset < 0 {
			break
		}
		fields := strings.Fields(strings.SplitN(body[:offset], "\n", 2)[0])
		if len(fields) > 1 {
			if value, err := strconv.Atoi(fields[1]); err == nil {
				status = &value
			}
		}
		body = body[offset+width:]
	}
	return []byte(body), status
}

func (g *GitHub) retainDispatchFailure(i Intent, failure DispatchDiagnostic) error {
	path := g.diagnosticPath(i, "historical-failure")
	if err := writeImmutable(path, jsonBytes(failure)); err != nil {
		return fmt.Errorf("retain original publication failure: %w", err)
	}
	return fmt.Errorf("publication failed during %s; safe diagnostics retained at %s", failure.Phase, path)
}

func (g *GitHub) recordObservationFailure(i Intent) error {
	diagnostic := DispatchDiagnostic{Phase: "reconciliation", At: time.Now().UTC().Format(time.RFC3339Nano)}
	path := g.diagnosticPath(i, "observation-"+digest(diagnostic))
	return writeImmutable(path, jsonBytes(diagnostic))
}

func (g *GitHub) readDispatchDiagnostics(i Intent) (*DispatchExecution, *DispatchDiagnostic, []DispatchDiagnostic, error) {
	var execution DispatchExecution
	var original DispatchDiagnostic
	var saved *DispatchExecution
	var first *DispatchDiagnostic
	if err := readJSON(g.diagnosticPath(i, "execution"), &execution); err == nil {
		if execution.Intent.Fingerprint() != i.Fingerprint() {
			return nil, nil, nil, fmt.Errorf("dispatch diagnostic identity differs")
		}
		saved = &execution
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil, err
	}
	if err := readJSON(g.diagnosticPath(i, "historical-failure"), &original); err == nil {
		first = &original
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil, err
	}
	paths, err := filepath.Glob(g.diagnosticPath(i, "observation-*"))
	if err != nil {
		return nil, nil, nil, err
	}
	later := []DispatchDiagnostic{}
	for _, path := range paths {
		var diagnostic DispatchDiagnostic
		if err := readJSON(path, &diagnostic); err != nil {
			return nil, nil, nil, err
		}
		later = append(later, diagnostic)
	}
	return saved, first, later, nil
}
