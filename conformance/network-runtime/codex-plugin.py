#!/usr/bin/env python3
"""Explicit opt-in native Codex/production-plugin proof; local fake services only.

No ordinary test target invokes this driver. Raw transcripts remain in the
private evidence directory. See codex-plugin.md for its bounded claims.
"""
import argparse
import gzip
import hashlib
import http.server
import json
import os
import pathlib
import queue
import shlex
import shutil
import signal
import socket
import subprocess
import struct
import sys
import tempfile
import threading
import time

PLUGIN_ID = "network-runtime@agent-workflow"
MARKER = "AGENT_WORKFLOW_NETWORK_REFERENCE"
KEYS = ("DOCKER_HOST", "CONTAINER_HOST", "AGENT_WORKFLOW_NETWORK_RUNTIME")
CASES = ("restricted", "native_profile", "snapshot", "snapshot_untrusted", "code_mode", "sandbox",
         "readonly", "approval_denied", "exit_status", "missing", "nonzero",
         "malformed_helper", "reference_mismatch", "namespace_mismatch")
ROOT = None
OWNER = None
BINARY = None
ASSETS = None

class RPC:

    def __init__(self, cmd, env, cwd, log):
        self.log = log
        self.frames = []
        self.seq = 0
        self.q = queue.Queue()
        self.errors = []
        self.errfile = open(log.with_suffix('.stderr'), 'w')
        self.p = subprocess.Popen(cmd, cwd=cwd, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=self.errfile, text=True, start_new_session=True, bufsize=1)

        def reader():
            for line in self.p.stdout:
                try:
                    v = json.loads(line)
                except Exception:
                    v = {'unparsed': line}
                self.frames.append(v)
                self.q.put(v)
            self.q.put({'eof': self.p.poll()})
        threading.Thread(target=reader, daemon=True).start()

    def send(self, method, params=None):
        self.seq += 1
        self.p.stdin.write(json.dumps({'id': self.seq, 'method': method, 'params': params or {}}) + '\n')
        self.p.stdin.flush()
        return self.seq

    def notify(self, method, params=None):
        self.p.stdin.write(json.dumps({'method': method, 'params': params or {}}) + '\n')
        self.p.stdin.flush()

    def wait(self, predicate, timeout=30):
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            try:
                v = self.q.get(timeout=max(0.01, end - time.monotonic()))
            except queue.Empty:
                break
            if 'method' in v and 'id' in v:
                self.errors.append({'unexpected_server_request': v})
                reply = {'id': v['id'], 'result': {'decision': 'decline'}} if v['method'].endswith('/requestApproval') else {'id': v['id'], 'error': {'code': -32601, 'message': 'fixture denies unconfigured server request'}}
                self.p.stdin.write(json.dumps(reply) + '\n')
                self.p.stdin.flush()
            if predicate(v):
                return v
            if 'eof' in v:
                raise RuntimeError('app-server EOF ' + str(v))
        raise TimeoutError('app-server deadline; ' + str(self.frames[-5:]))

    def call(self, method, params=None):
        seq = self.send(method, params)
        v = self.wait(lambda v: v.get('id') == seq)
        if 'error' in v:
            raise RuntimeError(method + ': ' + json.dumps(v))
        return v.get('result')

    def close(self):
        try:
            self.p.stdin.close()
        except Exception:
            pass
        try:
            self.p.wait(timeout=3)
        except subprocess.TimeoutExpired:
            os.killpg(self.p.pid, signal.SIGTERM)
            try:
                self.p.wait(timeout=3)
            except subprocess.TimeoutExpired:
                os.killpg(self.p.pid, signal.SIGKILL)
                self.p.wait()
        self.errfile.close()
        self.log.write_text(''.join((json.dumps(v) + '\n' for v in self.frames)))

