package prfix

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

func git(ctx context.Context, root string, args ...string) (string, error) {
	b, e := runCommand(ctx, root, nil, "git", args...)
	return strings.TrimSpace(string(b)), e
}
func capture(ctx context.Context, root string, provider Provider, p Plan) (Snapshot, error) {
	ev, e := provider.Capture(ctx, p.Identity)
	if e != nil {
		return Snapshot{}, e
	}
	m := ev.Metadata
	s := Snapshot{Identity: p.Identity, Author: m.User.Login, Viewer: ev.Viewer, HeadRepository: strings.ToLower(p.Identity.Host + "/" + m.Head.Repo.FullName), HeadBranch: m.Head.Ref, TargetBranch: m.Base.Ref, Target: m.Base.SHA, Head: m.Head.SHA, Open: m.State == "open", Draft: m.Draft, Instructions: digest(p.Instructions)}
	if s.Author == "" || s.Viewer == "" || s.HeadRepository == p.Identity.Host+"/" || !s.Open {
		return s, fmt.Errorf("complete identity of an open PR is required")
	}
	owned := strings.EqualFold(s.Author, s.Viewer)
	for _, account := range p.Selection.Accounts {
		owned = owned || strings.EqualFold(account.Host, s.Identity.Host) && account.ID > 0 && account.ID == m.User.ID
	}
	if !owned {
		if p.MaintainOther == nil {
			return s, fmt.Errorf("incoming review: explicit permission to maintain another author's PR is required")
		}
		if e = p.MaintainOther.Validate(); e != nil {
			return s, e
		}
	}
	if !validSHA(s.Head) || !validSHA(s.Target) {
		return s, fmt.Errorf("provider must pin complete head and target SHAs")
	}
	if _, e = ParseIdentity("https://" + s.HeadRepository + "/pull/1"); e != nil {
		return s, fmt.Errorf("invalid pinned head repository")
	}
	for _, branch := range []string{s.HeadBranch, s.TargetBranch} {
		if _, e = git(ctx, root, "check-ref-format", "refs/heads/"+branch); e != nil {
			return s, fmt.Errorf("invalid pinned branch")
		}
	}
	if s.LocalHead, e = git(ctx, root, "rev-parse", "--verify", "HEAD^{commit}"); e != nil {
		return s, e
	}
	if s.Base, e = git(ctx, root, "merge-base", s.Target, s.LocalHead); e != nil {
		return s, fmt.Errorf("target/head objects must be available locally: %w", e)
	}
	for _, pin := range []struct {
		ref string
		dst *string
	}{{s.Target, &s.TargetTree}, {s.Base, &s.BaseTree}, {s.Head, &s.HeadTree}, {s.LocalHead, &s.LocalTree}} {
		*pin.dst, e = git(ctx, root, "rev-parse", "--verify", pin.ref+"^{tree}")
		if e != nil {
			return s, e
		}
	}
	diff, e := git(ctx, root, "diff", "--binary", "--no-ext-diff", "--no-renames", s.Base, "--")
	if e != nil {
		return s, e
	}
	s.Diff = digest(diff)
	// A worktree fingerprint includes tracked edits. Untracked files are captured
	// explicitly so a worker cannot accidentally obtain coverage of omitted code.
	status, e := gitStatus(ctx, root)
	if e != nil {
		return s, e
	}
	if status != "" {
		s.LocalTree = digest([]string{s.LocalTree, diff, status})
	}
	paths, e := gitRaw(ctx, root, "diff", "--name-only", "--no-renames", "-z", s.Base, "--")
	if e != nil {
		return s, e
	}
	s.Paths = []string{}
	for _, path := range strings.Split(paths, "\x00") {
		if !utf8.ValidString(path) {
			return s, fmt.Errorf("non-UTF-8 repository paths are unsupported")
		}
		if path != "" {
			s.Paths = append(s.Paths, path)
		}
	}
	untrackedNames, e := gitRaw(ctx, root, "ls-files", "--others", "--exclude-standard", "-z", "--", ".", ":(exclude).agent-workflow", ":(exclude)workflows/.pr-fix")
	if e != nil {
		return s, e
	}
	untracked := map[string]any{}
	remaining := int64(64 << 20)
	for _, path := range strings.Split(untrackedNames, "\x00") {
		if path == "" {
			continue
		}
		if len(untracked) >= 10000 {
			return s, fmt.Errorf("untracked snapshot exceeds 10000 entries")
		}
		if !utf8.ValidString(path) {
			return s, fmt.Errorf("non-UTF-8 repository paths are unsupported")
		}
		full := filepath.Join(root, path)
		st, e := os.Lstat(full)
		if e != nil {
			return s, e
		}
		var hash string
		if st.Mode()&os.ModeSymlink != 0 {
			target, e := os.Readlink(full)
			if e != nil {
				return s, e
			}
			remaining -= int64(len(target))
			hash = rawHash([]byte(target))
		} else {
			if !st.Mode().IsRegular() || st.Size() > remaining {
				return s, fmt.Errorf("untracked snapshot requires regular files within 64 MiB total")
			}
			file, err := os.Open(full)
			if err != nil {
				return s, err
			}
			actual, err := file.Stat()
			if err != nil || !actual.Mode().IsRegular() || !os.SameFile(st, actual) {
				_ = file.Close()
				return s, fmt.Errorf("untracked file changed before reading: %s", path)
			}
			h := sha256.New()
			n, err := io.Copy(h, io.LimitReader(file, remaining+1))
			closeErr := file.Close()
			if err != nil {
				return s, err
			}
			if closeErr != nil {
				return s, closeErr
			}
			remaining -= n
			hash = hex.EncodeToString(h.Sum(nil))
		}
		if remaining < 0 {
			return s, fmt.Errorf("untracked snapshot exceeds 64 MiB total")
		}
		if e = ctx.Err(); e != nil {
			return s, e
		}
		untracked[path] = []any{hash, st.Mode().String()}
		s.Paths = append(s.Paths, path)
	}
	sort.Strings(s.Paths)
	s.Diff = digest([]any{diff, untracked})
	if status != "" {
		s.LocalTree = digest([]any{s.LocalTree, untracked})
	}
	history, e := git(ctx, root, "log", "--format=raw", s.Target+".."+s.LocalHead)
	if e != nil {
		return s, e
	}
	s.History = digest(history)
	s.Discussion = discussionDigest(ev)
	s.Decisions = digest(map[string]any{"body": m.Body, "comments": ev.Comments})
	selection, e := resolveSelection(ctx, root, p.Identity, p.Upstream, p.Options, s.Paths)
	if e != nil {
		return s, e
	}
	// Exact configuration is captured separately from PR content. A replacement
	// workflow is required for changed selections; tighter budgets stay retained.
	s.ReviewPreferences = digest(selection.Preferences["review"])
	s.ImplementationPreferences = implementationPreferenceFingerprint(selection.Preferences["implementation"])
	preferences := selection.Preferences
	selection.Preferences = nil
	s.Settings = digest(selection)
	options := jsonClone(selection.Config.Options)
	for _, key := range []string{"tasks", "finalizers", "reviewers", "limits"} {
		delete(options, key)
	}
	settings := jsonClone(selection.Settings)
	delete(settings, "legacy_process")
	s.TaskSettings = digest(map[string]any{"instructions": selection.Invocation.Instructions, "options": options, "settings": settings, "accounts": selection.Accounts})
	selection.Preferences = preferences
	resources := map[string]string{}
	for _, o := range p.Occurrences {
		for _, path := range o.resourcePaths() {
			originalPath := path
			if !filepath.IsAbs(path) {
				path = filepath.Join(root, path)
			}
			hash, e := resourceDigest(path)
			if e != nil {
				return s, fmt.Errorf("adapter resource %s: %w", originalPath, e)
			}
			resources[originalPath] = hash
		}
	}
	s.ResourceDigests = resources
	var roles struct{ Agents map[string]json.RawMessage }
	if e = json.Unmarshal(p.Roles, &roles); e != nil {
		return s, e
	}
	s.RoleDigests = map[string]string{}
	for role, configuration := range roles.Agents {
		s.RoleDigests[role] = digest(configuration)
	}
	manifest, e := source.ReadFile("source/PROVENANCE.json")
	if e != nil {
		return s, e
	}
	s.RepositoryInstructions, e = repositoryInstructions(root, s.Paths)
	if e != nil {
		return s, e
	}
	s.Resources = digest([]any{Implementation, Version, json.RawMessage(manifest), s.RepositoryInstructions})
	evidence := map[string]any{"pr": ev, "diff": diff, "status": status, "untracked": untracked, "history": history, "settings": selection, "instructions": p.Instructions, "resources": resources}
	if _, err := os.Stat(filepath.Join(root, ".agent-workflow", "pr-fix", "ledger.sqlite")); err == nil {
		ledger, err := OpenLedger(root)
		if err != nil {
			return s, err
		}
		rows, err := ledger.db.QueryContext(ctx, "SELECT id FROM effects WHERE run=? AND state='completed' ORDER BY rowid", p.LogicalRun)
		if err != nil {
			ledger.Close()
			return s, err
		}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				ledger.Close()
				return s, err
			}
			s.CompletedEffects = append(s.CompletedEffects, id)
		}
		err = rows.Err()
		rows.Close()
		ledger.Close()
		if err != nil {
			return s, err
		}
	} else if !os.IsNotExist(err) {
		return s, err
	}
	s.Evidence, e = artifact(root, evidence)
	return s, e
}

func gitRaw(ctx context.Context, root string, args ...string) (string, error) {
	b, e := runCommand(ctx, root, nil, "git", args...)
	return string(b), e
}
func gitStatus(ctx context.Context, root string) (string, error) {
	return gitRaw(ctx, root, "status", "--porcelain=v1", "--untracked-files=all", "--", ".", ":(exclude).agent-workflow", ":(exclude)workflows/.pr-fix")
}

// Unmatched task paths carry no implementation preference. Retain full selected
// records and their matched paths while allowing ordinary edits with no policy.
func implementationPreferenceFingerprint(raw any) string {
	data, _ := json.Marshal(raw)
	selection := map[string]any{}
	if json.Unmarshal(data, &selection) != nil {
		return digest(raw)
	}
	delete(selection, "paths")
	return digest(selection)
}
