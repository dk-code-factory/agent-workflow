# code-review-ad reviewer profile

This profile runs Toolkit, Standards and Spec as three fresh, parallel engine
sessions. Each uses the installed package's original pass brief, shared reporting
and inspection rules, selected checks, source-preparation helper and same-session
aspect driver. The existing independent verifier and fixer run afterward.

Prepare the authored profile before committing the workflow fixture:

```sh
python3 examples/pr-fix/profiles/code_review_ad.py \
  --plugin-root /path/to/installed/code-review-ad/1.1.0 \
  --workspace /path/to/fixture \
  --context review-context/linked-spec.json
```

The optional, repeatable `--context` selects existing frozen specification or
instruction files in the fixture. It must not contain previous findings. The
script snapshots package files byte for byte into
`examples/pr-fix/review-resources/code-review-ad`, records hashes, and writes
`examples/pr-fix/workflows/code-review-ad.yml`. It refuses to overwrite an existing profile
or resource snapshot. Commit both generated paths and supplemental context on the
fixture's base before adding the PR's source changes. They then stay outside the
reviewed PR diff. The workflow pins all selected resources for coverage reuse.

Package collection streams directory entries and allows at most 4096 files,
4 MiB per file, 16 MiB of file contents, and 4096 queued directories. Oversized
inputs fail before the script writes profile assets.

Set the deployment's model and existing credentials in `agents.local.yml`. The
example reviewer, verifier and fixer definitions select `high` reasoning effort,
matching the reference Codex trial; the deployment selects `gpt-6.1-sol` for that
comparison. These are operator settings, not changes to the package briefs.

Start from the clean PR head, with its target available and private request and
deployment files excluded from Git status:

```sh
agent-workflow validate --config examples/pr-fix/agents.local.yml \
  examples/pr-fix/workflows/code-review-ad.yml
agent-workflow run --config examples/pr-fix/agents.local.yml --detach \
  --input request=examples/pr-fix/request.json \
  --secret-env github_token=GH_TOKEN examples/pr-fix/workflows/code-review-ad.yml
```

The profile selects two retained review cycles, empty finalizers and no native or
CI retries. Publication permissions retain the example's false defaults. Original
request scope still governs source edits and local commits.

The engine schedules the native passes directly; it does not invoke the installed
plugin's nested coordinator or lifecycle/update machinery. Each pass collects its
own pinned source facts, while the standalone skill prepares those once in its
coordinator. Original pass outputs and successful aspect-driver commands belong
in each report's deliverables. Keep that execution difference visible when
comparing findings, and compare equivalent assertions/categories across passes
rather than treating duplicate candidate IDs as separate defects.
