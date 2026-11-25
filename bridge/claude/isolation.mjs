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
