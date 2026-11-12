package workflows

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/containerapi"
	"github.com/dk-code-factory/agent-workflow/pkg/process"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"gopkg.in/yaml.v3"
)

const GitHubContainersV1 = "github-containers-v1"

type JobContainer struct {
	Files          []string                 `json:"files,omitempty" yaml:"files,omitempty"`
	Image          string                   `json:"image" yaml:"image"`
	Implementation *ContainerImplementation `json:"implementation,omitempty" yaml:"-"`
}
type ContainerImplementation struct {
	ContractVersion int                       `json:"contract_version,omitempty"`
	Bundle          *HelperBundle             `json:"helper_bundle,omitempty"`
	Selection       containerapi.Selection    `json:"selection"`
	Image           containerapi.Image        `json:"image"`
	Guardian        runtimeexec.GuardianAsset `json:"guardian"`
	Policy          runtimeexec.Policy        `json:"policy"`
}

func (c *JobContainer) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		if n.Tag != "!!str" {
			return fmt.Errorf("container requires an image string or mapping")
		}
		c.Image = n.Value
		return nil
	}
	type plain JobContainer
	return n.Decode((*plain)(c))
}
func isDockerStep(s Step) bool { return strings.HasPrefix(s.Uses, "docker://") }
func staticContainerValue(value string) bool {
	return value != "" && !strings.Contains(value, "${{") && !strings.ContainsAny(value, "\x00\n\r")
}

