# UI user flows

These flows describe the current workflow editor and viewer from my point of
view as a user. They cover the UI in [`ui/`](../ui/README.md), including the
local playground. Each flow lists where I start, what I do, and what I see.

The host application supplies the document and may provide saving, validation,
agent choices, reusable workflows, and custom action forms. The embedded UI
authors workflow definitions; running workflows, monitoring runs, logging in,
and publishing pull requests are outside this UI. Adding a `github/pr` step
only changes the definition.

In keyboard shortcuts, **Mod** means Command on macOS and Ctrl elsewhere.
Property text fields apply when I leave them; Enter also applies single-line
fields. Escape discards an uncommitted field edit.

## Opening and navigating

### UF-01 — Explore an existing workflow

**UI tests**: [UI tests](../ui/test/browser/flows/uf-01.spec.ts)

- **Coverage:** Selection reveals long YAML definitions without moving focus, in both editor and viewer.

- **Starting point:** The host opens a workflow in the editor.
- **My flow:** I read the workflow name → inspect the job cards and dependency
  arrows → expand a job to see its steps → select a job or step.
- **What I see:** The inspector shows its properties. In Split or YAML mode,
  the matching YAML definition is selected and scrolled into view without
  taking keyboard focus away from the control I selected.

### UF-02 — Choose how I view the workflow

**UI tests**: [UI tests](../ui/test/browser/flows/uf-02.spec.ts)

- **Coverage:** View switches preserve source and reveal the selected job when YAML opens.

- **Starting point:** A workflow is open.
- **My flow:** I choose **Visual**, **YAML**, or **Split** in the toolbar.
- **What I see:** I get the graph, the source editor, or both. My document stays
  the same. If I select an item in Visual mode and then open YAML, its source
  definition is revealed.

### UF-03 — Find a job or step

**UI tests**: [UI tests](../ui/test/browser/flows/uf-03.spec.ts)

- **Coverage:** Job metadata, step names/IDs, prompts, direct and shell commands,
  duplicate names across jobs, document order, result counts, clearing and no results.
  Pointer, Tab, arrow, Enter/Space and Escape navigation cover collapsed/offscreen
  steps, YAML reveal, viewer mode, scrollable narrow layouts and independent instances.
  Compact Split mode retains canvas space while results scroll.
  Navigation preserves source, revision, undo/redo history and dependency edges;
  form edits, undo and host replacements refresh results.

- **Starting point:** A workflow has several jobs or steps.
- **My flow:** In Visual or Split mode, I type in **Search workflow** to search
  document content → choose a matching job or step from the results → inspect
  its definition. I can Tab through the result buttons, move between them with
  Arrow Up/Down, and activate with Enter or Space. Escape clears the query and
  returns focus to the search input. **Search palette** filters palette entries
  and job navigation.
- **What I see:** A scrollable list in document order identifies each matching
  step's parent job and effective ID. A status announces the result count or
  **No results**; an empty or whitespace-only query hides the list. Selecting a
  result selects that exact definition in the inspector and YAML source, expands
  its parent if needed, and brings the target into view. Viewer mode supports the
  same navigation. Unrelated jobs dim while dependency arrows remain. Search,
  selection, expansion and viewport changes stay outside document undo history.

### UF-04 — Arrange the graph for easier reading

**UI tests**: [UI tests](../ui/test/browser/flows/uf-04.spec.ts)

- **Coverage:** Collapse, zoom, fit, auto layout, and dragging preserve workflow source; drag positions reach host layout storage.

- **Starting point:** I am viewing the graph.
- **My flow:** I pan or zoom → drag job cards → collapse or expand cards → use
  **Fit view** to see the graph, or **Auto layout** to rearrange it.
- **What I see:** The visual arrangement changes without changing the YAML.
  Whether the arrangement survives reopening depends on the host's layout
  storage.

### UF-05 — Navigate in a narrow container

**UI tests**: [UI tests](../ui/test/browser/flows/uf-05.spec.ts)

- **Coverage:** Narrow drawers isolate hidden content and keep YAML usable in a short container.

- **Starting point:** The editor is embedded in a narrow space.
- **My flow:** I use **palette**, **canvas**, and **inspector** to switch panels
  → select a job or step → switch to YAML or Split when I need the source.
