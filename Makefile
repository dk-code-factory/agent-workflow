.PHONY: build test check dist
build:
	go build -o bin/agent-workflow ./cmd/agent-workflow
test:
	go test -race ./...
check:
	go vet ./...
	go run ./cmd/agent-workflow validate workflows/parallel-review.yml
	go run ./cmd/agent-workflow validate workflows/classify.yml
	go run ./cmd/agent-workflow validate workflows/develop.yml
dist:
	python3 scripts/build-release.py --smoke
