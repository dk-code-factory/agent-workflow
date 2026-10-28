import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once, EventEmitter } from 'node:events';
import { PassThrough } from 'node:stream';
import test from 'node:test';
import { readinessLine } from './readiness.mjs';

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
    const closed = once(child, 'close');
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
