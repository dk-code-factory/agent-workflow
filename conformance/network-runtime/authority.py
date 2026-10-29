#!/usr/bin/env python3
"""Model-free owned authority probes, invoked only by opt-in acceptance.mjs."""
import json
import os
import pathlib
import stat
import subprocess
import sys


def cold_prepare(runtime, name, uid, gid, groups, original_namespace):
    # Fail before mounts unless unshare established an independent, private view.
    assert os.readlink('/proc/self/ns/mnt') != original_namespace
    assert not any('shared:' in line.split(' - ', 1)[0]
                   for line in pathlib.Path('/proc/self/mountinfo').read_text().splitlines())
    for target in ['/var/lib', '/run']:
        subprocess.run(['mount', '-t', 'tmpfs', 'tmpfs', target], check=True)
    os.umask(0o077)
    subprocess.run([runtime, 'prepare', name, '--uid', str(uid), '--socket-boundary'], check=True, timeout=20)
    base = pathlib.Path('/var/lib/agent-workflow')
    registry = base / 'runtimes'
    installed = registry / name
    modes = {}
    for path in [base, registry, installed, pathlib.Path('/run/agent-workflow-runtimes')]:
        info = path.stat()
        assert info.st_uid == 0 and stat.S_IMODE(info.st_mode) == 0o755, path
        modes[str(path)] = oct(stat.S_IMODE(info.st_mode))
    launcher = installed / 'launcher'
    assert launcher.stat().st_uid == 0 and stat.S_IMODE(launcher.stat().st_mode) == 0o755
    for leaf in ['registration.json', 'owner.lock']:
        assert stat.S_IMODE((installed / leaf).stat().st_mode) == 0o600, leaf

    def caller():
        os.setgroups(groups)
        os.setgid(gid)
        os.setuid(uid)

    result = subprocess.run([str(launcher)], preexec_fn=caller,
                            capture_output=True, timeout=10)
    assert result.returncode == 1 and b'usage: agent-workflow-runtime' in result.stderr
    return dict(coldManagedPaths=modes, launcherMode='0o755', privateStateMode='0o600',
                enrolledUserExecuted=True, operatorUmask='0o077', privateMounts=True)


def group_parity(runtime, name, uid, gid, groups, namespace):
    parent = (os.getuid(), os.getgid(), os.getgroups())
    program = '''import json,os
mask=os.umask(0);os.umask(mask)
print(json.dumps(dict(uid=os.getuid(),euid=os.geteuid(),gid=os.getgid(),egid=os.getegid(),
groups=os.getgroups(),umask=mask,netns=os.readlink('/proc/self/ns/net'))))'''
    cases = [('ordinary', uid, gid, groups, 0o077),
             ('empty', uid, gid, [], 0o027),
             ('large', uid, gid, list(range(100000, 120000)), 0o022),
             ('root', 0, parent[1], parent[2], 0o077),
             ('root-empty', 0, parent[1], [], 0o007)]
    observations = []
    for label, caller_uid, caller_gid, caller_groups, mask in cases:
        def caller():
            os.setgroups(caller_groups)
            os.setgid(caller_gid)
            os.setuid(caller_uid)
            os.umask(mask)

        def observe(command):
            return json.loads(subprocess.check_output(command, preexec_fn=caller,
                                                      timeout=30))

        command = ['/usr/bin/python3', '-I', '-c', program]
        native = observe(command)
        contained = observe([runtime, 'exec', name, '--'] + command)
        assert native['groups'] == sorted(caller_groups), label
        assert contained['netns'] == namespace and native['netns'] != namespace, label
        expected = dict(native, netns=namespace)
        assert contained == expected, label
        assert parent == (os.getuid(), os.getgid(), os.getgroups()), 'root parent authority changed'
        observations.append(dict(case=label, native=native, contained=contained))
    return dict(cases=observations, allGroupsCompared=True, rootParentUnchanged=True)


if __name__ == '__main__':
    assert os.geteuid() == 0, 'explicit root fixture required'
    mode, runtime, name, uid, gid, groups, namespace = sys.argv[1:]
    function = {'cold-prepare': cold_prepare, 'groups': group_parity}[mode]
    print(json.dumps(function(runtime, name, int(uid), int(gid), json.loads(groups), namespace)))
