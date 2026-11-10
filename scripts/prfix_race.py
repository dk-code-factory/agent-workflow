#!/usr/bin/env python3
"""Partition the complete PR-fix race suite across independent CI jobs."""
import argparse
import json
from pathlib import Path
import re
import subprocess

ROOT = Path(__file__).resolve().parents[1]
PACKAGE = './pkg/prfix'


def parse_tests(output):
    names = []
    for line in output.splitlines():
        if re.fullmatch(r'(?:Test|Example|Fuzz)\w*', line):
            names.append(line)
        elif line.startswith(('ok ', '? ')):
            continue
        elif line.strip():
            raise ValueError(f'Unexpected Go test discovery output: {line}')
    if not names or len(names) != len(set(names)):
        raise ValueError('Go test discovery must return nonempty unique names')
    return sorted(names)


def select_tests(names, shard, shards):
    if shards < 1 or shard < 0 or shard >= shards or shards > len(names):
        raise ValueError('Every shard must have a valid index and at least one test')
    return names[shard::shards]


def run_pattern(names):
    return '^(' + '|'.join(re.escape(name) for name in names) + ')$'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--shard', type=int, required=True)
    parser.add_argument('--shards', type=int, required=True)
    parser.add_argument('--list', action='store_true', help='Print selection without running it')
    args = parser.parse_args()
    discovered = subprocess.run(
        ['go', 'test', PACKAGE, '-list', '^(Test|Example|Fuzz)'],
        cwd=ROOT, text=True, stdout=subprocess.PIPE, check=True)
    names = parse_tests(discovered.stdout)
    selected = select_tests(names, args.shard, args.shards)
    print(json.dumps({'shard': args.shard, 'shards': args.shards,
                      'total': len(names), 'tests': selected}), flush=True)
    if args.list:
        return 0
    return subprocess.run(['go', 'test', '-race', '-timeout', '60m', PACKAGE,
                           '-run', run_pattern(selected), '-count=1'], cwd=ROOT).returncode


if __name__ == '__main__':
    raise SystemExit(main())
