package development

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// pushNewBranch checks the remote SHA advertised for the actual push, closing
// the gap between ls-remote reconciliation and push negotiation without force.
// Git's receive-pack then performs its normal compare-and-swap ref update.
func (r *Runtime) pushNewBranch(ctx context.Context, remote, branch, sha string) error {
	original, err := r.git(ctx, r.Root, "rev-parse", "--path-format=absolute", "--git-path", "hooks/pre-push")
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp(filepath.Join(r.Root, ".agent-workflow"), "push-hooks-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	script := `#!/bin/sh
set -eu
umask 077
input="$AGENT_WORKFLOW_PUSH_HOOK_DIR/input"
cat > "$input"
while read -r local_ref local_sha remote_ref remote_sha; do
  if [ "$remote_ref" != "$AGENT_WORKFLOW_EXPECTED_REF" ] || [ "$local_sha" != "$AGENT_WORKFLOW_EXPECTED_SHA" ]; then
    echo 'unexpected push ref; publication stopped' >&2
    exit 1
  fi
  if [ "$remote_sha" != "$AGENT_WORKFLOW_ZERO_SHA" ] && [ "$remote_sha" != "$AGENT_WORKFLOW_EXPECTED_SHA" ]; then
    echo 'unexpected remote branch change during push negotiation; publication stopped' >&2
    exit 1
  fi
done < "$input"
if [ -x "$AGENT_WORKFLOW_ORIGINAL_PRE_PUSH" ]; then
  "$AGENT_WORKFLOW_ORIGINAL_PRE_PUSH" "$@" < "$input"
fi
`
	if err = os.WriteFile(filepath.Join(dir, "pre-push"), []byte(script), 0700); err != nil {
		return err
	}
	env := append(os.Environ(), "AGENT_WORKFLOW_PUSH_HOOK_DIR="+dir, "AGENT_WORKFLOW_EXPECTED_REF=refs/heads/"+branch, "AGENT_WORKFLOW_EXPECTED_SHA="+sha, "AGENT_WORKFLOW_ZERO_SHA="+strings.Repeat("0", len(sha)), "AGENT_WORKFLOW_ORIGINAL_PRE_PUSH="+original)
	_, err = r.gitEnv(ctx, r.Root, env, "-c", "core.hooksPath="+dir, "push", "--porcelain", remote, sha+":refs/heads/"+branch)
	if err != nil {
		return fmt.Errorf("guarded non-force push: %w", err)
	}
	return nil
}
