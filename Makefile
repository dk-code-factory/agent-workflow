.PHONY: build test check dist
build:
	go build -o bin/agent-workflow ./cmd/agent-workflow
test:
	go test -race ./...
check: release-tooling-test
	go vet ./...
	go run ./cmd/agent-workflow validate workflows/parallel-review.yml
	go run ./cmd/agent-workflow validate workflows/classify.yml
	go run ./cmd/agent-workflow validate workflows/develop.yml
dist:
	python3 scripts/build-release.py --smoke

.PHONY: release-check release-tooling-test
release-check: release-tooling-test
	python3 scripts/validate-release.py "$(TAG)"
release-tooling-test:
	python3 -m unittest discover -s scripts -p 'test_*.py' -v
