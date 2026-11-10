"""Private PR size settings and final-diff accounting (never commit churn)."""
import fcntl
import hashlib
import json
from pathlib import Path
import subprocess

from shared.ci_exceptions import data_root, fingerprint
from shared.private_state import write
from shared.repository_scope import identities

from skill_config import defaults as skill_defaults

DEFAULTS = skill_defaults('dk-workflows:dk-pr-split')
MODES = ('automatic', 'manual', 'disabled')


def validate(settings):
    if not isinstance(settings, dict) or set(settings) - DEFAULTS.keys():
        raise ValueError('invalid pr_size settings')
    for key, value in settings.items():
        if key == 'added_lines_threshold':
            if type(value) is not int or value < 1:
                raise ValueError('added_lines_threshold must be a positive integer')
        elif not isinstance(value, str) or value not in MODES:
            raise ValueError(key + ' must be automatic, manual or disabled')
    return settings


def path(identity):
    folder = 'user' if identity == 'user' else hashlib.sha256(identity.encode()).hexdigest()[:24]
    return data_root() / folder / 'pr-size.json'


def load(chain, scope='effective'):
    if scope not in ('effective', 'repo', 'upstream', 'user'):
        raise ValueError('invalid PR size scope')
    levels = [('repo' if i == 0 else 'upstream', key) for i, key in enumerate(chain)] + [('user', 'user')]
    result = {key: {'value': value, 'source': 'default', 'file': None} for key, value in DEFAULTS.items()}
    sources = []
    for level, identity in reversed(levels):
        if scope != 'effective' and scope != level:
            continue
        target = path(identity)
        raw = target.read_bytes() if target.exists() else None
        sources.append({'source': level, 'file': str(target),
                        'sha256': hashlib.sha256(raw).hexdigest() if raw is not None else None})
        if raw is None:
            continue
        value = json.loads(raw)
        if (not isinstance(value, dict) or set(value) != {'schema_version', 'identity', 'pr_size'}
                or type(value['schema_version']) is not int or value['schema_version'] != 1
                or value['identity'] != identity):
            raise ValueError('malformed PR size configuration: ' + str(target))
        for key, item in validate(value['pr_size']).items():
            result[key] = {'value': item, 'source': level, 'file': str(target)}
    if scope == 'effective':
        import skill_config
        key = chain[0].removeprefix('remote:') if chain else None
        upstream = chain[1].removeprefix('remote:') if len(chain) > 1 else None
        owned = skill_config.effective('dk-workflows:dk-pr-split', key, upstream)
        for key, value in owned['options'].items():
            origin = owned['provenance'][key]
            if origin not in owned['sources'][:2] and origin != result[key]['file']:
                result[key] = {'value': value, 'source': 'skill-configuration', 'file': origin}
            else:
                result[key]['value'] = value
        existing = {item['file'] for item in sources}
        for filename in owned['checked_paths']:
            if filename not in existing:
                target = Path(filename)
                sources.append({'source': 'skill-configuration', 'file': filename,
                                'sha256': hashlib.sha256(target.read_bytes()).hexdigest() if target.exists() else None})
    return {'pr_size': {key: item['value'] for key, item in result.items()},
            'settings': result, 'sources': sources, 'fingerprint': fingerprint(sources)}


def effective(cwd, pr=None):
    return load(identities(cwd, pr))


def configure(chain, action, *, scope='repo', settings=None, keys=None):
    if scope not in ('repo', 'user'):
        raise ValueError('edit upstream settings from its checkout')
    identity = 'user' if scope == 'user' else chain[0]
    target = path(identity)
    target.parent.mkdir(parents=True, exist_ok=True)
    with target.with_suffix('.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        if action == 'reset' and not keys:
            target.unlink(missing_ok=True)
        else:
            current = load([identity], scope='user' if scope == 'user' else 'repo')
            values = {key: item['value'] for key, item in current['settings'].items()
                      if item['source'] == scope}
            if action == 'set':
                if not settings:
                    raise ValueError('set requires at least one setting')
                values.update(validate(settings))
            elif action == 'reset':
                if set(keys) - DEFAULTS.keys():
                    raise ValueError('unknown PR size setting')
                for key in keys:
                    values.pop(key, None)
            else:
                raise ValueError('unknown PR size action')
            write(target, {'schema_version': 1, 'identity': identity, 'pr_size': values})
    return load(chain)


def additions(cwd, base, head):
    """NUL-delimited numstat retains rename detection and arbitrary path names."""
    raw = subprocess.check_output(['git', '-C', str(cwd), 'diff', '--no-ext-diff',
        '--no-textconv', '--no-relative', '--find-renames', '--numstat', '-z', base, head, '--'])
    fields, index, total = raw.split(b'\0'), 0, 0
    while index < len(fields) and fields[index]:
        added, deleted, name = fields[index].split(b'\t', 2)
        index += 1
        if not name:  # Rename: the next two NUL fields are old and new paths.
            index += 2
        if added != b'-':
            total += int(added)
    return total
