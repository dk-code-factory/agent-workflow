import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once, EventEmitter } from 'node:events';
import { PassThrough } from 'node:stream';
import test from 'node:test';
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { childCompletion, RuntimeAttempt, readinessLine } from './readiness.mjs';

function childFixture() {
  return Object.assign(new EventEmitter(), { stdout: new PassThrough(), exitCode: null, signalCode: null });
}

const listenerCounts = child => [
  ...['exit', 'error'].map(event => child.listenerCount(event)),
  ...['data', 'end', 'close', 'error'].map(event => child.stdout.listenerCount(event)),
];
const assertClean = (child, before = [0, 0, 0, 0, 0, 0]) => assert.deepEqual(listenerCounts(child), before);

test('readiness joins partial first-line delivery and ignores later lines', async () => {
  const child = childFixture();
  const ready = readinessLine(child, 'fixture');
  child.stdout.write('https://127.');
  child.stdout.write('0.0.1:1234\nother output\n');
  assert.equal(await ready, 'https://127.0.0.1:1234');
  assertClean(child);
});

test('exit without readiness rejects and removes listeners', async () => {
  const child = childFixture();
  const ready = readinessLine(child, 'fixture');
  child.stdout.write('partial');
  child.emit('exit', 1, null);
  await assert.rejects(ready, /fixture: exited before readiness/);
  assertClean(child);
});

test('EOF with an incomplete line rejects even while the child stays alive', async () => {
  const child = childFixture();
  const ready = readinessLine(child, 'fixture');
  child.stdout.end('partial');
  await assert.rejects(ready, /stdout ended before readiness/);
  assertClean(child);
});

test('stdout error rejects and removes listeners', async () => {
  const child = childFixture();
  const ready = readinessLine(child, 'fixture');
  const error = new Error('broken fixture stream');
  child.stdout.emit('error', error);
  await assert.rejects(ready, failure => failure.cause === error);
  assertClean(child);
});

test('spawn failure rejects and removes listeners', async () => {
  const child = spawn('/nonexistent-aw-readiness-fixture', [], { stdio: ['ignore', 'pipe', 'pipe'] });
  const before = listenerCounts(child);
  await assert.rejects(readinessLine(child, 'fixture'), failure => failure.cause?.code === 'ENOENT');
  assertClean(child, before);
});

test('short deadline reaches owned finally cleanup for a silent child', async () => {
  const child = spawn(process.execPath, ['-e', 'setInterval(() => {}, 1000)'], { stdio: ['ignore', 'pipe', 'pipe'] });
  await once(child, 'spawn');
  const before = listenerCounts(child);
  try {
    await assert.rejects(readinessLine(child, 'fixture', 20), /readiness timed out/);
    assertClean(child, before);
  } finally {
    const closed = childCompletion(child, 'readiness fixture teardown', 2000);
    child.kill('SIGKILL');
    await closed;
  }
  assert.equal(child.signalCode, 'SIGKILL');
});

test('readiness rejects output beyond its bound without retaining listeners', async () => {
  const child = childFixture();
  const ready = readinessLine(child, 'fixture');
  child.stdout.write(Buffer.alloc(4097, 65));
  await assert.rejects(ready, /exceeds 4096 bytes/);
  assertClean(child);
});

test('child completion preserves final output and exit code, including already closed children', async () => {
  const child = spawn(process.execPath, ['-e', 'process.stdout.write("final-output"); process.exitCode = 7'], { stdio: ['ignore', 'pipe', 'pipe'] });
  let output = '';
  child.stdout.on('data', data => { output += data; });
  assert.deepEqual(await childCompletion(child, 'early exit'), [7, null]);
  assert.equal(output, 'final-output');
  assert.deepEqual(await childCompletion(child, 'already closed', 20), [7, null]);
  assert.equal(child.listenerCount('close'), 0);
  assert.equal(child.listenerCount('error'), 0);
});

