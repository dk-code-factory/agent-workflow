package containerapi

import (
	"context"
	"fmt"
	"io"
	"strings"
)

const DockerSDK = "docker-api-v1"
const PodmanCLI = "podman-cli-v1"

// Selection is retained with an invocation. Reopening never consults the current
// Docker context or environment. Daemon identity is checked before every launch
// and cleanup; a transport failure cannot prove absence.
type Selection struct {
	Transport   string   `json:"transport"`
	Endpoint    string   `json:"endpoint,omitempty"`
	Identity    string   `json:"identity"`
	Executable  string   `json:"executable,omitempty"`
	Environment []string `json:"environment,omitempty"`
}
type Image struct {
	ID           string            `json:"id"`
	OS           string            `json:"os"`
	Architecture string            `json:"architecture"`
	Entrypoint   []string          `json:"entrypoint,omitempty"`
	Cmd          []string          `json:"cmd,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}
type Mount struct {
	Source, Target string
	ReadOnly       bool
}
type CreateRequest struct {
	Name, Image, User, WorkingDir string
	Entrypoint, Command, Env      []string
	Labels                        map[string]string
	Mounts                        []Mount
	Network                       string
	KeepID                        bool
	PrivatePaths                  []string
}
type ExecRequest struct {
	Command, Env     []string
	WorkingDir, User string
}
type Object struct {
	ID, Name string
	Labels   map[string]string
	Running  bool
}
type ExecStatus struct {
	ID, ContainerID string
	Running         bool
	ExitCode        int
}
type Streams struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// Attachment must be acquired before Start. Copy drains the separated output
// streams and sends stdin EOF. Close interrupts blocked copies on cancellation.
type Attachment interface {
	Copy(Streams) error
	Close() error
}
type Runtime interface {
	Close() error
	Identity(context.Context) (string, error)
	ResolveImage(context.Context, string) (Image, error)
	Create(context.Context, CreateRequest) (string, error)
	Attach(context.Context, string) (Attachment, error)
	Start(context.Context, string) error
	Wait(context.Context, string) (int, error)
	ExecCreate(context.Context, string, ExecRequest) (string, error)
	ExecAttach(context.Context, string) (Attachment, error)
	ExecInspect(context.Context, string) (ExecStatus, error)
	Inspect(context.Context, string) (Object, bool, error)
	Remove(context.Context, string) error
}

func Open(s Selection) (Runtime, error) {
	switch s.Transport {
	case DockerSDK:
		return openDocker(s)
	case PodmanCLI:
		return openPodman(s)
	default:
		return nil, fmt.Errorf("unsupported retained container transport %q", s.Transport)
	}
}
func CheckSelection(ctx context.Context, r Runtime, s Selection) error {
	identity, err := r.Identity(ctx)
	if err != nil {
		return err
	}
	if identity == "" || identity != s.Identity {
		return fmt.Errorf("container runtime identity changed")
	}
	return nil
}
func VerifyOwned(ctx context.Context, r Runtime, owner Owner) (Object, bool, error) {
	if owner.Selection == nil {
		return Object{}, false, fmt.Errorf("missing container selection")
	}
	if err := CheckSelection(ctx, r, *owner.Selection); err != nil {
		return Object{}, false, err
	}
	object, exists, err := r.Inspect(ctx, owner.Name)
	if err != nil || !exists {
		return object, exists, err
	}
	if strings.TrimPrefix(object.Name, "/") != owner.Name || object.ID == "" || owner.ID != "" && object.ID != owner.ID {
		return object, true, fmt.Errorf("container identity does not match ownership record")
	}
	if len(owner.Labels) == 0 {
		return object, true, fmt.Errorf("container ownership labels missing")
	}
	for key, value := range owner.Labels {
		if value == "" || object.Labels[key] != value {
			return object, true, fmt.Errorf("container ownership label mismatch: %s", key)
		}
	}
	return object, true, nil
}
func removeSelected(ctx context.Context, owner Owner) error {
	r, err := Open(*owner.Selection)
	if err != nil {
		return err
	}
	defer r.Close()
	object, exists, err := VerifyOwned(ctx, r, owner)
	if err != nil || !exists {
		return err
	}
	removeErr := r.Remove(ctx, object.ID)
	owner.ID = object.ID
	_, exists, err = VerifyOwned(ctx, r, owner)
	if err != nil {
		return err
	}
	if exists {
		if removeErr != nil {
			return removeErr
		}
		return fmt.Errorf("container removal did not establish absence")
	}
	return nil
}
func validContentID(id string) bool {
	return strings.HasPrefix(id, "sha256:") && len(id) == 71 && strings.Trim(id[7:], "0123456789abcdef") == ""
}
func ValidateImage(image Image) error {
	if !validContentID(image.ID) {
		return fmt.Errorf("runtime returned an invalid image content identity")
	}
	if image.OS != "linux" || image.Architecture != "amd64" && image.Architecture != "arm64" {
		return fmt.Errorf("unsupported container platform %s/%s", image.OS, image.Architecture)
	}
	return nil
}
