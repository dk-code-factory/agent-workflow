"""Local preflight tests for manual acceptance; never starts model conversations."""
import base64
from datetime import datetime, timezone
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
import time
import unittest

DIRECTORY = Path(__file__).resolve().parents[1] / "conformance/plugin"
spec = importlib.util.spec_from_file_location("plugin_acceptance", DIRECTORY / "acceptance.py")
acceptance = importlib.util.module_from_spec(spec)
sys.path.insert(0, str(DIRECTORY))
try:
    spec.loader.exec_module(acceptance)
finally:
    sys.path.pop(0)


class AcceptancePreflightTests(unittest.TestCase):
    def test_native_plugin_namespace_and_identity(self):
        expected = {name: {"name": "agent-workflow:" + name, "pluginId": "agent-workflow@agent-workflow", "enabled": True}
                    for name in ("setup", "build-workflow", "run-workflow", "report-bug", "convert-agent")}
        listing = {"data": [{"skills": [*expected.values(), {"name": "build-workflow", "pluginId": None, "enabled": True}]}]}
        self.assertEqual(acceptance.discovered_skills(listing), expected)

    def test_isolated_native_credential_retains_schema_and_has_no_refresh_authority(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "auth.json"
            payload = base64.urlsafe_b64encode(json.dumps({"exp": time.time() + 7200}).encode()).decode().rstrip("=")
            original = {"auth_mode": "chatgpt", "tokens": {"access_token": "header." + payload + ".signature", "refresh_token": "private-fixture", "account_id": "fixture", "id_token": "fixture"},
                        "last_refresh": datetime.now(timezone.utc).isoformat()}
            path.write_text(json.dumps(original))
            isolated = acceptance.isolated_credential(path)
            self.assertEqual(isolated["tokens"]["refresh_token"], "")
            self.assertEqual(json.loads(path.read_text()), original)
            original["last_refresh"] = "2020-01-01T00:00:00Z"
            path.write_text(json.dumps(original))
            with self.assertRaisesRegex(ValueError, "refresh"):
                acceptance.isolated_credential(path)

    def test_expired_credential_is_rejected_before_any_live_call(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "auth.json"
            payload = base64.urlsafe_b64encode(json.dumps({"exp": 1}).encode()).decode().rstrip("=")
            path.write_text(json.dumps({"auth_mode": "chatgpt", "tokens": {"access_token": "h." + payload + ".s"}, "last_refresh": datetime.now(timezone.utc).isoformat()}))
            with self.assertRaisesRegex(ValueError, "refresh"):
                acceptance.isolated_credential(path)


if __name__ == "__main__":
    unittest.main()