// SplitContainerArgs performs quote removal only: no shell expansion, globbing,
// substitution, or environment interpretation. Empty quoted arguments survive.
func SplitContainerArgs(text string) ([]string, error) {
	var out []string
	var word strings.Builder
	var quote rune
	escaped, active := false, false
	for _, r := range text {
		if escaped {
			word.WriteRune(r)
			escaped = false
			active = true
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			active = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			active = true
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
			active = true
		case ' ', '\t', '\n', '\r':
			if active {
				out = append(out, word.String())
				word.Reset()
				active = false
			}
		default:
			word.WriteRune(r)
			active = true
		}
	}
	if escaped || quote != 0 {
		return nil, fmt.Errorf("container args contain an unfinished quote or escape")
	}
	if active {
		out = append(out, word.String())
	}
	return out, nil
}
func dockerCommand(step Step, image containerapi.Image) ([]string, error) {
	entry := append([]string(nil), image.Entrypoint...)
	cmd := append([]string(nil), image.Cmd...)
	for key, value := range step.With {
		text, ok := value.(string)
		if !ok || strings.Contains(text, "${{") {
			return nil, fmt.Errorf("Docker %s must be a static string", key)
		}
		switch key {
		case "entrypoint":
			if text == "" {
				return nil, fmt.Errorf("Docker entrypoint cannot be empty")
			}
			entry = []string{text}
		case "args":
			var err error
			cmd, err = SplitContainerArgs(text)
			if err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unsupported Docker parameter %q", key)
		}
	}
	argv := append(entry, cmd...)
	if len(argv) == 0 || argv[0] == "" {
		return nil, fmt.Errorf("Docker step has no executable")
	}
	return argv, nil
}
func validateJobContainer(w *Workflow, id string, j Job) error {
	if j.Container != nil {
		for _, name := range j.Container.Files {
			if _, err := assetPath(name); err != nil {
				return err
			}
		}
	}
	for _, step := range lifecycleSteps(j.Steps) {
		if step.Files != nil && !isDockerStep(step) {
			return fmt.Errorf("helper files belong under container for job commands, or on Docker steps")
		}
		for _, name := range step.Files {
			if _, err := assetPath(name); err != nil {
				return err
			}
		}
	}
	if j.Container != nil {
		if !staticContainerValue(j.Container.Image) {
			return fmt.Errorf("job %s: container.image must be a static image", id)
		}
		if j.Uses != "" {
			return fmt.Errorf("job %s: reusable calls cannot declare a container", id)
		}
		for _, s := range lifecycleSteps(j.Steps) {
			if s.Parallel != nil || s.Conversation != nil || s.Uses != "" && !isDockerStep(s) {
				return fmt.Errorf("job %s: shared containers support sequential commands, Docker steps and sequence blocks only", id)
			}
		}
	}
	for _, s := range lifecycleSteps(j.Steps) {
		if !isDockerStep(s) {
			continue
		}
		if !staticContainerValue(strings.TrimPrefix(s.Uses, "docker://")) {
			return fmt.Errorf("Docker image must be static")
		}
		if len(s.Argv) > 0 || s.Run != "" || s.Shell != "" || s.Context != (RunContext{}) {
			return fmt.Errorf("Docker steps cannot combine with command or session fields")
		}
		for k, v := range s.With {
			text, ok := v.(string)
			if !ok || strings.Contains(text, "${{") || (k != "entrypoint" && k != "args") {
				return fmt.Errorf("Docker steps accept static with.entrypoint and with.args only")
			}
			if k == "args" {
				if _, err := SplitContainerArgs(text); err != nil {
					return err
				}
			}
		}
		if s.Timeout != "" {
			d, err := time.ParseDuration(s.Timeout)
			if err != nil || d <= 0 {
				return fmt.Errorf("Docker timeout must be a positive duration")
			}
		}
	}
	return nil
}
func (e *Executor) pinContainers(ctx context.Context, all map[string]*Workflow) error {
	pin := func(image string, retained *ContainerImplementation) (*ContainerImplementation, error) {
		if invocationStore(e.Store) == nil {
			return nil, fmt.Errorf("%s requires durable invocation ownership", GitHubContainersV1)
		}
		if retained != nil {
			if _, err := e.guardianForImplementation(retained); err != nil {
				return nil, err
			}
			r, err := containerapi.Open(retained.Selection)
			if err != nil {
				return nil, err
			}
			defer r.Close()
			if err = containerapi.CheckSelection(ctx, r, retained.Selection); err != nil {
				return nil, err
			}
			i, err := r.ResolveImage(ctx, retained.Image.ID)
			if err != nil || i.ID != retained.Image.ID || i.OS != retained.Image.OS || i.Architecture != retained.Image.Architecture {
				return nil, fmt.Errorf("retained image unavailable or changed")
			}
			return retained, nil
		}
		policy := e.CommandExecution
		policy.Mode = "container"
		policy.Image = image
		if policy.Runtime == "" {
			policy.Runtime = "docker"
		}
		if policy.NetworkRuntime != "" {
			return nil, fmt.Errorf("GitHub-style containers do not yet support network-runtime")
		}
		s, err := containerapi.Resolve(ctx, policy.Runtime, "")
		if err != nil {
			return nil, err
		}
		r, err := containerapi.Open(s)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		i, err := r.ResolveImage(ctx, image)
		if err != nil {
			return nil, err
		}
		directory := e.ContainerGuardianDirectory
		if directory == "" {
			directory = runtimeexec.DefaultGuardianDirectory()
		}
		g, err := runtimeexec.GuardianFor(directory, i)
		if err != nil {
			return nil, err
		}
		policy.Image = i.ID
		policy, err = runtimeexec.PinPolicy(ctx, policy)
		if err != nil {
			return nil, err
		}
		return &ContainerImplementation{ContractVersion: 1, Selection: s, Image: i, Guardian: g, Policy: policy}, nil
	}
	for ref, w := range all {
		for id, j := range w.Jobs {
			if j.Container != nil {
				p, err := pin(j.Container.Image, j.Container.Implementation)
				if err != nil {
					return fmt.Errorf("%s job %s: %w", ref, id, err)
				}
				j.Container.Implementation = p
			}
			var err error
			j.Steps, err = mapSteps(j.Steps, "", func(s Step) (Step, error) {
				if isDockerStep(s) {
					var err error
					s.ContainerImplementation, err = pin(strings.TrimPrefix(s.Uses, "docker://"), s.ContainerImplementation)
					if err != nil {
						return s, err
					}
					if _, err = dockerCommand(s, s.ContainerImplementation.Image); err != nil {
						return s, err
					}
				}
				return s, nil
			})
			if err != nil {
				return err
			}
			w.Jobs[id] = j
		}
	}
	return nil
}

type jobContainerKey struct{}
type containerCleanupCancelKey struct{}

func cancelContainerCleanup(ctx context.Context) {
	if cancel, ok := ctx.Value(containerCleanupCancelKey{}).(context.CancelFunc); ok {
		cancel()
	}
}

