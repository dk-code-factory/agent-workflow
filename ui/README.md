# Workflow UI

`@agent-workflow/ui` embeds this repository's workflow definition authoring in a host application. It includes a job graph, step navigation, property forms, and synchronized YAML. Persistence, execution, filesystem resolution, credentials, and agent availability belong to the host.

Development requires Node 24. React exports support React 18 and 19. The browser targets are modern Chrome, Firefox, and Safari. The package is independent of the Go build. It is prepared for packing locally; it has not been published to a registry.

## Install and embed

Build and pack from this repository:

```sh
make ui-install ui-check ui-test ui-build
cd ui && npm pack
# In your application:
npm install /path/to/agent-workflow-ui-0.1.0.tgz
```

React applications import the styles once and give the component a container with a height:

```tsx
import { WorkflowEditor, WorkflowViewer } from '@agent-workflow/ui';
import '@agent-workflow/ui/style.css';

<div style={{ height: 800 }}>
  <WorkflowEditor
    source={yamlSource}
    theme="system"
    onChange={snapshot => setYamlSource(snapshot.source)}
    onSave={async snapshot => persist(snapshot.source)}
  />
</div>
```

`source` is the initial document and accepts subsequent host replacements. The editor keeps its own draft between host updates; an unchanged source prop does not reset the draft. `onChange` runs after edits and validation/save state changes. Inline React callbacks and catalogs are supported. Use `validationKey` to rerun validation when external policy or host context changes without a source edit. React and ReactDOM are peers and are external in the ES module build.

Plain JavaScript module consumers use:

```js
import { mountWorkflowEditor, mountWorkflowViewer } from '@agent-workflow/ui';
import '@agent-workflow/ui/style.css';

const editor = mountWorkflowEditor(document.getElementById('workflow'), {
  source: yamlSource,
  onChange: snapshot => console.log(snapshot.diagnostics),
});
editor.update({ theme: 'dark' });
const draft = editor.getSnapshot();
editor.destroy();
```

`update` merges options. Host document replacements participate in the same undo history. `getSnapshot` returns an independent copy. `destroy` unmounts React, destroys CodeMirror, cleans up custom forms, and aborts pending validation. Each mount has its own document, selection, history, layout, and viewport.

For a framework-free HTML page, copy `dist/standalone.js` and `dist/style.css` to your application's static assets:

```html
<link rel="stylesheet" href="./style.css">
<div id="workflow" style="height:800px"></div>
<script src="./standalone.js"></script>
<script>
  const editor = AgentWorkflow.mountWorkflowEditor(
    document.getElementById('workflow'),
    { source: 'jobs: {hello: {steps: [{argv: [echo, hello]}]}}' }
  );
</script>
```

The IIFE includes React and the canvas/editor runtime. It needs no global framework installation. Both bundles use local styles and assets and make no provider, execution, or persistence requests.

## Snapshots, validation, and saving

A snapshot contains `source`, `workflow` when the current structure is usable, `previewWorkflow` for the last usable graph, `stale`, `diagnostics`, `revision`, history availability, and asynchronous save/validation state. The source remains authoritative. Opening/exporting without changes preserves every byte, including line endings. Visual edits use YAML document nodes to preserve comments, mapping order, block prompts, and unrecognized fields; formatting can be normalized when an edit is serialized. Explicitly replacing a structured field replaces that subtree, including its comments.

Diagnostics identify their `source` (`parser`, `browser`, or `host`), severity, code, document path, and source location where available. Browser parsing enforces one document, the 4 MiB byte limit, bounded nesting, no aliases/merge keys/duplicate keys, field shapes, and unknown fields. Browser admission checks IDs, dependency integrity/cycles, command/action requirements, repeat bounds, and standalone restrictions. Unknown fields remain in the document for round trips. Duplicate dependencies are a browser warning (the runner currently accepts them), and new duplicate connections are rejected.

The browser does not evaluate expressions or provide a complete runner/runtime admission substitute. It inspects recognized dot-path references in `${{ ... }}` expressions and bare `if`/`repeat.until` conditions, excluding quoted operands. Only fields evaluated by the runner participate; names, callable defaults, and imported metadata remain literal. Bracket/computed paths are not rewritten. Role availability and reusable resolution can use optional `agents` and `workflows` catalogs; these checks are advisory and do not resolve files or nested calls. Structured output contracts need the runner's schema admission (including Go RE2 semantics), which is explicitly marked as a host check.

