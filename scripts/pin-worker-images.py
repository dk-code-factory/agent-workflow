#!/usr/bin/env python3
"""Resolve public upstream image tags to immutable manifest digests. No pulls."""
import hashlib
import json
from pathlib import Path
import urllib.parse
import urllib.request

root = Path(__file__).resolve().parents[1]
pins = {"go_toolchain": "go1.27.3", "claude_sdk": "0.3.311", "codex_cli": "0.164.0", "guardian_protocol": "1", "bridge_protocol": "2"}
for key, repository, tag in [("go_image", "library/golang", "1.27.3-bookworm"), ("node_image", "library/node", "24.22.1-bookworm")]:
    query = urllib.parse.urlencode({"service": "registry.docker.io", "scope": "repository:" + repository + ":pull"})
    token = json.load(urllib.request.urlopen("https://auth.docker.io/token?" + query))["token"]
    request = urllib.request.Request("https://registry-1.docker.io/v2/" + repository + "/manifests/" + tag,
        headers={"Authorization": "Bearer " + token, "Accept": "application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json"})
    with urllib.request.urlopen(request) as response:
        data = response.read()
        digest = "sha256:" + hashlib.sha256(data).hexdigest()
        if response.headers.get("Docker-Content-Digest") != digest:
            raise SystemExit("registry manifest digest verification failed")
        pins[key] = "docker.io/" + repository + "@" + digest
        pins[key + "_source_tag"] = tag
        manifest = json.loads(data)
        platforms = [item["platform"] for item in manifest.get("manifests", [])]
        for arch in ["amd64", "arm64"]:
            if not any(item.get("os") == "linux" and item.get("architecture") == arch for item in platforms):
                raise SystemExit("required image architecture missing")
destination = root / "images" / "pins.json"
destination.parent.mkdir(exist_ok=True)
destination.write_text(json.dumps(pins, indent=2) + "\n")
print(destination)
