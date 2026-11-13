from concurrent.futures import ThreadPoolExecutor
import json
from pathlib import Path
import sqlite3
import sys
import tempfile
import threading
import time
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "workflows" / "helpers"))
from prfix import core


PR = "https://github.com/owner/repo/pull/17"
AUTH = {"request_id": "user-continue", "instruction": "Allow one more review cycle.", "source": "chat:17"}


class CoreTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="portable-pr-core-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    def test_json_bounds_count_bytes_and_reject_ambiguous_inputs(self):
        for raw in ('{"id":1,"id":2}', '{"n":NaN}', '{"n":Infinity}', '{} trailing'):
            with self.subTest(raw=raw), self.assertRaises(ValueError):
                core.bounded_json(raw)
        raw = json.dumps({"value": "é" * 10}, ensure_ascii=False)
        self.assertGreater(len(raw.encode()), len(raw))
        with self.assertRaisesRegex(ValueError, "byte limit"):
            core.bounded_json(raw, len(raw))
        self.assertEqual(core.bounded_json(raw, len(raw.encode())), {"value": "é" * 10})
        number = 9007199254740993
        self.assertEqual(core.bounded_json('{"id":' + str(number) + '}')["id"], number)

    def test_owned_directories_preserve_unrelated_files_and_reject_symlinks(self):
        unrelated = self.root / core.STATE
        unrelated.mkdir()
        (unrelated / "keep.txt").write_text("unrelated")
        with self.assertRaises(ValueError):
            core.owned_directory(self.root, core.STATE, create=True)
        self.assertEqual((unrelated / "keep.txt").read_text(), "unrelated")
        with tempfile.TemporaryDirectory(prefix="portable-pr-owned-") as outside:
            link = self.root / "link"
            link.symlink_to(outside, target_is_directory=True)
            with self.assertRaises(ValueError):
                core.owned_directory(self.root, "link", create=True)

    def test_artifacts_are_immutable_hash_addressed_and_scoped(self):
        core.owned_directory(self.root, core.STATE, create=True)
        value = {"original": [1, "literal ${{ value }}"]}
        reference = core.artifact(self.root, value)
        self.assertEqual(core.artifact(self.root, value), reference)
        self.assertEqual(core.read_artifact(self.root, reference), value)
        path = self.root / reference
        path.write_text('{"changed":true}')
        with self.assertRaisesRegex(ValueError, "identity"):
            core.read_artifact(self.root, reference)
        with self.assertRaisesRegex(ValueError, "conflict"):
            core.artifact(self.root, value)
        for invalid in ("../outside.json", str(path), core.STATE + "/ledger.sqlite"):
            with self.subTest(invalid=invalid), self.assertRaises(ValueError):
                core.read_artifact(self.root, invalid)

    def test_artifact_parent_cannot_escape_owned_storage_through_symlink(self):
        core.owned_directory(self.root, core.STATE, create=True)
        with tempfile.TemporaryDirectory(prefix="portable-pr-artifact-") as outside:
            parent = self.root / core.STATE / "artifacts"
            parent.symlink_to(outside, target_is_directory=True)
            value = {"source": "original"}
            with self.assertRaises(ValueError):
                core.artifact(self.root, value)
            external = Path(outside) / (core.digest(value) + ".json")
            external.write_bytes(core.canonical(value))
            with self.assertRaises(ValueError):
                core.read_artifact(self.root, core.STATE + "/artifacts/" + external.name)

    def test_reservation_response_loss_replays_after_restart_without_consuming_twice(self):
        with core.Ledger(self.root, "retained", PR) as ledger:
            self.assertEqual(ledger.reserve("reviews", "original", 1), 1)
        with core.Ledger(self.root, "retained", PR) as ledger:
            self.assertEqual(ledger.reserve("reviews", "original", 1), 1)
            with self.assertRaisesRegex(ValueError, "exhausted"):
                ledger.reserve("reviews", "replacement", 1)
            with self.assertRaises(ValueError):
                ledger.reserve("reviews", "original", 2)
            with self.assertRaises(ValueError):
                ledger.reserve("reviews", "new-limit", 2)

    def test_concurrent_replayed_reservation_has_one_transactional_effect(self):
        with core.Ledger(self.root, "concurrent", PR):
            pass
        ready = threading.Barrier(6, timeout=10)
        def reserve():
            with core.Ledger(self.root, "concurrent", PR) as ledger:
                ready.wait()
                return ledger.reserve("reviews", "same-operation", 1)
        with ThreadPoolExecutor(max_workers=6) as pool:
            results = list(pool.map(lambda _: reserve(), range(6)))
        self.assertEqual(results, [1] * 6)
        with core.Ledger(self.root, "concurrent", PR) as ledger:
            rows = ledger.db.execute("SELECT operation,amount FROM reservations").fetchall()
            self.assertEqual([(row[0], row[1]) for row in rows], [("same-operation", 1)])

    def test_concurrent_distinct_reservations_cannot_exceed_allowance(self):
        with core.Ledger(self.root, "concurrent", PR):
            pass
        ready = threading.Barrier(6, timeout=10)
        def reserve(number):
            with core.Ledger(self.root, "concurrent", PR) as ledger:
                ready.wait()
                try:
                    ledger.reserve("reviews", "operation-" + str(number), 2)
                    return True
                except ValueError as error:
                    if "exhausted" not in str(error):
                        raise
                    return False
        with ThreadPoolExecutor(max_workers=6) as pool:
            results = list(pool.map(reserve, range(6)))
        self.assertEqual(sum(results), 2)

    def test_explicit_continuation_is_deduplicated_and_retains_consumption(self):
        with core.Ledger(self.root, "retained", PR) as ledger:
            ledger.reserve("reviews", "first", 1)
            first = ledger.extend(AUTH["request_id"], "reviews", 1, AUTH)
        with core.Ledger(self.root, "retained", PR) as ledger:
            self.assertEqual(ledger.extend(AUTH["request_id"], "reviews", 1, AUTH), first)
            self.assertEqual(ledger.reserve("reviews", "second", 1), 2)
            with self.assertRaisesRegex(ValueError, "exhausted"):
                ledger.reserve("reviews", "third", 1)
            with self.assertRaisesRegex(ValueError, "conflict"):
                ledger.extend(AUTH["request_id"], "reviews", 2, AUTH)
            changed = dict(AUTH, instruction="Changed authorization text")
            with self.assertRaisesRegex(ValueError, "conflict"):
                ledger.extend(AUTH["request_id"], "reviews", 1, changed)

    def test_absolute_deadline_reopens_without_reset_or_duplicate_record(self):
        with core.Ledger(self.root, "retained", PR) as ledger:
            deadline = ledger.deadline("ci", 30)
            self.assertGreater(deadline, time.time())
        with core.Ledger(self.root, "retained", PR) as ledger:
            self.assertEqual(ledger.deadline("ci", 300), deadline)
            self.assertEqual(ledger.db.execute("SELECT count(*) FROM deadlines").fetchone()[0], 1)

    def test_read_only_hooks_and_group_binding_cannot_mutate_state(self):
        with core.Ledger(self.root, "retained", PR):
            pass
        with core.Ledger(self.root, "retained", PR, read_only=True) as ledger:
            with self.assertRaises(ValueError):
                ledger.reserve("reviews", "write", 1)
            with self.assertRaises(ValueError):
                ledger.deadline("ci", 30)
            with self.assertRaises(sqlite3.OperationalError):
                ledger.db.execute("DELETE FROM groups")
        with self.assertRaisesRegex(ValueError, "another PR"):
            core.Ledger(self.root, "retained", "https://github.com/owner/repo/pull/18")


if __name__ == "__main__":
    unittest.main()
