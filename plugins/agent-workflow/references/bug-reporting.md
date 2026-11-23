# Reviewed bug reports

Use private local evidence; include only selected, reviewed data in a report.
The helper accepts a JSON object with string fields `title`, `reproduction`,
`expected`, `actual`, `versions`, `platform`, and `evidence`. Supply a minimal
reproduction rather than a raw transcript. Its redaction removes common token
formats, credential assignments, authorization values and user-home paths, but
cannot guarantee removal of every secret or confidential fact. Review manually.

```sh
python3 /plugin/scripts/bug-report.py draft --input /private/facts.json --output /private/report.md
python3 /plugin/scripts/bug-report.py search --query 'short symptom terms'
```

Search includes open and closed issues. Read likely matches before deciding
whether this is new. Prefer linking a duplicate and preparing additional
evidence over creating another issue. Invalid definitions and environment
failures should be diagnosed locally before attributing a defect to the engine.

After explicit posting approval (or a clear original posting instruction):

```sh
python3 /plugin/scripts/bug-report.py post --body /private/report.md \
  --title 'Reviewed sanitized title' --approve-sha256 REVIEWED_BODY_SHA256 \
  --journal /private/report-post.json
```

The helper first searches for the body marker and records posting intent before
creation. Keep its journal. If creation fails or the response is lost, re-run
the same invocation to reconcile; it searches the marker and returns a found
issue, but never repeats an uncertain creation. If no match is visible, inspect
GitHub and retain uncertainty. Any later retry needs separately reconciled
evidence and renewed explicit authorization. A search failure is a failure.
Do not change the marker or journal to bypass this rule. Never post acceptance
test issues or refresh GitHub authentication.
