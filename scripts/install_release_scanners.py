#!/usr/bin/env python3
"""Install pinned release scanners from checksum-verified public archives."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import urllib.request

PINS = Path(__file__).with_name('release-scanners.json')


def sha256(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def install(arch, cache, destination, pins=None):
    pins = json.loads(PINS.read_text()) if pins is None else pins
    cache, destination = Path(cache), Path(destination)
    cache.mkdir(parents=True, exist_ok=True)
    destination.mkdir(parents=True, exist_ok=True)
    for tool, pin in pins.items():
        selected = pin['archives'][arch]
        archive = cache / selected['name']
        if not archive.exists():
            with tempfile.NamedTemporaryFile(dir=cache, delete=False) as temporary:
                downloaded = Path(temporary.name)
                try:
                    with urllib.request.urlopen(pin['release'] + selected['name'], timeout=120) as response:
                        shutil.copyfileobj(response, temporary)
                    temporary.close()
                    if sha256(downloaded) != selected['sha256']:
                        raise ValueError(f'{tool} download checksum mismatch')
                    os.replace(downloaded, archive)
                finally:
                    downloaded.unlink(missing_ok=True)
        # Recheck restored cache bytes before extracting or executing anything.
        if archive.is_symlink() or sha256(archive) != selected['sha256']:
            raise ValueError(f'{tool} cached archive checksum mismatch')
        with tarfile.open(archive, 'r:gz') as bundle:
            members = [member for member in bundle.getmembers() if member.name == tool]
            if len(members) != 1 or not members[0].isfile():
                raise ValueError(f'{tool} archive binary must be a regular file')
            with bundle.extractfile(members[0]) as binary:
                with tempfile.NamedTemporaryFile(dir=destination, delete=False) as temporary:
                    installed = Path(temporary.name)
                    try:
                        shutil.copyfileobj(binary, temporary)
                        temporary.close()
                        installed.chmod(0o755)
                        os.replace(installed, destination / tool)
                    finally:
                        installed.unlink(missing_ok=True)
        version = subprocess.run([str(destination / tool), '--version'], check=True,
                                 capture_output=True, text=True).stdout.splitlines()
        expected = f'syft {pin["version"]}' if tool == 'syft' else f'Version: {pin["version"]}'
        if not version or version[0] != expected:
            raise ValueError(f'{tool} binary version mismatch')
        print(f'Installed {tool} {pin["version"]} from verified {selected["name"]}', flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--arch', required=True, choices=['amd64', 'arm64'])
    parser.add_argument('--cache', type=Path, required=True)
    parser.add_argument('--bin-dir', type=Path, required=True)
    args = parser.parse_args()
    install(args.arch, args.cache, args.bin_dir)


if __name__ == '__main__':
    main()
