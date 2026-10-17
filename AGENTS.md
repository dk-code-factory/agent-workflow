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
