// Explicit opt-in. Never imported by deterministic CI. All resources use an
// unpredictable owned name; cleanup never prunes or resets shared Podman state.
import assert from 'node:assert/strict';
import { spawn, execFileSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, writeFileSync, readFileSync, readlinkSync, readdirSync, chmodSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { createServer } from 'node:net';
import { createRequire } from 'node:module';
import { once } from 'node:events';
import { readinessLine, childCompletion, RuntimeAttempt } from './readiness.mjs';
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
const runtimeAttempt = new RuntimeAttempt();
let image;
let ref;
let monitorOutput = '';
let monitorErrors = '';
let monitor;
let cleanupUncertain = false;
const ownedContainers = [];
const ownedNetworks = [];
const run = (cmd, args, options = {}) => execFileSync(cmd, args, { cwd: root, encoding: 'utf8', timeout: 120000, ...options }).trim();
const runtimeRun = (...args) => run(runtime, args);
const operator = (...args) => run('sudo', ['-n', '/bin/sh', '-c', 'umask 077; exec "$@"', 'runtime-operator', runtime, ...args]);
const inside = (...args) => runtimeRun('exec', name, '--', ...args);
const podman = (...args) => inside('podman', ...args);
const sleep = ms => new Promise(r => setTimeout(r, ms));
const port = async () => { const server = createServer(); server.listen(0, '127.0.0.1'); await once(server, 'listening'); const p = server.address().port; await new Promise(r => server.close(r)); return p; };
const status = () => JSON.parse(runtimeRun('status', name));
const startRuntime = () => runtimeAttempt.start(() => operator('start', name), () => status().reference);
// Bind the pidfd before challenging the same authenticated connection with the
// retained reference. A stop reply precedes owned teardown; wait for daemon exit.
const stopExact = reference => run('python3', ['-c', `import json,os,select,socket,struct,sys
reference=json.loads(sys.argv[1])
with socket.socket(socket.AF_UNIX,socket.SOCK_SEQPACKET) as s:
    s.settimeout(10)
    s.connect('/run/agent-workflow-runtimes/'+reference['name']+'/control.sock')
    pid,uid,gid=struct.unpack('3i',s.getsockopt(socket.SOL_SOCKET,socket.SO_PEERCRED,12))
    assert uid==0,'runtime peer is not root'
    fd=os.pidfd_open(pid)
    try:
        s.send(json.dumps({'operation':'stop','reference':reference}).encode())
        response=json.loads(s.recv(1048576))
        assert not response.get('error'),response.get('error')
        poll=select.poll();poll.register(fd,select.POLLIN)
        assert poll.poll(10000),'owned daemon shutdown unconfirmed'
    finally:os.close(fd)`, JSON.stringify(reference)], { timeout: 25000 });
const stopRuntime = () => runtimeAttempt.stop(stopExact);
const spawnInside = (...args) => { const c = spawn(runtime, ['exec', name, '--', ...args], { cwd: root, stdio: ['ignore', 'pipe', 'pipe'] }); children.push(c); return c; };
const settled = async () => { for (let n = 0; n < 100; n++) { if (Object.values(status().members).every(m => m.state === 'cleaned')) return; await sleep(100); } throw new Error('members did not clean'); };
const snapshot = () => ['link', 'address', 'route'].map(kind => JSON.parse(run('ip', ['-j', kind, 'show'])));
const stable = data => JSON.stringify(data, (key, value) => ['valid_life_time', 'preferred_life_time'].includes(key) ? undefined : value);
const subscribedMonitor = async child => {
  // Match the child's actual NETLINK_ROUTE socket inode and multicast groups.
  // A spawned process alone does not establish that monitoring is subscribed.
  const required = 0x551; // RTMGRP_LINK, IPv4/IPv6 IFADDR and ROUTE.
  for (let attempt = 0; attempt < 200; attempt++) {
    assert.equal(child.exitCode, null, 'host monitor exited before subscribing');
    assert.equal(child.signalCode, null, 'host monitor was signalled before subscribing');
    try {
      const sockets = new Set(readdirSync(`/proc/${child.pid}/fd`).flatMap(fd => {
        try { return [readlinkSync(`/proc/${child.pid}/fd/${fd}`).match(/^socket:\[(\d+)\]$/)?.[1]].filter(Boolean); }
        catch { return []; }
      }));
      for (const row of readFileSync(`/proc/${child.pid}/net/netlink`, 'utf8').trim().split('\n').slice(1)) {
        const fields = row.trim().split(/\s+/), groups = Number.parseInt(fields[3], 16);
        if (fields[1] === '0' && sockets.has(fields[9]) && (groups & required) === required) return fields[3];
      }
    } catch {}
    await sleep(10);
  }
  throw new Error('host monitor multicast subscription unconfirmed');
};
const evidence = { runtime: name, checks: [], browserRequests: 0 };
try {
  for (const [output, source] of [[runtime, './cmd/agent-workflow-runtime'], [workflow, './cmd/agent-workflow'], [fixture, './conformance/network-runtime/fixture'], [join(dir, 'guardian'), './cmd/agent-workflow-guardian']]) run('go', ['build', '-o', output, source], { env: { ...process.env, CGO_ENABLED: '0' } });
  // Host CA bundle is fixture content, not an agent home or credential store.
  writeFileSync(join(dir, 'Containerfile'), 'FROM scratch\nCOPY fixture /fixture\nCOPY guardian /usr/local/bin/agent-workflow-guardian\nCOPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt\nLABEL agent-workflow.guardian-protocol="1" agent-workflow.codex-cli="0.164.0"\nENTRYPOINT ["/fixture"]\n');
  writeFileSync(join(dir, 'ca-certificates.crt'), readFileSync('/etc/ssl/certs/ca-certificates.crt'));
  image = run('sudo', ['-n', 'podman', 'build', '--network=none', '--pull=never', '-q', '-t', 'localhost/' + name, dir]);
  assert.match(image, /^[a-f0-9]{64}$/); image = 'sha256:' + image;
  const hostStore = run('sudo', ['-n', 'podman', 'info', '--format', '{{.Store.GraphRoot}}']);
  monitor = spawn('ip', ['monitor', 'link', 'address', 'route'], { stdio: ['ignore', 'pipe', 'pipe'] }); children.push(monitor);
  monitor.stdout.on('data', data => { monitorOutput += data; });
  monitor.stderr.on('data', data => { monitorErrors += data; });
  await once(monitor, 'spawn');
  const subscriptionMask = await subscribedMonitor(monitor);
  const before = stable(snapshot());
  const authority = join(root, 'conformance/network-runtime/authority.py');
  const authorityArgs = [runtime, name, String(process.getuid()), String(process.getgid()), JSON.stringify(process.getgroups())];
  evidence.coldOperator = JSON.parse(run('sudo', ['-n', 'unshare', '--mount', '--propagation', 'private', '/usr/bin/python3', authority, 'cold-prepare', ...authorityArgs, run('readlink', ['/proc/self/ns/mnt'])]));
  operator('prepare', name, '--uid', String(process.getuid()));
  startRuntime();
  ref = runtimeAttempt.reference;
  assert.equal(inside('podman', 'info', '--format', '{{.Store.GraphRoot}}'), hostStore);
  assert.equal(podman('image', 'inspect', image, '--format', '{{.Id}}').replace(/^sha256:/, ''), image.replace(/^sha256:/, ''));
  const identity = JSON.parse(inside(fixture, 'identity'));
  assert.equal(identity.uid, process.getuid()); assert.equal(identity.gid, process.getgid()); assert.equal(identity.endpoint, ref.endpoint);
  assert.notEqual(identity.netns, run('readlink', ['/proc/self/ns/net']));
  evidence.checks.push('identity, endpoint and cached rootful image store');
  evidence.nativeGroups = JSON.parse(run('sudo', ['-n', '/usr/bin/python3', authority, 'groups', ...authorityArgs, identity.netns]));
  await settled();
  evidence.checks.push('restrictive operator umask preserves cold managed traversal/launcher, nonroot admission/DNS and every native supplementary group through actual nsenter/setpriv');

  // Bytes are passed by Python directly to Linux exec; the fixture reports hex
  // before JSON so invalid UTF-8 cannot be replaced on both sides of the oracle.
  const byteProof = JSON.parse(run('python3', ['-c', `import json,os,subprocess,sys
runtime,name,fixture,root=map(os.fsencode,sys.argv[1:])
raw_root=root+b'/native-bytes';os.mkdir(raw_root,0o700)
cwd=raw_root+b'/cwd-\\xff';os.mkdir(cwd,0o700)
executable=raw_root+b'/executable-\\xfd';os.symlink(fixture,executable)
argument=b'arg-\\xff-\\xc3('
environment={b'PATH':b'/usr/bin:/bin',b'PWD':cwd,b'AW21_RAW_ENV':b'env-\\xfe-\\xed\\xa0\\x80'}
environment.update({('AW21_MANY_%05d'%i).encode():('value-%05d'%i).encode() for i in range(10001)})
def observe(command):return json.loads(subprocess.check_output(command,cwd=cwd,env=environment,timeout=30))
native=observe([executable,b'native-bytes',argument])
contained=observe([runtime,b'exec',name,b'--',executable,b'native-bytes',argument])
assert native==contained,(native,contained)
assert native['arg_hex']==argument.hex() and native['cwd_hex']==cwd.hex()
assert native['executable_hex']==executable.hex() and native['env_hex']==environment[b'AW21_RAW_ENV'].hex()
assert native['many_count']==10001 and native['many_values_ok']
print(json.dumps({'native':native,'contained':contained,'byte_exact':True}))`, runtime, name, fixture, dir]));
  assert.equal(byteProof.byte_exact, true);
  evidence.nativeBytes = byteProof;
  await settled();
  evidence.checks.push('actual runtime preserves opaque Linux argv/env/cwd/executable bytes and every entry of a10001-variable environment');

  assert.throws(() => inside('/bin/sh', '-c', 'exit 125'), { status: 125 });
  await settled();
  const largeEnvironment = Object.fromEntries(Array.from({ length: 200 }, (_, i) => [`AW21_LARGE_${i}`, 'v'.repeat(1024)]));
  const aggregate = run(runtime, ['exec', name, '--', '/usr/bin/python3', '-c',
    'import os,sys; assert [len(v) for v in sys.argv[1:]] == [70000,70000]; assert all(len(os.environ["AW21_LARGE_"+str(i)]) == 1024 for i in range(200)); print("aggregate-preserved")',
    'a'.repeat(70000), 'b'.repeat(70000)], { env: { ...process.env, ...largeEnvironment } });
  assert.equal(aggregate, 'aggregate-preserved');
  await settled();
  evidence.checks.push('ordinary exit125 cleans membership and native aggregate arguments/environment survive the actual root-member path');

  const hookInput = join(dir, 'hook-input.json');
  writeFileSync(hookInput, JSON.stringify({ hook_event_name: 'PreToolUse', tool_name: 'Bash', tool_input: { command: 'printf "%s\\n" "$DOCKER_HOST" "$CONTAINER_HOST" "$AGENT_WORKFLOW_NETWORK_RUNTIME"; exit 7', fixture_option: 'kept' } }));
  const hook = join(root, 'plugins/network-runtime/hooks/pre-tool-use.sh');
  const rewritten = JSON.parse(inside('/bin/sh', '-c', 'DOCKER_HOST=unix:///stale.sock; CONTAINER_HOST=unix:///stale.sock; export DOCKER_HOST CONTAINER_HOST; /bin/sh "$1" < "$2"', 'hook-fixture', hook, hookInput));
  assert.equal(rewritten.hookSpecificOutput.permissionDecision, 'allow');
  assert.equal(rewritten.hookSpecificOutput.updatedInput.fixture_option, 'kept');
  const executed = spawn(runtime, ['exec', name, '--', '/bin/sh', '-c', rewritten.hookSpecificOutput.updatedInput.command], { cwd: root, stdio: ['ignore', 'pipe', 'pipe'] }); children.push(executed);
  let hookOutput = ''; executed.stdout.on('data', data => { hookOutput += data; });
  const [hookCode] = await childCompletion(executed, 'hook fixture'); assert.equal(hookCode, 7);
  assert.deepEqual(hookOutput.trim().split('\n'), [ref.endpoint, ref.endpoint, name]);
  const staleRef = { ...ref, generation: ref.generation + 1 };
  const denied = JSON.parse(inside('/bin/sh', '-c', 'AGENT_WORKFLOW_NETWORK_REFERENCE="$1"; export AGENT_WORKFLOW_NETWORK_REFERENCE; /bin/sh "$2" < "$3"', 'hook-stale', JSON.stringify(staleRef), hook, hookInput));
  assert.equal(denied.hookSpecificOutput.permissionDecision, 'deny');
  const outside = JSON.parse(run('/bin/sh', [hook], { input: readFileSync(hookInput), env: { PATH: process.env.PATH, DOCKER_HOST: ref.endpoint, CONTAINER_HOST: ref.endpoint, AGENT_WORKFLOW_NETWORK_RUNTIME: name, AGENT_WORKFLOW_NETWORK_REFERENCE: JSON.stringify(ref) } }));
  assert.equal(outside.hookSpecificOutput.permissionDecision, 'deny', 'host namespace spoof accepted');
  evidence.checks.push('production trampoline and real runtime/helper repair stale selectors, preserve exit/options and reject generation/namespace mismatch');

  // Each frontend has its own umask; the service must never apply it globally.
  run('python3', ['-c', `import concurrent.futures,os,stat,subprocess
runtime,name,root=${JSON.stringify([runtime, name, dir])}
def check(mask):
    path=os.path.join(root,'mask-'+str(mask));os.mkdir(path)
    result=subprocess.run([runtime,'exec',name,'--','/bin/sh','-c','umask; touch "$1/file"; mkdir "$1/directory"','sh',path],umask=mask,check=True,capture_output=True,text=True)
    assert int(result.stdout.strip(),8)==mask,result.stdout
    assert stat.S_IMODE(os.stat(path+'/file').st_mode)==(0o666 & ~mask)
    assert stat.S_IMODE(os.stat(path+'/directory').st_mode)==(0o777 & ~mask)
with concurrent.futures.ThreadPoolExecutor() as pool:list(pool.map(check,[0o077,0o022]))`]);
  evidence.checks.push('independent caller umasks and resulting file/directory permissions');
  evidence.terminal = JSON.parse(run('python3', [join(root, 'conformance/network-runtime/terminal.py'), runtime, name]));
  await settled();
  evidence.checks.push('actual root-member standalone PTY, input, resize, signals, exit and isolated descendant cleanup');

  const a = spawnInside('sleep', '60'), b = spawnInside('sleep', '60'), c = spawnInside('sleep', '60');
  for (let n = 0; n < 100 && Object.values(status().members).filter(m => m.state !== 'cleaned').length < 3; n++) await sleep(50);
  assert.throws(() => stopExact(runtimeAttempt.reference), /busy/);
  a.kill('SIGKILL'); await childCompletion(a, 'first independent caller');
  assert.equal(b.exitCode, null); assert.equal(c.exitCode, null);
  assert.throws(() => stopExact(runtimeAttempt.reference), /busy/);
  b.kill('SIGKILL'); c.kill('SIGKILL'); await Promise.all([childCompletion(b, 'second independent caller'), childCompletion(c, 'third independent caller')]); await settled();
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
  server.kill('SIGKILL'); await childCompletion(server, 'loopback server'); await settled();
  assert.equal(inside('curl', '--silent', '--fail', '--unix-socket', ref.endpoint.replace('unix://', ''), 'http://podman/_ping'), 'OK');
  evidence.checks.push('named-network DNS, HTTPS, API, published ports and runtime-loopback forwarding');

  mkdirSync(join(dir, 'workflows'), { recursive: true });
  run('git', ['init', '-q', '--initial-branch=fixture', dir]);
  run('git', ['-C', dir, '-c', 'user.name=Runtime Fixture', '-c', 'user.email=runtime-fixture@example.invalid', '-c', 'core.hooksPath=/dev/null', 'commit', '--allow-empty', '-qm', 'Initialize disposable runtime fixture']);
  writeFileSync(join(dir, 'agents.yml'), `execution:\n  network-runtime: ${name}\nagents:\n  fixture:\n    provider: codex-cli\n    command: ${fixture}\n    sandbox: read-only\n`);
  writeFileSync(join(dir, 'workflows/local.yml'), `on: {workflow_call: {}}\njobs:\n  test:\n    steps:\n      - id: command\n        argv: [${JSON.stringify(fixture)}, network, ${name}-command]\n      - id: agent\n        uses: agent/fixture\n        with:\n          prompt: ${name}-agent\n`);
  run(workflow, ['run', '--foreground', '--workspace', dir, 'workflows/local.yml']);
  // Doctor's configured executable probes must cross the actual root member path.
  run('git', ['-C', dir, 'rev-parse', '--verify', 'HEAD']);
  const doctor = JSON.parse(run(workflow, ['doctor', '--workspace', dir]));
  assert.equal(doctor.backends.fixture.native_cached_login, true);
  assert(doctor.backends.fixture.version.includes(identity.netns));
  assert(doctor.backends.fixture.version.includes(ref.endpoint));
  writeFileSync(join(dir, 'agents.yml'), `execution:\n  network-runtime: ${name}\nagents:\n  metadata:\n    provider: codex-app-server\n    authentication: {mode: api-key, credential-env: AW21_FIXTURE_KEY}\n    execution: {mode: container, runtime: podman, image: ${image}}\n`);
  const containerDoctor = JSON.parse(run(workflow, ['doctor', '--workspace', dir], { env: { ...process.env, AW21_FIXTURE_KEY: 'model-free-fixture' } }));
  assert(containerDoctor.backends.metadata.container_runtime_version);
  assert.equal(containerDoctor.backends.metadata.image_id.replace(/^sha256:/, ''), image.replace(/^sha256:/, ''));
  evidence.checks.push('doctor executable probes and container metadata use selected namespace/endpoint');
  writeFileSync(join(dir, 'agents.yml'), `execution:\n  network-runtime: ${name}\n  mode: container\n  runtime: podman\n  image: ${image}\nagents:\n  fixture:\n    provider: codex-cli\n    execution: {mode: local}\n`);
  writeFileSync(join(dir, 'workflows/container.yml'), 'on: {workflow_call: {}}\njobs:\n  test:\n    steps:\n      - id: command\n        argv: [/fixture, identity]\n');
  run(workflow, ['run', '--foreground', '--workspace', dir, 'workflows/container.yml']);
  evidence.checks.push('integrated local command, agent and container command policies');

  const h2 = spawn(fixture, ['h2'], { stdio: ['ignore', 'pipe', 'pipe'] }); children.push(h2);
  const url = await readinessLine(h2, 'H2 fixture');
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
  assert.equal(monitor.exitCode, null, 'host network monitor stopped early');
  assert.equal(monitor.signalCode, null, 'host network monitor was signalled early');
  assert.equal(monitorOutput.trim(), '', 'transient host link/address/route change');
  evidence.checks.push('normal Chrome overlapping HTTP/2 and unchanged host links/addresses/routes');
  await browser.close(); browser = undefined;
  podman('rm', '-f', peer); ownedContainers.splice(0); podman('network', 'rm', network); ownedNetworks.splice(0);
  await settled(); stopRuntime();
  startRuntime();
  assert.notEqual(status().reference.identity, ref.identity);
  const restored = spawnInside(fixture, 'serve', `127.0.0.1:${loopback}`); await sleep(200);
  assert.equal(run('curl', ['--fail', '--silent', '--max-time', '10', `http://127.0.0.1:${hostLoopback}/`]), 'runtime-service');
  restored.kill('SIGKILL'); await childCompletion(restored, 'restored server'); await settled();
  assert.throws(() => run('curl', ['--fail', '--silent', '--max-time', '2', '--unix-socket', ref.endpoint.replace('unix://', ''), 'http://podman/_ping']));

  // Lose admission health while an exact engine-owned container remains alive.
  // Cancellation must still inspect/remove it through the retained generation.
  writeFileSync(join(dir, 'workflows/recovery.yml'), 'on: {workflow_call: {}}\njobs:\n  test:\n    steps:\n      - id: command\n        argv: [/fixture, serve, "127.0.0.1:0"]\n');
  const recoveryRef = status().reference;
  const recovering = spawn(workflow, ['run', '--foreground', '--workspace', dir, 'workflows/recovery.yml'], { cwd: root, stdio: ['ignore', 'pipe', 'pipe'] }); children.push(recovering);
  let reservation;
  for (let attempt = 0; attempt < 200; attempt++) {
    reservation = Object.values(status().members).find(member => member.state === 'reserved');
    if (reservation) {
      try {
        const inspected = JSON.parse(run('podman', ['--remote', '--url', recoveryRef.endpoint, 'container', 'inspect', 'agent-workflow-' + reservation.id]));
        if (inspected[0].State.Running) break;
      } catch {}
    }
    assert.equal(recovering.exitCode, null, 'recovery fixture exited before health failure');
    await sleep(50);
    if (attempt === 199) throw new Error('owned recovery container did not start');
  }
  const recoveryContainer = 'agent-workflow-' + reservation.id;
  inside('sudo', '-n', '/usr/sbin/ip', 'link', 'set', 'tap0', 'down');
  assert.equal(status().healthy, false);
  assert.throws(() => inside('/bin/echo', 'new-work-must-fail'), /unhealthy/);
  recovering.kill('SIGTERM'); await childCompletion(recovering, 'recovering workflow');
  await settled();
  assert.equal(status().members[reservation.id].state, 'cleaned');
  const retainedNames = run('podman', ['--remote', '--url', recoveryRef.endpoint, 'ps', '--all', '--format', '{{.Names}}']);
  assert(!retainedNames.split(/\s+/).includes(recoveryContainer));
  stopRuntime();
  startRuntime();
  assert.notEqual(status().reference.identity, recoveryRef.identity);
  evidence.checks.push('unhealthy admission refuses new work while exact owned container cleanup releases reservation');

  const old = status().reference;
  const crashCaller = spawnInside('/bin/sh', '-c', 'sleep 60 & echo "$$ $!"; wait');
  const crashPIDs = (await readinessLine(crashCaller, 'crash caller')).split(/\s+/).map(Number);
  assert.equal(crashPIDs.length, 2); assert(crashPIDs.every(pid => Number.isInteger(pid) && pid > 1));
  const activeMembers = Object.values(status().members).filter(m => m.state !== 'cleaned').map(m => m.id);
  assert(activeMembers.length > 0);
  run('sudo', ['-n', 'python3', '-c', `import socket,struct,os,signal,json,select
s=socket.socket(socket.AF_UNIX,socket.SOCK_SEQPACKET)
s.connect(${JSON.stringify('/run/agent-workflow-runtimes/' + name + '/control.sock')})
pid,uid,gid=struct.unpack("3i",s.getsockopt(socket.SOL_SOCKET,socket.SO_PEERCRED,12))
assert uid==0
fd=os.pidfd_open(pid)
s.settimeout(10)
s.send(json.dumps({"operation":"check","reference":json.loads(${JSON.stringify(JSON.stringify(old))})}).encode())
assert not json.loads(s.recv(1048576)).get("error")
signal.pidfd_send_signal(fd,signal.SIGKILL)
poll=select.poll();poll.register(fd,select.POLLIN)
assert poll.poll(10000),'owned crashed daemon exit unconfirmed'
os.close(fd)`]);
  await childCompletion(crashCaller, 'daemon-loss caller');
  run('sudo', ['-n', 'python3', '-c', `import json,os,time
root=${JSON.stringify('/var/lib/agent-workflow/runtimes/' + name)}
members=${JSON.stringify(activeMembers)}
pids=${JSON.stringify(crashPIDs)}
for attempt in range(100):
    receipts=[]
    for member in members:
        try:
            with open(root+'/member-'+member+'.done') as f:receipt=json.load(f)
            receipts.append(receipt.get('id')==member and receipt.get('uid')==${process.getuid()} and receipt.get('state')=='cleaned')
        except (FileNotFoundError,ValueError):receipts.append(False)
    alive=[]
    for pid in pids:
        try:
            with open('/proc/'+str(pid)+'/stat') as f:alive.append(f.read().split(') ',1)[1].split()[0]!='Z')
        except FileNotFoundError:alive.append(False)
    if all(receipts) and not any(alive):break
    time.sleep(.1)
else:raise AssertionError('active daemon-loss cleanup/receipt unconfirmed')`]);
  runtimeAttempt.pending = false; // Exact daemon exit and member cleanup proved.
  startRuntime();
  assert.notEqual(status().reference.identity, old.identity);
  evidence.checks.push('clean restart, active-member daemon-loss cleanup and receipts, forwarding recovery and stale endpoint refusal');
  await sleep(100);
  assert.equal(stable(snapshot()), before, 'host links/addresses/routes changed during restart');
  assert.equal(monitor.exitCode, null, 'host network monitor stopped early');
  assert.equal(monitor.signalCode, null, 'host network monitor was signalled early');
  monitor.kill('SIGTERM'); await childCompletion(monitor, 'host monitor');
  assert.equal(monitorErrors.trim(), '', 'host network monitor failed');
  assert.equal(monitorOutput.trim(), '', 'transient host link/address/route change');
  evidence.hostMonitor = { command: 'ip monitor link address route', subscriptionMask, events: monitorOutput, stderr: monitorErrors, snapshotsUnchanged: true };
  evidence.checks.push('continuous host network monitor reports no transient link/address/route changes');
  console.log('PASS', JSON.stringify(evidence));
} finally {
  if (browser) { try { await browser.close(); } catch (e) { cleanupUncertain = true; console.error('browser cleanup unconfirmed:', e.message); } }
  const reaped = await Promise.allSettled(children.map(async (child, index) => {
    const completion = childCompletion(child, `owned child ${index}`, 2000);
    try {
      if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL');
      await completion;
    }
    finally { child.stdout?.destroy(); child.stderr?.destroy(); }
  }));
  for (const result of reaped) if (result.status === 'rejected') {
    cleanupUncertain = true;
    (evidence.cleanupErrors ??= []).push(result.reason.message);
    console.error('owned child cleanup unconfirmed:', result.reason.message);
  }
  if (runtimeAttempt.pending) {
    try {
      assert(runtimeAttempt.reference, 'attempted start has no confirmed runtime identity');
      assert.deepEqual(status().reference, runtimeAttempt.reference, 'owned runtime identity changed');
      const endpoint = runtimeAttempt.reference.endpoint;
      for (const container of ownedContainers) run('podman', ['--remote', '--url', endpoint, 'rm', '-f', container]);
      for (const network of ownedNetworks) run('podman', ['--remote', '--url', endpoint, 'network', 'rm', network]);
      await settled(); stopRuntime();
    } catch (e) {
      cleanupUncertain = true;
      (evidence.cleanupErrors ??= []).push(e.message);
      console.error('owned runtime needs inspection:', name, e.message);
    }
  }
  if (runtimeAttempt.pending) {
    cleanupUncertain = true;
    evidence.pendingRuntime = { name, reference: runtimeAttempt.reference ?? null };
  }
  if (image && !cleanupUncertain) { try { run('sudo', ['-n', 'podman', 'rmi', image]); } catch {} }
  console.log(JSON.stringify(evidence));
  // Root-owned registry and receipts are retained for operator audit.
  if (cleanupUncertain) {
    writeFileSync(join(dir, 'cleanup-uncertain.json'), JSON.stringify(evidence, null, 2) + '\n');
    console.error('retained owned fixture evidence:', dir);
    process.exitCode = 1;
  }
  else rmSync(dir, { recursive: true, force: true });
}