- **What I see:** The editor initially uses Visual mode in a narrow container.
  Selecting an item opens the inspector; hidden side panels stay out of the
  keyboard tab order.

## Creating and configuring a workflow

### UF-06 — Start a new workflow

**UI tests**: [UI tests](../ui/test/browser/flows/uf-06.spec.ts)

- **Coverage:** The starter has no browser admission errors and Undo restores the empty source.

- **Starting point:** The host opens an empty document.
- **My flow:** I select **Create workflow** → inspect the first job → change
  the workflow name through **Workflow properties**.
- **What I see:** An untitled workflow appears with a manual trigger and one
  job containing an example direct command. I can use **Undo** to return to
  the empty document. I can also start by typing YAML myself.

### UF-07 — Change workflow properties and triggers

**UI tests**: [UI tests](../ui/test/browser/flows/uf-07.spec.ts)

- **Coverage:** Name, both trigger toggles, and the advanced trigger mapping update the definition.

- **Starting point:** An editable workflow is open.
- **My flow:** I select **Workflow properties** → edit **Name** → enable or
  disable **manual** and **workflow_call** under **Triggers**.
- **What I see:** The title and YAML reflect my changes. For other trigger
  settings, I open **Advanced trigger and interface YAML** and edit the mapping.

### UF-08 — Define a reusable workflow's interface

**UI tests**: [UI tests](../ui/test/browser/flows/uf-08.spec.ts)

- **Coverage:** Callable input type/default/required, secret identifiers, and output expressions are stored through guided controls.

- **Starting point:** I have enabled **workflow_call** in Workflow properties.
- **My flow:** I open **Callable inputs**, **Callable secrets**, or **Callable
  outputs** → select **Add input**, **Add secret**, or **Add output** → edit
  the generated identifier and its settings.
- **What I see:** Inputs offer a type, YAML default, and required checkbox;
  secrets offer a required checkbox; outputs offer a value expression. These
  controls define the interface, rather than entering actual secret values.

## Authoring jobs and dependencies

### UF-09 — Add a job

**UI tests**: [UI tests](../ui/test/browser/flows/uf-09.spec.ts)

- **Coverage:** Job creation assigns unique IDs and stores the edited name and runner.

- **Starting point:** I have a usable, editable workflow.
- **My flow:** I select **Add job** → edit **Job ID**, **Name**, and, if needed,
  **Runner** in the inspector.
- **What I see:** A new job with a unique ID and an example command appears,
  is selected, and is placed clear of existing cards.

### UF-10 — Make one job depend on another

**UI tests**: [UI tests](../ui/test/browser/flows/uf-10.spec.ts)

- **Coverage:** Real connector gestures create an edge and reject a cycle without adding a dependency to the prerequisite.

- **Starting point:** My workflow has at least two jobs.
- **My flow:** I drag from the bottom connector of the prerequisite job to the
  top connector of the dependent job. Alternatively, I select the dependent
  job and check the prerequisite under **Dependencies**.
- **What I see:** An arrow and the corresponding YAML dependency appear.
  Self-dependencies, duplicate connections, and cycles are rejected with an
  error, leaving the dependency unchanged.

### UF-11 — Remove a dependency

**UI tests**: [UI tests](../ui/test/browser/flows/uf-11.spec.ts)

- **Coverage:** Unchecking a prerequisite removes both its structural dependency and graph edge.

- **Starting point:** A job depends on another job.
- **My flow:** I select the dependent job → uncheck the prerequisite in
  **Dependencies**.
- **What I see:** The dependency arrow and YAML entry are removed. I review
  diagnostics for expressions that may still rely on the removed dependency.

### UF-12 — Call another workflow

**UI tests**: [UI tests](../ui/test/browser/flows/uf-12.spec.ts)

- **Coverage:** Cancelling the steps-discard confirmation preserves the source and selected kind; confirmed reusable conversion stores inputs and secrets and supports Undo.

- **Starting point:** I want a job to invoke a reusable workflow.
- **My flow:** If the host provides a catalog, I choose an entry under
  **Reusable workflows**. Otherwise, I select a job, change **Job kind** to
  **Reusable workflow**, and edit **Reusable workflow** → fill in **Call inputs
  (YAML)** and **Call secrets (YAML)** as needed.
