// Package containerapi reconciles runtime objects using exact ownership labels.
// A failed inspection is never evidence that an object is absent.
package containerapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec/network"
	"os"
	"os/exec"
	"strings"
)

type Owner struct {
	Network  *network.Reference `json:"network_runtime,omitempty"`
	Runtime  string             `json:"runtime"`
	Identity string             `json:"identity"`
	Name     string             `json:"name"`
	ID       string             `json:"id,omitempty"`
	Labels   map[string]string  `json:"labels"`
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 1<<20 {
		return 0, fmt.Errorf("runtime diagnostics exceed limit")
	}
	return b.Buffer.Write(p)
}
func Output(ctx context.Context, runtime string, args ...string) ([]byte, error) {
	return OutputAt(ctx, runtime, nil, args...)
}
func OutputAt(ctx context.Context, runtime string, ref *network.Reference, args ...string) ([]byte, error) {
	return outputAt(ctx, runtime, ref, false, args...)
}

func outputAt(ctx context.Context, runtime string, ref *network.Reference, recovery bool, args ...string) ([]byte, error) {
	if ref != nil {
		check := network.Check
		if recovery {
			check = network.CheckRetained
		}
		if err := check(ctx, *ref); err != nil {
			return nil, err
		}
		args = append([]string{"--remote", "--url", ref.Endpoint}, args...)
	}
	cmd := exec.CommandContext(ctx, runtime, args...)
	if ref != nil {
		cmd.Env = network.Environment(nil, *ref)
	}
	var out limitedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		return nil, fmt.Errorf("container runtime %s failed: %w", args[0], err)
	}
	return out.Bytes(), nil
}

// Identity pins both the daemon/store identity and the selected endpoint.
func Identity(ctx context.Context, runtime string) (string, error) {
	return IdentityAt(ctx, runtime, nil)
}
func IdentityAt(ctx context.Context, runtime string, ref *network.Reference) (string, error) {
	return identityAt(ctx, runtime, ref, false)
}

func identityAt(ctx context.Context, runtime string, ref *network.Reference, recovery bool) (string, error) {
	data, err := outputAt(ctx, runtime, ref, recovery, "info", "--format", "{{json .}}")
	if err != nil {
		return "", err
	}
	var info struct {
		ID   string `json:"ID"`
		Host struct {
			Hostname     string `json:"hostname"`
			RemoteSocket struct {
				Path string `json:"path"`
			} `json:"remoteSocket"`
		} `json:"host"`
		Store struct {
			GraphRoot string `json:"graphRoot"`
		} `json:"store"`
	}
	if err = json.Unmarshal(data, &info); err != nil {
		return "", fmt.Errorf("invalid runtime identity response")
	}
	if ref != nil {
		if info.Host.Hostname == "" || info.Store.GraphRoot == "" {
			return "", fmt.Errorf("runtime has no supported rootful daemon/store identity")
		}
		stable, _ := json.Marshal(struct {
			Info      any
			Reference *network.Reference
		}{info, ref})
		hash := sha256.Sum256(stable)
		return hex.EncodeToString(hash[:]), nil
	}
	var endpoint []byte
	if info.ID != "" {
		endpoint, err = Output(ctx, runtime, "context", "inspect")
	} else {
		if info.Host.Hostname == "" || info.Store.GraphRoot == "" {
			return "", fmt.Errorf("runtime has no supported daemon/store identity")
		}
		endpoint, err = Output(ctx, runtime, "system", "connection", "list", "--format", "json")
	}
	if err != nil {
		return "", err
	}
	// Canonicalize endpoint metadata; irrelevant JSON whitespace is not identity.
	var connections any
	if err = json.Unmarshal(endpoint, &connections); err != nil {
		return "", fmt.Errorf("invalid runtime endpoint response")
	}
	endpoint, _ = json.Marshal(connections)
	stable, _ := json.Marshal(info)
	for _, key := range []string{"DOCKER_HOST", "DOCKER_CONTEXT", "CONTAINER_HOST", "CONTAINER_CONNECTION"} {
		stable = append(stable, []byte("\x00"+key+"="+os.Getenv(key))...)
	}
	hash := sha256.Sum256(append(append(stable, 0), endpoint...))
	return hex.EncodeToString(hash[:]), nil
}

type inspected struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

func Inspect(ctx context.Context, owner Owner) (id string, exists bool, err error) {
	identity, err := identityAt(ctx, owner.Runtime, owner.Network, true)
	if err != nil {
		return "", false, err
	}
	if identity != owner.Identity {
		return "", false, fmt.Errorf("container runtime identity changed")
	}
	data, inspectErr := outputAt(ctx, owner.Runtime, owner.Network, true, "container", "inspect", owner.Name)
	if inspectErr != nil {
		listing, listErr := outputAt(ctx, owner.Runtime, owner.Network, true, "ps", "--all", "--no-trunc", "--format", "{{.ID}} {{.Names}}")
		if listErr != nil {
			return "", false, fmt.Errorf("container absence cannot be confirmed: %w", listErr)
		}
		for _, line := range strings.Split(string(listing), "\n") {
			fields := strings.Fields(line)
			for _, field := range fields {
				for _, name := range strings.Split(field, ",") {
					if name == owner.Name || owner.ID != "" && name == owner.ID {
						return "", true, fmt.Errorf("container exists but inspection failed")
					}
				}
			}
		}
		return "", false, nil
	}
	var records []inspected
	if json.Unmarshal(data, &records) != nil || len(records) != 1 || records[0].ID == "" {
		return "", true, fmt.Errorf("invalid container inspection response")
	}
	object := records[0]
	if strings.TrimPrefix(object.Name, "/") != owner.Name {
		return "", true, fmt.Errorf("container name does not match ownership record")
	}
	if owner.ID != "" && object.ID != owner.ID {
		return "", true, fmt.Errorf("container identity does not match ownership record")
	}
	if len(owner.Labels) == 0 {
		return "", true, fmt.Errorf("container ownership labels are missing")
	}
	for name, value := range owner.Labels {
		if value == "" || object.Config.Labels[name] != value {
			return "", true, fmt.Errorf("container ownership label mismatch: %s", name)
		}
	}
	return object.ID, true, nil
}

func Remove(ctx context.Context, owner Owner) error {
	id, exists, err := Inspect(ctx, owner)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	_, removeErr := outputAt(ctx, owner.Runtime, owner.Network, true, "rm", "--force", id)
	owner.ID = id
	_, exists, err = Inspect(ctx, owner)
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