test('child completion rejects a failed spawn and clears its listeners', async () => {
  const child = spawn('/nonexistent-aw-completion-fixture', [], { stdio: ['ignore', 'pipe', 'pipe'] });
  await assert.rejects(childCompletion(child, 'spawn failure'), error => error.cause?.code === 'ENOENT');
  assert.equal(child.listenerCount('close'), 0);
  assert.equal(child.listenerCount('error'), 0);
  await childCompletion(child, 'failed spawn reap');
});

test('completion deadline reaches finally, kills and reaps the owned stalled child', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'aw-completion-'));
  const child = spawn(process.execPath, ['-e', 'setInterval(() => {}, 1000)'], { stdio: ['ignore', 'pipe', 'pipe'] });
  try {
    try { await assert.rejects(childCompletion(child, 'stalled', 20), /completion timed out/); }
    finally {
      child.kill('SIGKILL');
      await childCompletion(child, 'owned teardown', 2000);
      rmSync(dir, { recursive: true });
    }
    assert.equal(child.signalCode, 'SIGKILL');
    assert.equal(existsSync(dir), false);
    assert.equal(child.listenerCount('close'), 0);
    assert.equal(child.listenerCount('error'), 0);
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test('completion stays bounded after exit when owned pipes never confirm close', async () => {
  const child = childFixture();
  child.exitCode = 0;
  await assert.rejects(childCompletion(child, 'unreaped', 10), /completion timed out/);
  assert.equal(child.listenerCount('close'), 0);
  assertClean(child);
});

test('attempted starts retain audit evidence until exact shutdown succeeds', async () => {
  for (const scenario of ['success', 'before start failure', 'unconfirmed live start', 'identity failure', 'shutdown failure', 'restart failure']) {
    const dir = mkdtempSync(join(tmpdir(), 'aw-attempt-'));
    const audit = join(dir, 'audit.json');
    const ownership = new RuntimeAttempt();
    const reference = { name: 'disposable', identity: 'original' };
    let child;
    let stopCalls = 0;
    const stopExact = retained => {
      ++stopCalls;
      assert.equal(retained, reference);
      if (scenario === 'shutdown failure') throw new Error('shutdown unconfirmed');
    };
    try {
      try {
        ownership.start(() => {
          if (scenario === 'before start failure') throw new Error('start command failed');
          child = spawn(process.execPath, ['-e', 'setInterval(() => {}, 1000)'], { stdio: ['ignore', 'pipe', 'pipe'] });
          if (scenario === 'unconfirmed live start') throw new Error('readiness unconfirmed');
        }, () => {
          if (scenario === 'identity failure') throw new Error('identity unavailable');
          return reference;
        });
        if (scenario === 'restart failure') {
          ownership.stop(stopExact);
          ownership.start(() => { throw new Error('restart unconfirmed'); }, () => reference);
        }
      } catch (error) { assert.match(error.message, /failed|unconfirmed|unavailable/); }
      finally {
        try { ownership.stop(stopExact); } catch (error) { assert.match(error.message, /unconfirmed|no confirmed identity/); }
        if (ownership.pending) writeFileSync(audit, JSON.stringify({ reference: ownership.reference ?? null }));
        else rmSync(dir, { recursive: true });
      }
      if (scenario === 'unconfirmed live start') {
        assert(child.pid > 0);
        assert.equal(child.exitCode, null);
      }
      assert.equal(ownership.pending, scenario !== 'success');
      assert.equal(existsSync(audit), scenario !== 'success');
      if (ownership.pending) {
        const retained = JSON.parse(readFileSync(audit));
        assert.deepEqual(retained.reference, scenario === 'shutdown failure' ? reference : null);
      }
      assert.equal(stopCalls, ['success', 'shutdown failure', 'restart failure'].includes(scenario) ? 1 : 0);
    } finally {
      if (child) { child.kill('SIGKILL'); await childCompletion(child, 'fake runtime teardown', 2000); }
      rmSync(dir, { recursive: true, force: true });
    }
  }
});
