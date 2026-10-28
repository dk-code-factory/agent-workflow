// Read only the bounded first line; the harness owns child teardown in finally.
export function readinessLine(child, label, timeoutMs = 10000) {
  return new Promise((resolve, reject) => {
    const stdout = child.stdout;
    let pending = Buffer.alloc(0);
    const finish = (error, line) => {
      clearTimeout(timer);
      stdout.off('data', onData);
      stdout.off('end', onEnd);
      stdout.off('close', onEnd);
      stdout.off('error', onError);
      child.off('exit', onExit);
      child.off('error', onError);
      if (error) reject(error); else resolve(line);
    };
    const fail = reason => finish(new Error(`${label}: ${reason}`));
    const onData = data => {
      const newline = data.indexOf(10);
      const part = newline < 0 ? data : data.subarray(0, newline);
      if (pending.length + part.length > 4096) return fail('readiness line exceeds 4096 bytes');
      pending = Buffer.concat([pending, part]);
      if (newline >= 0) finish(null, pending.toString('utf8').trim());
    };
    const onEnd = () => fail('stdout ended before readiness');
    const onExit = () => fail('exited before readiness');
    const onError = error => finish(new Error(`${label}: readiness failed`, { cause: error }));
    const timer = setTimeout(() => fail('readiness timed out'), timeoutMs);
    stdout.on('data', onData);
    stdout.once('end', onEnd);
    stdout.once('close', onEnd);
    stdout.once('error', onError);
    child.once('exit', onExit);
    child.once('error', onError);
    if (child.exitCode !== null || child.signalCode !== null) onExit();
    else if (stdout.readableEnded || stdout.destroyed) onEnd();
  });
}

// Unlike exit, close also confirms that the owned stdout/stderr pipes drained.
export function childCompletion(child, label, timeoutMs = 10000) {
  return new Promise((resolve, reject) => {
    const finish = (error, code = child.exitCode, signal = child.signalCode) => {
      clearTimeout(timer);
      child.off('close', onClose);
      child.off('error', onError);
      if (error) reject(error); else resolve([code, signal]);
    };
    const onClose = (code, signal) => finish(null, code, signal);
    const onError = error => finish(new Error(`${label}: child completion failed`, { cause: error }));
    const timer = setTimeout(() => finish(new Error(`${label}: child completion timed out`)), timeoutMs);
    child.once('close', onClose);
    child.once('error', onError);
    if ((child.exitCode !== null || child.signalCode !== null) &&
        (!child.stdout || child.stdout.closed) && (!child.stderr || child.stderr.closed)) onClose(child.exitCode, child.signalCode);
  });
}

// An attempted start remains owned even if readiness or identity lookup fails.
export class RuntimeAttempt {
  pending = false;
  reference;
  start(start, identify) {
    this.pending = true;
    this.reference = undefined;
    start();
    this.reference = identify();
  }
  stop(stopExact) {
    if (!this.pending) return;
    if (!this.reference) throw new Error('attempted runtime start has no confirmed identity');
    stopExact(this.reference);
    this.pending = false;
  }
}
