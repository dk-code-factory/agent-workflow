ARG GO_IMAGE
ARG NODE_IMAGE
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS guardian
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY pkg/process ./pkg/process
COPY pkg/containerapi ./pkg/containerapi
COPY pkg/runtimeexec/network ./pkg/runtimeexec/network
COPY internal/socketfence ./internal/socketfence
COPY cmd/agent-workflow-guardian ./cmd/agent-workflow-guardian
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath -o /guardian ./cmd/agent-workflow-guardian

FROM --platform=$BUILDPLATFORM ${NODE_IMAGE} AS dependencies
ARG TARGETARCH
COPY bridge/claude/package.json bridge/claude/package-lock.json /bridge/
COPY images/codex/package.json images/codex/package-lock.json /codex/
RUN case "$TARGETARCH" in amd64) cpu=x64;; arm64) cpu=arm64;; *) exit 1;; esac; \
    npm ci --prefix /bridge --os=linux --cpu=$cpu --ignore-scripts --no-audit --no-fund && \
    npm ci --prefix /codex --os=linux --cpu=$cpu --ignore-scripts --no-audit --no-fund

FROM ${GO_IMAGE} AS go-runtime
FROM ${NODE_IMAGE}
LABEL agent-workflow.guardian-protocol="1" \
      agent-workflow.bridge-protocol="2" \
      agent-workflow.claude-sdk="0.3.311" \
      agent-workflow.codex-cli="0.164.0"
COPY --from=guardian /guardian /usr/local/bin/agent-workflow-guardian
COPY --from=go-runtime /usr/local/go /usr/local/go
COPY --from=dependencies /bridge /opt/agent-workflow/bridge
COPY --from=dependencies /codex /opt/agent-workflow/codex
COPY bridge/claude/bridge.mjs /opt/agent-workflow/bridge/bridge.mjs
ENV PATH="/usr/local/go/bin:/opt/agent-workflow/codex/node_modules/.bin:$PATH" \
    HOME="/state/home" CODEX_HOME="/state" CLAUDE_CONFIG_DIR="/state"
USER 65532:65532
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/agent-workflow-guardian"]
