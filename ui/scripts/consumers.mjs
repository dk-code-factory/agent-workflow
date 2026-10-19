import { execFileSync } from 'node:child_process';
import { mkdirSync, copyFileSync, readFileSync, writeFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { resolve } from 'node:path';
const root = resolve(import.meta.dirname, '..');
const artifacts = resolve(root, '.consumer-artifacts'); mkdirSync(artifacts, { recursive: true });
const run = (cmd, args, cwd = root) => execFileSync(cmd, args, { cwd, stdio: 'inherit' });
run('npm', ['pack', '--pack-destination', artifacts]);
const fixture = resolve(root, 'consumers/react');
const lockPath = resolve(fixture, 'package-lock.json');
const originalLock = readFileSync(lockPath, 'utf8');
const lock = JSON.parse(originalLock);
// The package is rebuilt for each test. Only its tarball checksum changes;
// registry dependencies remain pinned by the checked-in consumer lockfile.
lock.packages['node_modules/@agent-workflow/ui'].integrity = 'sha512-' + createHash('sha512').update(readFileSync(resolve(artifacts, 'agent-workflow-ui-0.1.0.tgz'))).digest('base64');
writeFileSync(lockPath, JSON.stringify(lock, null, 2) + '\n');
try { run('npm', ['ci', '--ignore-scripts', '--no-audit', '--no-fund'], fixture); }
finally { writeFileSync(lockPath, originalLock); }
run(process.execPath, [resolve(root, 'node_modules/vite/bin/vite.js'), 'build', '--base=./', '--outDir', '../../public/packed/react', '--emptyOutDir'], fixture);
const packed = resolve(fixture, 'node_modules/@agent-workflow/ui/dist');
const standalone = resolve(root, 'public/packed/standalone');
mkdirSync(standalone, { recursive: true });
copyFileSync(resolve(root, 'consumers/standalone/index.html'), resolve(standalone, 'index.html'));
for (const name of ['standalone.js', 'style.css']) copyFileSync(resolve(packed, name), resolve(standalone, name));
// Verify declarations resolve from the packed package in a real TS consumer.
run(process.execPath, [resolve(root, 'node_modules/typescript/bin/tsc'), '--noEmit', '--jsx', 'react-jsx', '--esModuleInterop', '--skipLibCheck', '--moduleResolution', 'bundler', '--module', 'esnext', '--target', 'es2022', 'main.tsx'], fixture);
console.log('Packed React 18 and standalone consumer builds passed.');
