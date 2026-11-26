import { test } from 'node:test';
import assert from 'node:assert/strict';
import { isolatedSettings, isolatedEnvironment, assertIsolatedInitialization, assertSkillAcknowledgement, assertSelectedInitialization } from './isolation.mjs';
import { mkdtemp, readFile, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawn } from 'node:child_process';

test('managed environment overrides inherited compaction settings', () => {
  const environment = { DISABLE_COMPACT: '0', DISABLE_AUTO_COMPACT: '0', ...isolatedEnvironment };
  assert.equal(environment.DISABLE_COMPACT, '1');
  assert.equal(environment.DISABLE_AUTO_COMPACT, '1');
  for (const name of ['autoCompactEnabled', 'precomputeCompactionEnabled', 'idleCompaction']) {
    assert.equal(isolatedSettings[name], false);
  }
});

test('known builtin native hooks are explicitly disabled', () => {
  for (const name of ['agents-md', 'telemetry', 'privacy', 'plugin-authoring', 'easel']) {
    assert.equal(isolatedSettings.enabledPlugins['cc-plugin-' + name + '@builtin'], false);
  }
});

test('bridge closes an unexpected init and denies native work before verification', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'bridge-isolation-'));
  try {
    await writeFile(join(dir, 'broker-stream.mjs'), await readFile(new URL('./broker-stream.mjs', import.meta.url)));
    await writeFile(join(dir, 'isolation.mjs'), await readFile(new URL('./isolation.mjs', import.meta.url)));
    await writeFile(join(dir, 'sdk.mjs'), `
      import { writeFileSync } from 'node:fs';
      export const tool = () => {}; export const createSdkMcpServer = () => {};
      export function query({options}) {
        return {
          close() { writeFileSync(${JSON.stringify(join(dir, 'closed'))}, 'yes'); },
          async *[Symbol.asyncIterator]() {
            const outcome = await options.hooks.PreToolUse[0].hooks[0]({tool_name:'Read',tool_use_id:'unexpected',tool_input:{}});
            writeFileSync(${JSON.stringify(join(dir, 'permit'))},JSON.stringify(outcome));
            yield {type:'system',subtype:'init',plugins:[{name:'unknown',path:'builtin'}],skills:[],tools:['Read']};
            yield {type:'result',subtype:'success',result:'MUST_NOT_ACCEPT'};
          }
        };
      }
    `);
    const source = (await readFile(new URL('./bridge.mjs', import.meta.url), 'utf8'))
      .replace("from '@anthropic-ai/claude-agent-sdk'", "from './sdk.mjs'")
      .replace("import { z } from 'zod';", 'const z = {};')
      .replace(/^const sdkMetadata = .*$/m, 'const sdkMetadata = {version: sdkVersion};');
    await writeFile(join(dir, 'bridge.mjs'), source);
    const child = spawn(process.execPath, [join(dir, 'bridge.mjs')], { stdio: ['pipe', 'pipe', 'pipe'] });
    let stdout = '', stderr = '';
    child.stdout.on('data', data => { stdout += data; if (stdout.includes('"type":"result"')) child.stdin.end(); });
    child.stderr.on('data', data => { stderr += data; });
    const timer = setTimeout(() => child.kill('SIGKILL'), 5000);
    const code = await new Promise((resolve, reject) => {
      child.on('error', reject); child.on('close', resolve);
      child.stdin.write(JSON.stringify({ version: 2, type: 'turn_start', workspace: dir, tools: ['Read'], prompt: 'fixture' }) + '\n');
    }).finally(() => clearTimeout(timer));
    assert.equal(code, 1, stderr);
    const frames = stdout.trim().split('\n').map(line => JSON.parse(line));
    assert.equal(frames.at(-1).status, 'failed');
    assert.match(frames.at(-1).error, /isolation/);
    assert.equal(frames.some(frame => frame.type === 'native_request'), false);
    assert.equal(await readFile(join(dir, 'closed'), 'utf8'), 'yes');
    assert.equal(JSON.parse(await readFile(join(dir, 'permit'), 'utf8')).hookSpecificOutput.permissionDecision, 'deny');
  } finally { await rm(dir, { recursive: true, force: true }); }
});

test('reject unknown plugins, missing evidence, skills and delegation while retaining ordinary native tools', () => {
  const clean = { plugins: [], skills: [], tools: ['Read', 'Bash'] };
  assert.doesNotThrow(() => assertIsolatedInitialization(clean));
  for (const changes of [
    { plugins: [{ name: 'new-builtin', path: 'builtin' }] }, { plugins: undefined },
    { skills: ['repo-skill'] }, { skills: undefined }, { tools: undefined },
    { tools: ['Skill'] }, { tools: ['Agent'] }, { tools: ['Task'] },
    { plugin_errors: [{ type: 'unknown' }] }, { plugin_errors: {} },
  ]) assert.throws(() => assertIsolatedInitialization({ ...clean, ...changes }), /isolation/);
});

test('selected skill requires qualified acknowledgement and exact later plugin set', () => {
  const init = {claude_code_version:'2.1.322',plugins_applied:true,hooks_applied:true,hook_on_failure_supported:true,commands:[{name:'aw-probe:probe'}]};
  assert.doesNotThrow(() => assertSkillAcknowledgement(init,'aw-probe:probe'));
  for (const changes of [{plugins_applied:false},{plugins_applied:undefined},{commands:[]},{claude_code_version:'2.1.319'},{hooks_applied:false},{hook_on_failure_supported:false}]) assert.throws(() => assertSkillAcknowledgement({...init,...changes},'aw-probe:probe'));
  const skill={name:'probe',qualified:'aw-probe:probe',root:'/owned/plugin'};
  const message={plugins:[{name:'aw-probe',path:skill.root}],skills:['builtin-catalog-name',skill.qualified],tools:['Skill','Read']};
  assert.doesNotThrow(() => assertSelectedInitialization(message,skill));
  for (const changes of [{plugins:[]},{plugins:[...message.plugins,{name:'unknown',path:'builtin'}]},{plugins:[{name:'aw-probe',path:'/foreign'}]},{tools:['Skill','Agent']},{skills:['probe']},{plugin_errors:[{type:'unknown'}]}]) assert.throws(() => assertSelectedInitialization({...message,...changes},skill));
  const childSkill={...skill,child:{type:'aw-child-review'}};
  const childMessage={...message,tools:['Task','Read','Skill']};
  assert.doesNotThrow(()=>assertSelectedInitialization(childMessage,childSkill));
  for(const tools of [['Agent','Read','Skill'],['Task','Read','Skill','Bash'],['Read','Skill'],['Task','Skill']]) assert.throws(()=>assertSelectedInitialization({...childMessage,tools},childSkill));
});
