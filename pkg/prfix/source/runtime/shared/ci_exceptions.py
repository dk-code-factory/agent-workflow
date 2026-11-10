"""Explicit private CI exceptions shared by dk-config and live CI decisions."""
import fcntl
import hashlib
import json
from pathlib import PurePosixPath

from shared.private_state import write
from shared.storage_paths import location


def data_root():
    return location('policies')


def config_path(identity, scope):
    if scope not in {'repo', 'user'}:
        raise ValueError('CI exception scope must be repo or user')
    folder = 'user' if scope == 'user' else hashlib.sha256(identity.encode()).hexdigest()[:24]
    return data_root() / folder / 'ci-exceptions.json'


def fingerprint(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def entry(kind, workflow, job, test, signature, reason):
    values = {'kind': kind, 'workflow': workflow, 'job': job, 'test': test, 'signature': signature, 'reason': reason}
    if not isinstance(kind, str) or kind not in {'test', 'pipeline'}:
        raise ValueError('CI exception kind must be test or pipeline')
    for key, value in values.items():
        if value is not None and (not isinstance(value, str) or not value.strip() or '\x00' in value):
            raise ValueError(f'{key} must be nonempty text without NUL bytes')
    if not workflow or not reason:
        raise ValueError('workflow and reason are required')
    path = PurePosixPath(workflow)
    if (not workflow.startswith('.github/workflows/') or len(path.parts) != 3
            or path.suffix not in {'.yml', '.yaml'} or str(path) != workflow):
        raise ValueError('workflow must be an exact .github/workflows/FILE.yml or .yaml path')
    if kind == 'test' and (not job or not test):
        raise ValueError('test exceptions require an exact job and test identity')
    if kind == 'pipeline' and (test is not None or signature is not None):
        raise ValueError('pipeline exceptions accept an optional job, but no test or signature')
    matcher = {k: v for k, v in values.items() if k != 'reason'}
    return {'id': 'ci-' + fingerprint(matcher)[:16], **values}


def validate(value, identity):
    if (not isinstance(value, dict) or set(value) != {'schema_version', 'identity', 'entries'}
            or type(value['schema_version']) is not int or value['schema_version'] != 1
            or value['identity'] != identity or not isinstance(value['entries'], list)):
        raise ValueError('malformed CI exception configuration; repair or reset it with dk-config')
    seen = set()
    for item in value['entries']:
        if not isinstance(item, dict) or set(item) != {'id', 'kind', 'workflow', 'job', 'test', 'signature', 'reason'}:
            raise ValueError('malformed CI exception entry')
        expected = entry(**{k: v for k, v in item.items() if k != 'id'})
        if item != expected or item['id'] in seen:
            raise ValueError('invalid or duplicate CI exception identity')
        seen.add(item['id'])
    return value


def load(identity, scope='effective'):
    if scope not in {'effective', 'repo', 'user'}:
        raise ValueError('CI exception scope must be effective, repo or user')
    sources = []
    selected = None
    for level in (('repo', 'user') if scope == 'effective' else (scope,)):
        path = config_path(identity, level)
        try:
            raw = path.read_bytes()
        except FileNotFoundError:
            raw = None
        sources.append({'path': str(path), 'sha256': hashlib.sha256(raw).hexdigest() if raw is not None else None})
        if selected is None and raw is not None:
            config = validate(json.loads(raw), 'user' if level == 'user' else identity)
            selected = {'entries': config['entries'], 'configured': True, 'source': level, 'file': str(path)}
    result = selected or {'entries': [], 'configured': False, 'source': None, 'file': None}
    return {**result, 'fingerprint': fingerprint(sources), 'sources': sources}


def configure(identity, action, *, scope='repo', value=None, entry_id=None):
    path = config_path(identity, scope)
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.with_name(path.name + '.lock').open('w') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        if action == 'reset':
            path.unlink(missing_ok=True)
        else:
            owner = 'user' if scope == 'user' else identity
            config = validate(json.loads(path.read_text()), owner) if path.exists() else {
                'schema_version': 1, 'identity': owner, 'entries': []}
            if action == 'add':
                checked = entry(**{k: v for k, v in value.items() if k != 'id'})
                config['entries'] = [item for item in config['entries'] if item['id'] != checked['id']] + [checked]
            elif action == 'remove':
                if not any(item['id'] == entry_id for item in config['entries']):
                    raise ValueError('unknown CI exception ID in this scope')
                config['entries'] = [item for item in config['entries'] if item['id'] != entry_id]
            else:
                raise ValueError('unknown CI exception operation')
            write(path, config)
    return load(identity)


def matching(entries, kind, context):
    return [item for item in entries if item['kind'] == kind and item['workflow'] == context.get('workflow')
            and (item['job'] is None or item['job'] == context.get('job'))
            and (kind == 'pipeline' or (item['test'] == context.get('test')
                 and (item['signature'] is None or item['signature'] == context.get('signature'))))]


def receipt(config, item):
    return {'id': item['id'], 'kind': item['kind'], 'reason': item['reason'],
            'scope': config['source'], 'file': config['file'], 'fingerprint': config['fingerprint']}
