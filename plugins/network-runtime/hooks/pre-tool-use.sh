#!/bin/sh
# Native Codex owns hook trust, approvals and execution. This only rewrites input.
LC_ALL=C
export LC_ALL

deny() {
    printf '%s\n' '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"Agent Workflow runtime routing could not be validated; restart with a matching prepared runtime and trusted plugin."}}'
    exit 0
}

# Ordinary host sessions can have DOCKER_HOST/CONTAINER_HOST set independently.
if [ "${AGENT_WORKFLOW_NETWORK_RUNTIME+x}" != x ] && [ "${AGENT_WORKFLOW_NETWORK_REFERENCE+x}" != x ]; then
    printf '%s\n' '{}'
    exit 0
fi
[ -n "${AGENT_WORKFLOW_NETWORK_RUNTIME-}" ] && [ -n "${AGENT_WORKFLOW_NETWORK_REFERENCE-}" ] || deny
case "$AGENT_WORKFLOW_NETWORK_RUNTIME" in
    [!a-zA-Z0-9]*|*[!a-zA-Z0-9_-]*) deny ;;
esac
[ "${#AGENT_WORKFLOW_NETWORK_RUNTIME}" -le 24 ] || deny

# No PATH lookup or environment-controlled executable/registry override.
helper="/var/lib/agent-workflow/runtimes/$AGENT_WORKFLOW_NETWORK_RUNTIME/launcher"
[ -x "$helper" ] || deny
output=$("$helper" __agent_workflow_codex_hook_v1 2>/dev/null) || deny
[ "${#output}" -le 1048608 ] || deny
# The matching Go helper owns bounded JSON validation and serialization. An old
# launcher can print help and exit successfully; never forward that as hook data.
header='AGENT_WORKFLOW_CODEX_HOOK_V1
'
case "$output" in
    "$header"?*) printf '%s\n' "${output#"$header"}" ;;
    *) deny ;;
esac
