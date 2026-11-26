// Pinned Claude Code 2.1.322 builtins. Unknown additions are rejected using the
// actual system/init plugin set, rather than trusting this list alone.
export const isolatedSettings = {
  autoMemoryEnabled: false,
  autoDreamEnabled: false,
  enabledPlugins: Object.fromEntries([
    'cc-plugin-agents-md', 'cc-plugin-telemetry', 'cc-plugin-privacy',
    'cc-plugin-plugin-authoring', 'cc-plugin-easel', 'cc-plugin-sec-default', 'sec-default',
  ].map(name => [name + '@builtin', false])),
};

export function assertIsolatedInitialization(message) {
  if (!Array.isArray(message.plugins) || message.plugins.length ||
      !Array.isArray(message.skills) || message.skills.length ||
      !Array.isArray(message.tools) || message.tools.some(name =>
        ['skill', 'agent', 'task', 'toolsearch', 'teamcreate', 'teamdelete', 'sendmessage']
          .includes(String(name).toLowerCase().replace(/[_-]/g, ''))) ||
      (message.plugin_errors !== undefined && (!Array.isArray(message.plugin_errors) || message.plugin_errors.length))) {
    throw new Error('Provider initialization violates engine skill/plugin isolation');
  }
}

// Initialization acknowledgement alone is insufficient: the pinned CLI also
// returns plugins_applied:true for missing/corrupt local plugin manifests.
export function assertSkillAcknowledgement(initialization, qualified) {
  if (initialization.claude_code_version !== '2.1.322' || initialization.plugins_applied !== true ||
      initialization.hooks_applied !== true || initialization.hook_on_failure_supported !== true ||
      !Array.isArray(initialization.commands) || !initialization.commands.some(command => command.name === qualified)) {
    throw new Error('Provider did not initialize the admitted skill before prompt release');
  }
}

export function assertSelectedInitialization(message, skill) {
  const expectedName = 'aw-' + skill.name;
  if (!Array.isArray(message.plugins) || message.plugins.length !== 1 ||
      message.plugins[0].name !== expectedName || message.plugins[0].path !== skill.root ||
      !Array.isArray(message.skills) || !message.skills.includes(skill.qualified) ||
      !Array.isArray(message.tools) || !message.tools.includes('Skill') ||
      message.tools.some(name => ['agent', 'task', 'toolsearch', 'teamcreate', 'teamdelete', 'sendmessage'].includes(String(name).toLowerCase().replace(/[_-]/g, ''))) ||
      (message.plugin_errors !== undefined && (!Array.isArray(message.plugin_errors) || message.plugin_errors.length))) {
    throw new Error('Provider initialization differs from admitted skill/plugin set');
  }
}