- **What I see:** The job shows a reusable-call badge and reference instead of
  a step list. Before replacing existing steps, a confirmation names the
  configuration to discard. I can cancel; after confirming, **Undo** restores
  the prior definition in one edit.

### UF-13 — Repeat a job until a condition is met

**UI tests**: [UI tests](../ui/test/browser/flows/uf-13.spec.ts)

- **Coverage:** Imported null values recover through guided controls; repeat configuration can be added and removed.

- **Starting point:** A job is selected.
- **My flow:** I open **Repeat** → enter **Maximum attempts** and **Until** →
  review the resulting YAML and diagnostics.
- **What I see:** The job card displays its repeat bound. **Remove repeat**
  removes the repeat configuration.

### UF-14 — Expose a job output

**UI tests**: [UI tests](../ui/test/browser/flows/uf-14.spec.ts)

- **Coverage:** Named output fields and advanced output mappings update the job definition.

- **Starting point:** A job is selected.
- **My flow:** I open **Job outputs** → select **Add output** → edit its
  identifier and **Output value**, using an expression when appropriate.
- **What I see:** The output is added to the job definition and becomes
  available in relevant expression suggestions. **Advanced output YAML** lets
  me edit the complete output mapping.

## Authoring steps

### UF-15 — Add a step to a job

**UI tests**: [UI tests](../ui/test/browser/flows/uf-15.spec.ts)

- **Coverage:** Templates append unique explicit IDs and stay disabled without a sequential job, including after confirming replacement of steps by a reusable call.

- **Starting point:** I have selected a job with sequential steps.
- **My flow:** I choose a step template from **Add to workflow**, using
  **Search palette** if needed, or select **Add step** in the job inspector.
- **What I see:** A step with a unique explicit ID is appended and selected.
  Step templates are unavailable until I select a job and cannot be appended
  to a reusable-call job.

### UF-16 — Configure an agent task

**UI tests**: [UI tests](../ui/test/browser/flows/uf-16.spec.ts)

- **Coverage:** Agent settings include prompt, message, model, workspace, and structured output; imported null command fields preserve agent inspection.

- **Starting point:** I add or select an agent step.
- **My flow:** I choose the agent under **Step type**, or edit **Action target**
  → write the **Prompt** → optionally set **Message**, **Model**, **Workspace**,
  and a **Structured output contract (YAML)**.
- **What I see:** The step definition and graph label update. If the host
  supplies an agent catalog, an unavailable agent produces a warning. Output
  contracts may still require host validation before execution.

### UF-17 — Configure a direct command

**UI tests**: [UI tests](../ui/test/browser/flows/uf-17.spec.ts)

- **Coverage:** Argument arrays, working directories, timeouts, and direct-command labels reflect form edits.

- **Starting point:** I add **Direct command** or choose it under Step type.
- **My flow:** I enter the executable and arguments in **Arguments (YAML
  array)** → optionally set **Working directory** and **Timeout**.
- **What I see:** The step stores an argument array and is labelled as a direct
  command. Diagnostics help me spot invalid command configuration.

### UF-18 — Configure a shell command

**UI tests**: [UI tests](../ui/test/browser/flows/uf-18.spec.ts)

- **Coverage:** Shell, command, working directory, and timeout are stored through guided controls.

- **Starting point:** I add **Shell command** or choose it under Step type.
- **My flow:** I set **Shell** and write **Command** → optionally set **Working
  directory** and **Timeout**.
- **What I see:** The step stores the shell and command text, and the graph
  identifies it as a shell command.

### UF-19 — Add workspace, Git, or pull request actions

**UI tests**: [UI tests](../ui/test/browser/flows/uf-19.spec.ts)

- **Coverage:** Parameterized cases confirm discarding the starter command, author all five controller actions, and inspect each configured parameter.

- **Starting point:** I have selected a sequential job.
- **My flow:** I choose an action template → fill in its inspector fields →
  review the resulting YAML.
- **What I configure:**

  | Action | Fields I fill in |
  | --- | --- |
  | `workspace/create` | Commit and Independent workspace |
  | `workspace/snapshot` | Commit |
  | `workspace/integrate` | Path and Commits (YAML) |
  | `git/commit` | Path and Message |
  | `github/pr` | Path, Commit, Title, and Body |

