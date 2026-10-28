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
