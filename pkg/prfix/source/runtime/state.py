"""Small, host-independent task cache and retained workflow budgets (stdlib only)."""
from contextlib import contextmanager
from copy import deepcopy
from pathlib import Path
from urllib.parse import urlsplit
import base64
import fcntl
import hashlib
import json
import mmap
import os
import re
import stat
import subprocess
import time
import uuid
import invocation
from shared.private_state import write as write_private
from shared.storage_paths import location

CONTRACT = 2
REVIEW_CYCLES = 5


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':'),
                                     ensure_ascii=True, allow_nan=False).encode()).hexdigest()


def has_output_content(original):
    return bool(original.decode('utf-8', errors='surrogateescape').strip())


def read(path):
    return json.loads(Path(path).read_text())


def write(path, value):
    """Durable replace; a failed write never clears an existing claim."""
    write_private(path, value, ensure_ascii=True)


def git(checkout, *args):
    return subprocess.check_output(['git', '-C', str(checkout), *args], stderr=subprocess.PIPE)


def identity(repository, pr):
    repository = repository.lower().removeprefix('remote:').removesuffix('.git')
    if not re.fullmatch(r'[a-z0-9.-]+/[a-z0-9_.-]+/[a-z0-9_.-]+', repository):
        raise ValueError('repository must be HOST/OWNER/REPO')
    url = urlsplit(pr)
    parts = url.path.rstrip('/').split('/')
    if (url.scheme != 'https' or url.username or url.password or url.query or url.fragment
            or len(parts) != 5 or parts[3] != 'pull' or not parts[4].isdigit()
            or int(parts[4]) <= 0 or '/'.join([url.netloc.lower(), parts[1].lower(), parts[2].lower()]) != repository):
        raise ValueError('PR URL must identify the canonical repository')
    return {'repository': repository, 'pr': f'https://{repository}/pull/{int(parts[4])}'}


def make_spec(*, repository, pr, skill_identity, resources, responsibility,
              assigned_instructions, instructions, inputs, options, git=None,
              governing_inputs_complete=False, cacheable=None, native_review=None):
    """Build an ordinary task spec from explicit inputs, without capturing them.

    Callers supply the complete governing resource/input maps. Generation does
    not discover resources, resolve Git refs, or add invocation/progress data.
    The normal command boundaries still capture and validate live resources.
    """
    subject = identity(repository, pr)
    invocation.skill_name(skill_identity)
    for name, value in (('skill identity', skill_identity), ('responsibility', responsibility),
                        ('assigned instructions', assigned_instructions)):
        if not isinstance(value, str) or not value:
            raise ValueError('Nonempty ' + name + ' is required')

    def paths(values):
        if not isinstance(values, dict) or any(not isinstance(key, str) for key in values):
            raise ValueError('Expected a label-to-resource-path map')
        result = {}
        for key, value in values.items():
            path = os.fspath(value)
            if not isinstance(path, str) or not path:
                raise ValueError('Resource paths must be nonempty strings')
            result[key] = path
        return result

    resources, instructions, inputs = paths(resources), paths(instructions), paths(inputs)
    if type(governing_inputs_complete) is not bool:
        raise ValueError('governing_inputs_complete must be boolean')
    if cacheable is None:
        cacheable = bool(resources) and governing_inputs_complete
    if type(cacheable) is not bool:
        raise ValueError('cacheable must be boolean')
    if cacheable and (not resources or not governing_inputs_complete):
        raise ValueError('Cache requires captured resources and complete governing inputs; use cacheable:false otherwise')
    if not isinstance(options, dict):
        raise ValueError('options must be an object')
    if 'invocation' in options:
        invocation.validate(options['invocation'])
    spec = {**subject, 'skill': {'identity': skill_identity, 'resources': resources},
            'task': {'id': responsibility, 'instructions': assigned_instructions},
            'instructions': instructions, 'inputs': inputs, 'options': deepcopy(options),
            'governing_inputs_complete': governing_inputs_complete, 'cacheable': cacheable}
    if git is not None:
        if not isinstance(git, dict):
            raise ValueError('git must be an object')
        source = deepcopy(git)
        source['checkout'] = os.fspath(source['checkout'])
        for key in ('checkout', 'target', 'base', 'head'):
            if not isinstance(source[key], str) or not source[key]:
                raise ValueError('Git ' + key + ' must be a nonempty string')
        for option in ('history', 'head_sensitive'):
            if type(source.get(option, False)) is not bool:
                raise ValueError(option + ' must be boolean')
        spec['git'] = source
    if native_review is not None:
        from native_review import contract
        spec['native_review'] = deepcopy(contract(native_review))
    digest(spec)  # Reject values that cannot survive the JSON task-spec contract.
    return spec


def resource(path):
    """Hash exact installed resource bytes, including directory names and symlink targets."""
    path = Path(path).absolute()
    def visit(item, ancestors):
        resolved = item.resolve(strict=True)
        if resolved in ancestors:
            raise ValueError('Cyclic resource symlink')
        if item.is_dir():
            return {'kind': 'directory', 'link': str(resolved) if item.is_symlink() else None,
                    'children': {p.name: visit(p, ancestors | {resolved}) for p in sorted(item.iterdir())
                                 if p.name not in {'.git', '__pycache__', '.pytest_cache'}}}
        if not item.is_file():
            raise ValueError('Resource is not a regular file: ' + str(item))
        return {'kind': 'file', 'sha256': hashlib.sha256(item.read_bytes()).hexdigest(),
                'executable': bool(item.stat().st_mode & 0o111),
                'link': str(resolved) if item.is_symlink() else None}
    return visit(path, set())


def _packed_refs_safe(path, names):
    """Fresh bounded lookups in Git's sorted packed-ref representation.

    Unknown formats decline sharing. Mapping avoids a Python copy of the file;
    each binary-search step reads one ref and its optional peeled row.
    """
    if not path.is_file():
        return False
    with path.open('rb') as source:
        header = source.readline(256)
        if not header:
            return True
        if (not header.endswith(b'\n') or not header.startswith(b'# pack-refs with: ')
                or b'sorted' not in header[len(b'# pack-refs with: '):].split()):
            return False
        with mmap.mmap(source.fileno(), 0, access=mmap.ACCESS_READ) as data:
            start, end = len(header), len(data)
            if data[-1:] != b'\n':
                return False

            def record(offset):
                stop = data.find(b'\n', offset)
                if stop < 0:
                    raise ValueError('Unterminated packed ref')
                oid, separator, name = data[offset:stop].partition(b' ')
                if (not separator or not re.fullmatch(rb'[0-9a-f]{40}|[0-9a-f]{64}', oid)
                        or not name.startswith(b'refs/')):
                    raise ValueError('Unsupported packed ref')
                following = stop + 1
                if data[following:following+1] == b'^':
                    stop = data.find(b'\n', following)
                    if stop < 0:
                        raise ValueError('Unterminated peeled ref')
                    following = stop + 1
                return name, following

            def lower_bound(wanted):
                low, high = start, end
                while low < high:
                    middle = (low + high)//2
                    boundary = data.rfind(b'\n', low, middle)
                    offset = low if boundary < 0 else boundary + 1
                    if data[offset:offset+1] == b'^':
                        boundary = data.rfind(b'\n', low, offset - 1)
                        offset = low if boundary < 0 else boundary + 1
                    name, following = record(offset)
                    if name < wanted:
                        low = following
                    else:
                        high = offset
                return record(low)[0] if low < end else b''

            if lower_bound(b'refs/replace/').startswith(b'refs/replace/'):
                return False
            return all(lower_bound(name) != name for name in names)


