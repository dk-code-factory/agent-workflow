"""Owned, disposable runtime services for release failure scenarios only."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time


def service_environment(spec):
    return dict(spec['environment'])


def control(path, action):
    spec = json.loads(path.read_text())
    root = path.parent
    env = service_environment(spec)
    if action == 'stop':
        pid = spec.get('pid')
        if pid:
            cmdline = Path('/proc/' + str(pid) + '/cmdline')
            if cmdline.exists():
                if str(root).encode() not in cmdline.read_bytes():
                    raise ValueError('runtime PID no longer belongs to this fixture')
                subprocess.run((['sudo'] if spec['runtime'] == 'docker' else []) + ['kill', '-TERM', str(pid)], check=True, capture_output=True)
                deadline = time.monotonic() + 30
                while cmdline.exists() and cmdline.read_bytes() and time.monotonic() < deadline:
                    time.sleep(.1)
                if cmdline.exists() and cmdline.read_bytes():
                    raise ValueError('owned runtime did not stop')
        return
    if action != 'start':
        raise ValueError('unknown runtime action')
    log = (root / 'service.log').open('ab')
    process = subprocess.Popen(spec['command'], env={k:v for k,v in env.items() if k not in ['CONTAINER_HOST','DOCKER_HOST']}, stdout=log, stderr=log, start_new_session=True)
    log.close()
    spec['pid'] = process.pid
    path.write_text(json.dumps(spec))
    deadline = time.monotonic() + 45
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise ValueError('disposable runtime service failed to start')
        if spec['runtime'] == 'docker' and (root / 'service.pid').exists() and (root / 'api.sock').exists():
            spec['pid'] = int((root / 'service.pid').read_text())
            path.write_text(json.dumps(spec))
            subprocess.run(['sudo', 'chown', str(os.getuid()) + ':' + str(os.getgid()), str(root / 'api.sock')], check=True, capture_output=True)
        result = subprocess.run([spec['runtime'], 'info'], env=env, capture_output=True, timeout=5)
        if result.returncode == 0:
            spec['pid'] = int((root / 'service.pid').read_text()) if spec['runtime'] == 'docker' else process.pid
            path.write_text(json.dumps(spec))
            return
        time.sleep(.2)
    control(path, 'stop')
    raise ValueError('disposable runtime service did not become ready')


class DisposableRuntime:
    def __init__(self, state, runtime, environment):
        self.environment = dict(environment)
        self.runtime = runtime
        self.root = Path(tempfile.mkdtemp(prefix='runtime-' + runtime + '-', dir=state)) if runtime else None
        self.control = None

    def __enter__(self):
        if not self.runtime:
            return self
        root = self.root
        if self.runtime == 'docker':
            endpoint = 'unix://' + str(root / 'api.sock')
            (root / 'daemon.json').write_text('{}')
            command = ['sudo', shutil.which('dockerd'), '--config-file', str(root / 'daemon.json'), '--data-root', str(root / 'data'), '--exec-root', str(root / 'exec'),
                       '--pidfile', str(root / 'service.pid'), '--host', endpoint, '--bridge', 'awe2e' + root.name[-6:],
                       '--bip', '172.29.0.1/24', '--default-address-pool', 'base=172.28.0.0/16,size=24']
            selected = {'DOCKER_HOST': endpoint}
        else:
            if os.getuid() == 0:
                raise ValueError('Podman E2E requires a non-root runner')
            endpoint = 'unix://' + str(root / 'api.sock')
            command = [shutil.which('podman'), '--root', str(root / 'storage'), '--runroot', str(root / 'run'),
                       '--storage-driver', 'vfs', '--tmpdir', str(root / 'tmp'), '--network-config-dir', str(root / 'networks'), 'system', 'service', '--time', '0', endpoint]
            selected = {'CONTAINER_HOST': endpoint, 'XDG_CONFIG_HOME': str(root / 'config'), 'XDG_DATA_HOME': str(root / 'user-data')}
        self.environment.update(selected)
        spec = root / 'service.json'
        pinned_env = {k:v for k,v in os.environ.items() if k in ['PATH','HOME','USER','LOGNAME','XDG_RUNTIME_DIR','LANG']}
        pinned_env.update(selected)
        spec.write_text(json.dumps({'runtime': self.runtime, 'command': command, 'environment': pinned_env}))
        self.control = root / 'control'
        # Paths are serialized with repr, never interpolated into shell code.
        self.control.write_text('#!' + sys.executable + '\nimport sys\nsys.path.insert(0, ' + repr(str(Path(__file__).parent)) + ')\nfrom release_runtime import control\nfrom pathlib import Path\ncontrol(Path(' + repr(str(spec)) + '), sys.argv[1])\n')
        self.control.chmod(0o700)
        try:
            control(spec, 'start')
        except BaseException:
            self.__exit__(None, None, None)
            raise
        return self

    def __exit__(self, *args):
        if not self.root:
            return
        control(self.root / 'service.json', 'stop')
        if self.runtime == 'docker':
            bridge = 'awe2e' + self.root.name[-6:]
            subprocess.run(['sudo', 'ip', 'link', 'delete', bridge], capture_output=True)
        # Each service has exclusive storage, so release it before the next row.
        subprocess.run(['sudo', 'rm', '-rf', '--', str(self.root)], check=True, capture_output=True)
