#!/usr/bin/env python3
"""Compress a saved image with fixed gzip metadata, updating its inventory."""
import gzip
import json
from pathlib import Path
import shutil
import sys
from release_lib import git, sha256

path = Path('dist') / ('worker-linux-' + sys.argv[1] + '.json')
report = json.loads(path.read_text())
source = path.parent / report['archive']
archive = source.with_suffix(source.suffix + '.gz')
with source.open('rb') as stream, archive.open('wb') as output:
    with gzip.GzipFile(filename='', mode='wb', fileobj=output, mtime=int(git('show', '-s', '--format=%ct', 'HEAD'))) as compressed:
        shutil.copyfileobj(stream, compressed)
report['archive'] = archive.name
report['archive_sha256'] = sha256(archive)
path.write_text(json.dumps(report, indent=2) + '\n')
source.unlink()
