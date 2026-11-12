import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest
from contextlib import redirect_stdout
from io import StringIO
from types import SimpleNamespace
from unittest.mock import patch

from prfix_race import main, parse_tests, run_pattern, select_tests

ROOT = Path(__file__).resolve().parents[1]


class PRFixRaceTests(unittest.TestCase):
    def test_every_discovered_case_runs_in_exactly_one_shard(self):
        names = parse_tests('TestZ\nExampleOutput\nFuzzInput\nTestA\nTestAA\nok  package 0.1s\n')
        groups = [select_tests(names, shard, 4) for shard in range(4)]
        self.assertEqual(sorted(name for group in groups for name in group), names)
        for group in groups:
            pattern = re.compile(run_pattern(group))
            self.assertEqual([name for name in names if pattern.search(name)], group)
            self.assertIsNone(pattern.search('TestAExtra'))

    def test_discovery_fails_closed(self):
        for output in ['', 'ok  package 0.1s\n', 'TestA\nTestA\n', 'TestA\nFAIL\n']:
            with self.subTest(output=output), self.assertRaises(ValueError):
                parse_tests(output)

    def test_invalid_or_empty_shards_are_rejected(self):
        for shard, shards in [(-1, 1), (1, 1), (0, 0), (0, 2)]:
            with self.subTest(shard=shard, shards=shards), self.assertRaises(ValueError):
                select_tests(['TestA'], shard, shards)

    def test_all_shards_finish_and_preserve_failure(self):
        def run(command, **kwargs):
            if '-list' in command:
                return SimpleNamespace(stdout='TestA\nTestB\nTestC\nTestD\n')
            pattern = command[command.index('-run') + 1]
            return SimpleNamespace(returncode=1 if 'TestB' in pattern else 0)

        with patch('sys.argv', ['prfix_race.py', '--all', '--shards', '4']), \
                patch('prfix_race.subprocess.run', side_effect=run) as process, \
                redirect_stdout(StringIO()):
            self.assertEqual(main(), 1)
        turns = [call.args[0] for call in process.call_args_list if '-run' in call.args[0]]
        self.assertEqual(len(turns), 4)
        patterns = [re.compile(command[command.index('-run') + 1]) for command in turns]
        for name in ['TestA', 'TestB', 'TestC', 'TestD']:
            self.assertEqual(sum(bool(pattern.search(name)) for pattern in patterns), 1)
        self.assertTrue(all('-race' in command and '-count=1' in command for command in turns))


class MakeTestTargetTests(unittest.TestCase):
    def run_target(self, packages, discovery_exit=0, test_exit=0):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            go = root / 'go'
            go.write_text('''#!/bin/sh
printf '%s\\n' "$*" >> "$GO_CALLS"
case "$1" in
    list) printf '%s' "$DISCOVERED_PACKAGES"; exit "$DISCOVERY_EXIT" ;;
    test) exit "$TEST_EXIT" ;;
esac
exit 1
''')
            go.chmod(0o755)
            python = root / 'python3'
            python.write_text('''#!/bin/sh
printf '%s\\n' "$*" >> "$SHARD_CALLS"
''')
            python.chmod(0o755)
            go_calls = root / 'go-calls'
            shard_calls = root / 'shard-calls'
            environment = dict(os.environ, PATH=f'{root}{os.pathsep}{os.environ["PATH"]}',
                               GO_CALLS=str(go_calls), SHARD_CALLS=str(shard_calls),
                               DISCOVERED_PACKAGES=packages, DISCOVERY_EXIT=str(discovery_exit),
                               TEST_EXIT=str(test_exit))
            result = subprocess.run(['make', '--no-print-directory', '-s', '-f', str(ROOT / 'Makefile'), 'test'],
                                    cwd=root, env=environment, capture_output=True, text=True, timeout=10)
            return (result.returncode,
                    go_calls.read_text().splitlines() if go_calls.exists() else [],
                    shard_calls.read_text().splitlines() if shard_calls.exists() else [])

    def test_package_discovery_failure_stops_before_running_any_suite(self):
        for output in ['', 'example/root\nexample/root/pkg/prfix\n']:
            with self.subTest(output=output):
                status, calls, shards = self.run_target(output, discovery_exit=1)
                self.assertNotEqual(status, 0)
                self.assertEqual(calls, ['list ./...'])
                self.assertEqual(shards, [])

    def test_empty_package_selection_stops_before_running_any_suite(self):
        for output in ['', 'example/root/pkg/prfix\n']:
            with self.subTest(output=output):
                status, calls, shards = self.run_target(output)
                self.assertNotEqual(status, 0)
                self.assertEqual(calls, ['list ./...'])
                self.assertEqual(shards, [])

    def test_success_runs_non_prfix_packages_then_all_shards(self):
        status, calls, shards = self.run_target('example/root\nexample/root/pkg/prfix\nexample/root/pkg/other\n')
        self.assertEqual(status, 0)
        self.assertEqual(calls, ['list ./...', 'test -race -timeout 60m example/root example/root/pkg/other'])
        self.assertEqual(shards, ['scripts/prfix_race.py --all --shards 4'])

    def test_non_prfix_failure_stops_before_shards(self):
        status, calls, shards = self.run_target('example/root\n', test_exit=1)
        self.assertNotEqual(status, 0)
        self.assertEqual(calls, ['list ./...', 'test -race -timeout 60m example/root'])
        self.assertEqual(shards, [])


if __name__ == '__main__':
    unittest.main()
