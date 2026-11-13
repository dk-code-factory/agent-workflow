# Historical Go PR-fix reference

This package preserves the previous Go application's deterministic regression
and parity fixtures. The stock CLI does not import this package, register its
actions, wrap native sessions with it, or provide the previous `pr-fix` command.
Legacy YAML and sample settings live under `testdata` and are excluded from the
stock `init` distribution.

The legacy agent-turn and checklist-follow-up fixtures set a five-minute
lifecycle-handler deadline for durable checkpoints on slow race-test hosts.
This uses the existing workflow setting; the engine's default stays unchanged.

Use the [portable Python/YAML example](../../examples/pr-fix/README.md) for new
workflows. Its [current contract](../../docs/PR_FIX.md) documents deliberate
compatibility changes; historical Go tests alone establish no portable parity.
