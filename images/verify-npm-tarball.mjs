import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';

const [archive, integrity] = process.argv.slice(2);
if (!archive || !/^sha512-[A-Za-z0-9+/]{86}==$/.test(integrity ?? '')) {
  throw new Error('Expected an npm archive and a SHA-512 integrity pin');
}
const actual = 'sha512-' + createHash('sha512').update(readFileSync(archive)).digest('base64');
if (actual !== integrity) {
  throw new Error('npm archive integrity mismatch');
}
