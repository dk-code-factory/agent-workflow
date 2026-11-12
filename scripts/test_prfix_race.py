import re
import unittest
from contextlib import redirect_stdout
from io import StringIO
from types import SimpleNamespace
from unittest.mock import patch

from prfix_race import main, parse_tests, run_pattern, select_tests


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


if __name__ == '__main__':
    unittest.main()
