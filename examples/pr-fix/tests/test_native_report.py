"""Strict native transport retains the application's open-ended report fields."""
import copy
import json
from pathlib import Path
import sys
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "workflows" / "helpers"))
from prfix.app import decode_native_report


class NativeReportTests(unittest.TestCase):
    def test_open_fields_are_decoded_without_changing_the_original(self):
        operation = {"kind": "edit_pr", "payload": {"title": "Reviewed title"}}
        deliverables = {"custom": {"paths": ["code.py"], "passed": True, "label": "café 🍀"}}
        wire = {"complete": True, "operations": [json.dumps(operation)],
                "deliverables": json.dumps(deliverables), "audience": None,
                "commit_message": None}
        before = copy.deepcopy(wire)
        self.assertEqual(decode_native_report(wire), {
            "complete": True, "operations": [operation], "deliverables": deliverables})
        self.assertEqual(wire, before)

    def test_retained_object_reports_preserve_their_representation(self):
        for report in ({"complete": True}, {"operations": [{"kind": "reply"}],
                                               "deliverables": {"custom": "value"}}):
            with self.subTest(report=report):
                self.assertEqual(decode_native_report(report), report)

    def test_encoded_values_must_be_objects_with_unique_keys(self):
        for field, value in (("operations", ["[]"]), ("operations", ["null"]),
                             ("operations", ['{"kind":"reply","kind":"merge"}']),
                             ("deliverables", "false"), ("deliverables", "[]")):
            with self.subTest(field=field, value=value), self.assertRaises(ValueError):
                decode_native_report({field: value})
