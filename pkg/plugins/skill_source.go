package plugins

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
)

// ResolveSkillPackage captures an explicitly supplied local, digest-pinned
// package. It neither consults host plugin installations nor fetches a registry.
func ResolveSkillPackage(ctx context.Context, ref workflows.SkillPackageReference) (*workflows.SkillSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(ref.Root) {
		return nil, fmt.Errorf("skill package root must be absolute")
	}
	captured, err := Capture(ref.Root)
	if err != nil {
		return nil, err
	}
	if captured.Digest() != ref.Digest {
		return nil, fmt.Errorf("skill package digest changed")
	}
	selected, err := captured.Binding(ref.Skill)
	if err != nil {
		return nil, err
	}
	var entries []workflows.AssetInput
	prefix := path.Dir(selected.Path) + "/"
	for _, resource := range captured.Descriptor().Resources {
		resource = path.Clean(strings.TrimPrefix(resource, "./"))
		if resource != path.Dir(selected.Path) && !strings.HasPrefix(resource, prefix) {
			return nil, fmt.Errorf("skill package resources outside the selected skill directory are not supported")
		}
	}
	for _, entry := range captured.manifest.Entries {
		if captured.resourceFiles[entry.Path] && !strings.HasPrefix(entry.Path, prefix) {
			return nil, fmt.Errorf("skill package resources outside the selected skill directory are not supported")
		}
		if !strings.HasPrefix(entry.Path, prefix) {
			continue
		}
		if path.Base(entry.Path) == "SKILL.md" && entry.Path != selected.Path {
			return nil, fmt.Errorf("selected skill directory contains another SKILL.md")
		}
		file := captured.files[entry.Path]
		entries = append(entries, workflows.AssetInput{Path: entry.Path, Mode: file.mode, Data: append([]byte(nil), file.data...)})
	}
	return workflows.NewSkillSource(selected.Name, selected.Path, captured.Digest(), entries)
}
