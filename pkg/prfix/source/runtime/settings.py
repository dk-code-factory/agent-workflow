"""Read selected settings once; preserve domain precedence and legacy addresses."""
import hashlib
from pathlib import Path

import commit_policy
import config
from shared import ci_exceptions, pr_partition_policy, pr_size
from shared.private_state import read
from shared.repository_scope import identities, scope_keys
from shared.storage_paths import location


DOMAINS = ('rules', 'drafting', 'completion', 'commit-policy')
SECTIONS = ('workflow', *DOMAINS, 'pr-size', 'ci-exceptions', 'pr-partition', 'preferences')


def domain_path(domain, key):
    if domain not in DOMAINS:
        raise ValueError('Unknown PR settings domain')
    if domain == 'commit-policy':
        return commit_policy.path(key)
    if domain == 'completion':
        base = location('completion')
        return base/'user.txt' if key == 'user' else base/'repositories'/(hashlib.sha256(key.encode()).hexdigest()[:24]+'.txt')
    data = location('policies')
    folder = (data/'user' if domain == 'rules' else data) if key == 'user' else data/hashlib.sha256(key.encode()).hexdigest()[:24]
    return folder/(domain + '.json')


def domain_load(domain, chain, scope='effective'):
    if domain == 'commit-policy':
        return commit_policy.load(chain, scope)
    levels = scope_keys(chain, scope)
    if domain == 'drafting':
        merged, sources = {}, {}
        for key in reversed(levels):
            target = domain_path(domain, key)
            if target.exists():
                for role, value in read(target, {}).items():
                    if role == '$schema':
                        continue
                    import invocation
                    invocation.validate(value)
                    merged[role] = value
                    sources[role] = {'source': key, 'file': str(target)}
        return {'value': merged, 'sources': sources}
    for key in levels:
        target = domain_path(domain, key)
        if target.exists():
            value = {'prompt': target.read_text()} if domain == 'completion' else read(target, None)
            if domain == 'completion' and (not value['prompt'].strip() or '\x00' in value['prompt']):
                raise ValueError('Malformed completion prompt; repair it rather than falling back')
            if domain == 'rules' and not value.get('rules'):
                continue
            return {'value': value, 'source': key, 'file': str(target)}
    if domain == 'completion':
        target = Path(__file__).resolve().parents[1]/'docs/pr/default-completion.md'
        return {'value': {'prompt': target.read_text()}, 'source': 'default', 'file': str(target)}
    return {'value': None, 'source': 'default'}


def load_sections(repository, *, checkout='.', pr=None, upstream=None, sections=('workflow',)):
    if (not isinstance(sections, (list, tuple)) or not sections
            or any(section not in SECTIONS for section in sections) or len(set(sections)) != len(sections)):
        raise ValueError('Select unique settings sections')
    chain = identities(checkout, pr) if any(section in DOMAINS or section == 'pr-size' for section in sections) else None
    result = {}
    for section in sections:
        if section == 'workflow':
            result[section] = config.effective(repository, upstream)
        elif section in DOMAINS:
            result[section] = domain_load(section, chain)
        elif section == 'pr-size':
            result[section] = pr_size.load(chain)
        elif section == 'ci-exceptions':
            result[section] = ci_exceptions.load('remote:' + repository.lower().removeprefix('remote:'))
        elif section == 'pr-partition':
            if pr is None:
                raise ValueError('PR partition settings require the exact PR')
            result[section] = pr_partition_policy.load(pr)
        else:
            import preferences
            result[section] = preferences.list_records(checkout)
    return result