- **What I see:** The action is part of the workflow definition. Authoring it
  does not create a workspace, commit, or pull request.

### UF-20 — Use a custom action form

**UI tests**: [UI tests](../ui/test/browser/flows/uf-20.spec.ts)

- **Coverage:** Host fields and the custom prompt renderer update the appended action parameters. Registered `action/<name>` forms edit bindings while preserving nested custom evidence parameters; browser validation reports that registration and typed schemas require the host runtime.

- **Starting point:** The host provides custom action choices and forms.
- **My flow:** I add the action from the palette or select its Step type →
  complete the custom fields in the inspector.
- **What I see:** My changes appear in the step's YAML parameters. Available
  controls depend on the host application.

### UF-21 — Change a step's type

**UI tests**: [UI tests](../ui/test/browser/flows/uf-21.spec.ts)

- **Coverage:** Incompatible step conversions list discarded fields, cancellation leaves source unchanged, and one Undo restores the original. Command conversions retain timeout/directory settings; Action target uses the same reviewed conversion and retains shared controller parameters. Keyboard Save retries a cancelled target with the latest active Message draft, including a newly populated Message, and submits the converted settings without restoring discarded parameters.

- **Starting point:** A step already has command or action settings.
- **My flow:** I select a different **Step type** → review any confirmation
  naming populated fields that will be discarded → cancel or confirm → fill
  in the new type's fields and inspect the YAML.
- **What I see:** Agent changes keep their parameters, including prompts.
  Switching between direct and shell commands keeps timeout and working
  directory. Controller actions keep parameters supported by both actions.
  Other incompatible settings are removed only after confirmation; the new
  type gets starting values. **Action target** follows the same rules.
  Cancellation preserves the document. If I edit a parameter and retry the
  pending target through keyboard **Save**, confirmation includes the latest
  populated settings before saving the converted definition. One **Undo**
  restores a conversion.

### UF-22 — Control conditions and error handling

**UI tests**: [UI tests](../ui/test/browser/flows/uf-22.spec.ts)

- **Coverage:** Job and step conditions and error flags update the definition and graph indicators.

- **Starting point:** I have selected a job or step.
- **My flow:** I enter **Condition** and optionally check **Continue on error**.
- **What I see:** The YAML updates and the graph shows the corresponding
  condition or error-handling indicator. The UI does not evaluate the condition
  or predict its execution result.

### UF-23 — Insert a reference into an expression

**UI tests**: [UI tests](../ui/test/browser/flows/uf-23.spec.ts)

- **Coverage:** Expression insertion replaces selected text and preserves the cursor; YAML completion offers preceding steps and property hints exclude literal fields.

- **Starting point:** I am editing a field that supports expressions.
- **My flow:** I use field suggestions, choose **Insert expression…** in a
  supported multiline field, or press **Ctrl-Space** at the relevant YAML
  cursor position → select an input or output reference → continue typing.
- **What I see:** The expression is inserted at my cursor or replaces my text
  selection. Suggestions depend on context, including preceding steps and
  dependency outputs; literal metadata fields do not offer expressions.

## Revising and removing content

### UF-24 — Rename an identifier safely

**UI tests**: [UI tests](../ui/test/browser/flows/uf-24.spec.ts)

- **Coverage:** Conflicting IDs can be corrected; callable renames rewrite references and deletion exposes affected paths.

- **Starting point:** I select a job, step, callable interface entry, or job output.
- **My flow:** I edit **Job ID**, **Step ID**, or the entry's **Identifier** →
  apply the edit → review affected expressions and diagnostics.
- **What I see:** Recognized references update together with the identifier;
  job dependencies also follow job renames. Invalid or conflicting identifiers
  show an error so I can correct them. Computed or bracket-style expression
  references need manual review.

### UF-25 — Duplicate a job or step

**UI tests**: [UI tests](../ui/test/browser/flows/uf-25.spec.ts)

- **Coverage:** Job and step copies have unique identifiers and each duplication is undoable.

- **Starting point:** A job or step is selected.
- **My flow:** I select **Duplicate job** or **Duplicate step** → inspect the
  copy → adjust its name, settings, and references for its new purpose.
- **What I see:** A copy with a unique identifier appears. I can undo the
  duplication as one document change.

### UF-26 — Change the order of steps

