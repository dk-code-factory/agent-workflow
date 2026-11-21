#!/usr/bin/env python3
"""Partition a complete Go package race suite across independent CI jobs."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import json
import math
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


def select_tests(names, shard, shards, durations=None):
    if shards < 1 or shard < 0 or shard >= shards or shards > len(names):
        raise ValueError('Every shard must have a valid index and at least one test')
    if not durations:
        return names[shard::shards]
    groups = [[] for _ in range(shards)]
    totals = [0.0] * shards
    # Longest-first scheduling limits the slowest job while preserving complete
    # discovery. New tests are included with a small positive fallback weight.
    for name in sorted(names, key=lambda name: (-durations.get(name, 1.0), name)):
        selected = min(range(shards), key=lambda index: (totals[index], index))
        groups[selected].append(name)
        totals[selected] += durations.get(name, 1.0)
    return sorted(groups[shard])


def load_durations(path, package):
    data = json.loads(path.read_text())
    packages = data['packages']
    if not isinstance(packages, dict) or package not in packages:
        raise ValueError('Race timing inventory must contain the selected package')
    durations = packages[package]
    if not isinstance(durations, dict) or any(
            not isinstance(name, str) or type(seconds) not in (int, float)
            or not math.isfinite(seconds) or seconds <= 0
            for name, seconds in durations.items()):
        raise ValueError('Race timings must be finite positive seconds by test name')
    return durations


def run_pattern(names):
    return '^(' + '|'.join(re.escape(name) for name in names) + ')$'


def run_selected(names, package=PACKAGE):
    timeout = '90m' if package == './pkg/workflows' else '60m'
    return subprocess.run(['go', 'test', '-race', '-timeout', timeout, package,
                           '-run', run_pattern(names), '-count=1'], cwd=ROOT).returncode


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    selection = parser.add_mutually_exclusive_group(required=True)
    selection.add_argument('--shard', type=int)
    selection.add_argument('--all', action='store_true', help='Run all shards concurrently')
    parser.add_argument('--package', default=PACKAGE, choices=[PACKAGE, './pkg/workflows'])
    parser.add_argument('--shards', type=int, required=True)
    parser.add_argument('--list', action='store_true', help='Print selection without running it')
    args = parser.parse_args()
    discovered = subprocess.run(
        ['go', 'test', args.package, '-list', '^(Test|Example|Fuzz)'],
        cwd=ROOT, text=True, stdout=subprocess.PIPE, check=True)
    names = parse_tests(discovered.stdout)
    durations = load_durations(ROOT / 'scripts' / 'race-test-durations.json', args.package)
    if args.all:
        groups = [select_tests(names, shard, args.shards, durations) for shard in range(args.shards)]
        if not groups:
            parser.error('At least one shard is required')
        if args.list:
            print(json.dumps({'shards': groups, 'total': len(names)}), flush=True)
            return 0
        with ThreadPoolExecutor(max_workers=args.shards) as executor:
            results = list(executor.map(lambda group: run_selected(group, args.package), groups))
        return next((result for result in results if result != 0), 0)
    selected = select_tests(names, args.shard, args.shards, durations)
    print(json.dumps({'shard': args.shard, 'shards': args.shards,
                      'total': len(names), 'tests': selected}), flush=True)
    if args.list:
        return 0
    return run_selected(selected, args.package)


if __name__ == '__main__':
    raise SystemExit(main())