class FakeModel(http.server.ThreadingHTTPServer):

    def __init__(self, command, login=False, code_mode=False, escalate=False):
        self.command = command
        self.login = login
        self.code_mode = code_mode
        self.escalate = escalate
        self.requests = []
        self.count = 0
        parent = self

        class Handler(http.server.BaseHTTPRequestHandler):

            def log_message(self, *a):
                pass

            def do_GET(self):
                self.send_response(404)
                self.end_headers()

            def do_POST(self):
                body = self.rfile.read(int(self.headers.get('Content-Length', '0')))
                if self.headers.get('Content-Encoding') == 'gzip':
                    body = gzip.decompress(body)
                try:
                    request = json.loads(body)
                except Exception:
                    request = {'raw': body.decode(errors='replace')}
                parent.requests.append({'path': self.path, 'body': request})
                parent.count += 1
                if parent.count == 1:
                    events = [{'type': 'response.created', 'response': {'id': 'fixture-1'}}, {'type': 'response.output_item.done', 'item': {'type': 'function_call', 'call_id': 'fixture-command', 'name': 'exec_command', 'arguments': json.dumps({'cmd': parent.command, 'login': parent.login, 'yield_time_ms': 10000, 'max_output_tokens': 4000})}}, {'type': 'response.completed', 'response': {'id': 'fixture-1', 'usage': {'input_tokens': 0, 'output_tokens': 0, 'total_tokens': 0}}}]
                if parent.count == 1 and parent.escalate:
                    a = json.loads(events[1]['item']['arguments'])
                    a['sandbox_permissions'] = 'require_escalated'
                    a['justification'] = 'Controlled fixture approval check'
                    events[1]['item']['arguments'] = json.dumps(a)
                if parent.count == 1 and parent.code_mode:
                    args = {'cmd': parent.command, 'login': parent.login, 'yield_time_ms': 10000, 'max_output_tokens': 4000}
                    events[1]['item'] = {'type': 'custom_tool_call', 'call_id': 'fixture-command', 'name': 'exec', 'input': 'text(await tools.exec_command(' + json.dumps(args) + '));'}
                if parent.count != 1:
                    events = [{'type': 'response.created', 'response': {'id': 'fixture-2'}}, {'type': 'response.output_item.done', 'item': {'type': 'message', 'id': 'fixture-message', 'role': 'assistant', 'content': [{'type': 'output_text', 'text': 'fixture complete'}]}}, {'type': 'response.completed', 'response': {'id': 'fixture-2', 'usage': {'input_tokens': 0, 'output_tokens': 0, 'total_tokens': 0}}}]
                data = ''.join(('event: ' + event['type'] + '\ndata: ' + json.dumps(event) + '\n\n' for event in events)).encode()
                self.send_response(200)
                self.send_header('Content-Type', 'text/event-stream')
                self.send_header('Content-Length', str(len(data)))
                self.end_headers()
                self.wfile.write(data)
        super().__init__(('127.0.0.1', 0), Handler)
        threading.Thread(target=self.serve_forever, daemon=True).start()

PROBE = r'''import json, os, pathlib, socket
p = pathlib.Path(__file__).resolve().parent
keys = ['DOCKER_HOST', 'CONTAINER_HOST', 'AGENT_WORKFLOW_NETWORK_RUNTIME']
record = {'uid': os.getuid(), 'gid': os.getgid(), 'cwd': os.getcwd(), 'env': {k: os.environ.get(k) for k in keys + ['SHOULD_STAY_FILTERED', 'KEEP']}}
endpoint = record['env']['DOCKER_HOST']
if endpoint and endpoint.startswith('unix://'):
    try:
        s = socket.socket(socket.AF_UNIX)
        s.settimeout(2)
        s.connect(endpoint[7:])
        s.sendall(b'GET /_ping HTTP/1.0\r\n\r\n')
        record['api'] = s.recv(1024).decode()
        s.close()
    except Exception as e:
        record['api_error'] = str(e)
try:
    (p / 'workspace-marker').write_text('executed')
    record['workspace_write'] = True
except Exception as e:
    record['workspace_write'] = False
    record['workspace_error'] = str(e)
try:
    (p.parent / 'outside-marker').write_text('executed')
    record['outside_write'] = True
except Exception as e:
    record['outside_write'] = False
    record['outside_error'] = str(e)
try:
    pathlib.Path(os.environ['HOME']).joinpath('tool-record.json').write_text(json.dumps(record))
except Exception as e:
    record['record_error'] = str(e)
try:
    (p / 'probe-record.json').write_text(json.dumps(record))
except Exception:
    pass
print('PROBE_RESULT=' + json.dumps(record), flush=True)
'''

class API:

    def __init__(self, path):
        self.path = path
        self.count = 0
        self.sock = socket.socket(socket.AF_UNIX)
        self.sock.bind(str(path))
        self.sock.listen()

        def serve():
            while True:
                try:
                    c, _ = self.sock.accept()
                except OSError:
                    return
                with c:
                    c.recv(4096)
                    self.count += 1
                    c.sendall(b'HTTP/1.0 200 OK\r\nContent-Length: 15\r\n\r\nruntime-fixture')
        threading.Thread(target=serve, daemon=True).start()

    def close(self):
        self.sock.close()


def reference(case):
    name = 'plugin-' + str(CASES.index(case))
    identity = hashlib.sha256(case.encode()).hexdigest()[:32]
    return {'name': name, 'identity': identity, 'generation': 1,
            'endpoint': 'unix:///run/agent-workflow-runtimes/' + name + '/' + identity + '/podman.sock'}