**UI tests**: [UI tests](../ui/test/browser/flows/uf-26.spec.ts)

- **Coverage:** Reordering unnamed steps materializes stable IDs, preserves references, and reverses through Undo.

- **Starting point:** I select a step in a sequential job.
- **My flow:** I select **Move up** or **Move down** → review the step list and
  any expressions that depend on earlier steps.
- **What I see:** The graph and YAML show the new order. Existing unnamed
  steps receive explicit IDs before structural changes so their original
  identities are preserved. Boundary move buttons are disabled.

### UF-27 — Delete a job, step, input, secret, or output

**UI tests**: [UI tests](../ui/test/browser/flows/uf-27.spec.ts)

- **Coverage:** Deletion exposes structural impacts; Cancel preserves source and a host replacement invalidates a pending confirmation.

- **Starting point:** I select the item and its delete control.
- **My flow:** I read the confirmation and affected document paths → choose
  **Cancel** or **Confirm delete**.
- **What I see:** Cancel preserves the document. Confirmation removes the
  item; deleting a job also removes structural dependencies on it. Expression
  references remain and receive diagnostics so I can repair them. If the
  document changes while the dialog is open, I must review deletion again.

### UF-28 — Undo or redo my edits

**UI tests**: [UI tests](../ui/test/browser/flows/uf-28.spec.ts)

- **Coverage:** YAML and form changes share Undo and Redo while subsequent typing stays at the restored cursor.

- **Starting point:** I have changed the document through forms, the graph, or YAML.
- **My flow:** I select **Undo** or **Redo**, or use **Mod-Z** and
  **Mod-Shift-Z** in the YAML editor; YAML also supports **Mod-Y** for redo.
- **What I see:** Document edits share a history across all three editing
  surfaces. Navigation, zoom, and card positions are separate from that history.
  While typing in a property field, native text undo applies to its local draft.

## Editing source and recovering from errors

### UF-29 — Edit or paste workflow YAML

**UI tests**: [UI tests](../ui/test/browser/flows/uf-29.spec.ts)

- **Coverage:** Pasting source updates the graph and retains comments and unfamiliar fields.

- **Starting point:** I choose YAML or Split mode in an editable document.
- **My flow:** I type or paste YAML into **Workflow YAML** → inspect the graph,
  inspector, and diagnostics as the document updates.
- **What I see:** Usable source updates the visual representation. Comments
  and unfamiliar fields remain available in the source; visual edits may
  normalize formatting. Pasting source replaces text I select, rather than
  opening a file-import dialog.

### UF-30 — Search and replace in YAML

**UI tests**: [UI tests](../ui/test/browser/flows/uf-30.spec.ts)

- **Coverage:** Search and replacement work in narrow light and dark containers with visible labels and focus restoration.

- **Starting point:** The YAML pane is visible and focused.
- **My flow:** I press **Mod-F** → enter my search → navigate matches → use the
  search panel's replacement controls when I want to change matching text.
- **What I see:** Matches are highlighted. Replacements update the workflow
  and can be undone; in the viewer I can search but cannot replace source.

### UF-31 — Fix validation problems

**UI tests**: [UI tests](../ui/test/browser/flows/uf-31.spec.ts)

- **Coverage:** Correcting command arguments clears browser errors; rejected host validation appears in diagnostics. Correcting an invalid registered action name clears browser errors and exposes the host registration/schema check without requiring agent parameters.

- **Starting point:** The diagnostics area reports warnings or errors.
- **My flow:** I expand diagnostics → read the message, document path, source,
  and line/column when available → locate the field or YAML → correct it.
- **What I see:** Diagnostics and the YAML error count update. If host
  validation is configured, I may see **Validating…** while it runs. A host
  validation failure is reported as a diagnostic; browser checks alone do not
  guarantee that the runner will accept the workflow.

### UF-32 — Recover from malformed YAML

**UI tests**: [UI tests](../ui/test/browser/flows/uf-32.spec.ts)

- **Coverage:** Malformed source retains its last preview until a YAML repair; stale previews discard unapplied property drafts before editing resumes.

- **Starting point:** My source becomes malformed or structurally unusable.
- **My flow:** I read **Stale preview — fix the YAML structure to resume visual
  editing.** → fix the YAML, or undo the breaking edit.