class _GitContextGuard:
    """Observe context versions without retaining a path-based content cache."""
    def __init__(self):
        self.environment = tuple(sorted(os.environ.items()))
        self.versions = {}

    @staticmethod
    def version(path):
        def stamp(value):
            return (value.st_dev, value.st_ino, value.st_mode, value.st_nlink,
                    value.st_size, value.st_mtime_ns, value.st_ctime_ns)
        try:
            own = path.lstat()
        except FileNotFoundError:
            return None
        target = None
        if stat.S_ISLNK(own.st_mode):
            try:
                target = stamp(path.stat())
            except FileNotFoundError:
                pass
        return stamp(own), target

    def watch(self, path):
        path = Path(path).absolute()
        # Ancestors also detect creation/removal of absent inputs and temporary
        # symlink or directory replacement that restores the original bytes.
        for candidate in (path, path.resolve()):
            for item in (candidate, *candidate.parents):
                if item not in self.versions:
                    self.versions[item] = self.version(item)
        return path

    def stable(self):
        try:
            return (self.environment == tuple(sorted(os.environ.items()))
                    and all(self.version(path) == version for path, version in self.versions.items()))
        except (OSError, ValueError, RuntimeError):
            return False


def _git_context(source, *, _guard=None):
    """Cheap context check for ordinary repositories and linked worktrees.

    Unusual Git layouts and indirection fall back to independent captures. This
    is only a command-local optimization; resource capture never uses this key.
    """
    if (os.environ.get('XDG_CONFIG_HOME') and not Path(os.environ['XDG_CONFIG_HOME']).is_absolute()
            or any('\n' in os.environ.get(name, '') or '\r' in os.environ.get(name, '')
                   for name in ('HOME', 'XDG_CONFIG_HOME', 'GIT_CONFIG_GLOBAL', 'GIT_CONFIG_SYSTEM'))):
        return None
    if any(name in os.environ for name in (
            'GIT_DIR', 'GIT_COMMON_DIR', 'GIT_WORK_TREE', 'GIT_OBJECT_DIRECTORY',
            'GIT_ALTERNATE_OBJECT_DIRECTORIES',
            'GIT_CONFIG_COUNT', 'GIT_CONFIG_PARAMETERS', 'GIT_REPLACE_REF_BASE',
            'GIT_SHALLOW_FILE', 'GIT_GRAFT_FILE', 'GIT_INDEX_FILE', 'GIT_NAMESPACE',
            'GIT_ATTR_SOURCE')):
        return None
    guard = _guard or _GitContextGuard()
    try:
        checkout = guard.watch(source['checkout']).resolve()
        directory = guard.watch(checkout/'.git')
        if directory.is_file():
            value = os.fsdecode(directory.read_bytes().rstrip(b'\r\n'))
            if not value.startswith('gitdir: '):
                return None
            directory = guard.watch(checkout/value[8:]).resolve()
        if not directory.is_dir():
            return None
        common = directory
        metadata = guard.watch(directory/'commondir')
        if metadata.is_file():
            common = guard.watch(directory/os.fsdecode(metadata.read_bytes().rstrip(b'\r\n'))).resolve()
        for path in (common/'refs/replace', common/'info/grafts', common/'shallow'):
            guard.watch(path)
            if path.exists() and (not path.is_dir() or any(path.iterdir())):
                return None
        # A hex spelling may also name a mutable ref. Do not treat that
        # ambiguous spelling as an immutable object input, even if Git happens
        # to prefer the object on this version.
        ref_names = {name for pin in (source[key] for key in ('target', 'base', 'head'))
                     for name in (pin, 'refs/'+pin, 'refs/tags/'+pin, 'refs/heads/'+pin,
                                  'refs/remotes/'+pin, 'refs/remotes/'+pin+'/HEAD')}
        if any(guard.watch(root/name).exists() for root in (directory, common) for name in ref_names):
            return None
        packed = guard.watch(common/'packed-refs')
        if packed.exists() and not _packed_refs_safe(packed, {name.encode() for name in ref_names}):
            return None
        # Ask this Git installation for its actual paths, including relocatable
        # system prefixes and global overrides. Older Git without these vars
        # takes the ordinary independent-capture path.
        variables = {key: [] for key in ('GIT_CONFIG_SYSTEM', 'GIT_CONFIG_GLOBAL',
                                         'GIT_ATTR_SYSTEM', 'GIT_ATTR_GLOBAL')}
        # Local/global configuration can influence Git's path-variable receipt.
        # Register known paths before invoking Git, then the discovered paths
        # before reading their contents.
        for path in (common/'config', directory/'config.worktree'):
            guard.watch(path)
        if 'GIT_CONFIG_GLOBAL' not in os.environ:
            for path in (Path.home()/'.gitconfig',
                         Path(os.environ.get('XDG_CONFIG_HOME') or Path.home()/'.config')/'git/config'):
                guard.watch(path)
        for name in ('GIT_CONFIG_SYSTEM', 'GIT_CONFIG_GLOBAL'):
            if name in os.environ:
                guard.watch(os.environ[name])
        for line in git(checkout, 'var', '-l').splitlines():
            key, separator, value = line.partition(b'=')
            name = key.decode(errors='replace')
            if separator and name in variables:
                variables[name].append(Path(os.fsdecode(value)))
        for key, disabled in (('GIT_CONFIG_SYSTEM', 'GIT_CONFIG_NOSYSTEM'),
                              ('GIT_ATTR_SYSTEM', 'GIT_ATTR_NOSYSTEM')):
            if (os.environ.get(disabled, '').lower() not in {'1', 'true', 'yes', 'on'}
                    and len(variables[key]) != 1):
                return None
        if not variables['GIT_CONFIG_GLOBAL'] or len(variables['GIT_ATTR_GLOBAL']) != 1:
            return None
        if any(not path.is_absolute() for paths in variables.values() for path in paths):
            return None
        configs = [*variables['GIT_CONFIG_SYSTEM'], *variables['GIT_CONFIG_GLOBAL'],
                   common/'config', directory/'config.worktree']
        snapshots = []
        for path in configs:
            guard.watch(path)
            content = path.read_bytes() if path.exists() else None
            # Git skips an initial UTF-8 BOM. Indirect configuration, alternate
            # attribute roots and signature verification need a wider context.
            normalized = content.removeprefix(b'\xef\xbb\xbf') if content else b''
            if (re.search(rb'(?i)\[\s*(?:include|attr)', normalized)
                    or any(word in normalized.lower() for word in
                           (b'refstorage', b'attributesfile', b'orderfile', b'submodule', b'worktree'))
                    or (source.get('history') and b'showsignature' in normalized.lower())):
                return None
            snapshots.append((str(path.resolve()), content))
        # A committed diff still reads attributes from the working tree. Probe
        # changed paths, then read only their attribute ancestry, not the tree.
        names = git(checkout, '-c', 'diff.renames=false', 'diff', '--name-only', '-z',
                    '--no-relative', '--no-ext-diff', '--no-textconv', '--no-renames',
                    source['base'], source['head'], '--')
        attributes = {common/'info/attributes', directory/'info/attributes',
                      *variables['GIT_ATTR_SYSTEM'], *variables['GIT_ATTR_GLOBAL']}
        for name in names.split(b'\0'):
            if not name:
                continue
            parent = checkout/Path(os.fsdecode(name)).parent
            if not parent.is_relative_to(checkout):
                return None
            while parent.is_relative_to(checkout):
                attributes.add(parent/'.gitattributes')
                parent = parent.parent
        for path in sorted(attributes):
            guard.watch(path)
            snapshots.append((str(path.resolve()), path.read_bytes() if path.exists() else None))
        # Indexed attributes and checkout submodule configuration also affect diffs.
        for path in (directory/'index', checkout/'.gitmodules'):
            guard.watch(path)
            file_hash = None
            if path.exists():
                with path.open('rb') as source_file:
                    file_hash = hashlib.file_digest(source_file, 'sha256').digest()
            snapshots.append((str(path.resolve()), file_hash))
        if not guard.stable():
            return None
        return (str(checkout), str(directory.resolve()), str(common), tuple(snapshots),
                hashlib.sha256(names).digest(),
                tuple(sorted(os.environ.items())))
    except (OSError, ValueError, RuntimeError, subprocess.CalledProcessError):
        return None


