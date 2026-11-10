import re
import unittest

from prfix_race import parse_tests, run_pattern, select_tests


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


if __name__ == '__main__':
    unittest.main()