- **What I see:** My source stays available. If there was a usable graph, it
  remains visible as the last preview, while visual authoring is disabled and
  source navigation from the preview is suspended. Recovery restores visual
  editing. If no usable workflow exists, **Create workflow** offers a starter
  replacement that I can undo.

### UF-33 — Correct an invalid inspector field

**UI tests**: [UI tests](../ui/test/browser/flows/uf-33.spec.ts)

- **Coverage:** Correction, Escape, and Discard edit recover rejected YAML and identifier drafts; recovery alone never invokes saving.

- **Starting point:** A structured YAML field or identifier rejects my edit.
- **My flow:** I read the error → correct the value and apply it again, or press
  **Escape** in the field or select **Discard edit** to restore the stored value.
- **What I see:** An invalid YAML draft in a structured field does not replace
  the stored field value. The field is marked invalid and explains that its edit has not been applied. Escape and Discard edit restore the stored value and clear the rejection. I select Save again after recovery.

## Saving, viewing, and the playground

### UF-34 — Save my workflow

**UI tests**: [UI tests](../ui/test/browser/flows/uf-34.spec.ts)

- **Coverage:** Toolbar and keyboard saves reject invalid YAML and all built-in identifier drafts without calling the host. Desktop and narrow layouts reveal collapsed rejected fields, track multiple errors, and allow explicit recovery. Corrected job IDs save in the same toolbar click. Active valid drafts apply once without erasing a rejected sibling draft; editors save independently, in-flight saves are guarded, malformed source remains saveable, and failures remain retryable. Persistent status distinguishes unsaved, saving, saved, and failed states. Saves completing after committed edits or uncommitted inspector edits leave those edits unsaved at desktop and narrow widths; undo and discarding rejected drafts restore the saved state.

- **Starting point:** The host provides a **Save** button.
- **My flow:** I finish my edit → select **Save**, or press **Mod-S** to apply
  pending inspector fields and save → wait for **Saving…** to finish and check **Saved** or **Unsaved changes**.
- **What I see:** The host handles the exact submitted source. **Unsaved changes** appears until a successful save matches the current source and no inspector edits remain pending. Edits made during saving remain unsaved when it finishes. A failed save shows **Save failed** and an
  alert lets me read and **Dismiss** the error; I can retry. If an inspector edit cannot apply, saving stops and its field is revealed and focused. I correct it, press Escape, or choose Discard edit, then explicitly save again. Save accepts source drafts
  with diagnostics and is never automatic. Without host saving, I use export.

### UF-35 — Download the YAML

**UI tests**: [UI tests](../ui/test/browser/flows/uf-35.spec.ts)

- **Coverage:** Downloads preserve original source bytes and include malformed drafts. Exporting a changed draft leaves host persistence status unsaved; saving that draft through the host can mark it saved.

- **Starting point:** A workflow or draft is open in the editor or viewer.
- **My flow:** I select **Export YAML** → receive `workflow.yml` through my
  browser's download handling.
- **What I see:** The download contains the current source, including a draft
  with diagnostics. If I have not edited the source, its original bytes are
  preserved. Exporting does not change host save status. Canvas layout is separate from the exported YAML.

### UF-36 — Inspect a workflow in read-only mode

**UI tests**: [UI tests](../ui/test/browser/flows/uf-36.spec.ts)

- **Coverage:** Viewer inspection and expansion remain available while source and mutation controls are read-only.

- **Starting point:** The host opens the workflow in the **Viewer**.
- **My flow:** I navigate jobs and steps → inspect properties → switch views
  → search YAML → expand cards, pan, or zoom → optionally export YAML.
- **What I see:** I can explore the definition while document-changing controls
  are unavailable or read-only. The host can still replace the displayed source.

### UF-37 — Explore the local playground

**UI tests**: [UI tests](../ui/test/browser/flows/uf-37.spec.ts)

- **Coverage:** All four examples export unchanged source; theme controls, example-named downloads, layout storage, and reloading original source are exercised. The download-only notice is visible and no Save button or host save status claims draft persistence.

- **Starting point:** I open the development playground.
- **My flow:** I choose an **Example** (`classify`, `parallel-review`, `develop`,
  or `develop-iteration`) → explore or edit it → choose **Theme** (`system`,
  `light`, or `dark`) → use **Export YAML** to download my changes.