def _capture_git(source, *, batched=False):
    checkout = source['checkout']
    def resolve(values, kind):
        if batched:
            resolved = git(checkout, 'rev-parse', '--revs-only', '--end-of-options',
                           *(value+'^{'+kind+'}' for value in values.values())).decode().splitlines()
            if len(resolved) != len(values) or any(not re.fullmatch(r'[0-9a-f]{40}|[0-9a-f]{64}', sha)
                                                   for sha in resolved):
                raise ValueError('Git did not resolve every pinned '+kind)
            return dict(zip(values, resolved))
        resolved = {}
        for key, value in values.items():
            args = ('--verify',) if kind == 'commit' else ()
            resolved[key] = git(checkout, 'rev-parse', *args, value + '^{'+kind+'}').decode().strip()
        return resolved
    commits = resolve({key: source[key] for key in ('target', 'base', 'head')}, 'commit')
    trees = resolve(commits, 'tree')
    diff = git(checkout, '-c', 'diff.renames=false', 'diff', '--binary', '--full-index',
               '--no-ext-diff', '--no-textconv', '--no-relative', '--no-renames', commits['base'], commits['head'], '--')
    inputs = {'trees': trees, 'diff_sha256': hashlib.sha256(diff).hexdigest()}
    for option in ('history', 'head_sensitive'):
        if type(source.get(option, False)) is not bool:
            raise ValueError(option + ' must be boolean')
    if source.get('history'):
        history = git(checkout, 'log', '--format=raw', '--no-abbrev', '--no-decorate', '--no-notes',
                      commits['target'] + '..' + commits['head'])
        inputs['history'] = hashlib.sha256(history).hexdigest()
    if source.get('head_sensitive'):
        inputs['commits'] = commits
    return inputs, {'checkout': str(Path(checkout).resolve()), 'commits': commits}


class _CaptureContext:
    """Share only identical immutable Git calculations within one command."""
    def __init__(self):
        self.git_captures = {}

    def git(self, source):
        pins = tuple(source[key] for key in ('target', 'base', 'head'))
        options = tuple(source.get(key, False) for key in ('history', 'head_sensitive'))
        context = None
        guard = _GitContextGuard()
        if (all(isinstance(pin, str) and re.fullmatch(r'(?:[0-9a-f]{40}|[0-9a-f]{64})', pin)
                for pin in pins) and all(type(value) is bool for value in options)):
            context = _git_context(source, _guard=guard)
        if context is None:
            return _capture_git(source)
        key = context, pins, options
        if key not in self.git_captures:
            captured = _capture_git(source, batched=True)
            if tuple(captured[1]['commits'][name] for name in ('target', 'base', 'head')) != pins:
                return captured
            if guard.stable():
                self.git_captures[key] = captured
            return deepcopy(captured)
        captured = self.git_captures[key]
        if not guard.stable():
            return _capture_git(source)
        return deepcopy(captured)