```ts
const options = {
  source: yamlSource,
  agents: [{ id: 'claude', label: 'Claude' }],
  workflows: [{ id: 'workflows/classify.yml', label: 'Classifier' }],
  validate: async (snapshot, signal) => {
    const response = await fetch('/validate', {
      method: 'POST', body: snapshot.source, signal,
    });
    if (!response.ok) throw new Error('Validation unavailable');
    return response.json(); // Diagnostic[] with source: 'host'
  },
  onSave: async snapshot => {
    await saveDraft(snapshot.source);
  },
};
```

`validationKey` accepts a string or number identifying the host validation context; changing it reruns validation without editing the source. Superseded validation requests receive an aborted signal and their results are ignored even if the host ignores cancellation. Hook failures become diagnostics or `saveError` and are visible in the editor. Source edits clear old save errors; a failure for an older revision is ignored. Save and export accept drafts with diagnostics. Saving requires an `onSave` hook and is never automatic. Treat submitted sources and host validation as untrusted input on your server.

Malformed or structurally unusable YAML retains its text and the last graph, marks the preview stale, and suspends visual edits until recovery. Syntax edits, property edits, and graph mutations share undo/redo. Uncommitted text in a form is applied on blur or Enter. YAML supports Mod-Z/Mod-Shift-Z/Mod-Y, search (Mod-F), and expression completion (Ctrl-Space) based on the cursor context. Multiline prompts and commands provide an Insert expression picker; literal metadata fields do not suggest expressions. Mod-S commits the active property field and invokes optional save. Graph controls expose zoom and fit; Auto layout explicitly recalculates positions. The dependency checkboxes provide keyboard authoring without canvas gestures.

Deleting a job or step shows referenced document paths and requires confirmation. Job deletion removes structural dependencies; expression references remain and produce diagnostics. Renaming a job, step, callable input/secret, or job output updates recognized references atomically. Guided interface and output sections also offer identifier fields and deletion-impact review. Before reordering, duplicating, deleting, or appending steps, existing unnamed steps receive their current `step_N` IDs. New steps get explicit unique IDs.

`WorkflowViewer` and `mountWorkflowViewer` retain navigation, source search, selection, expansion, zoom, and inspection while disabling document editing. Source can still be replaced by the host through `update`.

Selecting a job or step in the graph, navigation, or inspector reveals and selects its definition in the YAML source. Focus stays on the selected control. Re-selecting an element reveals it again after manually scrolling the source; opening YAML after a selection reveals that element too. Navigation does not change the document or undo history, and is suspended while the preview is stale.

## Custom action authoring

Host definitions change labels and authoring forms; they do not register runner actions or relax runner admission:

```ts
const actions = [{
  uses: 'agent/claude',
  label: 'Reviewer',
  fields: [{ key: 'model', label: 'Review model', type: 'text' }],
  renderInspector(element, context) {
    const button = document.createElement('button');
    button.type = 'button';
    button.textContent = 'Use review prompt';
    button.disabled = context.readOnly;
    button.onclick = () => context.setField(['with', 'prompt'], 'Review this task');
    element.append(button);
    return () => { button.onclick = null; }; // Called on update/unmount
  },
}];
```

Fields support `text`, `textarea`, `boolean`, and `yaml`. Keys are relative to the step's `with` mapping. A custom renderer gets a copied `step`, the absolute document `path`, `readOnly`, and a guarded `setField` accepting paths relative to that step. Respect `readOnly` in custom controls. Cleanup runs before a replacement render and when unmounted. Host-required field policies belong in additional validation. Built-in forms cover agent prompts/output contracts, direct argv commands, explicit shell commands, all five controller actions, reusable calls, conditions, repeats, and error handling. Guided callable fields cover input types/defaults, required secrets, and outputs; YAML fields expose advanced mappings.

## Styling and layout persistence

Set `theme` to `light`, `dark`, or `system` (default). Use the `tokens` option or scoped CSS custom properties: `--aw-bg`, `--aw-panel`, `--aw-text`, `--aw-muted`, `--aw-border`, `--aw-accent`, `--aw-focus`, `--aw-error`, `--aw-radius`, `--aw-syntax-key`, `--aw-syntax-string`, and `--aw-syntax-literal`. Token overrides belong in `tokens` because theme defaults are defined on the editor root. Styles are scoped to `.aw-editor`; React Flow also supplies its standard `.react-flow` styles. Focus indicators and reduced-motion preferences are supported. Container queries turn the side panels into drawers below 760px, independently for each instance. Narrow containers initially show Visual mode, with YAML and Split available; hidden panels are excluded from keyboard focus. Canvas resizing refits the graph, and new jobs avoid existing positions.

