# Repository verification

Ordinary tests, CI fixes, release preparation, and production-readiness work
authorize deterministic checks only. Agents must not run live-provider or
GitHub publication conformance scenarios unless the user explicitly requests
those scenarios in the current session. The mandatory named provider scenarios
in `.github/workflows/release.yml` are authorized only for that repository's
version-tag push through the protected `pre-release` environment. This exception
does not authorize local live calls, PR/main CI or GitHub publication conformance.
Historical budgets in release evidence
do not authorize new live calls.

After changing Go dependencies, run `make go-deps` and commit both `go.mod` and
`go.sum`. This runs tidy followed by `go mod download all`; tidy alone removes
checksums needed by the release dependency inventory. `make go-locks-check`
verifies the complete locks without modifying the checkout and runs in
`make check` and native CI before tests or packaging.

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

# Release publication

`.github/workflows/release.yml` is the sole supported publication path. Candidate
validation and deterministic checks precede artifact builds/scans, then mandatory
smoke/session and full provider/runtime E2E phases. The `release-certification`
human gate must remain unavailable until every automated prerequisite succeeds.
A signed report must match the immutable automated artifact from the same
candidate and workflow run. Preserve successful E2E artifacts when retrying a
failed certification job. Never replace required live coverage with synthetic
reports, rebuild certification images, initiate provider login, or rotate GitHub
secrets implicitly. See `docs/RELEASE_PIPELINE.md` for setup and recovery.
