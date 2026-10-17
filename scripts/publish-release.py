#!/usr/bin/env python3
"""Tag-CI publisher. Reuses matching drafts; never replaces assets or image labels.

Only invoked by release.yml after validate-release.py and deterministic CI pass.
"""
import argparse
import base64
import json
import os
from pathlib import Path
import subprocess
import tempfile
import urllib.error
import urllib.request
from urllib.parse import urlencode

from release_lib import ROOT, git, sha256, source_fingerprint

REPO = 'dk-code-factory/agent-workflow'
IMAGE = 'ghcr.io/' + REPO + '-worker'
WORKFLOW = REPO + '/.github/workflows/release.yml'

def run(*args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)

def gh_json(*args):
    return json.loads(subprocess.check_output(['gh', *args], text=True))

def require(condition, message):
    if not condition:
        raise ValueError(message)

def manifest(reference):
    name = REPO + '-worker'
    token_url = 'https://ghcr.io/token?' + urlencode({'service': 'ghcr.io', 'scope': 'repository:' + name + ':pull'})
    request = urllib.request.Request(token_url)
    auth = base64.b64encode((os.environ['GITHUB_ACTOR'] + ':' + os.environ['GH_TOKEN']).encode()).decode()
    request.add_header('Authorization', 'Basic ' + auth)
    token = json.load(urllib.request.urlopen(request))['token']
    request = urllib.request.Request('https://ghcr.io/v2/' + name + '/manifests/' + reference,
        headers={'Authorization': 'Bearer ' + token, 'Accept': ', '.join([
            'application/vnd.oci.image.index.v1+json', 'application/vnd.oci.image.manifest.v1+json',
            'application/vnd.docker.distribution.manifest.list.v2+json', 'application/vnd.docker.distribution.manifest.v2+json'])})
    try:
        with urllib.request.urlopen(request) as response:
            raw = response.read()
            import hashlib
            digest = 'sha256:' + hashlib.sha256(raw).hexdigest()
            require(response.headers['Docker-Content-Digest'] == digest, 'registry bytes/digest mismatch')
            return digest, json.loads(raw)
    except urllib.error.HTTPError as error:
        if error.code == 404:
            return None
        raise

def registry(out, tag):
    digests = {}
    for arch in ['amd64', 'arm64']:
        metadata = json.loads((out / ('worker-linux-' + arch + '.json')).read_text())
        evidence = json.loads((out / 'conformance-evidence.json').read_text())
        for record in evidence['records'] + evidence.get('runtime_recovery', []):
            if record.get('runtime') and record['arch'] == arch:
                require(record['image_digest'] == metadata['image'], 'distributed image differs from certified image')
        label = tag + '-' + arch
        existing = manifest(label)
        if existing:
            require(existing[1].get('config', {}).get('digest') == metadata['image'], 'conflicting existing image label: ' + label)
        else:
            archive = out / metadata['archive']
            require(sha256(archive) == metadata['archive_sha256'], 'image archive changed')
            run('docker', 'load', '--input', str(archive))
            run('docker', 'tag', metadata['image'], IMAGE + ':' + label)
            run('docker', 'push', IMAGE + ':' + label)
        digest, document = manifest(label)
        require(document.get('config', {}).get('digest') == metadata['image'], 'pushed image config mismatch')
        digests[arch] = digest
    existing = manifest(tag)
    if not existing:
        run('docker', 'manifest', 'create', IMAGE + ':' + tag, *[IMAGE + '@' + digests[a] for a in ['amd64', 'arm64']])
        run('docker', 'manifest', 'push', IMAGE + ':' + tag)
    digest, document = manifest(tag)
    entries = document.get('manifests', [])
    actual = {v['platform']['architecture']: v['digest'] for v in entries if v['platform']['os'] == 'linux'}
    require(len(entries) == 2 and actual == digests, 'conflicting version image index')
    (out / 'registry.json').write_text(json.dumps({'image': IMAGE, 'digest': digest, 'platforms': digests}, indent=2) + '\n')
    if os.environ.get('GITHUB_OUTPUT'):
        with open(os.environ['GITHUB_OUTPUT'], 'a') as output:
            output.write('digest=' + digest + '\n')
            for arch, value in digests.items():
                output.write(arch + '=' + value + '\n')

def inventory(out, tag):
    commit = git('rev-parse', 'HEAD')
    require(commit == git('rev-parse', 'refs/tags/' + tag + '^{commit}'), 'checkout does not match release tag')
    registry_info = json.loads((out / 'registry.json').read_text())
    files = {p.name: sha256(p) for p in sorted(out.iterdir()) if p.is_file() and p.name not in ['release-manifest.json', 'SHA256SUMS']}
    report = {'repository': REPO, 'tag': tag, 'commit': commit, 'source_fingerprint': source_fingerprint(), 'registry': registry_info, 'assets': files}
    (out / 'release-manifest.json').write_text(json.dumps(report, indent=2, sort_keys=True) + '\n')
    files['release-manifest.json'] = sha256(out / 'release-manifest.json')
    (out / 'SHA256SUMS').write_text(''.join(d + '  ' + n + '\n' for n, d in sorted(files.items())))

def checksums(out, tag):
    (out / 'SHA256SUMS').write_text(''.join(sha256(p) + '  ' + p.name + '\n' for p in sorted(out.iterdir()) if p.is_file() and p.name != 'SHA256SUMS'))

