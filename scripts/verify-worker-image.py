#!/usr/bin/env python3
"""Exercise the distribution worker's native tools and pinned SDK offline."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--runtime', choices=['docker', 'podman'], required=True)
    parser.add_argument('--image', required=True)
    args = parser.parse_args()
    if not re.fullmatch(r'sha256:[0-9a-f]{64}', args.image):
        parser.error('image must be a local immutable image ID')
    root = Path(__file__).resolve().parents[1]
    pins = json.loads((root / 'images/pins.json').read_text())
    with tempfile.TemporaryDirectory(prefix='worker-offline-verification-') as directory:
        fixture = Path(directory)
        # This public fixture is mounted under the image's non-root user. No
        # provider configuration, host home directory or credentials are mounted.
        fixture.chmod(0o755)
        (fixture / 'pkg/clirunner').mkdir(parents=True)
        (fixture / 'bridge').mkdir()
        for path in [fixture / 'pkg', fixture / 'pkg/clirunner', fixture / 'bridge']:
            path.chmod(0o755)
        (fixture / 'bridge/claude').symlink_to('/opt/agent-workflow/bridge')
        binary = fixture / 'clirunner.test'
        subprocess.run(['go', 'test', '-c', '-o', str(binary), './pkg/clirunner'],
                       cwd=root, env=dict(os.environ, CGO_ENABLED='0'), check=True, timeout=180)
        common = [args.runtime, 'run', '--rm', '--pull=never', '--network', 'none',
                  '--read-only', '--cap-drop', 'all', '--security-opt', 'no-new-privileges',
                  '--pids-limit', '512', '--memory', '1g', '--tmpfs', '/tmp:rw,size=512m',
                  '--env', 'HOME=/tmp/home']

        def run(options):
            # The private cidfile can only identify the container created by
            # this invocation. Cleanup never searches or removes other workers.
            cidfile = fixture / 'container.cid'
            cidfile.unlink(missing_ok=True)
            try:
                subprocess.run(common + ['--cidfile', str(cidfile)] + options,
                               check=True, timeout=420)
            finally:
                if cidfile.exists():
                    container = cidfile.read_text().strip()
                    if re.fullmatch(r'[0-9a-f]{64}', container):
                        subprocess.run([args.runtime, 'rm', '--force', container],
                                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                       timeout=30, check=False)

        run(['--entrypoint', '/bin/sh', args.image, '-ec',
             'test "$(go env GOVERSION)" = "' + pins['go_toolchain'] + '"; '
             'test "$(npm --version)" = "' + pins['npm_cli'] + '"; '
             'test "$(codex --version)" = "codex-cli ' + pins['codex_cli'] + '"; '
             'node --version; git --version; bash --version; test "$(go env CGO_ENABLED)" = 0'])
        run(['--env', 'AGENT_WORKFLOW_SDK_TEST=1', '--volume', str(fixture) + ':/verification:ro',
             '--workdir', '/verification/pkg/clirunner', '--entrypoint', '/verification/clirunner.test',
             args.image, '-test.run', '^TestSDK(Bridge|Broker|Conversation)AgainstFakeGateway$',
             '-test.v', '-test.timeout', '5m'])


if __name__ == '__main__':
    main()
