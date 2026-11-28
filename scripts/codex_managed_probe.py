#!/usr/bin/env python3
"""Bounded deterministic proof against one already built managed app-server.

This is a local fake Responses API, never live-provider conformance. It keeps
profile enablement, recovery certification and production process admission false.
"""
import argparse
import hashlib
import http.server
import json
import os
from pathlib import Path
import queue
import resource
import select
import shutil
import signal
import socket
import subprocess
import threading
import time


def sha_file(path):
    digest = hashlib.sha256()
    with open(path, "rb") as stream:
        for part in iter(lambda: stream.read(1 << 20), b""):
            digest.update(part)
    return digest.hexdigest()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--build", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--case", choices=["batch", "next-turn", "journal", "recover-sealed", "recover-partial", "recover-accepted", "recover-marker", "recover-stale", "stream-cut", "journal-write-failure", "invalid-permit", "duplicate-permit", "cancel", "denied-tool", "empty-tools", "warm-policy", "startup", "startup-control", "cold-policy"], default="batch")
    parser.add_argument("--permit-shape", choices=["epoch", "legacy-v12"], default="epoch")
    parser.add_argument("--denied-tool", default="exec_command")
    args = parser.parse_args()
    args.output.mkdir(mode=0o700, parents=True, exist_ok=False)
    (args.output / "harness.py").write_bytes(Path(__file__).read_bytes())
    receipt = {"scope": "local fake gateway; no production admission", "case": args.case, "harness_sha256": sha_file(__file__), "passed": False}
    frames, requests, problems = [], [], []
    events = queue.Queue()
    stream_started, finish_stream = threading.Event(), threading.Event()
    deadline = time.monotonic() + 45
    process = None
    server = None
    binary_fd = None
    restricted_directory = None
    cut_ready = cut_writer = None
    try:
        manifest = json.loads((args.build / "managed-binary.json").read_text())
        binary = Path(manifest["binary_path"])
        binary_fd = os.open(binary, os.O_RDONLY | os.O_NOFOLLOW)
        descriptor = f"/proc/self/fd/{binary_fd}"
        assert sha_file(descriptor) == manifest["binary_sha256"], "binary digest changed"
        source_stat = os.fstat(binary_fd)
        home, workspace = args.output / "home", args.output / "workspace"
        home.mkdir(mode=0o700)
        workspace.mkdir(mode=0o700)
        def journal(kind):
            paths = list(home.rglob("*.managed-" + kind + ".json"))
            assert len(paths) == 1, paths
            return json.loads(paths[0].read_text())

        broker_names = [] if args.case == "empty-tools" else ["broker_" + "0" * 24, "broker_" + "1" * 24]

        class Gateway(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_GET(self):
                problems.append("unexpected GET " + self.path)
                self.send_error(404)

            def do_POST(self):
                try:
                    assert self.path.rstrip("/").endswith("/responses"), self.path
                    size = int(self.headers.get("Content-Length", "0"))
                    assert 0 < size < 4 * 1024 * 1024, "request bound"
                    body = json.loads(self.rfile.read(size))
                    requests.append(body)
                    advertised = [tool.get("name") for tool in body["tools"]]
                    assert sorted(advertised) == broker_names, advertised
                    self.send_response(200)
                    self.send_header("Content-Type", "text/event-stream")
                    self.end_headers()

                    def send(event):
                        self.wfile.write(("event: " + event["type"] + "\ndata: " + json.dumps(event) + "\n\n").encode())
                        self.wfile.flush()

                    number = len(requests)
                    send({"type": "response.created", "response": {"id": f"response-{number}"}})
                    if number == 1 and args.case in {"denied-tool", "empty-tools"}:
                        send({"type": "response.output_item.done", "item": {"type": "function_call", "id": "fc-denied", "call_id": "denied-call", "name": args.denied_tool, "arguments": '{"cmd":"touch managed-must-not-exist","command":"touch managed-must-not-exist"}'}})
                        stream_started.set()
                    elif number == 1:
                        send({"type": "response.output_item.done", "item": {"type": "function_call", "id": "fc-a", "call_id": "original-a", "name": broker_names[0], "arguments": '{ "nonce": "a" }'}})
                        stream_started.set()
                        if args.case == "stream-cut":
                            self.close_connection = True
                            self.connection.shutdown(socket.SHUT_RDWR)
                            self.connection.close()
                            return
                        assert finish_stream.wait(10), "final delta gate timeout"
                        send({"type": "response.output_item.done", "item": {"type": "function_call", "id": "fc-b", "call_id": "original-b", "name": broker_names[1], "arguments": '{ "nonce": "b" }'}})
                    else:
                        assert number == 2 or args.case == "next-turn" and number == 3, "unexpected extra model request"
                        if args.case in {"journal", "next-turn"} or args.case.startswith("recover-"):
                            saved = journal("broker")
                            assert saved["phase"] == "completed" and set(saved["results"]) == {"original-a", "original-b"}, saved
                        outputs = [item for item in body["input"] if item.get("type") == "function_call_output"]
                        assert len(outputs) == 2 and all("aborted" not in json.dumps(item.get("output")) for item in outputs), outputs
                        original_results = {item.get("call_id"): item.get("output") for item in outputs}
                        assert set(original_results) == {"original-a", "original-b"}, original_results
                        assert "receipt-a" in json.dumps(original_results["original-a"])
                        assert "receipt-b" in json.dumps(original_results["original-b"])
                        send({"type": "response.output_item.done", "item": {"type": "message", "role": "assistant", "id": "msg-final", "content": [{"type": "output_text", "text": "MANAGED_FIXTURE_OK"}]}})
                    send({"type": "response.completed", "response": {"id": f"response-{number}", "usage": {"input_tokens": 11, "output_tokens": 3, "total_tokens": 14}}})
                except Exception as error:
                    problems.append(str(error))
                    stream_started.set()

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Gateway)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        url = f"http://127.0.0.1:{server.server_port}"
        argv = [descriptor, *manifest["startup_argv"]]
        for value in ['skills.bundled.enabled=false', 'skills.include_instructions=false', 'features.plugins=false', 'model_provider="fixture"', 'model="gpt-5.4"', 'model_providers.fixture.name="fixture"', f'model_providers.fixture.base_url="{url}"', 'model_providers.fixture.wire_api="responses"', 'model_providers.fixture.requires_openai_auth=false']:
            argv += ["--config", value]
        marker = workspace / "managed-startup-must-not-exist"
        if args.case in {"startup", "startup-control"}:
            native_mcp = args.output / "ambient-mcp"
            native_mcp.write_text("#!/bin/sh\nprintf native-startup > '" + str(marker) + "'\n")
            native_mcp.chmod(0o700)
            for value in ['features.async_compaction=true', 'features.shell_snapshot=true', 'features.code_mode_prewarm=true', 'features.recommended_plugins=true', 'model_auto_compact_token_limit=1', 'mcp_servers.ambient.command=' + json.dumps(str(native_mcp))]:
                argv += ["--config", value]
        if args.case == "stream-cut":
            for value in ['model_providers.fixture.stream_max_retries=9', 'model_providers.fixture.request_max_retries=9', 'model_providers.fixture.supports_websockets=true', 'features.unbounded_connection_retries=true', 'features.responses_websockets=true', 'features.responses_websockets_v2=true']:
                argv += ["--config", value]
        env = {"PATH": "/usr/bin:/bin", "HOME": str(home), "CODEX_HOME": str(home), "TMPDIR": str(args.output), "RUST_LOG": "error"}
        extra_fds = (binary_fd,)
        if args.case == "recover-marker":
            fixture = args.output / "testdata"
            fixture.mkdir()
            source = Path(__file__).parent / "testdata" / "codex_fsync_cut.c"
            copied = fixture / source.name
            shutil.copyfile(source, copied)
            compiler = Path(shutil.which("cc")).resolve(strict=True)
            shim = args.output / "fsync-cut.so"
            compile_argv = [str(compiler), "-shared", "-fPIC", "-O2", "-Wall", "-Wextra", "-Werror", str(copied), "-o", str(shim), "-ldl", "-pthread"]
            subprocess.run(compile_argv, check=True, timeout=15, env={"PATH": "/usr/bin:/bin", "TMPDIR": str(args.output)})
            cut_ready, cut_writer = os.pipe2(os.O_CLOEXEC | os.O_NONBLOCK)
            cut_control = args.output / "fsync-cut.control"
            env.update(LD_PRELOAD=str(shim), AGENT_WORKFLOW_TEST_FSYNC_CUT_CONTROL=str(cut_control), AGENT_WORKFLOW_TEST_FSYNC_CUT_READY_FD=str(cut_writer))
            extra_fds += (cut_writer,)
            receipt["interposition_fixture"] = {"scope": "test-only real-fsync crash cut; no hardware/storage failure claim", "source_sha256": sha_file(copied), "shim_sha256": sha_file(shim), "compiler": str(compiler), "compiler_sha256": sha_file(compiler), "compile_argv": compile_argv, "launch_environment": {key: env[key] for key in ["LD_PRELOAD", "AGENT_WORKFLOW_TEST_FSYNC_CUT_CONTROL", "AGENT_WORKFLOW_TEST_FSYNC_CUT_READY_FD"]}}
        error_file = open(args.output / "stderr.log", "wb")
        process = subprocess.Popen(argv, executable=descriptor, pass_fds=extra_fds, cwd=workspace, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=error_file, start_new_session=True)
        if cut_writer is not None:
            os.close(cut_writer)
            cut_writer = None
        # Observe the actual executed image before sending even initialization.
        executed = Path(f"/proc/{process.pid}/exe")
        while time.monotonic() < deadline:
            observed = executed.stat()
            if (observed.st_dev, observed.st_ino) == (source_stat.st_dev, source_stat.st_ino):
                break
            assert process.poll() is None, "process exited before exec identity"
            time.sleep(0.01)
        else:
            raise AssertionError("executed image identity timeout")
        assert sha_file(executed) == manifest["binary_sha256"]
        receipt["executed_binary_sha256"] = manifest["binary_sha256"]
        receipt["build_id"] = manifest["build_id"]

        def read_frames():
            try:
                for line in process.stdout:
                    assert len(line) < 4 * 1024 * 1024, "RPC frame bound"
                    frame = json.loads(line)
                    frames.append(frame)
                    events.put(frame)
            except Exception as error:
                events.put({"readerError": str(error)})
            events.put({"readerClosed": True})

        reader_thread = threading.Thread(target=read_frames, daemon=True)
        reader_thread.start()

        def send(frame):
            process.stdin.write((json.dumps(frame) + "\n").encode())
            process.stdin.flush()

        def until(predicate):
            while time.monotonic() < deadline:
                frame = events.get(timeout=max(0.01, deadline - time.monotonic()))
                assert not frame.get("readerClosed"), "app-server stdout closed"
                assert not frame.get("readerError"), frame
                if predicate(frame):
                    return frame
            raise AssertionError("RPC deadline exceeded")

        def rpc(identifier, method, params):
            send({"id": identifier, "method": method, "params": params})
            reply = until(lambda frame: frame.get("id") == identifier and "method" not in frame)
            assert "error" not in reply, reply
            return reply["result"]

        def restart_owned():
            nonlocal process, reader_thread, events
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            process.wait(timeout=3)
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            reader_thread.join(timeout=3)
            assert not reader_thread.is_alive()
            events = queue.Queue()
            for name in ["LD_PRELOAD", "AGENT_WORKFLOW_TEST_FSYNC_CUT_CONTROL", "AGENT_WORKFLOW_TEST_FSYNC_CUT_READY_FD"]:
                env.pop(name, None)
            process = subprocess.Popen(argv, executable=descriptor, pass_fds=(binary_fd,), cwd=workspace, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=error_file, start_new_session=True)
            executed = Path(f"/proc/{process.pid}/exe")
            for _ in range(300):
                observed = executed.stat()
                if (observed.st_dev, observed.st_ino) == (source_stat.st_dev, source_stat.st_ino):
                    break
                assert process.poll() is None
                time.sleep(0.01)
            assert sha_file(executed) == manifest["binary_sha256"]
            reader_thread = threading.Thread(target=read_frames, daemon=True)
            reader_thread.start()
            restarted = rpc(8, "initialize", {"clientInfo": {"name": "managed-fixture", "version": "1"}, "capabilities": {"experimentalApi": True}})
            assert "agent-workflow-managed-v1/" + manifest["build_id"] in restarted["userAgent"]
            send({"method": "initialized", "params": {}})

        initialized = rpc(1, "initialize", {"clientInfo": {"name": "managed-fixture", "version": "1"}, "capabilities": {"experimentalApi": True}})
        assert "agent-workflow-managed-v1/" + manifest["build_id"] in initialized["userAgent"]
        send({"method": "initialized", "params": {}})
        policy = {"protocol": 1, "buildId": manifest["build_id"], "policyId": "a" * 64, "brokerTools": broker_names}
        start_params = {"cwd": str(workspace), "approvalPolicy": "never", "sandbox": "read-only", "agentWorkflowPolicy": policy, "dynamicTools": [{"name": name, "description": "Fixture broker", "inputSchema": {"type": "object", "properties": {"nonce": {"type": "string"}}, "required": ["nonce"], "additionalProperties": False}} for name in broker_names]}
        if args.case == "startup-control":
            del start_params["agentWorkflowPolicy"]
        started = rpc(2, "thread/start", start_params)
        if args.case == "startup-control":
            for _ in range(100):
                if marker.exists():
                    break
                time.sleep(0.02)
            assert marker.read_text() == "native-startup"
            assert not requests and not problems
            receipt.update(passed=True, native_mcp_positive_control=True, model_requests=0, production_admission_enabled=False)
            return
        thread = started["thread"]["id"]
        turn = rpc(3, "turn/start", {"threadId": thread, "agentWorkflowInvocation": "original-invocation", "input": [{"type": "text", "text": "Fixture tool batch", "text_elements": []}]})["turn"]["id"]
        assert stream_started.wait(10), "model did not stream first call"
        if args.case == "stream-cut":
            completed = until(lambda frame: frame.get("method") == "turn/completed")
            assert completed["params"]["turn"]["status"] != "completed", completed
            assert journal("model")["completed"] is None
            assert not any(frame.get("method") in {"item/tool/call", "agentWorkflow/batchParked"} for frame in frames)
            rpc(13, "turn/start", {"threadId": thread, "agentWorkflowInvocation": "must-not-regrant", "input": [{"type": "text", "text": "Do not bypass unknown usage", "text_elements": []}]})
            later = until(lambda frame: frame.get("method") == "turn/completed")
            assert later["params"]["turn"]["status"] != "completed", later
            assert len(requests) == 1 and not problems, problems
            restart_owned()
            send({"id": 14, "method": "thread/resume", "params": {"threadId": thread, "agentWorkflowPolicy": policy}})
            resumed = until(lambda frame: frame.get("id") == 14 and "method" not in frame)
            if "error" not in resumed:
                rpc(15, "turn/start", {"threadId": thread, "agentWorkflowInvocation": "cold-must-not-regrant", "input": [{"type": "text", "text": "No new allowance", "text_elements": []}]})
                cold = until(lambda frame: frame.get("method") == "turn/completed")
                assert cold["params"]["turn"]["status"] != "completed", cold
            assert len(requests) == 1 and not problems, problems
            receipt["cold_restart_unknown_attempt_held"] = True
            receipt.update(passed=True, unknown_native_attempt_held=True, forced_retry_settings_ignored=True, subsequent_turn_blocked=True, model_requests=1, callback_count=0, restart_recovery_proven=False, production_admission_enabled=False)
            return
        if args.case in {"denied-tool", "empty-tools"}:
            completed = until(lambda frame: frame.get("method") == "turn/completed")
            assert completed["params"]["turn"]["status"] != "completed", completed
            assert not (workspace / "managed-must-not-exist").exists()
            assert not any(frame.get("method") in {"item/tool/call", "agentWorkflow/batchParked"} for frame in frames)
            assert len(requests) == 1 and not problems, problems
            receipt.update(passed=True, denied_tool=args.denied_tool, callback_count=0, marker_created=False, model_requests=1, production_admission_enabled=False)
            return
        time.sleep(0.15)
        assert not any(frame.get("method") in {"item/tool/call", "agentWorkflow/batchParked"} for frame in frames), "callback before stream sealed"
        if args.case == "journal-write-failure":
            rollouts = list(home.rglob("rollout-*.jsonl"))
            assert len(rollouts) == 1
            restricted_directory = rollouts[0].parent
            restricted_directory.chmod(0o500)
        finish_stream.set()
        if args.case == "journal-write-failure":
            completed = until(lambda frame: frame.get("method") == "turn/completed")
            assert completed["params"]["turn"]["status"] != "completed", completed
            assert not any(frame.get("method") in {"item/tool/call", "agentWorkflow/batchParked"} for frame in frames)
            assert len(requests) == 1 and not problems, problems
            errors = [frame["params"]["error"]["message"] for frame in frames if frame.get("method") == "error"]
            assert any("Permission denied (os error 13)" in message and str(restricted_directory) in message for message in errors), errors
            restricted_directory.chmod(0o700)
            restricted_directory = None
            receipt.update(passed=True, actual_directory_write_denial=True, callback_count=0, model_requests=1, production_admission_enabled=False)
            return
        permit = until(lambda frame: frame.get("method") == "agentWorkflow/batchParked")["params"]
        if args.permit_shape == "epoch":
            assert set(permit) == {"batch", "permitEpoch", "usage"} and permit["permitEpoch"]
            assert permit["usage"]["total_tokens"] == 14
            parked = permit["batch"]
        else:
            assert "batch" not in permit
            parked = permit
        calls = {frame["params"]["callId"]: frame for frame in frames if frame.get("method") == "item/tool/call"}
        assert set(calls) == {"original-a", "original-b"}, calls
        assert parked["callIds"] == ["original-a", "original-b"]
        assert parked["threadId"] == thread and parked["turnId"] == turn
        assert parked["invocationId"] == "original-invocation"
        if args.case in {"recover-sealed", "recover-partial", "recover-accepted", "recover-marker", "recover-stale"}:
            assert args.permit_shape == "epoch"
            if args.case == "recover-partial":
                send({"id": calls["original-b"]["id"], "result": {"contentItems": [{"type": "inputText", "text": "receipt-b"}], "success": True, "agentWorkflowPermit": permit}})
                time.sleep(0.15)
            assert journal("broker")["phase"] == "sealed" and len(requests) == 1
            original_permit = permit
            if args.case in {"recover-accepted", "recover-marker"}:
                rollout = list(home.rglob("rollout-*.jsonl"))
                assert len(rollout) == 1
                # Change only the already image-verified owned native process.
                actual = Path(f"/proc/{process.pid}/exe")
                assert process.poll() is None and sha_file(actual) == manifest["binary_sha256"]
                if args.case == "recover-accepted":
                    before = resource.prlimit(process.pid, resource.RLIMIT_FSIZE)
                    limit = rollout[0].stat().st_size
                    resource.prlimit(process.pid, resource.RLIMIT_FSIZE, (limit, before[1]))
                    receipt["owned_file_size_limit"] = {"pid": process.pid, "before": before, "after": resource.prlimit(process.pid, resource.RLIMIT_FSIZE), "rollout_bytes": limit}
                else:
                    cut_control.write_text(str(rollout[0]) + "\n")
                for call, text in [("original-b", "receipt-b"), ("original-a", "receipt-a")]:
                    send({"id": calls[call]["id"], "result": {"contentItems": [{"type": "inputText", "text": text}], "success": True, "agentWorkflowPermit": permit}})
                if args.case == "recover-marker":
                    readable, _, _ = select.select([cut_ready], [], [], min(10, max(0, deadline-time.monotonic())))
                    assert readable and os.read(cut_ready, 100) == b"real-fsync-complete\n", "actual fsync cut did not trigger"
                    saved = journal("broker")
                    rows = [json.loads(line) for line in rollout[0].read_text().splitlines()]
                    raw_outputs = [row["payload"] for row in rows if row["type"] == "response_item" and row["payload"].get("type") == "function_call_output"]
                    assert saved["phase"] == "resultsAccepted" and len(raw_outputs) == 2 and {item["call_id"] for item in raw_outputs} == {"original-a", "original-b"}
                    assert all("aborted" not in json.dumps(item["output"]) for item in raw_outputs) and len(requests) == 1
                    receipt.update(raw_outputs_synced_before_completion_marker_cut=True, raw_output_count_at_cut=2, raw_rollout_sha256_at_cut=sha_file(rollout[0]))
                    assert process.poll() is None and sha_file(Path(f"/proc/{process.pid}/exe")) == manifest["binary_sha256"]
                    os.killpg(process.pid, signal.SIGKILL)
            else:
                os.killpg(process.pid, signal.SIGKILL)
            process.wait(timeout=3)
            receipt["original_process_cut_exit"] = process.returncode
            reader_thread.join(timeout=3)
            assert not reader_thread.is_alive()
            if args.case == "recover-accepted":
                saved = journal("broker")
                assert saved["phase"] == "resultsAccepted" and set(saved["results"]) == {"original-a", "original-b"}, saved
                raw_outputs = [json.loads(line)["payload"] for line in rollout[0].read_text().splitlines() if json.loads(line).get("type") == "response_item" and json.loads(line)["payload"].get("type") == "function_call_output"]
                assert len(raw_outputs) < 2 and len(requests) == 1, raw_outputs
                receipt["accepted_results_before_raw_outputs_cut"] = True
                receipt["raw_output_count_at_cut"] = len(raw_outputs)
            cut_index = len(frames)
            restart_owned()
            if args.case == "recover-marker":
                assert "LD_PRELOAD" not in env and not any(key.startswith("AGENT_WORKFLOW_TEST_FSYNC_CUT") for key in env)
                receipt["recovery_without_interposition"] = True
            if args.case == "recover-accepted":
                restored = resource.prlimit(process.pid, resource.RLIMIT_FSIZE)
                assert restored == resource.getrlimit(resource.RLIMIT_FSIZE) == before
                receipt["recovery_file_size_limit"] = restored
            rpc(18, "thread/resume", {"threadId": thread, "agentWorkflowPolicy": policy})
            recovered = rpc(19, "turn/start", {"threadId": thread, "agentWorkflowInvocation": "original-invocation", "agentWorkflowRecovery": parked, "input": []})
            assert recovered["turn"]["id"] == turn
            permit = until(lambda frame: frame.get("method") == "agentWorkflow/batchParked")["params"]
            assert permit["batch"] == parked and permit["usage"] == original_permit["usage"]
            assert permit["permitEpoch"] != original_permit["permitEpoch"]
            replayed = [frame for frame in frames[cut_index:] if frame.get("method") == "item/tool/call"]
            assert len(replayed) == 2
            calls = {frame["params"]["callId"]: frame for frame in replayed}
            assert set(calls) == {"original-a", "original-b"} and len(requests) == 1
            receipt.update(native_original_seal_replay=True, original_turn_preserved=True, fresh_control_epoch=True, original_usage_witness_preserved=True, engine_recovery_proven=False)
            if args.case == "recover-stale":
                send({"id": calls["original-a"]["id"], "result": {"contentItems": [{"type": "inputText", "text": "receipt-a"}], "success": True, "agentWorkflowPermit": original_permit}})
                failed = until(lambda frame: frame.get("method") == "turn/completed")
                assert failed["params"]["turn"]["status"] != "completed" and len(requests) == 1
                receipt.update(passed=True, stale_recovery_epoch_rejected=True, production_admission_enabled=False)
                return
        if args.case in {"journal", "next-turn"}:
            saved = journal("broker")
            assert saved["identity"] == parked and saved["phase"] == "sealed" and not saved["results"], saved
            assert saved["calls"] == [{"id": "original-a", "tool": broker_names[0], "arguments": '{ "nonce": "a" }'}, {"id": "original-b", "tool": broker_names[1], "arguments": '{ "nonce": "b" }'}], saved
            assert saved["usage"]["total_tokens"] == 14
            receipt["sealed_original_calls_present_before_results"] = True
        if args.case in {"invalid-permit", "duplicate-permit", "cancel"}:
            result = {"contentItems": [{"type": "inputText", "text": "receipt-a"}], "success": True, "agentWorkflowPermit": json.loads(json.dumps(permit))}
            semantic_permit = result["agentWorkflowPermit"]["batch"] if args.permit_shape == "epoch" else result["agentWorkflowPermit"]
            if args.case == "invalid-permit":
                semantic_permit["batchId"] = "stale-batch"
                send({"id": calls["original-a"]["id"], "result": result})
            elif args.case == "duplicate-permit":
                # One RPC presents a full-batch permit with a duplicated logical
                # call ID. This malformed call set cannot authorize delivery.
                semantic_permit["callIds"] = ["original-a", "original-a"]
                send({"id": calls["original-a"]["id"], "result": result})
            else:
                rpc(4, "turn/interrupt", {"threadId": thread, "turnId": turn})
            completed = until(lambda frame: frame.get("method") == "turn/completed")
            assert completed["params"]["turn"]["status"] != "completed", completed
            time.sleep(0.15)
            assert len(requests) == 1 and not problems, problems
            receipt.update(passed=True, rejected_control=args.case, model_requests=1, next_model_requests=0, production_admission_enabled=False)
            return
        # Reorder results. A partial result must not allow another model request.
        for call, text in [("original-b", "receipt-b"), ("original-a", "receipt-a")]:
            send({"id": calls[call]["id"], "result": {"contentItems": [{"type": "inputText", "text": text}], "success": True, "agentWorkflowPermit": permit}})
            if call == "original-b":
                time.sleep(0.15)
                assert len(requests) == 1, "partial result resumed model"
        completed = until(lambda frame: frame.get("method") == "turn/completed")
        assert completed["params"]["turn"]["status"] == "completed", completed
        assert len(requests) == 2 and not problems, problems
        if args.case in {"journal", "next-turn"}:
            assert journal("model")["completed"]["batch"]["responseId"] == "response-2"
            receipt["completed_results_present_before_next_model"] = True
        if args.case == "next-turn":
            rpc(12, "turn/start", {"threadId": thread, "agentWorkflowInvocation": "subsequent-invocation", "input": [{"type": "text", "text": "A normal subsequent turn", "text_elements": []}]})
            later = until(lambda frame: frame.get("method") == "turn/completed")
            assert later["params"]["turn"]["status"] == "completed", later
            assert len(requests) == 3 and not problems, problems
            receipt["normal_completed_subsequent_turn"] = True
        if args.case == "cold-policy":
            restart_owned()
            changed = dict(policy)
            changed["policyId"] = "b" * 64
            send({"id": 9, "method": "thread/resume", "params": {"threadId": thread, "agentWorkflowPolicy": changed}})
            rejected = until(lambda frame: frame.get("id") == 9 and "method" not in frame)
            assert "error" in rejected, rejected
            send({"id": 10, "method": "thread/resume", "params": {"threadId": thread}})
            missing = until(lambda frame: frame.get("id") == 10 and "method" not in frame)
            assert "error" in missing, missing
            resumed = rpc(11, "thread/resume", {"threadId": thread, "agentWorkflowPolicy": policy})
            assert resumed["thread"]["id"] == thread
            assert len(requests) == 2 and not problems, problems
            receipt["cold_resume_exact_policy_only"] = True
            receipt["cold_restart_executed_image_sha256"] = manifest["binary_sha256"]
        if args.case == "warm-policy":
            changed = dict(policy)
            changed["policyId"] = "b" * 64
            send({"id": 4, "method": "thread/resume", "params": {"threadId": thread, "agentWorkflowPolicy": changed}})
            rejected = until(lambda frame: frame.get("id") == 4 and "method" not in frame)
            assert "error" in rejected, rejected
            # Missing policy must also fail; warm attach cannot regrant stock authority.
            send({"id": 5, "method": "thread/resume", "params": {"threadId": thread}})
            missing = until(lambda frame: frame.get("id") == 5 and "method" not in frame)
            assert "error" in missing, missing
            resumed = rpc(6, "thread/resume", {"threadId": thread, "agentWorkflowPolicy": policy})
            assert resumed["thread"]["id"] == thread
            assert len(requests) == 2 and not problems, problems
            receipt["warm_resume_exact_policy_only"] = True
        if args.case == "startup":
            assert not marker.exists(), "configured native MCP started"
            # Manual compaction is separately rejected before any model work.
            send({"id": 7, "method": "thread/compact/start", "params": {"threadId": thread}})
            rejected = until(lambda frame: frame.get("id") == 7 and "method" not in frame)
            assert "error" in rejected, rejected
            time.sleep(0.15)
            assert len(requests) == 2 and not marker.exists(), "unsupported startup/background work ran"
            receipt.update(configured_mcp_startup_marker_absent=True, forced_async_compaction=True, forced_prewarm=True, manual_compaction_rejected=True)
        receipt.update(passed=True, original_call_ids=sorted(calls), callbacks_before_stream_complete=0, model_requests_after_partial_results=0, model_requests=len(requests), restart_recovery_proven=False, production_admission_enabled=False)
    except Exception as error:
        receipt["error"] = repr(error)
        raise
    finally:
        finish_stream.set()
        if restricted_directory is not None:
            restricted_directory.chmod(0o700)
        if process is not None:
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                pass
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait(timeout=3)
            receipt["process_exit"] = process.returncode
        if binary_fd is not None:
            os.close(binary_fd)
        for fd in [cut_ready, cut_writer]:
            if fd is not None:
                os.close(fd)
        if server is not None:
            server.shutdown()
            server.server_close()
        (args.output / "frames.json").write_text(json.dumps(frames, indent=2))
        (args.output / "requests.json").write_text(json.dumps(requests, indent=2))
        (args.output / "receipt.json").write_text(json.dumps(receipt, indent=2))


if __name__ == "__main__":
    main()
