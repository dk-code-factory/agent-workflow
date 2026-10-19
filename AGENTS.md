# Repository verification

Ordinary tests, CI fixes, release preparation, and production-readiness work
authorize deterministic checks only. Agents must not run live-provider or
GitHub publication conformance scenarios unless the user explicitly requests
those scenarios in the current session. Historical budgets in release evidence
do not authorize new live calls.

Do not automatically refresh provider login, create repositories, publish pull
requests, merge, or delete remote resources. Use disposable fixtures for storage
exhaustion and runtime-failure tests; never exhaust the shared temporary or state
filesystem. Preserve unrelated workspace files.

`make test` and `make check` are deterministic. The pinned-SDK fake-gateway test
uses a local fake API and requires its locked Node dependencies. Real provider
and publication evidence remains separate from deterministic validation.

# UI flow verification on every PR

Before declaring any PR ready, run `make ui-pr-check BASE=<PR target>`
(for example `BASE=origin/main`). Run it again after subsequent relevant edits.
This command checks documentation/test synchronization and runs affected user
flows in Chromium without package builds or provider access. Even unrelated PRs
must run it; they validate structure without starting a browser. Do not skip a
missing baseline: fetch the target or supply an available target explicitly.

Each `UF-NN` section in `docs/UI_USER_FLOWS.md` owns
`ui/test/browser/flows/uf-NN.spec.ts` and links to it. Change both when changing
either substantive prose or test behavior. Shared test helper/fixture changes
require meaningful Coverage notes in every affected flow. If behavior stays the
same, explain revised assertions or recovery scenarios in the Coverage note;
comments, formatting, link edits, timestamps, and checksums do not acknowledge
a change. Preserve stable UF IDs. See `ui/README.md` for setup and repairs.