- **What I see:** **Export YAML** downloads an example-named YAML file. A notice explains that workflow drafts are not retained. The playground
  remembers layout separately for each example in browser storage; switching
  examples reloads the selected example's original source, so I download edits
  I want to keep before switching.

## Connecting work across jobs and steps

### UF-38 — Plan parallel work followed by a combined result

**UI tests**: [UI tests](../ui/test/browser/flows/uf-38.spec.ts)

- **Coverage:** Two review jobs remain independent while both feed the summary through dependency checkboxes.

- **Starting point:** I want two independent reviews and a final summary.
- **My flow:** I add jobs named `review_a`, `review_b`, and `summarize` → add
  the review steps → select `summarize` and check both review jobs under
  **Dependencies** → use **Auto layout** to inspect the structure.
- **What I see:** Both review jobs lead into the summary job, with no dependency
  between the reviews. The definition allows the reviews to run independently
  before the summary, subject to the runner's execution capacity.

### UF-39 — Pass a result from one job to another

**UI tests**: [UI tests](../ui/test/browser/flows/uf-39.spec.ts)

- **Coverage:** A declared producer output becomes an expression option for a dependent summary prompt.

- **Starting point:** My `review` job contains an agent step with ID `analyze`,
  and a `summarize` job needs its result.
- **My flow:** In `review`, I add a **Job output** named `report` with value
  `${{ steps.analyze.outputs.text }}` → select `summarize` and check `review`
  under **Dependencies** → select the summary agent step and insert
  `${{ needs.review.outputs.report }}` into its **Prompt**.
- **What I see:** The YAML contains both the dependency and the data reference.
  The dependent job's expression suggestions include the declared `report`
  output. I am defining how results will be passed; actual values appear when
  the workflow runs outside this UI.

### UF-40 — Use an earlier step's result in a later step

**UI tests**: [UI tests](../ui/test/browser/flows/uf-40.spec.ts)

- **Coverage:** Only preceding command outputs appear in the prompt picker; reordering preserves the reference for manual review.

- **Starting point:** A sequential job has a direct command with ID `collect`
  followed by an agent step.
- **My flow:** I select the later agent step → write a prompt explaining what
  to do with the command result → insert `${{ steps.collect.outputs.stdout }}`
  using **Insert expression…** → review the step order.
- **What I see:** The prompt references the earlier command's output. Suggestions
  offer preceding steps rather than later steps. If I move the agent before
  `collect`, I need to correct the order or reference before execution.

## Additional editing and recovery

### UF-41 — Edit action parameters beyond the guided form

**UI tests**: [UI tests](../ui/test/browser/flows/uf-41.spec.ts)

- **Coverage:** Replacing advanced parameters updates both the stored mapping and guided prompt/model fields.

- **Starting point:** A selected action needs parameters that its guided fields
  do not expose, or I want to edit several parameters together.
- **My flow:** I open **Advanced parameters** → edit **Parameters (YAML)** →
  leave the field to apply the mapping → inspect any guided fields and diagnostics.
- **What I see:** The action's complete parameter mapping is replaced by my
  entry, and guided fields reflect the resulting values. I include existing
  parameters I want to keep; replacing the mapping also replaces comments
  inside it. Invalid YAML stays a field draft until I correct or cancel it.

### UF-42 — Replace a reusable call with local steps

**UI tests**: [UI tests](../ui/test/browser/flows/uf-42.spec.ts)

- **Coverage:** The confirmation names the reusable reference, inputs, and secrets; cancellation preserves source, while confirmed conversion creates starter steps and one Undo restores the exact original call. An imported job containing both a reusable call and local steps retains its configured steps and comments; the dialog names only the discarded call settings, and Undo restores the exact imported source.

- **Starting point:** A job currently calls a reusable workflow, and I want to
  define its steps directly.
- **My flow:** I select the job → change **Job kind** to **Sequential steps**
  → review the reference, inputs, and secrets to discard and confirm or cancel
  → edit the existing local steps, or the starter command when no steps exist,
  and add the steps I need → review the job outputs
  and any downstream references.
- **What I see:** The reusable reference, call inputs, and call secrets are
  removed. Existing local steps are preserved; otherwise an example
  direct-command step appears. The reusable workflow's
  contents are not copied into the job. **Undo** restores the previous call.

