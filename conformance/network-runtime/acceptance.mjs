// Explicit opt-in. Never imported by deterministic CI. All resources use an
// unpredictable owned name; cleanup never prunes or resets shared Podman state.
import assert from 'node:assert/strict';
import { spawn, execFileSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, writeFileSync, readFileSync, chmodSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { createServer } from 'node:net';
import { createRequire } from 'node:module';
import { once } from 'node:events';
const require = createRequire(resolve('ui/package.json'));
if (!process.argv.includes('--run-privileged')) throw new Error('requires --run-privileged; uses sudo/rootful Podman and normal Chrome, no models');
const { chromium } = require('@playwright/test');
const root = resolve('.');
const dir = mkdtempSync(join(tmpdir(), 'aw21-'));
chmodSync(dir, 0o755);
const name = 'aw21-' + dir.split('-').at(-1).toLowerCase();
const runtime = join(dir, 'runtime');
const fixture = join(dir, 'fixture');
const workflow = join(dir, 'workflow');
const children = [];
let browser;
let started = false;
let image;
let ref;
const ownedContainers = [];
const ownedNetworks = [];
const run = (cmd, args, options = {}) => execFileSync(cmd, args, { cwd: root, encoding: 'utf8', timeout: 120000, ...options }).trim();
const runtimeRun = (...args) => run(runtime, args);
const inside = (...args) => runtimeRun('exec', name, '--', ...args);
const podman = (...args) => inside('podman', ...args);
const sleep = ms => new Promise(r => setTimeout(r, ms));
const port = async () => { const server = createServer(); server.listen(0, '127.0.0.1'); await once(server, 'listening'); const p = server.address().port; await new Promise(r => server.close(r)); return p; };
const status = () => JSON.parse(runtimeRun('status', name));
const spawnInside = (...args) => { const c = spawn(runtime, ['exec', name, '--', ...args], { cwd: root, stdio: ['ignore', 'pipe', 'pipe'] }); children.push(c); return c; };
const settled = async () => { for (let n = 0; n < 100; n++) { if (Object.values(status().members).every(m => m.state === 'cleaned')) return; await sleep(100); } throw new Error('members did not clean'); };
const snapshot = () => ['link', 'address', 'route'].map(kind => JSON.parse(run('ip', ['-j', kind, 'show'])));
const stable = data => JSON.stringify(data, (key, value) => ['valid_life_time', 'preferred_life_time'].includes(key) ? undefined : value);
const evidence = { runtime: name, checks: [], browserRequests: 0 };
try {
  for (const [output, source] of [[runtime, './cmd/agent-workflow-runtime'], [workflow, './cmd/agent-workflow'], [fixture, './conformance/network-runtime/fixture'], [join(dir, 'guardian'), './cmd/agent-workflow-guardian']]) run('go', ['build', '-o', output, source], { env: { ...process.env, CGO_ENABLED: '0' } });
  // Host CA bundle is fixture content, not an agent home or credential store.
  writeFileSync(join(dir, 'Containerfile'), 'FROM scratch\nCOPY fixture /fixture\nCOPY guardian /usr/local/bin/agent-workflow-guardian\nCOPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt\nLABEL agent-workflow.guardian-protocol="1"\nENTRYPOINT ["/fixture"]\n');
  writeFileSync(join(dir, 'ca-certificates.crt'), readFileSync('/etc/ssl/certs/ca-certificates.crt'));
  image = run('sudo', ['-n', 'podman', 'build', '--network=none', '--pull=never', '-q', '-t', 'localhost/' + name, dir]);
  assert.match(image, /^[a-f0-9]{64}$/); image = 'sha256:' + image;
  const hostStore = run('sudo', ['-n', 'podman', 'info', '--format', '{{.Store.GraphRoot}}']);
  const before = stable(snapshot());
  run('sudo', ['-n', runtime, 'prepare', name, '--uid', String(process.getuid())]);
  run('sudo', ['-n', runtime, 'start', name]); started = true;
  ref = status().reference;
  assert.equal(inside('podman', 'info', '--format', '{{.Store.GraphRoot}}'), hostStore);
  assert.equal(podman('image', 'inspect', image, '--format', '{{.Id}}').replace(/^sha256:/, ''), image.replace(/^sha256:/, ''));
  const identity = JSON.parse(inside(fixture, 'identity'));
  assert.equal(identity.uid, process.getuid()); assert.equal(identity.gid, process.getgid()); assert.equal(identity.endpoint, ref.endpoint);
  assert.notEqual(identity.netns, run('readlink', ['/proc/self/ns/net']));
  evidence.checks.push('identity, endpoint and cached rootful image store');

  const a = spawnInside('sleep', '60'), b = spawnInside('sleep', '60'), c = spawnInside('sleep', '60');
  for (let n = 0; n < 100 && Object.values(status().members).filter(m => m.state !== 'cleaned').length < 3; n++) await sleep(50);
  assert.throws(() => runtimeRun('stop', name), /busy/);
  a.kill('SIGKILL'); await once(a, 'exit');
  assert.equal(b.exitCode, null); assert.equal(c.exitCode, null);
  assert.throws(() => runtimeRun('stop', name), /busy/);
  b.kill('SIGKILL'); c.kill('SIGKILL'); await Promise.all([once(b, 'exit'), once(c, 'exit')]); await settled();
  evidence.checks.push('three independent callers, disconnect and atomic busy stop');

  const network = name + '-dns'; ownedNetworks.push(network); podman('network', 'create', network);
  const published = await port(), forwarded = await port();
  const peer = name + '-peer'; ownedContainers.push(peer);
  podman('run', '-d', '--pull=never', '--name', peer, '--network', network, '--network-alias', 'peer', '-p', `127.0.0.1:${published}:8080`, image, 'serve', ':8080');
  assert.match(podman('run', '--rm', '--pull=never', '--network', network, image, 'probe', 'peer'), /dns\+https-ok/);
  runtimeRun('forward', name, String(forwarded), String(published));
  assert.equal(run('curl', ['--fail', '--silent', '--max-time', '10', `http://127.0.0.1:${forwarded}/`]), 'runtime-service');
  assert.throws(() => runtimeRun('forward', name, String(forwarded), String(published + 1)), /owned/);
  const loopback = await port(), hostLoopback = await port();
  const server = spawnInside(fixture, 'serve', `127.0.0.1:${loopback}`);
  runtimeRun('forward', name, String(hostLoopback), String(loopback)); await sleep(200);
  assert.equal(run('curl', ['--fail', '--silent', '--max-time', '10', `http://127.0.0.1:${hostLoopback}/`]), 'runtime-service');
  server.kill('SIGKILL'); await once(server, 'exit'); await settled();
  assert.equal(inside('curl', '--silent', '--fail', '--unix-socket', ref.endpoint.replace('unix://', ''), 'http://podman/_ping'), 'OK');
  evidence.checks.push('named-network DNS, HTTPS, API, published ports and runtime-loopback forwarding');

  mkdirSync(join(dir, 'workflows'), { recursive: true });
  writeFileSync(join(dir, 'agents.yml'), `execution:\n  network-runtime: ${name}\nagents:\n  fixture:\n    provider: codex-cli\n    command: ${fixture}\n    sandbox: read-only\n`);
  writeFileSync(join(dir, 'workflows/local.yml'), `on: {workflow_call: {}}\njobs:\n  test:\n    steps:\n      - id: command\n        argv: [${JSON.stringify(fixture)}, network, ${name}-command]\n      - id: agent\n        uses: agent/fixture\n        with:\n          prompt: ${name}-agent\n`);
  run(workflow, ['run', '--foreground', '--workspace', dir, 'workflows/local.yml']);
  writeFileSync(join(dir, 'agents.yml'), `execution:\n  network-runtime: ${name}\n  mode: container\n  runtime: podman\n  image: ${image}\nagents:\n  fixture:\n    provider: codex-cli\n    execution: {mode: local}\n`);
  writeFileSync(join(dir, 'workflows/container.yml'), 'on: {workflow_call: {}}\njobs:\n  test:\n    steps:\n      - id: command\n        argv: [/fixture, identity]\n');
  run(workflow, ['run', '--foreground', '--workspace', dir, 'workflows/container.yml']);
  evidence.checks.push('integrated local command, agent and container command policies');

  const h2 = spawn(fixture, ['h2'], { stdio: ['ignore', 'pipe', 'pipe'] }); children.push(h2);
  const url = await new Promise((resolve, reject) => { h2.stdout.once('data', data => resolve(String(data).trim())); h2.once('error', reject); });
  browser = await chromium.launch({ channel: 'chrome', headless: true });
  const context = await browser.newContext({ ignoreHTTPSErrors: true });
  const page = await context.newPage(); const failures = []; page.on('requestfailed', request => failures.push(request.failure()?.errorText));
  await page.goto(url);
  const pending = page.evaluate(async () => Promise.all(Array.from({ length: 24 }, (_, i) => fetch(`/request?delay=${2000 + i * 80}`).then(async r => ({ ok: r.ok, body: await r.text() })))));
  await sleep(200);
  for (let i = 0; i < 6; i++) inside(fixture, 'network', name + '-chrome-' + i);
  const responses = await pending; assert.equal(responses.length, 24); assert(responses.every(r => r.ok && r.body === 'h2-ok')); assert.deepEqual(failures, []);
  evidence.browserRequests = responses.length;
  assert.equal(stable(snapshot()), before, 'host links/addresses/routes changed');
  evidence.checks.push('normal Chrome overlapping HTTP/2 and unchanged host links/addresses/routes');
  await browser.close(); browser = undefined;
  podman('rm', '-f', peer); ownedContainers.splice(0); podman('network', 'rm', network); ownedNetworks.splice(0);
  await settled(); runtimeRun('stop', name); started = false;
  run('sudo', ['-n', runtime, 'start', name]); started = true;
  assert.notEqual(status().reference.identity, ref.identity);
  const restored = spawnInside(fixture, 'serve', `127.0.0.1:${loopback}`); await sleep(200);
  assert.equal(run('curl', ['--fail', '--silent', '--max-time', '10', `http://127.0.0.1:${hostLoopback}/`]), 'runtime-service');
  restored.kill('SIGKILL'); await once(restored, 'exit'); await settled();
  assert.throws(() => run('curl', ['--fail', '--silent', '--max-time', '2', '--unix-socket', ref.endpoint.replace('unix://', ''), 'http://podman/_ping']));
  const old = status().reference;
  run('sudo', ['-n', 'python3', '-c', `import socket,struct,os,signal,json
s=socket.socket(socket.AF_UNIX,socket.SOCK_SEQPACKET)
s.connect(${JSON.stringify('/run/agent-workflow-runtimes/' + name + '/control.sock')})
s.send(json.dumps({"operation":"check","reference":json.loads(${JSON.stringify(JSON.stringify(old))})}).encode())
assert not json.loads(s.recv(1048576)).get("error")
pid,uid,gid=struct.unpack("3i",s.getsockopt(socket.SOL_SOCKET,socket.SO_PEERCRED,12))
assert uid==0
fd=os.pidfd_open(pid)
signal.pidfd_send_signal(fd,signal.SIGKILL)`]);
  await sleep(1000); run('sudo', ['-n', runtime, 'start', name]);
  assert.notEqual(status().reference.identity, old.identity);
  evidence.checks.push('clean restart, crash recovery, forwarding recovery and stale endpoint refusal');
  console.log('PASS', JSON.stringify(evidence));
} finally {
  if (browser) await browser.close();
  for (const c of children) if (c.exitCode === null && c.signalCode === null) c.kill('SIGKILL');
  if (started) {
    for (const container of ownedContainers) { try { podman('rm', '-f', container); } catch {} }
    for (const network of ownedNetworks) { try { podman('network', 'rm', network); } catch {} }
    try { await settled(); runtimeRun('stop', name); } catch (e) { console.error('owned runtime needs inspection:', name, e.message); }
  }
  if (image) { try { run('sudo', ['-n', 'podman', 'rmi', image]); } catch {} }
  console.log(JSON.stringify(evidence));
  // Root-owned registry and receipts are retained for operator audit.
  rmSync(dir, { recursive: true, force: true });
}
