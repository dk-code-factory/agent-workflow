.PHONY: build test check dist
build:
	go build -o bin/agent-workflow ./cmd/agent-workflow
	go build -o bin/agent-workflow-runtime ./cmd/agent-workflow-runtime
test:
	go test -race -timeout 60m ./...
check: release-tooling-test
	go vet ./...
	go run ./cmd/agent-workflow validate workflows/parallel-review.yml
	go run ./cmd/agent-workflow validate workflows/classify.yml
	go run ./cmd/agent-workflow validate workflows/develop.yml
dist:
	python3 scripts/build-release.py --smoke

.PHONY: go-deps go-locks-check release-check release-tooling-test
go-deps:
	go mod tidy
	go mod download all
go-locks-check:
	python3 scripts/go_locks.py
release-check: release-tooling-test
	python3 scripts/validate-release.py "$(TAG)"
release-tooling-test: go-locks-check
	python3 -m unittest discover -s scripts -p 'test_*.py' -v

.PHONY: ui-install ui-check ui-test ui-build ui-browser-test
ui-install:
	npm ci --prefix ui --ignore-scripts
ui-check:
	npm run check --prefix ui
ui-test:
	npm test --prefix ui
ui-build:
	npm run build --prefix ui
ui-browser-test: ui-build
	npm run test:consumers --prefix ui
	npm run test:browser --prefix ui

.PHONY: ui-flows-check ui-pr-check ui-flow-checker-test
BASE ?= origin/main
ui-flows-check:
	node ui/scripts/flows-check.mjs --base="$(BASE)"
ui-flow-checker-test:
	node --test ui/scripts/flows-check.test.mjs
ui-pr-check: ui-flow-checker-test
	node ui/scripts/flows-check.mjs --base="$(BASE)" --run
