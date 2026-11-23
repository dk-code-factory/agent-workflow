"""Deterministic local workflows for the packaged authoring/recovery contracts."""
import importlib.util
import json
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import unittest
from test_plugin_acceptance import acceptance

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("plugin_subjects", ROOT / "conformance/plugin/subjects.py")
subjects = importlib.util.module_from_spec(spec)
spec.loader.exec_module(subjects)


class WorkflowTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.build = tempfile.TemporaryDirectory(prefix="plugin-workflow-tests-")
        cls.engine = Path(cls.build.name) / "agent-workflow"
        subprocess.run(["go", "build", "-o", str(cls.engine), "./cmd/agent-workflow"], cwd=ROOT, check=True, capture_output=True, timeout=120)

    @classmethod
    def tearDownClass(cls):
        cls.build.cleanup()

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="plugin-workspace-")
        self.addCleanup(self.temporary.cleanup)
        self.workspace = Path(self.temporary.name)
        (self.workspace / "workflows").mkdir()
        self.controller = None
        self.addCleanup(self.stop_controller)

    def cli(self, verb, *args, okay=True):
        result = subprocess.run([str(self.engine), verb, "--workspace", str(self.workspace), "--parallel", "2", *args], capture_output=True, text=True, timeout=20)
        if okay:
            self.assertEqual(result.returncode, 0, result.stderr)
        return result

    def write(self, text):
        (self.workspace / "workflows/main.yml").write_text(text)

    def start_controller(self):
        self.controller = subprocess.Popen([str(self.engine), "controller", "--workspace", str(self.workspace), "--parallel", "2"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        address_file = self.workspace / ".agent-workflow/controller.address"
        for _ in range(100):
            if self.controller.poll() is not None:
                self.fail("owned fixture controller failed to start")
            try:
                address = address_file.read_text().strip()
                with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
                    connection.settimeout(0.05)
                    connection.connect(address)
                return
            except (OSError, ValueError):
                time.sleep(0.02)
        self.fail("owned controller socket did not appear")

    def test_controller_readiness_uses_relocated_socket(self):
        self.workspace = self.workspace / ("long-workspace-" * 8)
        self.workspace.mkdir()
        self.start_controller()
        address = (self.workspace / ".agent-workflow/controller.address").read_text().strip()
        self.assertNotEqual(address, str(self.workspace / ".agent-workflow/controller.sock"))
        self.assertTrue(Path(address).is_socket())

    def stop_controller(self):
        if self.controller and self.controller.poll() is None:
            self.controller.terminate()
            try:
                self.controller.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.controller.kill()
                self.controller.wait()

    def wait_state(self, run_id, desired):
        for _ in range(200):
            result = json.loads(self.cli("status", run_id).stdout)
            if result["status"] in desired:
                return result
            time.sleep(0.02)
        self.fail("run did not reach " + repr(desired))

    def test_valid_invalid_and_bounded_failure(self):
        self.write("jobs: {bad: {needs: missing, steps: [{argv: [python3, -V]}]}}\n")
        self.assertNotEqual(self.cli("validate", "workflows/main.yml", okay=False).returncode, 0)
        (self.workspace / "count.py").write_text("from pathlib import Path\np=Path('count')\np.write_text(str(int(p.read_text())+1 if p.exists() else 1))\nraise SystemExit(1)\n")
        self.write("""jobs:
  repair:
    repeat: {max-attempts: 2, until: '${{ steps.test.outputs.exit_code == 0 }}'}
    steps:
      - id: test
        argv: [python3, count.py]
        timeout: 10s
        continue-on-error: true
""")
        self.cli("validate", "workflows/main.yml")
        result = self.cli("run", "--foreground", "workflows/main.yml", okay=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.workspace / "count").read_text(), "2")
        self.assertEqual(json.loads(result.stdout)["status"], "failed")

    def gate_definition(self):
        subjects.seed(self.workspace, "plugin-gated-report-recovery")
        self.write("""jobs:
  report:
    steps:
      - id: process
        argv: [python3, fixture/process.py]
        timeout: 10s
      - id: approval
        uses: input/wait
        with:
          prompt: Approve the prepared report?
          evidence-id: '${{ steps.process.outputs.stdout }}'
      - id: render
        if: '${{ steps.approval.outputs.answer == "approve" }}'
        argv: [python3, fixture/render.py]
        timeout: 10s
""")
        self.cli("validate", "workflows/main.yml")

    def test_gate_recovery_preserves_run_evidence_and_processing(self):
        self.gate_definition()
        self.start_controller()
        admitted = json.loads(self.cli("run", "--detach", "workflows/main.yml").stdout)
        run_id = admitted["id"]
        self.wait_state(run_id, {"waiting"})
        gates = json.loads(self.cli("inputs", run_id).stdout)
        self.assertEqual(len(gates), 1)
        gate = gates[0]
        self.stop_controller()
        self.start_controller()
        self.assertEqual(json.loads(self.cli("inputs", run_id).stdout)[0]["gate_id"], gate["gate_id"])
        answer = self.workspace / "answer.json"
        answer.write_text('"approve"')
        flags = ["--gate", gate["gate_id"], "--answer-id", "delivery-1", "--answer-file", str(answer)]
        stale = self.cli("answer", *flags, "--evidence-id", "stale", run_id, okay=False)
        self.assertNotEqual(stale.returncode, 0)
        self.cli("answer", *flags, "--evidence-id", gate["evidence_id"], run_id)
        done = self.wait_state(run_id, {"succeeded", "failed", "needs_attention"})
        self.assertEqual(done["status"], "succeeded", done)
        self.assertEqual(done["id"], run_id)
        self.assertEqual((self.workspace / "fixture/processed.count").read_text(), "1")
        self.assertEqual((self.workspace / "fixture/report.txt").read_text(), "Total: 5\n")
        self.assertEqual(json.loads(self.cli("inputs", run_id).stdout)[0]["state"], "consumed")

    def test_declined_gate_does_not_render_report(self):
        self.gate_definition()
        self.start_controller()
        run_id = json.loads(self.cli("run", "--detach", "workflows/main.yml").stdout)["id"]
        self.wait_state(run_id, {"waiting"})
        gate = json.loads(self.cli("inputs", run_id).stdout)[0]
        answer = self.workspace / "answer.json"
        answer.write_text('"decline"')
        self.cli("answer", "--gate", gate["gate_id"], "--evidence-id", gate["evidence_id"], "--answer-id", "declined", "--answer-file", str(answer), run_id)
        self.assertEqual(self.wait_state(run_id, {"succeeded", "failed"})["status"], "succeeded")
        self.assertFalse((self.workspace / "fixture/report.txt").exists())

    def test_invalid_answer_is_classified_by_actual_yaml_condition(self):
        self.gate_definition()
        path = self.workspace / "workflows/main.yml"
        source = path.read_text().replace("      - id: render\n", """      - id: reject
        if: '${{ steps.approval.outputs.answer != "approve" and steps.approval.outputs.answer != "decline" }}'
        argv: [python3, -c, 'raise SystemExit(1)']
        timeout: 10s
      - id: render
""")
        path.write_text(source)
        self.cli("validate", "workflows/main.yml")
        self.start_controller()
        run_id = json.loads(self.cli("run", "--detach", "workflows/main.yml").stdout)["id"]
        self.wait_state(run_id, {"waiting"})
        gate = json.loads(self.cli("inputs", run_id).stdout)[0]
        answer = self.workspace / "invalid.json"
        answer.write_text('"unexpected"')
        self.cli("answer", "--gate", gate["gate_id"], "--evidence-id", gate["evidence_id"], "--answer-id", "invalid", "--answer-file", str(answer), run_id)
        done = self.wait_state(run_id, {"succeeded", "failed"})
        self.assertEqual(done["status"], "failed")
        self.assertEqual(done["jobs"]["report"]["steps"]["reject"]["status"], "failed")
        self.assertFalse((self.workspace / "fixture/report.txt").exists())

    def test_csv_validation_rejects_invalid_input_before_gate(self):
        self.gate_definition()
        (self.workspace / "fixture/data.csv").write_text("name,amount\nalpha,-1\n")
        result = self.cli("run", "--foreground", "workflows/main.yml", okay=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.workspace / "fixture/prepared.json").exists())

    def test_manual_harness_recovery_uses_same_admitted_run(self):
        import os
        self.gate_definition()
        self.controller = acceptance.owned_controller(self.engine, self.workspace, dict(os.environ), subprocess.DEVNULL)
        run_id = json.loads(self.cli("run", "--detach", "workflows/main.yml").stdout)["id"]
        self.wait_state(run_id, {"waiting"})
        evidence = self.workspace / "evidence"
        evidence.mkdir()
        try:
            acceptance.recover_gate(self.engine, self.workspace, dict(os.environ), evidence, run_id, time.monotonic() + 20)
        finally:
            self.cli("stop", okay=False)
        self.assertEqual(json.loads((evidence / "recovery.json").read_text())["processing_count"], 1)


if __name__ == "__main__":
    unittest.main()