def verify_attestation(path, commit, tag):
    run('gh', 'attestation', 'verify', str(path), '--repo', REPO,
        '--signer-workflow', WORKFLOW, '--source-digest', commit,
        '--source-ref', 'refs/tags/' + tag, '--signer-digest', commit,
        '--bundle', str(path.parent / ('manifest-provenance.jsonl' if path.name == 'release-manifest.json' else 'archive-provenance.jsonl')))

def compatible_draft(release, tag, commit, expected):
    require(release.get('draft') is True, 'published versions cannot be reused or repaired')
    require(release.get('tag_name') == tag and release.get('target_commitish') == commit, 'draft commit mismatch')
    names = [a['name'] for a in release['assets']]
    require(len(names) == len(set(names)) and set(names) <= set(expected), 'unexpected or duplicate draft assets')

def publish(out, tag):
    local = json.loads((out / 'release-manifest.json').read_text())
    commit = local['commit']
    require(local['tag'] == tag and local['repository'] == REPO, 'manifest identity mismatch')
    require(commit == git('rev-parse', 'HEAD') and local['source_fingerprint'] == source_fingerprint(), 'source changed')
    expected = dict(local['assets'], **{'release-manifest.json': sha256(out / 'release-manifest.json'), 'SHA256SUMS': sha256(out / 'SHA256SUMS'), 'manifest-provenance.jsonl': sha256(out / 'manifest-provenance.jsonl')})
    require({p.name for p in out.iterdir()} == set(expected), 'unmanifested output files')
    for name, digest in expected.items():
        require(Path(name).name == name and sha256(out / name) == digest, 'local asset mismatch: ' + name)
    # Recheck all gates immediately before external effects. No offline override.
    run('python3', str(ROOT / 'scripts/validate-release.py'), tag)
    for path in sorted(out.glob('*.tar.gz')):
        verify_attestation(path, commit, tag)
    verify_attestation(out / 'release-manifest.json', commit, tag)
    reg = local['registry']
    for digest in [reg['digest'], *reg['platforms'].values()]:
        require(manifest(digest)[0] == digest, 'registry digest missing')
        run('gh', 'attestation', 'verify', 'oci://' + IMAGE + '@' + digest, '--repo', REPO,
            '--signer-workflow', WORKFLOW, '--source-digest', commit, '--signer-digest', commit,
            '--source-ref', 'refs/tags/' + tag)
    require(manifest(tag)[0] == reg['digest'], 'version label changed')
    # A 404 is the only condition that permits creating a draft.
    result = subprocess.run(['gh', 'api', 'repos/' + REPO + '/releases/tags/' + tag], capture_output=True, text=True)
    if result.returncode:
        require('HTTP 404' in result.stderr, 'cannot inspect existing release: ' + result.stderr)
        run('gh', 'release', 'create', tag, '--repo', REPO, '--draft', '--verify-tag', '--target', commit,
            '--title', tag, '--notes', 'Verified immutable release for commit ' + commit + '. See release-manifest.json for digests and support evidence.')
    release = gh_json('api', 'repos/' + REPO + '/releases/tags/' + tag)
    compatible_draft(release, tag, commit, expected)
    with tempfile.TemporaryDirectory(prefix='release-download-') as directory:
        downloaded = Path(directory)
        for asset in release['assets']:
            name = asset['name']
            with (downloaded / name).open('wb') as stream:
                run('gh', 'api', '-H', 'Accept: application/octet-stream', 'repos/' + REPO + '/releases/assets/' + str(asset['id']), stdout=stream)
            require(sha256(downloaded / name) == expected[name], 'existing draft asset conflict: ' + name)
        present = {a['name'] for a in release['assets']}
        for name in sorted(set(expected) - present):
            run('gh', 'release', 'upload', tag, str(out / name), '--repo', REPO)
        # Fetch every uploaded byte again before closing the draft.
        release = gh_json('api', 'repos/' + REPO + '/releases/tags/' + tag)
        compatible_draft(release, tag, commit, expected)
        require({a['name'] for a in release['assets']} == set(expected), 'incomplete draft')
        for asset in release['assets']:
            path = downloaded / asset['name']
            with path.open('wb') as stream:
                run('gh', 'api', '-H', 'Accept: application/octet-stream', 'repos/' + REPO + '/releases/assets/' + str(asset['id']), stdout=stream)
            require(sha256(path) == expected[path.name], 'draft download checksum mismatch')
        for path in downloaded.glob('*.tar.gz'):
            verify_attestation(path, commit, tag)
        verify_attestation(downloaded / 'release-manifest.json', commit, tag)
    require(manifest(tag)[0] == reg['digest'], 'registry tag changed before publication')
    run('gh', 'release', 'edit', tag, '--repo', REPO, '--draft=false')
    run('gh', 'release', 'verify', tag, '--repo', REPO)
    for name in sorted(expected):
        run('gh', 'release', 'verify-asset', tag, str(out / name), '--repo', REPO)

if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('phase', choices=['registry', 'inventory', 'checksums', 'publish'])
    parser.add_argument('tag')
    parser.add_argument('--output', default='dist')
    args = parser.parse_args()
    require(os.environ.get('GITHUB_ACTIONS') == 'true' and os.environ.get('GITHUB_REPOSITORY') == REPO
            and os.environ.get('GITHUB_REF') == 'refs/tags/' + args.tag
            and os.environ.get('GITHUB_EVENT_NAME') == 'push', 'publisher only accepts repository tag-push CI')
    require(args.tag == 'v' + json.loads((ROOT / 'release/policy.json').read_text())['version'], 'version mismatch')
    {'registry': registry, 'inventory': inventory, 'checksums': checksums, 'publish': publish}[args.phase](Path(args.output).resolve(), args.tag)
