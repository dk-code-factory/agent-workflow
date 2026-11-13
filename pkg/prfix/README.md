# Historical Go PR-fix reference

This package preserves the previous Go application's deterministic regression
and parity fixtures. The stock CLI does not import this package, register its
actions, wrap native sessions with it, or provide the previous `pr-fix` command.
Legacy YAML and sample settings live under `testdata` and are excluded from the
stock `init` distribution.

Use the [portable Python/YAML example](../../examples/pr-fix/README.md) for new
workflows. Its [current contract](../../docs/PR_FIX.md) documents deliberate
compatibility changes; historical Go tests alone establish no portable parity.
