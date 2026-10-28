# Codex network-runtime plugin

This optional plugin restores `DOCKER_HOST`, `CONTAINER_HOST` and
`AGENT_WORKFLOW_NETWORK_RUNTIME` inside covered local POSIX shell tool calls.
Launch Codex inside an already prepared Agent Workflow runtime first. The hook
uses an independent nonsecret admitted-reference marker, so login/snapshot
changes to the public selectors can be repaired without reading Codex settings.

## Install, enable and trust

Use the plugin from the source checkout or extracted Agent Workflow archive:

```sh
codex plugin marketplace add /absolute/path/to/agent-workflow
codex plugin add network-runtime@agent-workflow
```

The distribution root contains `.agents/plugins/marketplace.json`; the plugin
contains `.codex-plugin/plugin.json`, `hooks/hooks.json` and the executable
`hooks/pre-tool-use.sh`. Review those files before trusting the hook.

In Codex, explicitly enable the plugin, enable its plugin/hook features where
required, and review/trust its current definition in `/hooks`. Installation alone
does not grant hook trust. Review changed definitions again after an update.
These commands are operator actions; Agent Workflow never changes or trusts your
normal Codex installation automatically.

Prepare/start the runtime with the matching packaged launcher, then start a fresh
session:

```sh
agent-workflow-runtime exec development -- codex --enable plugins --enable hooks
agent-workflow-runtime exec development -- codex exec --profile work 'Run the tests'
```

Existing native profile, authentication, approval and sandbox settings still
apply. The rewrite changes the policy-visible command prefix, so an existing
allow rule can require fresh approval. Do not bypass approval or hook trust as a
workaround.

The trampoline invokes only the protected installed
`/var/lib/agent-workflow/runtimes/NAME/launcher` internal hook handler. Missing or
old helpers produce ordinary valid denial where the wrapper can catch the
failure; stop/reprepare/restart with the matching launcher before enabling the
plugin. No Python interpreter is a production prerequisite.

For configured `codex-cli` compatibility roles, use the same explicitly enabled
and trusted native setup. Managed `codex-app-server` roles instead use isolated
engine-owned `CODEX_HOME` and do not inherit normal user plugins/config/trust.
Their hook coverage is not automatic. Do not copy user configuration or trust
into those directories or relax managed worker/tool-subset certification gates.

## Coverage and limits

The earlier disposable-plugin Codex0.164.0 experiments cover direct Bash/exec_command,
code-mode nested dispatch, a real named-profile CLI invocation, native
approval/sandbox preservation, unrelated filtered values and original tool exits.
The production assets use a shared Go handler and a POSIX trampoline; they do not
use the prototype's Python handler. Normal helper validation/probe errors deny covered work; outside a selected
runtime the globally installed plugin is a no-op. The helper only rewrites input;
Codex executes the original tool through its existing sandbox and approval flow.

Raw hook termination/timeouts/malformed output, untrusted/disabled/modified hooks,
a later competing rewrite, direct command/exec RPC and native Codex-managed MCP
startup can bypass repair. The experiment did not establish per-turn subprocess
termination: baseline and plugin both let a long command continue after a turn
interrupt. Agent Workflow's invocation guardian remains a separate lifetime
boundary. Engine-controlled stdio MCP is separately supervised.

When selectors disappear, a saved client connection can reach an API outside the
runtime even while the client remains namespaced. The hook is a routing
guardrail, not mandatory enforcement of every execution path. Stronger API
fencing and fallback closure are tracked for the whole PR in
https://github.com/dk-code-factory/agent-workflow/issues/38. Restricted provider
containers retain their socket/mount exclusions and receive no rootful API
authority from this plugin. POSIX/local tool coverage does not imply remote
executor or non-POSIX coverage.

Native setup references: https://developers.openai.com/plugins/build/plugins and
https://learn.chatgpt.com/docs/hooks. The opt-in production integration experiment
must use only disposable state, trusted fixture definitions and local fake APIs;
ordinary `make test` and `make check` require no installed provider or root.
