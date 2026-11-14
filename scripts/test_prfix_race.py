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
        names = parse_tests('TestZ\nExampleOutput\nFuzzInput\nTestA\nTestAA\nTestB\nTestC\nTestD\nTestE\nok  package 0.1s\n')
        for shards in [1, 4, 8]:
            with self.subTest(shards=shards):
                groups = [select_tests(names, shard, shards) for shard in range(shards)]
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
    portable_command = '-B -m unittest discover -s examples/pr-fix/tests -p test_*.py -v'
    shard_command = 'scripts/prfix_race.py --all --shards 4'

    def run_target(self, packages, discovery_exit=0, test_exit=0,
                   portable_exit=0, shard_exit=0, target='test'):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            go = root / 'go'
            go.write_text('''#!/bin/sh
printf '%s\\n' "$*" >> "$GO_CALLS"
printf 'go %s\\n' "$*" >> "$RUN_CALLS"
case "$1" in
    list) printf '%s' "$DISCOVERED_PACKAGES"; exit "$DISCOVERY_EXIT" ;;
    test) exit "$TEST_EXIT" ;;
esac
exit 1
''')
            go.chmod(0o755)
            python = root / 'python3'
            python.write_text('''#!/bin/sh
printf '%s\\n' "$*" >> "$PYTHON_CALLS"
printf 'python3 %s\\n' "$*" >> "$RUN_CALLS"
case "$1" in
    -B) exit "$PORTABLE_EXIT" ;;
    scripts/prfix_race.py) exit "$SHARD_EXIT" ;;
esac
exit 1
''')
            python.chmod(0o755)
            go_calls = root / 'go-calls'
            python_calls = root / 'python-calls'
            run_calls = root / 'run-calls'
            environment = dict(os.environ, PATH=f'{root}{os.pathsep}{os.environ["PATH"]}',
                               GO_CALLS=str(go_calls), PYTHON_CALLS=str(python_calls),
                               RUN_CALLS=str(run_calls), PORTABLE_EXIT=str(portable_exit),
                               SHARD_EXIT=str(shard_exit),
                               DISCOVERED_PACKAGES=packages, DISCOVERY_EXIT=str(discovery_exit),
                               TEST_EXIT=str(test_exit))
            result = subprocess.run(['make', '--no-print-directory', '-s', '-f', str(ROOT / 'Makefile'), target],
                                    cwd=root, env=environment, capture_output=True, text=True, timeout=10)
            return (result.returncode,
                    go_calls.read_text().splitlines() if go_calls.exists() else [],
                    python_calls.read_text().splitlines() if python_calls.exists() else [],
                    run_calls.read_text().splitlines() if run_calls.exists() else [])

    def test_package_discovery_failure_stops_before_go_tests_and_shards(self):
        for output in ['', 'example/root\nexample/root/pkg/prfix\n']:
            with self.subTest(output=output):
                status, calls, python, sequence = self.run_target(output, discovery_exit=1)
                self.assertNotEqual(status, 0)
                self.assertEqual(calls, ['list ./...'])
                self.assertEqual(python, [self.portable_command])
                self.assertEqual(sequence, ['python3 ' + self.portable_command, 'go list ./...'])

    def test_empty_package_selection_stops_before_go_tests_and_shards(self):
        for output in ['', 'example/root/pkg/prfix\n']:
            with self.subTest(output=output):
                status, calls, python, sequence = self.run_target(output)
                self.assertNotEqual(status, 0)
                self.assertEqual(calls, ['list ./...'])
                self.assertEqual(python, [self.portable_command])
                self.assertEqual(sequence, ['python3 ' + self.portable_command, 'go list ./...'])

    def test_success_runs_portable_then_non_prfix_packages_then_all_shards(self):
        status, calls, python, sequence = self.run_target('example/root\nexample/root/pkg/prfix\nexample/root/pkg/other\n')
        self.assertEqual(status, 0)
        self.assertEqual(calls, ['list ./...', 'test -race -timeout 60m example/root example/root/pkg/other'])
        self.assertEqual(python, [self.portable_command, self.shard_command])
        self.assertEqual(sequence, [
            'python3 ' + self.portable_command, 'go list ./...',
            'go test -race -timeout 60m example/root example/root/pkg/other',
            'python3 ' + self.shard_command,
        ])

    def test_non_prfix_failure_stops_before_shards(self):
        status, calls, python, sequence = self.run_target('example/root\n', test_exit=1)
        self.assertNotEqual(status, 0)
        self.assertEqual(calls, ['list ./...', 'test -race -timeout 60m example/root'])
        self.assertEqual(python, [self.portable_command])
        self.assertEqual(sequence, [
            'python3 ' + self.portable_command, 'go list ./...',
            'go test -race -timeout 60m example/root',
        ])

    def test_core_runs_portable_and_non_prfix_packages_without_shards(self):
        status, calls, python, sequence = self.run_target(
            'example/root\nexample/root/pkg/prfix\nexample/root/pkg/other\n', target='test-core')
        self.assertEqual(status, 0)
        self.assertEqual(calls, ['list ./...', 'test -race -timeout 60m example/root example/root/pkg/other'])
        self.assertEqual(python, [self.portable_command])
        self.assertEqual(sequence, [
            'python3 ' + self.portable_command, 'go list ./...',
            'go test -race -timeout 60m example/root example/root/pkg/other',
        ])

    def test_portable_failure_stops_before_package_discovery_and_go_suites(self):
        status, calls, python, sequence = self.run_target('example/root\n', portable_exit=1)
        self.assertNotEqual(status, 0)
        self.assertEqual(calls, [])
        self.assertEqual(python, [self.portable_command])
        self.assertEqual(sequence, ['python3 ' + self.portable_command])

    def test_shard_failure_is_preserved_after_successful_prerequisites(self):
        status, calls, python, sequence = self.run_target('example/root\n', shard_exit=1)
        self.assertNotEqual(status, 0)
        self.assertEqual(calls, ['list ./...', 'test -race -timeout 60m example/root'])
        self.assertEqual(python, [self.portable_command, self.shard_command])
        self.assertEqual(sequence, [
            'python3 ' + self.portable_command, 'go list ./...',
            'go test -race -timeout 60m example/root', 'python3 ' + self.shard_command,
        ])


if __name__ == '__main__':
    unittest.main()