`layout` and `onLayoutChange` carry `{ positions, collapsed, viewport }` outside the workflow YAML. Store them separately, keyed by the host's document ID. Positions use job IDs as keys. Initial layout is automatic when positions are missing. Renames carry layout entries to the new ID; undo restores the document without rolling back navigation metadata.

```ts
mountWorkflowEditor(element, {
  source,
  layout: savedLayout,
  onLayoutChange: layout => persistLayout(documentID, layout),
});
```

## Development and verification

```sh
make ui-install
npm exec --prefix ui -- playwright install --with-deps chromium firefox webkit
make ui-check ui-test ui-browser-test
make test check
npm run dev --prefix ui
```

The playground loads all four repository examples and saves only layout to local storage. Browser tests exercise module embedding under React 19/StrictMode, plain mounts, read-only controls, custom renderer cleanup, keyboard dependencies, independent instances, themes, and container resizing, real canvas gestures, controlled hosts, host form embedding, and WCAG contrast/focus checks. Packed consumer fixtures build a React 18 application and a standalone HTML page, then exercise them in Chromium, Firefox, and WebKit. Shared `testdata/ui-compat` cases run through JavaScript checks and Go `Parse`/`Validate`. `Workflow UI` is included in CI's required aggregate job. All these checks are deterministic and use no live providers or GitHub publication.

## Required flow gate

Every PR, including an unrelated PR, must pass this local command before it is
ready and again after relevant edits:

```sh
make ui-install
cd ui && npx playwright install --with-deps chromium
cd ..
make ui-pr-check BASE=origin/main
```

Use the PR target branch for `BASE`. The checker compares its merge base with
HEAD against the complete working tree (committed, staged, unstaged, and
untracked inputs). If history is missing it prints the exact fetch and rerun
commands; it never skips pairing silently. For discovery and synchronization
without launching browsers, run `make ui-flows-check BASE=origin/main`.
`make ui-flow-checker-test` runs disposable Git fixture tests of the gate itself.

The [user flows](../docs/UI_USER_FLOWS.md) each link to their dedicated
`test/browser/flows/uf-NN.spec.ts`. IDs stay stable when flows are added or
removed. Tests interact with visible controls; host APIs set up fixtures and
inspect outcomes. The gate discovers cases through Playwright without launching
a browser. It rejects missing/duplicate/malformed IDs, missing/orphan specs,
broken links, focused tests, and skipped or expected-failure declarations.
At runtime every selected case must pass and execute a successful assertion in
its test body; an empty case or assertions only in setup hooks cannot cover a flow.

A changed UF section requires a substantive change to its owning test. A changed
test requires a substantive change to that UF section. Shared test imports are
followed in both revisions so removed imports and deleted helpers still count.
The browser HTML/TSX harness conservatively affects every harness flow. A helper
change can satisfy the test side of pairing, but every affected UF still needs
a prose update. Formatting, comments, and coverage-link edits do not count.
When behavior has not changed, revise the short **Coverage** note to explain the
new assertions or recovery scenario. Do not use timestamps, checksums, or empty
acknowledgments. Failures name the UF and title, changed file/section, missing
counterpart, repair action, and rerun command; shared helper failures identify
the affected group. CI emits the same findings as annotations and job summaries.

Browser selection is conservative:

- UF/spec changes select those flows; shared helper or fixture changes select
  their affected flows.
- UI source, styles, playground, examples, dependencies, and browser/build/gate
  configuration select all flows.
- Unrelated changes still check all mappings but start no browser.

The local run uses Chromium, two workers, no retries, and automatic waiting.
It needs neither packed-consumer artifacts nor package builds or provider
credentials. The Workflow UI CI job runs the structural gate before builds and
browser installation. PR pairing uses the actual PR base SHA with full Git
history. Push and reusable-workflow runs validate structure and run all flows.
The existing Chromium/Firefox/WebKit suite executes the UF specs once alongside
technical embedding and packed-consumer checks, retaining traces and uploading
browser reports when it fails. Passing the gate establishes traceability and
executed assertions; reviewers still assess whether each test represents its
flow. The seven separately tracked UX issues remain outside this coverage work.

With installed dependencies and browsers on the implementation machine (Node 24,
two workers), structural validation took about 1 second; all 65 UF cases in
Chromium took 14.9 seconds, and the complete `make ui-pr-check` command including
checker self-tests took 23.7 seconds. These are local measurements, with targets
of roughly 2 seconds for structure and under 30 seconds for the full local run.
Unrelated-change fixtures verified that the command starts no browser.
