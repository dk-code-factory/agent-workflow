#!/usr/bin/env python3
"""Compress a saved image with fixed gzip metadata, updating its inventory."""
import gzip
import json
from pathlib import Path
import shutil
import sys
from release_lib import git, sha256

def compress(directory, arch, epoch):
    path = Path(directory) / ('worker-linux-' + arch + '.json')
    report = json.loads(path.read_text())
    source = path.parent / report['archive']
    archive = source.with_suffix(source.suffix + '.gz')
    with source.open('rb') as stream, archive.open('wb') as output:
        with gzip.GzipFile(filename='', mode='wb', fileobj=output, mtime=epoch, compresslevel=1) as compressed:
            shutil.copyfileobj(stream, compressed)
    report['archive'] = archive.name
    report['archive_sha256'] = sha256(archive)
    path.write_text(json.dumps(report, indent=2) + '\n')
    source.unlink()


if __name__ == '__main__':
    compress('dist', sys.argv[1], int(git('show', '-s', '--format=%ct', 'HEAD')))
