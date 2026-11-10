"""Resolve local repository and fork-parent identities for private settings."""
import json
from pathlib import Path
import subprocess
from urllib.parse import urlparse
from shared.github_reads import run as github_run


def scope_keys(chain, scope='effective'):
    """Repository, upstream, user precedence for whole-policy settings."""
    if scope not in {'effective', 'repo', 'user'}:
        raise ValueError('Scope must be effective, repo or user')
    if not chain:
        raise ValueError('Repository identity is required')
    return [*chain, 'user'] if scope == 'effective' else ['user' if scope == 'user' else chain[0]]


def checkout_roots(cwd):
    """Locate checkout and shared Git storage; a non-repository cwd protects neither."""
    cwd = Path(cwd).resolve()
    result = subprocess.run(['git', '-C', str(cwd), 'rev-parse', '--path-format=absolute',
                             '--show-toplevel', '--git-common-dir'],
                            capture_output=True, text=True, timeout=15)
    if result.returncode:
        # A bare repository has no worktree but its metadata is still protected.
        result = subprocess.run(['git', '-C', str(cwd), 'rev-parse',
                                 '--path-format=absolute', '--git-common-dir'],
                                capture_output=True, text=True, timeout=15)
    if result.returncode:
        if 'not a git repository' in result.stderr.lower():
            return []
        raise RuntimeError(result.stderr.strip() or 'Could not resolve repository storage')
    return [Path(line).resolve() for line in result.stdout.splitlines()]


def inside_checkout(path, cwd):
    return any(Path(path).resolve().is_relative_to(root) for root in checkout_roots(cwd))


def normalize_remote(value):
    value = value.strip()
    if '://' not in value and ':' in value and not value.startswith('/'):
        host, path = value.split(':', 1)
        host = host.rsplit('@', 1)[-1]
    else:
        parsed = urlparse(value)
        host, path = parsed.hostname, parsed.path
    parts = path.strip('/').removesuffix('.git').split('/')
    if host and len(parts) == 2 and all(parts):
        return 'remote:' + host.lower() + '/' + '/'.join(parts).lower()
    return None


def git_remote(root, name):
    result = subprocess.run(['git', '-C', str(root), 'remote', 'get-url', name],
                            capture_output=True, text=True)
    return result.stdout.strip() if result.returncode == 0 else None


def repository(root):
    root = Path(root).resolve()
    remote = git_remote(root, 'origin')
    if remote:
        return normalize_remote(remote) or 'remote:' + remote.removesuffix('.git').rstrip('/').lower()
    return 'path:' + str(root)


def upstream(root, identity):
    """Return the fork parent's identity, or None when it cannot be verified."""
    if (not identity.startswith('remote:')
            or normalize_remote('https://' + identity.removeprefix('remote:')) != identity):
        return None
    candidate = normalize_remote(git_remote(root, 'upstream') or '')
    if candidate and candidate != identity:
        return candidate
    repository_name = identity.removeprefix('remote:')
    try:
        result = github_run(['gh', 'repo', 'view', repository_name, '--json', 'isFork,parent'],
                                cwd=root, capture_output=True, text=True, timeout=8)
    except (OSError, subprocess.TimeoutExpired):
        result = None
    if result is not None and result.returncode == 0:
        try:
            metadata = json.loads(result.stdout)
            if metadata.get('isFork') is False:
                return None
            parent = (metadata.get('parent') or {}).get('nameWithOwner')
            host = repository_name.split('/', 1)[0]
            if metadata.get('isFork') is True and parent:
                normalized = normalize_remote(f'https://{host}/{parent}.git')
                if normalized and normalized != identity:
                    return normalized
        except (ValueError, AttributeError, TypeError):
            pass
    return None


def identities(root, pr=None):
    """Return fork, upstream, then local identities (without duplicates)."""
    root = Path(root).resolve()
    if pr:
        try:
            result = github_run(['gh', 'pr', 'view', pr, '--json', 'number,url,headRepository,headRefOid,headRefName,isCrossRepository'],
                                    cwd=root, capture_output=True, text=True, timeout=15)
        except (OSError, subprocess.TimeoutExpired) as exc:
            raise RuntimeError('could not resolve PR repository identities') from exc
        if result.returncode:
            raise RuntimeError(result.stderr.strip() or 'could not resolve PR repository identities')
        try:
            metadata = json.loads(result.stdout)
        except ValueError as exc:
            raise RuntimeError('PR repository metadata is invalid') from exc
        if not isinstance(metadata, dict):
            raise RuntimeError('PR repository metadata is invalid')
        from shared.pr_identity import resolve
        host, base_repo, head_repo = resolve(metadata, root)
        base = normalize_remote(f'https://{host}/{base_repo}.git')
        head = normalize_remote(f'https://{host}/{head_repo}.git')
        return [head] if head == base else [head, base]
    primary = repository(root)
    parent = upstream(root, primary)
    return [primary, parent] if parent else [primary]