type jobContainerScope struct {
	failed         error
	implementation *ContainerImplementation
	session        *runtimeexec.ContainerSession
	held           bool
	attachment     containerapi.Attachment
	drained        chan struct{}
	slots          chan struct{}
	executor       *Executor
}

func (s *jobContainerScope) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.held {
		return nil
	}
	select {
	case s.slots <- struct{}{}:
		s.held = true
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *jobContainerScope) closeSession() error {
	if s.session == nil {
		return nil
	}
	err := s.session.Close()
	if s.attachment != nil {
		_ = s.attachment.Close()
		<-s.drained
	}
	if err == nil {
		s.session, s.attachment, s.drained = nil, nil, nil
	}
	return err
}
func (s *jobContainerScope) close(ctx context.Context) error {
	err := s.closeSession()
	if err != nil {
		// Cancel before releasing capacity even if cleanup uncertainty could not be journaled.
		cancelContainerCleanup(ctx)
	}
	if s.held {
		<-s.slots
		s.held = false
	}
	return err
}
func (s *jobContainerScope) start(ctx context.Context) (returned error) {
	if s.session != nil {
		return nil
	}
	// Startup follows the command deadline; teardown owns the parent heartbeat.
	parent := ctx
	if b, ok := runtimeexec.BindingFrom(ctx); ok {
		b.LogicalID = "job-container/" + b.LogicalID
		parent = runtimeexec.WithBinding(parent, b)
	}
	o, err := s.executor.launchOptions(parent, s.implementation, []string{runtimeexec.MountedGuardian, "__container_idle_v1"})
	if err != nil {
		return err
	}
	session, err := runtimeexec.LaunchContainer(parent, o)
	if err != nil {
		return err
	}
	s.session = session
	defer func() {
		if returned != nil {
			returned = errors.Join(returned, s.closeSession())
		}
	}()
	a, err := session.Start(parent)
	if err != nil {
		return err
	}
	s.attachment = a
	s.drained = make(chan struct{})
	go func() { defer close(s.drained); _ = a.Copy(containerapi.Streams{}); _ = a.Close() }()
	if err = session.Runtime.Start(parent, session.Owner.ID); err != nil {
		return err
	}
	if err = s.waitForRunning(parent); err != nil {
		return err
	}
	return session.Invocation.Save(parent, "running")
}
func (s *jobContainerScope) waitForRunning(ctx context.Context) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		object, exists, err := containerapi.VerifyOwned(ctx, s.session.Runtime, s.session.Owner)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("shared container disappeared during startup")
		}
		if object.Running {
			return nil
		}
		select {
		case <-s.drained:
			return fmt.Errorf("shared container exited before becoming ready")
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func (e *Executor) launchOptions(ctx context.Context, p *ContainerImplementation, argv []string) (runtimeexec.LaunchOptions, error) {
	b, ok := runtimeexec.BindingFrom(ctx)
	if !ok {
		return runtimeexec.LaunchOptions{}, fmt.Errorf("container requires owned execution")
	}
	state := filepath.Join(b.Dir, "containers", containerStateOrigin(e.bundleOrigin, b.RunID), runtimeexec.NewID())
	if err := os.MkdirAll(filepath.Join(state, "home"), 0700); err != nil {
		return runtimeexec.LaunchOptions{}, err
	}
	actions := ""
	if p.Bundle != nil {
		store := invocationStore(e.Store)
		var err error
		actions, err = store.HelperBundlePath(p.Bundle)
		if err != nil {
			return runtimeexec.LaunchOptions{}, err
		}
	}
	guardian, err := e.guardianForImplementation(p)
	if err != nil {
		return runtimeexec.LaunchOptions{}, err
	}
	return runtimeexec.LaunchOptions{Actions: actions, Policy: p.Policy, Selection: p.Selection, Image: p.Image, Guardian: guardian, Workspace: e.WorkspaceDir, State: state, Root: e.WorkspaceDir, Command: argv}, nil
}
func containerWorkingDirectory(dir string) string {
	if filepath.IsAbs(dir) {
		return dir
	}
	return filepath.ToSlash(filepath.Join("/workspace", dir))
}
func (e *Executor) runContainerIO(ctx context.Context, step Step, argv, env []string, streams containerapi.Streams, slots chan struct{}) (code int, queueTime int64, returned error) {
	scope, _ := ctx.Value(jobContainerKey{}).(*jobContainerScope)
	queued := time.Now()
	if scope != nil {
		if scope.failed != nil {
			return -1, 0, scope.failed
		}
		if bundle := scope.implementation.Bundle; bundle != nil {
			if err := invocationStore(e.Store).VerifyHelperBundle(bundle); err != nil {
				return -1, 0, err
			}
		}
		if err := scope.acquire(ctx); err != nil {
			return -1, queueTime, err
		}
	} else {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-ctx.Done():
			return -1, queueTime, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return -1, queueTime, err
	}
	defer func() {
		if code == 125 {
			returned = errors.Join(returned, fmt.Errorf("%w: container guardian exited with code %d", process.ErrCleanup, code))
		}
		if errors.Is(returned, process.ErrCleanup) {
			cancelContainerCleanup(ctx)
		}
	}()
	queueTime = time.Since(queued).Milliseconds()
	var err error
	if isDockerStep(step) {
		p := step.ContainerImplementation
		if p == nil {
			return -1, queueTime, fmt.Errorf("Docker step was not admitted")
		}
		o, err := e.launchOptions(ctx, p, argv)
		if err != nil {
			return -1, queueTime, err
		}
		o.WorkingDir = containerWorkingDirectory(step.WorkingDirectory)
		o.Env = env
		session, err := runtimeexec.LaunchContainer(ctx, o)
		if err != nil {
			return -1, queueTime, err
		}
		defer func() { returned = errors.Join(returned, session.Close()) }()
		code, err = session.Run(ctx, streams)
	} else {
		if scope == nil {
			return -1, queueTime, fmt.Errorf("shared container scope unavailable")
		}
		if err := scope.start(ctx); err != nil {
			return -1, queueTime, err
		}
		code, err = scope.session.Exec(ctx, containerapi.ExecRequest{Command: argv, Env: env, WorkingDir: containerWorkingDirectory(step.WorkingDirectory)}, streams)
		if err != nil || code == 125 {
			scope.failed = errors.Join(err, fmt.Errorf("shared container command lifetime is unconfirmed; later commands are blocked"))
			err = errors.Join(scope.failed, scope.session.Close())
		}
	}

	return code, queueTime, err
}
func (e *Executor) executeContainerCommand(ctx context.Context, step Step, argv []string, slots chan struct{}) (out map[string]any, returned error) {
	if isDockerStep(step) {
		var err error
		argv, err = dockerCommand(step, step.ContainerImplementation.Image)
		if err != nil {
			return nil, err
		}
	}
	var stdout, stderr cappedOutput
	env, _ := ctx.Value(commandEnvKey{}).([]string)
	code, queueTime, err := e.runContainerIO(ctx, step, argv, env, containerapi.Streams{Stdout: &stdout, Stderr: &stderr}, slots)
	out = map[string]any{"stdout": stdout.String(), "stderr": stderr.String(), "exit_code": code, "queue_time_ms": queueTime}
	if errors.Is(err, process.ErrCleanup) {
		return out, err
	}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	if stdout.overflow || stderr.overflow {
		return out, fmt.Errorf("command output exceeds 16 MiB")
	}
	if err != nil {
		return out, err
	}
	if code != 0 {
		return out, fmt.Errorf("container command exited with code %d", code)
	}
	return out, nil
}

// Retained identity is portable; a moved release can supply the same verified binary.
func (e *Executor) guardianForImplementation(p *ContainerImplementation) (runtimeexec.GuardianAsset, error) {
	if err := p.Guardian.Verify(); err == nil {
		return p.Guardian, nil
	}
	directory := e.ContainerGuardianDirectory
	if directory == "" {
		directory = runtimeexec.DefaultGuardianDirectory()
	}
	g, err := runtimeexec.GuardianFor(directory, p.Image)
	if err != nil {
		return g, err
	}
	if g.SHA256 != p.Guardian.SHA256 || g.Platform != p.Guardian.Platform {
		return g, fmt.Errorf("retained guardian identity unavailable")
	}
	return g, nil
}
