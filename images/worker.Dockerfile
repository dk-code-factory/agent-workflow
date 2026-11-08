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
ARG NPM_VERSION
ARG NPM_INTEGRITY
COPY images/verify-npm-tarball.mjs /tmp/verify-npm-tarball.mjs
RUN npm pack --ignore-scripts --pack-destination /tmp "npm@${NPM_VERSION}" && \
    node /tmp/verify-npm-tarball.mjs "/tmp/npm-${NPM_VERSION}.tgz" "$NPM_INTEGRITY" && \
    npm install --global --offline --ignore-scripts --no-audit --no-fund "/tmp/npm-${NPM_VERSION}.tgz" && \
    rm /tmp/npm-*.tgz /tmp/verify-npm-tarball.mjs
COPY bridge/claude/package.json bridge/claude/package-lock.json /bridge/
COPY images/codex/package.json images/codex/package-lock.json /codex/
RUN case "$TARGETARCH" in amd64) cpu=x64;; arm64) cpu=arm64;; *) exit 1;; esac; \
    npm ci --prefix /bridge --os=linux --cpu=$cpu --libc=musl --ignore-scripts --no-audit --no-fund && \
    npm ci --prefix /codex --os=linux --cpu=$cpu --libc=musl --ignore-scripts --no-audit --no-fund

FROM ${GO_IMAGE} AS go-runtime
FROM ${NODE_IMAGE}
COPY images/alpine-runtime-packages.txt /tmp/runtime-packages.txt
RUN xargs apk add --no-cache --upgrade < /tmp/runtime-packages.txt && \
    awk -F: '$1 == "P" { package = $2 } $1 == "V" { print package "=" $2 }' /lib/apk/db/installed | sort > /tmp/installed-packages.txt && \
    sort /tmp/runtime-packages.txt > /tmp/expected-packages.txt && \
    cmp /tmp/installed-packages.txt /tmp/expected-packages.txt && \
    rm /tmp/runtime-packages.txt /tmp/installed-packages.txt /tmp/expected-packages.txt && \
    rm -rf /usr/local/lib/node_modules/npm /opt/yarn-* /usr/local/bin/yarn /usr/local/bin/yarnpkg
COPY --from=dependencies /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/npm
LABEL agent-workflow.guardian-protocol="1" \
      agent-workflow.bridge-protocol="2" \
      agent-workflow.claude-sdk="0.3.319" \
      agent-workflow.codex-cli="0.167.0" \
      agent-workflow.npm-cli="12.2.2"
COPY --from=guardian /guardian /usr/local/bin/agent-workflow-guardian
COPY --from=go-runtime /usr/local/go /usr/local/go
COPY --from=dependencies /bridge /opt/agent-workflow/bridge
COPY --from=dependencies /codex /opt/agent-workflow/codex
COPY bridge/claude/bridge.mjs /opt/agent-workflow/bridge/bridge.mjs
ENV PATH="/usr/local/go/bin:/opt/agent-workflow/codex/node_modules/.bin:$PATH" \
    HOME="/state/home" CODEX_HOME="/state" CLAUDE_CONFIG_DIR="/state" CGO_ENABLED="0"
USER 65532:65532
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/agent-workflow-guardian"]
