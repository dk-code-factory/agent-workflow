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
	source := &workflows.SkillSource{Name: selected.Name, Entry: selected.Path, SourceDigest: captured.Digest()}
	prefix := path.Dir(selected.Path) + "/"
	for _, entry := range captured.manifest.Entries {
		if !strings.HasPrefix(entry.Path, prefix) && !captured.resourceFiles[entry.Path] {
			continue
		}
		if path.Base(entry.Path) == "SKILL.md" && entry.Path != selected.Path {
			continue
		}
		file := captured.files[entry.Path]
		source.Entries = append(source.Entries, workflows.AssetInput{Path: entry.Path, Mode: file.mode, Data: append([]byte(nil), file.data...)})
	}
	return source, nil
}