### UF-43 — Repair references after removing an item

**UI tests**: [UI tests](../ui/test/browser/flows/uf-43.spec.ts)

- **Coverage:** Deleting a referenced step reports its consuming path; editing the remaining prompt clears the reference diagnostic.

- **Starting point:** I confirmed deletion of a referenced job, step, input,
  secret, or output, and diagnostics now identify remaining references.
- **My flow:** I read the affected paths → navigate to each expression →
  replace the reference with a valid input or output, remove the expression,
  or undo the deletion if I still need the original item.
- **What I see:** Diagnostics update as I repair references. If I replace a
  deleted job with a different producer, I also select that producer under the
  consuming job's **Dependencies**; editing an expression alone adds no edge.

### UF-44 — Resolve an unavailable agent or reusable workflow warning

**UI tests**: [UI tests](../ui/test/browser/flows/uf-44.spec.ts)

- **Coverage:** Correcting agent references through both Action target and Step type keeps the prompt; correcting a workflow reference clears the remaining catalog warning.

- **Starting point:** The host supplies catalogs, and diagnostics say a
  referenced agent or workflow is unavailable.
- **My flow:** I select the affected step or job → review its **Action target**
  or **Reusable workflow** → correct the reference to an available catalog
  entry → review its parameters and diagnostics.
- **What I see:** The warning clears when the reference matches the catalog.
  Both **Action target** and **Step type** keep parameters when changing agents
  and review incompatible changes as described in UF-21. Catalog membership
  does not verify provider authentication or resolve the reusable file.

### UF-45 — Author a workflow using the keyboard

**UI tests**: [UI tests](../ui/test/browser/flows/uf-45.spec.ts)

- **Coverage:** Keyboard job navigation, field commits, and dependency toggles author a workflow; deletion traps focus and Escape restores the originating button.

- **Starting point:** I am in an editable workflow and prefer keyboard controls.
- **My flow:** I use Tab and Shift-Tab to reach the palette's job buttons →
  activate a job with Enter or Space → tab through inspector fields → apply
  single-line edits with Enter → toggle **Dependencies** with Space → use
  **Mod-S** when saving is available.
- **What I see:** I can author dependencies without dragging canvas connectors.
  If I open a deletion dialog, focus starts on **Cancel**, Tab stays inside the
  dialog, and Escape cancels and returns focus to the originating control when
  it is still present.

### UF-46 — Make a focused edit to a hand-written workflow

**UI tests**: [UI tests](../ui/test/browser/flows/uf-46.spec.ts)

- **Coverage:** A focused name edit preserves unrelated comments, metadata, prompt text, and mapping order in the export.

- **Starting point:** The host opens an existing workflow containing comments,
  multiline prompts, or fields without dedicated form controls.
- **My flow:** I choose **Split** → inspect the source → change a supported
  property in the inspector → review the surrounding YAML → select
  **Export YAML** to keep the updated definition.
- **What I see:** Ordinary property edits preserve unrelated comments, mapping
  order, and unrecognized fields, although formatting can change. Unknown
  fields may still produce diagnostics. I review whole-mapping replacements
  separately because they replace that subtree and its comments.

### UF-47 — Recover my draft after the host replaces the source

**UI tests**: [UI tests](../ui/test/browser/flows/uf-47.spec.ts)

- **Coverage:** Undo after a host replacement restores the prior local draft for export.

- **Starting point:** The host replaces the definition in the same open editor
  instance, for example after loading an updated version.
- **My flow:** I inspect the new graph and YAML → select **Undo** if I want my
  previous draft back → review it and save or export the version I want to keep.
- **What I see:** Source replacement participates in the document history, so
  the previous draft can be restored while that editor instance remains open.
  This is document undo; it does not reverse storage changes already made by
  the host or recover history after the editor is destroyed and reopened.

## Implementation references

These flows describe existing behavior rather than proposed screens. Their
sources are the [UI guide](../ui/README.md),
[editor controls](../ui/src/editor.tsx),
[property forms](../ui/src/forms.tsx),
[YAML editor](../ui/src/source.tsx), and
[playground](../ui/playground.ts). Related interaction coverage lives in the
[browser tests](../ui/test/browser/).