def capture(spec, *, _context=None):
    if 'skill_options_configuration' in spec:
        from skill_config import check_options
        check_options(spec)
    if 'skill_configuration' in spec:
        from skill_config import check_selection as check_skill_selection
        check_skill_selection(spec)
    if 'configuration' in spec:
        from config import check_selection
        check_selection(spec)
    if 'invocation' in spec.get('options', {}):
        invocation.validate(spec['options']['invocation'])
    subject = identity(spec['repository'], spec['pr'])
    task, skill = spec['task'], spec['skill']
    invocation.skill_name(skill.get('identity') if isinstance(skill, dict) else None)
    if (not isinstance(task, dict) or not isinstance(task.get('id'), str) or not task['id']
            or not isinstance(task.get('instructions'), str) or not task['instructions']
            or not isinstance(skill, dict) or not isinstance(skill.get('identity'), str) or not skill['identity']):
        raise ValueError('Task id, assigned instructions and skill identity are required')
    def files(values):
        if not isinstance(values, dict) or any(not isinstance(k, str) for k in values):
            raise ValueError('Expected a label-to-resource-path map')
        return {key: resource(value) for key, value in sorted(values.items())}
    resources = files(skill.get('resources', {}))
    cacheable = spec.get('cacheable', True)
    if type(cacheable) is not bool:
        raise ValueError('cacheable must be boolean')
    if cacheable and (not resources or spec.get('governing_inputs_complete') is not True):
        raise ValueError('Cache requires captured resources and complete governing inputs; use cacheable:false otherwise')
    inputs = files(spec.get('inputs', {}))
    governing = files(spec.get('instructions', {}))
    provenance = {}
    if 'git' in spec:
        git_inputs, provenance = (_context.git(spec['git']) if _context is not None
                                  else _capture_git(spec['git']))
        if 'git' in inputs:
            # Input labels are unrestricted; retain a resource named git too.
            git_inputs = {**git_inputs, 'resource': inputs['git']}
        inputs['git'] = git_inputs
    fingerprint_inputs = {'contract': CONTRACT, **subject, 'task': task, 'skill': {
        'identity': skill['identity'], 'resources': resources}, 'instructions': governing,
        'inputs': inputs, 'options': {key: value for key, value in spec.get('options', {}).items()
                                      if key != 'hook_phase'}}
    if 'native_review' in spec:
        from native_review import contract
        fingerprint_inputs['native_review'] = deepcopy(contract(spec['native_review']))
    key = digest({**subject, 'task': task['id'], 'skill': skill['identity']})
    return {'key': key, 'fingerprint': digest(fingerprint_inputs), 'components': fingerprint_inputs,
            'provenance': provenance, 'cacheable': cacheable, 'subject': subject,
            'hook_phase': deepcopy(spec.get('options', {}).get('hook_phase'))}


def _pointer(parts):
    return ''.join('/' + str(part).replace('~', '~0').replace('/', '~1') for part in parts)


def _component_changes(old, new, parts=()):
    """Compare captured values, exposing identities rather than instruction bodies."""
    if digest(old) == digest(new):
        return

    def summary(value, path):
        # Content/Git hashes are already safe, useful identities. All other
        # values (including assigned instructions and option strings) are hashed.
        if ((path and path[0] in {'skill', 'instructions', 'inputs'} and path[-1] == 'sha256'
             or path[:2] == ('inputs', 'git'))
                and isinstance(value, str) and re.fullmatch(r'[0-9a-f]{40}|[0-9a-f]{64}', value)):
            return {'hash': value}
        return {'sha256': digest(value)}

    if isinstance(old, dict) and isinstance(new, dict):
        for key in sorted(old.keys() | new.keys()):
            path = (*parts, key)
            if key not in old:
                yield {'path': _pointer(path), 'change': 'added', 'new': summary(new[key], path)}
            elif key not in new:
                yield {'path': _pointer(path), 'change': 'removed', 'old': summary(old[key], path)}
            else:
                yield from _component_changes(old[key], new[key], path)
    else:
        yield {'path': _pointer(parts), 'change': 'changed', 'old': summary(old, parts), 'new': summary(new, parts)}


