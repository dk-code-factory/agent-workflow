# Registered Go application actions

An embedding Go application can register deterministic operations and run them
under workflow sequencing. `workflows.Capabilities()` and controller `GET /health`
advertise `registered-go-actions-v1`. The capability describes the runtime
contract; each host must still register every action its workflows reference.
The CLI does not load application plugins.

```go
type CaptureInput struct {
    Repository string `json:"repository"`
}
type CaptureOutput struct {
    EvidencePath string `json:"evidence_path"`
}

registry := &workflows.ActionRegistry{}
err := workflows.RegisterAction(registry, "capture", workflows.ActionSpec[CaptureInput, CaptureOutput]{
    Implementation: "code-factory/capture@sha256:immutable-build-digest",
    Effect: workflows.ActionReadOnly,
    Bindings: func(ctx context.Context, op workflows.ActionContext, in CaptureInput) (json.RawMessage, error) {
        // Resolve and pin the application repository, configuration, and policy.
        return json.Marshal(map[string]string{"repository": in.Repository})
    },
    Handler: func(ctx context.Context, op workflows.ActionContext, in CaptureInput) (workflows.ActionResult[CaptureOutput], error) {
        // Application helpers capture evidence into application-owned storage.
        path, digest, err := capture(ctx, in.Repository, op.OperationKey)
        evidence, marshalErr := json.Marshal(map[string]string{"path": path, "sha256": digest})
        return workflows.ActionResult[CaptureOutput]{
            Output: CaptureOutput{EvidencePath: path}, Evidence: evidence,
        }, errors.Join(err, marshalErr)
    },
    ValidateCheckpoint: func(ctx context.Context, op workflows.ActionContext, cp workflows.ActionCheckpoint[CaptureInput, CaptureOutput]) error {
        // Read and verify the original evidence, digest, and repository binding.
        return verifyCapture(ctx, cp.Input.Repository, cp.Result.Output.EvidencePath, cp.Result.Evidence)
    },
})
if err != nil { return err }
executor.ApplicationActions = registry
executor.Store = sqliteStore
```

The example's `capture` and `verifyCapture` helpers belong to the embedding
application. Input and output must describe JSON objects. Schemas are derived
from Go types through `jsonschema-go`: JSON tags determine names and optional
fields; struct schemas reject unknown properties. The engine validates rendered
`with` before calling the handler and validates serialized output before durable
completion. Outputs are directly available at `steps.<id>.outputs.<field>`.

```yaml
jobs:
  prepare:
    steps:
      - id: evidence
        uses: action/capture
        with: {repository: dk-code-factory/example}
```

Names follow the existing identifier rules: `[A-Za-z_][A-Za-z0-9_-]*`.
Duplicate registrations, missing implementation/effect/hooks, invalid object
schemas, and missing registrations fail admission. The registry freezes on first
execution/admission; use a new registry for changed contracts. Implementations
must use immutable identities and change identity whenever executable semantics
change. Go closures and their dependencies remain the application's responsibility.

The admitted snapshot pins the action name, implementation, input/output schemas,
effect, and canonical fingerprint. Schema v8 stores action attempts linked to
ordinary step attempts, with rendered input, bindings, input/binding fingerprints,
operation identity, result, evidence, receipts, and reconciliation history.
Admission commits before the handler; completion commits before dependents.
`ActionJournal` is an optional durable store extension; registered actions refuse
stores that do not implement it. SQLite implements the extension transactionally,
using the existing originating-execution owner/generation fence and cascading
retention. `SQLiteStore.ActionAttempts(ctx, runID)` includes descendant records.

Hooks receive run, job, step, repeat iteration, attempt ID/number, execution
generation, and a stable logical `OperationKey`. Bindings, checkpoint validation,
and reconciliation must be read-only, cancellation-aware application operations.
Bind resources independently of the attempt/generation: retries must resolve the
same application identity. Credential values belong in application-owned clients
or closures, referenced through nonsensitive input/bindings. Known credential
literals in input/bindings are rejected before the handler, because redacting
an identity would change its recovery meaning. Known credentials in output,
evidence, receipts, errors, and reconciliation proofs are redacted before storage.
Register application credentials as run secrets for redaction even if hooks use
them through closures; they must be available again when required on resume.

On explicit resume, the engine compares current contracts and current bindings
and invokes the checkpoint validator against retained completed output/evidence.
This check runs before successful job and child shortcuts, including retained
repeat iterations. Implementation, schema, effect, binding, or evidence drift
requires a new run. Integrity and durability failures block dependents and cannot
be suppressed by step/job `continue-on-error` or explicit dependency conditions.

Interrupted `read_only` actions may rerun on explicit resume. An
`external_mutation` may have taken effect even when its handler returns an error
or its completion commit fails. A mutation without a reconciliation hook remains
uncertain. `--retry-uncertain` cannot override this state. An optional typed
`Reconcile` hook receives the original attempt context, input, bindings, output,
evidence and receipt. It returns `ActionReconciliation[O]`:

| Outcome | Required evidence | Runtime behavior |
| --- | --- | --- |
| `completed` | Non-null JSON proof and schema-valid recovered output/evidence accepted by the checkpoint validator | Commit completion, continue without calling the mutation handler |
| `no_effect` | Non-null JSON proof of absence | Commit proof before admitting a new attempt with the same operation key |
| `unknown`, invalid outcome, or error | Retained observation/error | Keep uncertainty and require attention |

Every reconciliation observation is retained with its generation, including
repeated unknown/error outcomes; earlier attempts and receipts remain inspectable.
If a completed recovered result cannot commit, the next resume reconciles again.
After a committed `no_effect` proof, a later attempt retains the logical operation
key and obtains a new physical attempt ID. Do not use a successful read or an
idempotency claim as proof of publication: application code owns GitHub authority,
repository/head verification, publication policy, and durable publication receipts.

The UI accepts `action/<name>`, preserves arbitrary parameters, and supports
existing host custom action fields/renderers. It delegates registration and typed
schema admission to the embedding host. Deterministic tests use local SQLite
fixtures and fake receipts; no provider credentials or real GitHub publication
are needed.
