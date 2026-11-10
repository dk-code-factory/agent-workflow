"""Resolve GitHub PR repository ownership without inferring a fork owner."""
import json
import re
import subprocess
from urllib.parse import urlparse

from shared.github_reads import run as github_run


def resolve(metadata, cwd=None, *, api=None, allow_unavailable_head=False):
    """Return host, base repository, head repository; verify incomplete CLI data via REST.

    Older gh versions supplied nameWithOwner; newer versions can supply only
    name and a node ID. REST is authoritative only after all available evidence
    has been cross-checked. Failed reads and contradictions fail closed. Read-only
    consumers may permit an explicitly absent head repository, returned as None.
    """
    if not isinstance(metadata, dict):
        raise RuntimeError('PR repository metadata is invalid')
    parsed = urlparse(metadata.get('url') or '')
    parts = parsed.path.strip('/').split('/')
    if (parsed.scheme != 'https' or not parsed.hostname or len(parts) != 4
            or not all(parts) or parts[2] != 'pull' or not parts[3].isdigit()
            or int(parts[3]) < 1 or parsed.query or parsed.fragment):
        raise RuntimeError('PR repository identity is incomplete')
    host, base, number = parsed.hostname.lower(), '/'.join(parts[:2]), int(parts[3])
    if metadata.get('number', number) != number:
        raise RuntimeError('PR repository identity is inconsistent')
    if 'isCrossRepository' in metadata and not isinstance(metadata['isCrossRepository'], bool):
        raise RuntimeError('PR repository metadata is invalid')
    head = metadata.get('headRepository')
    head = {} if head is None else head
    if not isinstance(head, dict):
        raise RuntimeError('PR head repository is unavailable')
    base_graph = metadata.get('baseRepository')
    base_graph = {} if base_graph is None else base_graph
    if not isinstance(base_graph, dict):
        raise RuntimeError('PR repository metadata is invalid')
    name = head.get('nameWithOwner')

    def repository(graph, rest):
        if not isinstance(rest, dict):
            raise RuntimeError('PR head repository is unavailable')
        full = rest.get('full_name')
        if not isinstance(full, str) or not re.fullmatch(r'[^/\s]+/[^/\s]+', full):
            raise RuntimeError('PR head repository is unavailable')
        if rest.get('name') and rest['name'].lower() != full.split('/')[1].lower():
            raise RuntimeError('PR repository identity is inconsistent')
        for field, target in (('nameWithOwner', full), ('name', rest.get('name')),
                              ('id', rest.get('node_id')), ('databaseId', rest.get('id'))):
            value = graph.get(field)
            matches = (str(value) == str(target) if field in {'id', 'databaseId'}
                       else str(value).lower() == str(target).lower())
            if value and (not target or not matches):
                raise RuntimeError('PR repository identity is inconsistent')
        owner = graph.get('owner') or {}
        if owner.get('login') and owner['login'].lower() != full.split('/')[0].lower():
            raise RuntimeError('PR repository identity is inconsistent')
        return full

    if not name:
        if api is None:
            def api(*args):
                result = github_run(['gh', *args], cwd=cwd, text=True, capture_output=True)
                if result.returncode:
                    raise RuntimeError(result.stderr.strip() or 'PR head repository is unavailable')
                return json.loads(result.stdout)
        try:
            rest = api('api', '--hostname', host, f'repos/{base}/pulls/{number}')
        except (OSError, subprocess.TimeoutExpired) as exc:
            raise RuntimeError('could not resolve PR repository identities from GitHub') from exc
        if not isinstance(rest, dict) or not isinstance(rest.get('head'), dict) or not isinstance(rest.get('base'), dict):
            raise RuntimeError('PR head repository is unavailable')
        if rest.get('number') != number or rest.get('html_url', '').lower() != metadata['url'].lower():
            raise RuntimeError('PR repository identity is inconsistent')
        remote_head, remote_base = rest['head'], rest['base']
        base_name = repository(base_graph, remote_base.get('repo') or {})
        if base_name.lower() != base.lower():
            raise RuntimeError('PR base repository identity is inconsistent')
        for remote, prefix in ((remote_head, 'head'), (remote_base, 'base')):
            if (not isinstance(remote.get('sha'), str) or not re.fullmatch(r'[0-9a-fA-F]{40,64}', remote['sha'])
                    or not isinstance(remote.get('ref'), str) or not remote['ref']):
                raise RuntimeError('PR repository identity is incomplete')
            for suffix, field in (('RefOid', 'sha'), ('RefName', 'ref')):
                if metadata.get(prefix + suffix) and metadata[prefix + suffix] != remote[field]:
                    raise RuntimeError('PR head or base identity is inconsistent')
        if allow_unavailable_head and 'repo' in remote_head and remote_head['repo'] is None:
            fork = metadata.get('isCrossRepository')
            if fork is False:
                raise RuntimeError('PR repository IDs and fork status are inconsistent')
            for graph_field, rest_field in (('id', 'node_id'), ('databaseId', 'id')):
                left, right = head.get(graph_field), remote_base['repo'].get(rest_field)
                if any(value is not None and type(value) not in (str, int)
                       for value in (left, right)):
                    raise RuntimeError('PR repository metadata is invalid')
                if left and right:
                    if str(left) == str(right):
                        raise RuntimeError('PR repository IDs and fork status are inconsistent')
                    fork = True
            if fork is not True:
                raise RuntimeError('PR repository IDs and fork status are inconsistent')
            return host, base.lower(), None
        name = repository(head, remote_head.get('repo') or {})
        for field in ('id', 'node_id'):
            left, right = remote_head['repo'].get(field), remote_base['repo'].get(field)
            if left and right and (left == right) is not (name.lower() == base.lower()):
                raise RuntimeError('PR repository IDs and fork status are inconsistent')
    if not isinstance(name, str) or not re.fullmatch(r'[^/\s]+/[^/\s]+', name):
        raise RuntimeError('PR repository identity is incomplete')
    if (head.get('name') and head['name'].lower() != name.split('/')[1].lower()
            or (head.get('owner') or {}).get('login')
            and head['owner']['login'].lower() != name.split('/')[0].lower()
            or (metadata.get('headRepositoryOwner') or {}).get('login')
            and metadata['headRepositoryOwner']['login'].lower() != name.split('/')[0].lower()):
        raise RuntimeError('PR repository identity is inconsistent')
    if (base_graph.get('nameWithOwner') and base_graph['nameWithOwner'].lower() != base.lower()
            or base_graph.get('name') and base_graph['name'].lower() != base.split('/')[1].lower()):
        raise RuntimeError('PR base repository identity is inconsistent')
    cross = name.lower() != base.lower()
    if 'isCrossRepository' in metadata and metadata['isCrossRepository'] is not cross:
        raise RuntimeError('PR repository identity is inconsistent')
    metadata['headRepository'] = {**head, 'nameWithOwner': name}
    return host, base.lower(), name.lower()