class StatusFixture:
    """Root peer on canonical transport; status is intentionally a fake."""
    def __init__(self, case):
        self.ref = reference(case)
        self.requests = []
        self.peer_contexts = []
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_SEQPACKET)
        parent = pathlib.Path('/run/agent-workflow-runtimes') / self.ref['name']
        parent.mkdir(parents=True)
        self.sock.bind(str(parent / 'control.sock'))
        (parent / 'control.sock').chmod(0o666)
        self.sock.listen()
        self.api = None
        if case not in ('missing', 'nonzero', 'malformed_helper'):
            endpoint = pathlib.Path(self.ref['endpoint'][7:])
            endpoint.parent.mkdir()
            self.api = API(endpoint)
            endpoint.chmod(0o666)
        def serve():
            while True:
                try:
                    conn, _ = self.sock.accept()
                except OSError:
                    return
                with conn:
                    try:
                        request = json.loads(conn.recv(1 << 20))
                        # Fixture-only observation: selected nonsecret markers from
                        # the real helper peer, without modifying production assets.
                        pid, uid, gid = struct.unpack('3i', conn.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
                        context = {'uid': uid, 'gid': gid}
                        try:
                            values = dict(item.split(b'=', 1) for item in pathlib.Path('/proc/' + str(pid) + '/environ').read_bytes().split(b'\0') if b'=' in item)
                            context['env'] = {key: values.get(key.encode(), b'').decode() for key in (*KEYS, MARKER)}
                        except OSError as error:
                            context['observation_error'] = str(error)
                        self.peer_contexts.append(context)
                        self.requests.append(request)
                        if request != {'operation': 'check', 'reference': self.ref,
                                       'forward': {'host_port': 0, 'guest_port': 0, 'uid': 0, 'id': 0}}:
                            reply = {'error': 'fixture rejects non-exact admitted reference'}
                        else:
                            status = {'reference': dict(self.ref), 'healthy': True,
                                      'network_namespace': os.readlink('/proc/self/ns/net')}
                            if case == 'reference_mismatch':
                                status['reference']['generation'] += 1
                            if case == 'namespace_mismatch':
                                status['network_namespace'] = 'net:[0]'
                            reply = {'status': status}
                        conn.sendall(json.dumps(reply).encode())
                    except (OSError, ValueError):
                        pass
        threading.Thread(target=serve, daemon=True).start()

    def close(self):
        self.sock.close()
        if self.api:
            self.api.close()


def clean_env(directory, ref):
    home = directory / 'home'
    return {'PATH': '/usr/bin:/bin', 'HOME': str(home), 'USER': 'fixture',
            'LOGNAME': 'fixture', 'SHELL': '/bin/bash', 'LANG': 'C.UTF-8',
            'CODEX_HOME': str(directory / 'codex'),
            'XDG_CONFIG_HOME': str(home / '.config'),
            'XDG_DATA_HOME': str(home / '.local/share'),
            'XDG_CACHE_HOME': str(directory / 'cache'),
            'XDG_RUNTIME_DIR': str(directory / 'run'),
            'DBUS_SESSION_BUS_ADDRESS': 'unix:path=' + str(directory / 'no-bus'),
            'GIT_CONFIG_NOSYSTEM': '1', 'GIT_CONFIG_GLOBAL': '/dev/null',
            'SHOULD_STAY_FILTERED': 'sensitive-fixture-value', 'KEEP': 'kept-fixture-value',
            MARKER: json.dumps(ref), 'DOCKER_HOST': ref['endpoint'],
            'CONTAINER_HOST': ref['endpoint'], 'AGENT_WORKFLOW_NETWORK_RUNTIME': ref['name']}


def strings(value):
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for item in value.values():
            yield from strings(item)
    elif isinstance(value, list):
        for item in value:
            yield from strings(item)


def run_case(name):
    directory = ROOT / name
    directory.mkdir()
    for child in ('codex', 'home', 'workspace', 'run'):
        (directory / child).mkdir(mode=0o700)
    home, workspace = directory / 'codex', directory / 'workspace'
    git = workspace / '.git'
    (git / 'refs/heads').mkdir(parents=True)
    (git / 'objects').mkdir()
    (git / 'HEAD').write_text('ref: refs/heads/main\n')
    (git / 'config').write_text('[core]\nrepositoryformatversion = 0\nbare = false\n')
    plugin = home / 'plugins/cache/agent-workflow/network-runtime/local'
    shutil.copytree(ASSETS, plugin)
    ref = reference(name)
    env = clean_env(directory, ref)
    snapshot = name in ('snapshot', 'snapshot_untrusted')
    if snapshot:
        for rc in ('.bash_profile', '.zshenv', '.zprofile'):
            (directory / 'home' / rc).write_text("export DOCKER_HOST='unix:///stale-snapshot-fixture.sock'\nexport CONTAINER_HOST='unix:///stale-snapshot-fixture.sock'\n")
    sandbox = 'read-only' if name == 'readonly' else 'workspace-write'
    if name in ('restricted', 'snapshot', 'snapshot_untrusted', 'code_mode', 'exit_status'):
        sandbox = 'danger-full-access'
    approval = 'on-request' if name == 'approval_denied' else 'never'
    source = PROBE + ('\nraise SystemExit(7)\n' if name == 'exit_status' else '')
    (workspace / 'probe.py').write_text(source)
    model = FakeModel('/usr/bin/python3 ' + shlex.quote(str(workspace / 'probe.py')),
                      login=snapshot, code_mode=name == 'code_mode',
                      escalate=name == 'approval_denied')
    config = '''model = "mock-model"
model_provider = "fixture"
approval_policy = %s
sandbox_mode = %s
[model_providers.fixture]
name = "Local deterministic fixture"
base_url = %s
wire_api = "responses"
requires_openai_auth = false
supports_websockets = false
[features]
plugins = true
hooks = true
shell_snapshot_v2 = %s
code_mode = %s
[shell_environment_policy]
inherit = "all"
include_only = ["PATH", "HOME", "KEEP"]
[sandbox_workspace_write]
network_access = %s
[plugins."network-runtime@agent-workflow"]
enabled = true
[projects.%s]
trust_level = "trusted"
''' % (json.dumps(approval), json.dumps(sandbox),
       json.dumps('http://127.0.0.1:' + str(model.server_address[1]) + '/v1'),
       str(snapshot).lower(), str(name == 'code_mode').lower(),
       str(name == 'native_profile').lower(), json.dumps(str(workspace)))
    if name == 'native_profile':
        # The base allows this variable; the native named profile removes it.
        # Its absence proves profile selection remains owned by Codex.
        config = config.replace('include_only = ["PATH", "HOME", "KEEP"]',
                                'include_only = ["PATH", "HOME", "KEEP", "SHOULD_STAY_FILTERED"]')
    (home / 'config.toml').write_text(config)
    rpc = None
    result = {'case': name, 'reference': ref, 'sandbox': sandbox}
    try:
        rpc = RPC([BINARY, 'app-server'], env, str(workspace), directory / 'rpc.jsonl')
        rpc.call('initialize', {'clientInfo': {'name': 'production-plugin-fixture', 'version': '1'},
                                'capabilities': {'experimentalApi': True}})
        rpc.notify('initialized')
        hooks = rpc.call('hooks/list', {'cwds': [str(workspace)]})
        entries = [hook for item in hooks.get('data', []) for hook in item.get('hooks', [])]
        assert entries and all(hook.get('pluginId') == PLUGIN_ID for hook in entries), 'unexpected hook source'
        if name != 'snapshot_untrusted':
            rpc.call('config/batchWrite', {'edits': [{'keyPath': 'hooks.state',
                     'value': {h['key']: {'trusted_hash': h['currentHash']} for h in entries},
                     'mergeStrategy': 'upsert'}], 'reloadUserConfig': True})
        if name == 'native_profile':
            (home / 'fixture.config.toml').write_text('[shell_environment_policy]\ninherit = "all"\ninclude_only = ["PATH", "HOME", "KEEP"]\n')
            rpc.close()
            rpc = None
            command = [BINARY, 'exec', '--json', '--skip-git-repo-check', '--profile', 'fixture',
                       '--sandbox', sandbox, 'Run the deterministic fixture command.']
            native = subprocess.run(command, cwd=workspace, env=env, capture_output=True, text=True,
                                    timeout=35, start_new_session=True)
            (directory / 'native.stdout').write_text(native.stdout)
            (directory / 'native.stderr').write_text(native.stderr)
            result['native_exit'] = native.returncode
            assert native.returncode == 0, 'native profile invocation failed'
        else:
            thread = rpc.call('thread/start', {'cwd': str(workspace), 'model': 'mock-model',
                              'modelProvider': 'fixture', 'approvalPolicy': approval,
                              'approvalsReviewer': 'user', 'sandbox': sandbox, 'ephemeral': True})
            rpc.call('turn/start', {'threadId': thread['thread']['id'],
                     'input': [{'type': 'text', 'text': 'Execute the deterministic fixture command.',
                                'text_elements': []}]})
            if not any(frame.get('method') == 'turn/completed' for frame in rpc.frames):
                rpc.wait(lambda frame: frame.get('method') == 'turn/completed', timeout=35)
            result['native_approval_requests'] = sum(
                entry['unexpected_server_request'].get('method', '').endswith('/requestApproval')
                for entry in rpc.errors)
            result['hook_completions'] = [frame for frame in rpc.frames if frame.get('method') == 'hook/completed']
    except Exception as error:
        result['error'] = str(error)
    finally:
        if rpc:
            rpc.close()
        model.shutdown()
        result['fixture_model_requests'] = len(model.requests)
        (directory / 'fake-model-requests.json').write_text(json.dumps(model.requests, indent=2))
        record = directory / 'home/tool-record.json'
        if not record.exists():
            record = workspace / 'probe-record.json'
        result['record'] = json.loads(record.read_text()) if record.exists() else None
        # Read-only cannot persist the record; extract only the fixture marker from private frames.
        if result['record'] is None and rpc:
            for text in strings(rpc.frames):
                if 'PROBE_RESULT=' in text:
                    try:
                        result['record'] = json.JSONDecoder().raw_decode(text.split('PROBE_RESULT=', 1)[1])[0]
                        break
                    except ValueError:
                        pass
        result['workspace_write'] = (workspace / 'workspace-marker').exists()
        result['outside_write'] = (directory / 'outside-marker').exists()
        if rpc:
            result['exit_seven_observed'] = any(
                frame.get('method') == 'item/completed' and
                frame.get('params', {}).get('item', {}).get('exitCode') == 7
                for frame in rpc.frames)
        (directory / 'result.json').write_text(json.dumps(result, indent=2))
    return result


def direct_protocol():
    """Original input fields/command bytes and ordinary malformed input denial."""
    env = clean_env(ROOT / 'restricted', reference('restricted'))
    hook = ASSETS / 'hooks/pre-tool-use.sh'
    original = "printf '%s\\n' 'command bytes; $KEEP'\nexit 7"
    fields = {'command': original, 'timeout_ms': 1234, 'description': 'fixture option',
              'nested': {'untouched': ['x', 42]}}
    payload = {'hook_event_name': 'PreToolUse', 'tool_name': 'Bash', 'tool_input': fields}
    def invoke(data, selected=env):
        process = subprocess.run(['/bin/sh', str(hook)], input=data, text=True,
                                 capture_output=True, env=selected, timeout=8)
        assert process.returncode == 0 and not process.stderr, 'hook protocol process failed'
        return json.loads(process.stdout)
    out = invoke(json.dumps(payload))['hookSpecificOutput']
    updated = dict(out['updatedInput'])
    command = updated.pop('command')
    assert updated == {k: v for k, v in fields.items() if k != 'command'}, 'input options changed'
    assert command.endswith(original) and out['permissionDecision'] == 'allow', 'command bytes changed'
    assert invoke('{malformed')['hookSpecificOutput']['permissionDecision'] == 'deny'
    for case in ('missing', 'nonzero', 'malformed_helper', 'reference_mismatch', 'namespace_mismatch'):
        selected = clean_env(ROOT / 'restricted', reference(case))
        assert invoke(json.dumps(payload), selected)['hookSpecificOutput']['permissionDecision'] == 'deny', case
    return ['original command bytes and every input option preserved',
            'malformed native payload returns valid deny',
            'missing, nonzero, unrecognized helper and mismatched reference/namespace return valid deny']


def verify(results):
    checks = []
    for result in results:
        name = result['case']
        assert not result.get('error'), name + ': ' + str(result.get('error'))
        assert 1 <= result['fixture_model_requests'] <= 3, name + ': unexpected fake response count'
        denied = name in ('missing', 'nonzero', 'malformed_helper', 'reference_mismatch', 'namespace_mismatch', 'approval_denied')
        if denied:
            assert result['record'] is None and not result['workspace_write'], name + ': denied tool ran'
            checks.append(name + ': original tool did not execute')
            if name != 'approval_denied':
                assert any(hook['params']['run']['status'] == 'blocked' for hook in result['hook_completions']), name + ': no native hook denial'
            if name == 'approval_denied':
                assert result['native_approval_requests'] > 0, 'native approval was bypassed'
                checks.append('native approval requested and denied')
            continue
        record = result['record']
        if name == 'snapshot_untrusted':
            assert record and all(record['env'][key] is None for key in KEYS), 'untrusted snapshot selectors not filtered'
            assert record['env']['SHOULD_STAY_FILTERED'] is None
            checks.append('untrusted snapshot baseline leaves public selectors filtered')
            continue
        assert record, name + ': fixture tool did not execute'
        assert record['env'] == {**{k: result['reference']['endpoint'] for k in ('DOCKER_HOST', 'CONTAINER_HOST')},
                                 'AGENT_WORKFLOW_NETWORK_RUNTIME': result['reference']['name'],
                                 'SHOULD_STAY_FILTERED': None, 'KEEP': 'kept-fixture-value'}, name + ': filtering/routing changed'
        assert record['uid'] == OWNER['uid'] and record['gid'] == OWNER['gid'], name + ': caller identity changed'
        assert record['cwd'] == str(ROOT / name / 'workspace'), name + ': working directory changed'
        checks.append(name + ': three routing values repaired; unrelated filtering, uid/gid/cwd preserved')
        if name in ('restricted', 'native_profile', 'snapshot', 'code_mode', 'exit_status'):
            assert 'runtime-fixture' in record.get('api', ''), name + ': local API not reached'
            checks.append(name + ': selected fake API reached')
        if name == 'sandbox':
            assert result['workspace_write'] and not result['outside_write'], 'workspace sandbox changed'
            assert 'api_error' in record, 'native network restriction changed'
            checks.append('workspace sandbox and network restriction preserved')
        if name == 'readonly':
            assert not result['workspace_write'] and not result['outside_write'], 'read-only sandbox changed'
            checks.append('read-only sandbox preserved')
        if name == 'exit_status':
            assert result['exit_seven_observed'], 'original exit status not observed'
            checks.append('original native tool exit status 7 preserved')
    return checks


def load_fixture(args):
    global ROOT, OWNER, BINARY, ASSETS
    ROOT = pathlib.Path(args.fixture).resolve()
    OWNER = json.loads((ROOT / 'owner.json').read_text())
    BINARY = OWNER['codex_binary']
    ASSETS = ROOT / 'production-plugin'


def install_marketplace():
    directory = ROOT / 'marketplace-install'
    directory.mkdir()
    for child in ('codex', 'home', 'workspace', 'run'):
        (directory / child).mkdir(mode=0o700)
    env = clean_env(directory, reference('restricted'))
    (directory / 'codex/config.toml').write_text('[features]\nplugins = true\nhooks = true\n')
    commands = ([BINARY, 'plugin', 'marketplace', 'add', str(ROOT / 'distribution'), '--json'],
                [BINARY, 'plugin', 'add', PLUGIN_ID, '--json'])
    observations = []
    for command in commands:
        process = subprocess.run(command, env=env, cwd=directory / 'workspace',
                                 capture_output=True, text=True, timeout=15)
        observations.append({'command': command, 'exit_code': process.returncode,
                             'stdout': process.stdout, 'stderr': process.stderr})
        (directory / 'install.json').write_text(json.dumps(observations, indent=2))
        assert process.returncode == 0, 'disposable native marketplace installation failed'
    installed = list((directory / 'codex/plugins/cache').rglob('pre-tool-use.sh'))
    assert len(installed) == 1 and installed[0].read_bytes() == (ASSETS / 'hooks/pre-tool-use.sh').read_bytes()
    return 'native local marketplace add/install succeeds with production metadata and assets'


def worker_user(args):
    load_fixture(args)
    assert os.getuid() == OWNER['uid'] != 0
    assert os.readlink('/proc/self/ns/net') != OWNER['host_network_namespace']
    assert {name for _, name in socket.if_nameindex()} == {'lo'}, 'unexpected external network interface'
    assert not any(line.split()[1] == '00000000' for line in pathlib.Path('/proc/net/route').read_text().splitlines()[1:]), 'external default route'
    version_home = ROOT / 'version'
    version_home.mkdir()
    for child in ('codex', 'home', 'workspace', 'run'):
        (version_home / child).mkdir(mode=0o700)
    version = subprocess.run([BINARY, '--version'],
                             env=clean_env(version_home, reference('restricted')),
                             cwd=version_home, text=True, capture_output=True,
                             check=True, timeout=5).stdout.strip()
    assert version == OWNER['codex_version'], 'proof requires codex-cli 0.164.0; found ' + version
    marketplace_check = install_marketplace()
    results = []
    for name in CASES:
        results.append(run_case(name))
        print(json.dumps({'completed_case': name, 'error': results[-1].get('error')}), flush=True)
    report = {'status': 'failed', 'api': 'local fake Unix HTTP service; not Podman',
              'runtime_status': 'root-authenticated fake control/status; not an actual runtime',
              'network_namespace': os.readlink('/proc/self/ns/net'),
              'host_network_namespace': OWNER['host_network_namespace'],
              'network_interfaces': [name for _, name in socket.if_nameindex()],
              'external_default_route': False, 'host_login_used': False,
              'real_model_calls': 0, 'cases': results,
              'codex_version': OWNER['codex_version'],
              'codex_binary_sha256': OWNER['codex_binary_sha256'],
              'helper_sha256': OWNER['helper_sha256'],
              'asset_sha256': OWNER['asset_sha256'],
              'marketplace_sha256': OWNER['marketplace_sha256'], 'driver_sha256': OWNER['driver_sha256']}
    try:
        report['checks'] = [marketplace_check] + verify(results) + direct_protocol()
        report['status'] = 'observations_verified'
    except Exception as error:
        report['error'] = str(error)
    (ROOT / 'report.json').write_text(json.dumps(report, indent=2))
    return 0 if report['status'] == 'observations_verified' else 1


def worker_root(args):
    load_fixture(args)
    assert os.getuid() == 0, 'root namespace fixture requires root'
    subprocess.run(['/usr/bin/mount', '--make-rprivate', '/'], check=True)
    # Existing mount targets only. Both replacements disappear with this namespace.
    for target in ('/run', '/var/lib'):
        assert pathlib.Path(target).is_dir()
        subprocess.run(['/usr/bin/mount', '-t', 'tmpfs', '-o', 'mode=755,nosuid,nodev',
                        'plugin-fixture', target], check=True)
    subprocess.run(['/usr/sbin/ip', 'link', 'set', 'lo', 'up'], check=True)
    # Login shells consult passwd instead of HOME. Redirect only this namespace's
    # passwd entry to the disposable home containing stale public endpoint values.
    login_home = ROOT / 'login-home'
    login_home.mkdir()
    for rc in ('.bash_profile', '.zshenv', '.zprofile'):
        (login_home / rc).write_text("export DOCKER_HOST='unix:///stale-snapshot-fixture.sock'\nexport CONTAINER_HOST='unix:///stale-snapshot-fixture.sock'\n")
    passwd = []
    for line in pathlib.Path('/etc/passwd').read_text().splitlines():
        fields = line.split(':')
        if len(fields) == 7 and fields[2] == str(OWNER['uid']):
            fields[5] = str(login_home)
            line = ':'.join(fields)
        passwd.append(line)
    private_passwd = ROOT / 'fixture-passwd'
    private_passwd.write_text('\n'.join(passwd) + '\n')
    private_passwd.chmod(0o644)
    subprocess.run(['/usr/bin/mount', '--bind', str(private_passwd), '/etc/passwd'], check=True)
    services = []
    for name in CASES:
        ref = reference(name)
        registry = pathlib.Path('/var/lib/agent-workflow/runtimes') / ref['name']
        registry.mkdir(parents=True)
        launcher = registry / 'launcher'
        if name == 'nonzero':
            launcher.write_text('#!/bin/sh\nexit 17\n')
        elif name == 'malformed_helper':
            launcher.write_text('#!/bin/sh\nprintf malformed-output\n')
        elif name != 'missing':
            shutil.copyfile(ROOT / 'launcher', launcher)
        if launcher.exists():
            launcher.chmod(0o755)
        services.append(StatusFixture(name))
    try:
        process = subprocess.run(['/usr/bin/python3', str(ROOT / 'driver.py'), '--worker-user',
                                  '--fixture', str(ROOT)], env={'PATH': '/usr/bin:/bin', 'LANG': 'C.UTF-8'},
                                 user=OWNER['uid'], group=OWNER['gid'], extra_groups=OWNER['groups'], timeout=300)
        report_path = ROOT / 'report.json'
        if report_path.exists():
            report = json.loads(report_path.read_text())
            report['status_fixture_requests'] = {CASES[i]: service.requests for i, service in enumerate(services)}
            report['api_calls'] = {CASES[i]: service.api.count if service.api else 0 for i, service in enumerate(services)}
            report['helper_peer_contexts'] = {CASES[i]: service.peer_contexts for i, service in enumerate(services)}
            snapshot = services[CASES.index('snapshot')].peer_contexts
            if not (snapshot and snapshot[0].get('env', {}).get('DOCKER_HOST') == 'unix:///stale-snapshot-fixture.sock'
                    and snapshot[0]['env'].get('CONTAINER_HOST') == 'unix:///stale-snapshot-fixture.sock'
                    and json.loads(snapshot[0]['env'][MARKER]) == reference('snapshot')):
                report['status'] = 'failed'
                report['error'] = 'native stale public hook selectors were not independently observed'
            elif report['status'] == 'observations_verified':
                report['checks'].append('native login/snapshot gives real helper stale public selectors and intact admitted reference; tool receives repaired endpoints')
            report_path.write_text(json.dumps(report, indent=2))
            os.chown(report_path, OWNER['uid'], OWNER['gid'])
        return process.returncode if report_path.exists() and report['status'] == 'observations_verified' else 1
    finally:
        for service in services:
            service.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run-isolated-local-fake', action='store_true',
                        help='explicitly authorize privileged isolated local-fake proof')
    parser.add_argument('--codex', help='installed Codex 0.164.0 binary; never uses normal config/auth')
    parser.add_argument('--evidence-parent', help='private parent for a newly created owned evidence directory')
    parser.add_argument('--worker-root', action='store_true', help=argparse.SUPPRESS)
    parser.add_argument('--worker-user', action='store_true', help=argparse.SUPPRESS)
    parser.add_argument('--fixture', help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.worker_root:
        return worker_root(args)
    if args.worker_user:
        return worker_user(args)
    if not args.run_isolated_local_fake:
        parser.error('requires --run-isolated-local-fake; no checks were run')
    if 'CI' in os.environ or sys.platform != 'linux' or os.getuid() == 0:
        parser.error('requires a non-root Linux caller outside CI')
    if not args.codex:
        parser.error('--codex is required')
    binary = pathlib.Path(args.codex).resolve(strict=True)
    repository = pathlib.Path(__file__).resolve().parents[2]
    evidence = pathlib.Path(tempfile.mkdtemp(prefix='aw-production-plugin-', dir=args.evidence_parent))
    evidence.chmod(0o700)
    shutil.copyfile(__file__, evidence / 'driver.py')
    shutil.copytree(repository / 'plugins/network-runtime', evidence / 'production-plugin')
    distribution = evidence / 'distribution'
    (distribution / '.agents/plugins').mkdir(parents=True)
    shutil.copyfile(repository / '.agents/plugins/marketplace.json', distribution / '.agents/plugins/marketplace.json')
    shutil.copytree(evidence / 'production-plugin', distribution / 'plugins/network-runtime')
    subprocess.run(['go', 'build', '-o', str(evidence / 'launcher'), './cmd/agent-workflow-runtime'],
                   cwd=repository, check=True)
    version = 'codex-cli 0.164.0'  # Verified only after entering the isolated namespace.
    sha = lambda path: hashlib.sha256(path.read_bytes()).hexdigest()
    owner = {'uid': os.getuid(), 'gid': os.getgid(), 'groups': os.getgroups(),
             'host_network_namespace': os.readlink('/proc/self/ns/net'), 'codex_binary': str(binary),
             'codex_version': version, 'codex_binary_sha256': sha(binary),
             'helper_sha256': sha(evidence / 'launcher'),
             'marketplace_sha256': sha(distribution / '.agents/plugins/marketplace.json'),
             'driver_sha256': sha(evidence / 'driver.py'),
             'asset_sha256': {str(path.relative_to(evidence / 'production-plugin')): sha(path)
                              for path in sorted((evidence / 'production-plugin').rglob('*')) if path.is_file()}}
    (evidence / 'owner.json').write_text(json.dumps(owner, indent=2))
    print('PRIVATE_EVIDENCE=' + str(evidence), flush=True)
    command = ['sudo', '-n', '/usr/bin/unshare', '--net', '--mount', '--pid', '--fork', '--kill-child=SIGKILL', '--mount-proc',
               '/usr/bin/python3', str(evidence / 'driver.py'), '--worker-root', '--fixture', str(evidence)]
    with (evidence / 'driver.log').open('w') as output:
        result = subprocess.run(command, stdout=output, stderr=subprocess.STDOUT, timeout=330)
    report_path = evidence / 'report.json'
    if not report_path.exists():
        raise RuntimeError('fixture failed before report; see private driver.log in ' + str(evidence))
    report = json.loads(report_path.read_text())
    summary = {key: report.get(key) for key in ('status', 'error', 'checks', 'api', 'runtime_status',
               'codex_version', 'codex_binary_sha256', 'helper_sha256', 'asset_sha256', 'marketplace_sha256', 'driver_sha256',
               'host_login_used', 'real_model_calls', 'external_default_route')}
    summary['behavioral_cases'] = len(report['cases'])
    summary['fixture_model_requests'] = sum(case['fixture_model_requests'] for case in report['cases'])
    (evidence / 'sanitized-summary.json').write_text(json.dumps(summary, indent=2))
    print(json.dumps(summary, indent=2))
    return result.returncode


if __name__ == '__main__':
    raise SystemExit(main())
