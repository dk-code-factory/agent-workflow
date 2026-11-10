package prfix

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
)

// Source includes MIT notices and immutable 5.2.0 configuration readers. Only
// read_settings.py is executed; no source task state or provider runner is used.
//
//go:embed source read_settings.py
var source embed.FS

func materializeSource(root string) (string, error) {
	reader, e := source.ReadFile("read_settings.py")
	if e != nil {
		return "", e
	}
	base := filepath.Join(root, ".agent-workflow", "pr-fix", "contracts", Implementation+"-"+digest(string(reader))[:16])
	err := fs.WalkDir(source, ".", func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		b, e := source.ReadFile(path)
		if e != nil {
			return e
		}
		target := filepath.Join(base, filepath.FromSlash(path))

		return writeImmutable(target, b)
	})
	return base, err
}
func resolveSelection(ctx context.Context, root string, id Identity, upstream string, options map[string]any, paths []string) (Selection, error) {
	if paths == nil {
		paths = []string{}
	}
	base, e := materializeSource(root)
	if e != nil {
		return Selection{}, e
	}
	request := map[string]any{"repository": id.Repository, "pr": id.URL, "upstream": upstream, "options": options, "checkout": root, "paths": paths}
	b, e := json.Marshal(request)
	if e != nil {
		return Selection{}, e
	}
	out, e := runCommand(ctx, root, b, "python3", "-I", "-B", filepath.Join(base, "read_settings.py"))
	if e != nil {
		return Selection{}, fmt.Errorf("read pinned settings: %w", e)
	}
	var result Selection
	e = json.Unmarshal(out, &result)
	if e != nil {
		return result, e
	}
	return result, result.Limits().Validate()
}
