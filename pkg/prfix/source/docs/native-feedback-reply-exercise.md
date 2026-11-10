# Bounded native feedback reply exercise

Issue #152 concerns the agent's destination and attribution choices. The offline
publisher scenarios in `tests/pr/test_feedback_replies.py` use explicit requests
to verify endpoints, exact bodies, source retention and lost-response recovery.
They do not establish that a native agent chooses those requests from the skill.

Use `tests/pr/fixtures/feedback_reply_exercise.py` for a separate controlled native
sample. It makes no model or GitHub requests. It supplies original feedback,
validation and an offline transport to the actual publisher; the agent chooses
each request. Its inline follow-up and review-body finding belong to the same
review, and the review body includes a file/line reference. A third source is a
top-level comment. The fixture deliberately does not classify the review-body
text or select destinations for the agent.

Initialize a new directory outside the checkout:

```sh
python3 tests/pr/fixtures/feedback_reply_exercise.py --root /tmp/feedback-reply-sample init
```

Give a fresh native agent the returned `skill.md`, `publication.md` and
`evidence.json`. Ask it to answer all affected sources using the saved skill and
validation, with only offline publication through the fixture. Do not supply the
expected destinations or attribution text in the agent's task. The two Markdown
files are frozen copies of the current guidance; normal governing contracts
remain applicable. The supplied evidence and validation stand in for live
collection and code checks.

For each agent-authored request, call:

```sh
python3 tests/pr/fixtures/feedback_reply_exercise.py --root /tmp/feedback-reply-sample submit --request /tmp/reply-request.json --lose-response
```

Use `--lose-response` for at least one request. It applies the write and loses the
response, returning the retained operation and diagnostic. Resume with
`reconcile --operation FILE` through the same interface. A later `submit` with an
identical request also recovers the same operation. Neither path authorizes a
second dispatch. Use `inspect` to examine saved remote state.

Evaluate the native result independently: the actual inline follow-up should
receive one reply under its thread root, preserving the follow-up's own source
id and URL. The review-body and top-level-comment answers should each receive one
standalone response beginning with `In Reply To: ` and a Markdown link to that
original source, followed by a blank line. Their embedded references must not
redirect the replies. Retained operations should preserve exact source records,
body, actor, head and routing parent, and lost-response recovery should leave one
write per reply. Record the native handle, result, operation paths and inspected
state privately.

For issue #162, first publish and reconcile one reply, then use `change-reply
--request FILE` with the published receipt's id, for example
`{"id":902,"state":"unavailable"}` or
`{"id":902,"state":"edited","body":"Revised answer"}`. Give a fresh native
agent the historical operation path, current guidance and evidence about the
current answer obligation. Ask it to recover the finalizer through the offline
interface without supplying the expected decision. The interface accepts
`inspect-reply --operation FILE` and `dispose-reply --operation FILE --request
DISPOSITION.json`; the latter uses the production disposition schema. A new
`submit` accepts separately authorized replacement requests with `replaces`.
Inspect retained requests, receipts, observations, dispositions, replacement
links and write counts afterward. Test obsolete and still-required answers in
separate samples; never use fixture changes against live GitHub.

This sample exercises one native agent on frozen evidence and simulated GitHub
writes. It does not establish complete-workflow adherence, live GitHub behavior
or independent Claude host execution. Ordinary regression tests remain offline
and never launch a model.

## Response obligations

For issue #164, use the inputs in
`tests/pr/fixtures/feedback-response-cases.json` for a separate bounded decision
sample. Give a fresh native agent the current reply skill, its response-obligation
guidance and the case inputs, without the evaluator expectations below. All
threads start unresolved. Authorize replies and fully answered thread resolution
within each case's instruction; each comment id identifies its original source.
Ask for each case's source ids needing replies, whether to resolve the thread,
whether reply finalization can complete after any required supported replies,
whether user clarification is needed and a brief reason. No real publication is
authorized; these inputs stand in for live discussion and validation.

Evaluate independently: `other-maintainer`, `optional-scope` and `settled-answer`
need no new reply. Leave the first two unresolved; leaving the already answered
thread unresolved is also valid. `direct-author` and `unaddressed-question` need
answers about the verified test. `mixed-requests` and `later-author-request`
need only the author-directed answer and must remain unresolved for the
maintainer. `explicit-reply` needs the specifically requested response and must
remain unresolved. None needs user clarification merely to omit an optional
response. Each case can finish reply finalization once its required supported
answers are supplied. Reassess with an added author request or explicit user reply
instruction rather than reusing the earlier no-reply conclusion.

Keep the native result and evaluation private. This checks declared decisions
from frozen evidence, not live publication, persistent reuse or full workflow
execution. The ordinary offline suite does not call a model to score these cases.