class Store:
    def __init__(self, repository, pr, *, checkout=None):
        self.subject = identity(repository, pr)
        self.root = (location('tasks')/digest(self.subject)).resolve()
        from shared.repository_scope import checkout_roots
        roots = [Path(__file__).resolve().parents[1], *checkout_roots(checkout or Path.cwd())]
        # Also reject state nested in any other Git checkout.
        probe = self.root
        while not probe.exists():
            probe = probe.parent
        roots.extend(checkout_roots(probe))
        if any(self.root.is_relative_to(p) for p in roots):
            raise ValueError('Workflow state must be outside repositories and the plugin')
        self.root.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.manifest = self.root/'manifest.json'

    @contextmanager
    def locked(self):
        with (self.root/'.lock').open('a') as lock:
            os.chmod(lock.name, 0o600)
            fcntl.flock(lock, fcntl.LOCK_EX)
            manifest = read(self.manifest) if self.manifest.exists() else {
                'contract': CONTRACT, 'subject': self.subject, 'entries': {}, 'runs': {}}
            if manifest.get('contract') != CONTRACT or manifest.get('subject') != self.subject:
                raise ValueError('Unsupported or damaged manifest; do not reset retained budgets')
            yield manifest

    def saved(self, entry):
        return self._load_saved(entry)[0]

    def findings_evidence(self, entry, saved):
        """Derive effective findings and their sources from immutable reports."""
        from checklists import supplemental
        result = deepcopy(saved['normalized'])
        findings = list(result.get('findings', []))
        sources = [saved['path']]
        gate = entry.get('checklist', {})
        if gate.get('fingerprint') == saved['fingerprint'] and gate.get('result') == entry['result']['hash']:
            added, paths = supplemental(self, entry)
            seen = {digest(finding) for finding in findings}
            for finding in added:
                if digest(finding) not in seen:
                    findings.append(finding)
                    seen.add(digest(finding))
            sources.extend(paths)
        result['findings'] = findings
        return result, sources

    def _load_saved(self, entry):
        reference = entry.get('result')
        if not reference:
            return None, 'missing_result'
        try:
            path = self.root/'results'/reference['file']
            if path.parent.resolve() != (self.root/'results').resolve():
                return None, 'damaged_result'
            result = read(path)
            original = base64.b64decode(result['original'], validate=True)
            if (digest(result) != reference['hash'] or result.get('contract') != CONTRACT
                    or result['status'] not in {'complete','failed','incomplete'}
                    or type(result['normalized']['complete']) is not bool
                    or (result['status'] == 'complete') != result['normalized']['complete']
                    or (result['status'] == 'complete' and not has_output_content(original))
                    or hashlib.sha256(original).hexdigest() != result['original_sha256']
                    or result['fingerprint'] != reference['fingerprint']
                    or result['key'] != entry['key']):
                return None, 'damaged_result'
            return {'path': str(path), **result}, None
        except FileNotFoundError:
            return None, 'missing_result_artifact'
        except (ValueError, OSError, KeyError, TypeError):
            return None, 'damaged_result'

    def describe(self, captured, entry, *, explain=False):
        saved, result_problem = self._load_saved(entry)
        changed = []
        old = entry.get('components', {})
        for key, value in captured['components'].items():
            if value != old.get(key):
                changed.append(key)
        from native_review import assess, required
        native_problem = None
        if saved and saved['status'] == 'complete' and required(captured['components']):
            native = saved.get('native_execution', {})
            try:
                evidence = base64.b64decode(native['evidence'], validate=True) if native.get('evidence') is not None else None
                native_problem = assess(captured['components'], {'id': saved['claim'],
                    'fingerprint': saved['fingerprint'], 'provenance': saved['provenance']},
                    base64.b64decode(saved['original'], validate=True), evidence)
            except (ValueError, KeyError, TypeError, AttributeError):
                native_problem = {'code': 'malformed_provenance', 'reason': 'Retained native observations are damaged'}
        coverage_complete = bool(not native_problem and not entry.get('invalidated') and not entry.get('claim')
                                 and saved and saved['status'] == 'complete' and saved['fingerprint'] == captured['fingerprint'])
        analysis_complete = coverage_complete
        from checklists import contract as checklist_contract, summary
        checklist = summary(self, entry, captured)
        if 'managed' in entry:
            expected = checklist_contract(captured['components'], entry['managed']['definition'])
            if checklist is None or entry['checklist']['contract'] != expected:
                checklist = {**(checklist or {}), 'complete': False, 'accepted': False,
                             'outcome': 'refresh_required', 'required': expected['required'],
                             'max_checks': expected['max_checks']}
        if checklist is not None:
            coverage_complete = coverage_complete and checklist['accepted']
        reusable = bool(coverage_complete and captured['cacheable'] and saved.get('cacheable') is True)
        described = {'key': captured['key'], 'task': captured['components']['task']['id'],
                'reusable': reusable, 'coverage_complete': coverage_complete, 'changed': changed,
                'unfinished': entry.get('claim'), 'attempts': len(entry.get('attempts', [])),
                'result': saved['path'] if saved else None, 'completion': saved['status'] if saved else None,
                'findings': self.findings_evidence(entry, saved)[0]['findings'] if reusable else None,
                'invalidated': entry.get('invalidated'),
                'reason': ('reusable' if reusable else native_problem['code'] if native_problem else 'unfinished' if entry.get('claim') else
                           'invalidated' if entry.get('invalidated') else
                           'changed_inputs' if changed else saved['status'] if saved and saved['status'] != 'complete' else 'missing_or_incomplete_result')}
        if checklist is not None:
            described.update(analysis_complete=analysis_complete, checklist=checklist,
                             checklist_complete=bool(analysis_complete and checklist['complete']))
            if analysis_complete and not checklist['accepted']:
                described['reason'] = 'checklist_' + (checklist['outcome'] or 'pending')
        if explain:
            blockers = []
            if not captured['cacheable']:
                blockers.append('noncacheable_task')
            if entry.get('claim'):
                blockers.append('unfinished_claim')
            if entry.get('invalidated'):
                blockers.append('explicit_invalidation')
            if 'components' not in entry:
                blockers.append('no_previous_capture')
            elif digest(old) != captured['fingerprint']:
                blockers.append('changed_inputs')
            if native_problem:
                blockers.append(native_problem['code'])
            if checklist is not None and not checklist['accepted']:
                blockers.append('checklist_not_accepted')
            if result_problem:
                blockers.append(result_problem)
            elif saved:
                if saved['status'] != 'complete':
                    blockers.append(saved['status'] + '_result')
                if saved.get('cacheable') is not True:
                    blockers.append('noncacheable_result')
                if saved['fingerprint'] != captured['fingerprint']:
                    blockers.append('result_fingerprint_mismatch')
            described['explanation'] = {
                'blockers': blockers,
                'changes': list(_component_changes(old, captured['components'])),
                'previous_capture': {'manifest': str(self.manifest),
                                     'pointer': _pointer(('entries', captured['key'], 'components'))}
                                    if 'components' in entry else None,
                'current_fingerprint': captured['fingerprint'],
                'previous_capture_fingerprint': digest(old) if 'components' in entry else None,
                'result_fingerprint': saved['fingerprint'] if saved else None,
            }
        return described

    def status(self, specs, *, compact=False, run=None, explain=False, tolerate_missing=False):
        context = _CaptureContext()
        captures, unavailable = [], []
        for spec in specs:
            try:
                captures.append(capture(spec, _context=context))
            except (ValueError, KeyError, TypeError, OSError, RuntimeError) as exc:
                if not tolerate_missing:
                    raise
                unavailable.append({
                    'key': digest({**self.subject, 'task': spec['task']['id'], 'skill': spec['skill']['identity']}),
                    'reusable': False, 'coverage_complete': False,
                    'reason': 'refresh_required', 'detail': str(exc)})
        if any(c['subject'] != self.subject for c in captures):
            raise ValueError('Specs refer to another PR')
        with self.locked() as manifest:
            if run is not None and run not in manifest['runs']:
                raise ValueError('Unknown run id: ' + run)
            selected = manifest['runs'] if run is None else {run: manifest['runs'][run]}
            runs = {name: {**state, 'review_budget': self.review_budget(state)}
                    for name, state in selected.items()}
            if compact:
                runs = {
                    name: {
                        **{key: value for key, value in state.items() if key != 'progress'},
                        'cycle': state['cycles'][-1] if state['cycles'] else None,
                        'progress_count': len(state['progress']),
                        'progress_ref': {
                            'manifest': str(self.manifest),
                            'pointer': '/runs/' + name.replace('~', '~0').replace('/', '~1') + '/progress',
                        },
                    }
                    for name, state in runs.items()
                }
            return {'contract': CONTRACT, 'subject': self.subject, 'manifest': str(self.manifest),
                    'tasks': [self.describe(c, manifest['entries'].get(c['key'], {}), explain=explain) for c in captures] + unavailable,
                    'runs': runs,
                    'hook_phases': [{'key': key, 'snapshot': digest(value),
                                     'occurrence': value['occurrence']}
                                    for key, value in manifest.get('hook_phases', {}).items()
                                    if run is None or value['occurrence']['run'] == run],
                    'unfinished': [e['claim'] for e in manifest['entries'].values() if e.get('claim')]}

    def reconcile_hooks(self, phase, expected, reason):
        """Record a deliberate phase-list change after prior effects are reconciled."""
        if (not isinstance(phase, dict) or set(phase) != {'occurrence', 'hooks'}
                or not isinstance(phase['occurrence'], dict) or not isinstance(phase['hooks'], list)
                or not isinstance(reason, str) or not reason.strip()):
            raise ValueError('Hook reconciliation requires a phase snapshot and original reason')
        key = digest(phase['occurrence'])
        with self.locked() as manifest:
            previous = manifest.get('hook_phases', {}).get(key)
            if previous is None or digest(previous) != expected:
                raise ValueError('Hook phase snapshot changed; inspect current progress')
            for entry in manifest['entries'].values():
                active = entry.get('claim')
                occurrence = entry.get('components', {}).get('options', {}).get('cycle_hook', {})
                if active and all(occurrence.get(field) == value for field, value in phase['occurrence'].items()):
                    raise ValueError('Reconcile and close active hook claims before changing the list')
            manifest['hook_phases'][key] = deepcopy(phase)
            write(self.manifest, manifest)
        return {'reconciled': True, 'snapshot': digest(phase)}

    @staticmethod
    def review_budget(state):
        """Legacy runs have five cycles; only retained authorizations add more."""
        limit, requests = state.get('limits', {}).get('review_cycles', REVIEW_CYCLES), set()
        allowances = state.get('review_allowances', [])
        if not isinstance(allowances, list):
            raise ValueError('Damaged review allowance history; do not reset retained budgets')
        for allowance in allowances:
            if (not isinstance(allowance, dict)
                    or any(not isinstance(allowance.get(key), str) or not allowance[key].strip()
                           for key in ('request_id', 'authorization', 'source'))
                    or allowance['request_id'] in requests
                    or type(allowance.get('cycles')) is not int
                    or not 1 <= allowance['cycles'] <= REVIEW_CYCLES
                    or allowance.get('previous_limit') != limit
                    or allowance.get('cycle_limit') != limit + allowance['cycles']):
                raise ValueError('Damaged review allowance history; do not reset retained budgets')
            requests.add(allowance['request_id'])
            limit = allowance['cycle_limit']
        used = len(state['cycles'])
        return {'used': used, 'limit': limit, 'remaining': max(0, limit - used)}

    def continue_run(self, *, run, request_id, authorization, source, cycles=REVIEW_CYCLES):
        """Record a renewed user allowance without changing any existing work.

        The coordinating agent must supply an explicit user request, not infer
        authorization from a resume or repository content. This is bookkeeping,
        not authentication of the supplied evidence. Replays retain the original
        grant, including after later cycles have consumed it.
        """
        if any(not isinstance(value, str) or not value.strip()
               for value in (run, request_id, authorization, source)):
            raise ValueError('Run, stable request id, exact user authorization and source are required')
        if type(cycles) is not int or not 1 <= cycles <= REVIEW_CYCLES:
            raise ValueError('Continuation requires between one and five additional review cycles')
        with self.locked() as manifest:
            if run not in manifest['runs']:
                raise ValueError('Unknown run id: ' + run)
            state = manifest['runs'][run]
            budget = self.review_budget(state)
            intent = {'request_id': request_id, 'authorization': authorization,
                      'source': source, 'cycles': cycles}
            for allowance in state.get('review_allowances', []):
                if allowance['request_id'] == request_id:
                    if any(allowance[key] != value for key, value in intent.items()):
                        raise ValueError('Continuation request id already has a different authorization')
                    return {'action': 'reuse', 'allowance': deepcopy(allowance),
                            'review_budget': budget, 'manifest': str(self.manifest)}
            if budget['remaining'] or budget['used'] != budget['limit']:
                raise ValueError('Continuation requires an exhausted review allowance')
            allowance = {**intent, 'previous_limit': budget['limit'],
                         'cycle_limit': budget['limit'] + cycles, 'authorized_at': time.time()}
            state.setdefault('review_allowances', []).append(allowance)
            write(self.manifest, manifest)
            return {'action': 'continued', 'allowance': deepcopy(allowance),
                    'review_budget': self.review_budget(state), 'manifest': str(self.manifest)}

    @staticmethod
    def budget(manifest, *, run, cycle, retry, ci_repair, ci_rerun, head, task, limits=None):
        ceilings = {'review_cycles': 5, 'retries_per_task': 1,
                    'ci_repairs_per_cycle': 5, 'ci_reruns_per_head': 3}
        if limits is not None and (not isinstance(limits, dict) or set(limits) - ceilings.keys()
                or any(type(value) is not int or not (1 if key == 'review_cycles' else 0) <= value <= ceilings[key]
                       for key, value in limits.items())):
            raise ValueError('Invalid process limits')
        limits = {**ceilings, **(limits or {})}
        if not run:
            if any((cycle, retry, ci_repair, ci_rerun)):
                raise ValueError('Budgeted execution requires a retained run id')
            return None
        state = manifest['runs'].setdefault(run, {'cycles': [], 'retries': {}, 'ci_repairs': {},
                                                 'ci_reruns': {}, 'review_tasks': {}, 'progress': []})
        # A later spec cannot restore a more generous limit or reset consumed work.
        limits = {key: min(value, state.get('limits', {}).get(key, value)) for key, value in limits.items()}
        if state.get('review_allowances') and limits['review_cycles'] != state.get('limits', {}).get('review_cycles', REVIEW_CYCLES):
            raise ValueError('Retain the initial review limit after explicit continuation allowances')
        state['limits'] = limits
        if cycle and cycle not in state['cycles']:
            if ci_repair or ci_rerun:
                raise ValueError('CI work must retain an existing cycle or use cycle zero')
            if retry:
                raise ValueError('Retry requires an existing cycle')
            budget = Store.review_budget(state)
            if not budget['remaining']:
                label = 'Five' if budget['limit'] == REVIEW_CYCLES else str(budget['limit'])
                raise ValueError(label + ' review-and-fix cycle limit reached; '
                                 'explicit user continuation is required')
            state['cycles'].append(cycle)
        if retry:
            if not cycle:
                raise ValueError('Same-cycle retry requires its cycle id')
            key = cycle + ':' + task
            if state['retries'].get(key, 0) >= limits['retries_per_task']:
                if limits['retries_per_task'] == 0:
                    raise ValueError('Same-cycle retries are disabled')
                raise ValueError('Same-cycle retry already consumed')
            state['retries'][key] = state['retries'].get(key, 0) + 1
        if ci_repair:
            key = cycle or '0'
            if state['ci_repairs'].get(key, 0) >= limits['ci_repairs_per_cycle']:
                count = 'Five' if limits['ci_repairs_per_cycle'] == 5 else str(limits['ci_repairs_per_cycle'])
                raise ValueError(count + ' CI repair attempts per cycle reached')
            state['ci_repairs'][key] = state['ci_repairs'].get(key, 0) + 1
        if ci_rerun:
            if not head:
                raise ValueError('CI rerun requires exact head')
            if state['ci_reruns'].get(head, 0) >= limits['ci_reruns_per_head']:
                count = 'Three' if limits['ci_reruns_per_head'] == 3 else str(limits['ci_reruns_per_head'])
                raise ValueError(count + ' CI reruns per head reached')
            state['ci_reruns'][head] = state['ci_reruns'].get(head, 0) + 1
        return state

    def _start_captured(self, manifest, captured, *, recover=None, run=None, cycle=None,
                        retry=False, ci_repair=False, ci_rerun=False, head=None, checklist=None):
        """Stage one start in the caller's locked manifest; never persist here."""
        options = captured['components'].get('options', {})
        phase = captured.get('hook_phase')
        if phase is not None:
            if cycle or retry or ci_repair or ci_rerun:
                raise ValueError('Hook claims do not use reviewer or CI budget flags')
            if (not isinstance(phase, dict) or set(phase) != {'occurrence', 'hooks'}
                    or not isinstance(phase['occurrence'], dict) or not isinstance(phase['hooks'], list)
                    or phase['occurrence'].get('run') != run):
                raise ValueError('Hook phase requires its original run and list snapshot')
            phase_key = digest(phase['occurrence'])
            snapshots = manifest.setdefault('hook_phases', {})
            if phase_key in snapshots and snapshots[phase_key] != phase:
                raise ValueError('Hook list changed; reconcile the original phase before continuing')
            snapshots[phase_key] = phase
        entry = manifest['entries'].setdefault(captured['key'], {'key': captured['key'], 'attempts': []})
        previous = self.describe(captured, entry)
        if entry.get('claim'):
            if recover != entry['claim']['id']:
                return {'action': 'recover_required', **previous}
            if run != entry['claim']['run'] or (
                    captured['fingerprint'] == entry['claim']['fingerprint']
                    and cycle != entry['claim']['cycle']):
                raise ValueError('Recovery must retain the original run and unchanged-input cycle budgets')
            entry['attempts'][-1]['status'] = 'interrupted'
            entry['attempts'][-1]['recovery'] = 'explicit recovery by coordinating agent'
            del entry['claim']
        elif recover:
            raise ValueError('Recovery id is not the active claim')
        if checklist is not None:
            from checklists import attach, reply, reserve
            attach(self, entry, captured, checklist)
            previous = self.describe(captured, entry)
            if previous['analysis_complete'] and not previous['checklist']['accepted']:
                gate = entry['checklist']
                if previous['checklist']['outcome'] == 'stale':
                    raise ValueError('Checklist evidence is damaged; retain results and reconcile its response references')
                reserve(gate)
                return {**previous, **reply(self, entry)}
        if previous['reusable']:
            if checklist is not None and entry['checklist']['outcome'] == 'best_effort':
                return {**previous, **reply(self, entry), 'action': 'reuse'}
            return {'action': 'reuse', **previous}
        state = self.budget(manifest, run=run, cycle=cycle, retry=retry, ci_repair=ci_repair,
                            ci_rerun=ci_rerun, head=head, task=captured['key'],
                            limits=options.get('process_limits'))
        if cycle and not (ci_repair or ci_rerun):
            tasks = state['review_tasks'].setdefault(cycle, {})
            previous_fingerprint = tasks.get(captured['key'])
            if previous_fingerprint and previous_fingerprint != captured['fingerprint']:
                raise ValueError('Changed review inputs require a new cycle; retain the existing budget')
            if previous_fingerprint and not (retry or recover):
                raise ValueError('Repeated same-cycle execution requires explicit retry or claim recovery')
            if retry and not previous_fingerprint:
                raise ValueError('Same-cycle retry requires a recorded original task attempt')
            tasks[captured['key']] = captured['fingerprint']
        claim = {'id': uuid.uuid4().hex, 'key': captured['key'], 'fingerprint': captured['fingerprint'],
                 'cacheable': captured['cacheable'], 'started': time.time(), 'run': run, 'cycle': cycle,
                 'provenance': captured['provenance']}
        entry.update(claim=claim, components=captured['components'])
        entry['attempts'].append({**claim, 'status': 'started'})
        return {'action': 'invoke', 'claim': claim, 'manifest': str(self.manifest),
                'changed': previous['changed']}

    def start(self, spec, *, recover=None, run=None, cycle=None, retry=False,
              ci_repair=False, ci_rerun=False, head=None):
        captured = capture(spec)
        if captured['subject'] != self.subject:
            raise ValueError('Spec refers to another PR')
        with self.locked() as manifest:
            result = self._start_captured(manifest, captured, recover=recover, run=run,
                                          cycle=cycle, retry=retry, ci_repair=ci_repair,
                                          ci_rerun=ci_rerun, head=head)
            if result['action'] == 'invoke':
                write(self.manifest, manifest)
            return result

    def native_binding(self, spec, claim_id):
        """Expose expected binding only for an unchanged active invocation."""
        from native_review import binding
        captured = capture(spec)
        if captured['subject'] != self.subject:
            raise ValueError('Spec refers to another PR')
        with self.locked() as manifest:
            claim = manifest['entries'].get(captured['key'], {}).get('claim')
            if not claim or claim['id'] != claim_id:
                raise ValueError('Claim is absent or owned by another attempt')
            if claim['fingerprint'] != captured['fingerprint']:
                raise ValueError('Inputs changed during execution; retain claim and explicitly recover')
            return {'binding': binding(captured['components'], claim),
                    'requirements': captured['components']['native_review']}

    def start_batch(self, specs, *, recover=None, run=None, cycle=None, retry=False,
                    ci_repair=False, ci_rerun=False, head=None, retained=None, checklists=None):
        """Claim a set of tasks atomically, sharing only command-local Git work.

        All captures and checks must succeed before one manifest replacement.
        An unfinished task without explicit recovery returns recover_required
        while eligible siblings can proceed. Validation and budget errors commit
        no batch changes.
        If the response is lost, status exposes every committed claim; callers
        must reconcile those claims rather than silently retry an invocation.
        """
        context = _CaptureContext()
        captures = [capture(spec, _context=context) for spec in specs]
        if not captures:
            raise ValueError('A batch requires at least one task spec')
        if any(item['subject'] != self.subject for item in captures):
            raise ValueError('Specs refer to another PR')
        keys = {item['key'] for item in captures}
        if len(keys) != len(captures):
            raise ValueError('Duplicate task keys in batch')
        if recover is None:
            recover = {}
        if (not isinstance(recover, dict)
                or any(not isinstance(key, str) or not isinstance(value, str) or not value
                       for key, value in recover.items())):
            raise ValueError('Batch recovery requires a task-key-to-claim-id map')
        if recover.keys() - keys:
            raise ValueError('Recovery key is not a task in this batch')
        retained = {} if retained is None else retained
        if (not isinstance(retained, dict) or retained.keys() - keys
                or any(not isinstance(value, dict) for value in retained.values())):
            raise ValueError('Retained definitions must belong to this batch')
        digest(retained)
        checklists = {} if checklists is None else checklists
        if checklists.keys() - retained.keys():
            raise ValueError('Checklists require a retained managed definition')
        with self.locked() as manifest:
            before = digest(manifest)
            # Explicit recovery mistakes reject the whole batch before staging
            # sibling starts. A missing recovery request is a normal outcome.
            for key, claim_id in recover.items():
                active = manifest['entries'].get(key, {}).get('claim')
                if not active or active['id'] != claim_id:
                    raise ValueError('Recovery id is not the active claim')
            tasks = []
            retained_changed = False
            for captured in captures:
                result = self._start_captured(
                    manifest, captured, recover=recover.get(captured['key']), run=run,
                    cycle=cycle, retry=retry, ci_repair=ci_repair, ci_rerun=ci_rerun, head=head,
                    checklist=checklists.get(captured['key']))
                tasks.append({**result, 'key': captured['key'],
                              'task': captured['components']['task']['id']})
                if result['action'] in {'invoke', 'reuse', 'recheck', 'failed'} and captured['key'] in retained:
                    entry = manifest['entries'][captured['key']]
                    if entry.get('managed') != retained[captured['key']]:
                        entry['managed'] = deepcopy(retained[captured['key']])
                        retained_changed = True
            if retained_changed or digest(manifest) != before:
                write(self.manifest, manifest)
            return {'tasks': tasks, 'manifest': str(self.manifest),
                    'unfinished': [entry['claim'] for entry in manifest['entries'].values()
                                   if entry.get('claim')]}

    def record(self, spec, claim_id, original, normalized, status, *, progress=None, native_evidence=None, checklist=None):
        captured = capture(spec)
        if captured['subject'] != self.subject:
            raise ValueError('Spec refers to another PR')
        if (status not in {'complete', 'failed', 'incomplete'} or not isinstance(normalized, dict)
                or type(normalized.get('complete')) is not bool
                or not isinstance(normalized.get('findings', []), list)
                or (status == 'complete') != normalized['complete']):
            raise ValueError('Explicit completion status and normalized complete/findings are required')
        if status == 'complete' and not has_output_content(original):
            raise ValueError('A complete task requires nonempty original output')
        with self.locked() as manifest:
            entry = manifest['entries'].get(captured['key'], {})
            claim = entry.get('claim')
            submission = digest({'original': base64.b64encode(original).decode(), 'report': normalized,
                                 'status': status, 'progress': progress,
                                 'native': base64.b64encode(native_evidence).decode() if native_evidence is not None else None})
            if (not claim and entry.get('checklist', {}).get('initial_submission') == submission
                    and (checklist is None or entry['checklist']['contract'] == checklist)):
                saved, problem = self._load_saved(entry)
                if (not problem and saved['claim'] == claim_id and not entry.get('invalidated')
                        and saved['fingerprint'] == captured['fingerprint']):
                    from checklists import reply
                    return reply(self, entry)
            if not claim or claim['id'] != claim_id:
                raise ValueError('Claim is absent or owned by another attempt')
            if claim['fingerprint'] != captured['fingerprint']:
                raise ValueError('Inputs changed during execution; retain claim and explicitly recover')
            if checklist is not None:
                from checklists import attach
                attach(self, entry, captured, checklist)
            if 'checklist' in entry and 'checklist' in normalized:
                from checklists import validate_report
                validate_report(entry['checklist'], normalized)
            from native_review import assess, required
            native = None
            if required(captured['components']):
                diagnostic = assess(captured['components'], claim, original, native_evidence)
                native = {'evidence': base64.b64encode(native_evidence).decode() if native_evidence is not None else None,
                          'diagnostic': diagnostic, 'requested_status': status,
                          'requested_normalized': deepcopy(normalized)}
                if status == 'complete' and diagnostic:
                    status = 'incomplete'
                    normalized = {**normalized, 'complete': False, 'execution_diagnostic': diagnostic}
            envelope = {'contract': CONTRACT, 'key': captured['key'], 'fingerprint': captured['fingerprint'],
                        'cacheable': claim['cacheable'], 'provenance': claim['provenance'], 'claim': claim_id, 'status': status,
                        'original': base64.b64encode(original).decode(),
                        'original_sha256': hashlib.sha256(original).hexdigest(), 'normalized': normalized}
            if native is not None:
                envelope['native_execution'] = native
            fingerprint = digest(envelope)
            filename = fingerprint + '.json'
            path = self.root/'results'/filename
            if path.exists():
                if read(path) != envelope:
                    raise ValueError('Immutable result collision')
            else:
                write(path, envelope)
            entry['result'] = {'file': filename, 'hash': fingerprint, 'fingerprint': captured['fingerprint']}
            entry['attempts'][-1].update(status=status, result=filename, finished=time.time())
            entry.pop('claim')
            entry.pop('invalidated', None)
            if progress is not None:
                if not claim['run']:
                    raise ValueError('Progress requires a retained run id')
                manifest['runs'][claim['run']]['progress'].append({'claim': claim_id, 'value': progress})
            followup = None
            if 'checklist' in entry:
                from checklists import attach, reply, reserve
                attach(self, entry, captured, entry['checklist']['contract'])
                if status == 'complete':
                    entry['checklist']['initial_submission'] = submission
                    if 'checklist' in normalized:
                        entry['checklist']['outcome'] = 'complete'
                    else:
                        reserve(entry['checklist'])
                    followup = reply(self, entry)
            write(self.manifest, manifest)
            if followup is not None:
                return followup
            return {'status': status, 'result': str(path), 'reusable': status == 'complete' and captured['cacheable'],
                    **({'execution_diagnostic': native['diagnostic']} if native and native['diagnostic'] else {})}

    def check(self, spec, claim_id, round_id, original, report):
        """Accept one reserved follow-up without replacing the primary report."""
        from checklists import submit
        captured = capture(spec)
        if captured['subject'] != self.subject:
            raise ValueError('Spec refers to another PR')
        if not has_output_content(original):
            raise ValueError('Checklist checks require nonempty original output')
        with self.locked() as manifest:
            entry = manifest['entries'].get(captured['key'], {})
            described = self.describe(captured, entry)
            gate = entry.get('checklist')
            saved, problem = self._load_saved(entry)
            if (not gate or problem or saved['claim'] != claim_id or not described.get('analysis_complete')
                    or gate['fingerprint'] != captured['fingerprint'] or gate['result'] != entry['result']['hash']):
                raise ValueError('Checklist requires unchanged complete primary evidence and its original claim')
            result = submit(self, entry, round_id, original, report)
            write(self.manifest, manifest)
            return result

    def invalidate(self, keys, reason, *, recover=None):
        if not reason:
            raise ValueError('An invalidation reason is required')
        with self.locked() as manifest:
            for key in keys:
                if key not in manifest['entries']:
                    raise ValueError('Unknown task key: ' + key)
                active = manifest['entries'][key].get('claim')
                if active and recover != active['id']:
                    raise ValueError('Active claim requires its explicit recovery id for invalidation')
                if recover and (not active or recover != active['id']):
                    raise ValueError('Recovery id is not the active claim')
            for key in keys:
                entry = manifest['entries'][key]
                if entry.pop('claim', None):
                    entry['attempts'][-1].update(status='interrupted', recovery=reason, finished=time.time())
                manifest['entries'][key]['invalidated'] = {'reason': reason, 'at': time.time()}
            write(self.manifest, manifest)
        return {'invalidated': keys}
