#!/usr/bin/env python3
"""Explicit, bounded preparation of missing pinned crates in owned build storage.

This command is separate from offline Cargo stages. It never resolves versions,
changes locks, installs dependencies, runs build scripts, or writes shared caches.
"""

import argparse
import hashlib
import json
from pathlib import Path
import shutil
import tempfile
import time
import tomllib
import urllib.request
from urllib.parse import quote, urlsplit

MAX_ARCHIVE = 64 << 20
MAX_TOTAL = 512 << 20
MIN_FREE = 24 << 30
MAX_SECONDS = 15 * 60
REGISTRY = "index.crates.io-1949cf8c6b5b557f"
SOURCE = "registry+https://github.com/rust-lang/crates.io-index"


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def canonical(url):
    parsed = urlsplit(url)
    if parsed.scheme != "https" or parsed.hostname != "static.crates.io" or parsed.port not in (None, 443) or parsed.username or parsed.password or parsed.fragment or parsed.query:
        raise ValueError("archive URL leaves the canonical HTTPS host")
    return url


class Redirects(urllib.request.HTTPRedirectHandler):
    def __init__(self, receipt):
        self.receipt = receipt

    def redirect_request(self, request, response, code, message, headers, target):
        canonical(target)
        self.receipt.append({"from": request.full_url, "to": target, "status": code})
        return super().redirect_request(request, response, code, message, headers, target)


def save_archive(stream, destination, expected, guard):
    if destination.exists() or destination.is_symlink():
        raise ValueError("archive installation requires an absent owned destination")
    size, checksum = 0, hashlib.sha256()
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(dir=destination.parent, prefix=".crate-", delete=False) as output:
            temporary = Path(output.name)
            while chunk := stream.read(1 << 20):
                size += len(chunk)
                if size > MAX_ARCHIVE:
                    raise ValueError("archive exceeds its byte limit")
                guard(size)
                checksum.update(chunk)
                output.write(chunk)
        if checksum.hexdigest() != expected:
            raise ValueError("archive differs from the original locked SHA-256")
        # No process other than this explicit preparation command owns this cache.
        temporary.replace(destination)
        return size
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)


def prepare(args):
    root = args.output.resolve(strict=True)
    cache = root / "cargo-home"
    destination_root = cache / "registry/cache" / REGISTRY
    if any(path.is_symlink() for path in (cache, cache / "registry", cache / "registry/cache", destination_root)) or not destination_root.is_dir():
        raise ValueError("owned registry archive directory is missing")
    # Cargo's owned Git checkouts may hard-link their owned database packs.
    # Only the registry archive tree is writable by this preparation command.
    for path in destination_root.rglob("*"):
        if path.is_symlink() or path.is_file() and path.stat().st_nlink != 1:
            raise ValueError("owned cache contains a shared alias")
    receipt_path = root / args.receipt
    if Path(args.receipt).name != args.receipt or receipt_path.exists():
        raise ValueError("archive receipt must be a new file within owned storage")
    lock = root / "source/codex-rs/Cargo.lock"
    lock_hash = digest(lock)
    if lock_hash not in {
        "9e71c74824a7ea114d0a3a4c94e93d81a1ddd13ca8fa6f698cbd1284c2e9ff2f",
        "9efc26116852723f76540da85d9e4537979014f166c39494bef61e49a9cc0883",
    }:
        raise ValueError("archive preparation requires the reviewed exact 0.167.1 lock")
    packages = {p["name"] + "-" + p["version"] + ".crate": p for p in tomllib.loads(lock.read_text())["package"] if p.get("source") == SOURCE}
    missing = json.loads((root / "cache.json").read_text())["missing"]
    names = sorted({entry.removeprefix("archive:") for entry in missing if entry.startswith("archive:")})
    if len(names) > 53 or any(name not in packages for name in names):
        raise ValueError("archive preparation exceeds its explicit pinned set")
    started, total, rows = time.monotonic(), 0, []
    report = {"original_lock_sha256": lock_hash, "limits": {"archives": 53, "archive_bytes": MAX_ARCHIVE, "total_bytes": MAX_TOTAL, "seconds": MAX_SECONDS, "minimum_free_bytes": MIN_FREE}, "archives": rows}

    def guard(size=0):
        if time.monotonic() - started >= MAX_SECONDS or total + size > MAX_TOTAL or shutil.disk_usage(root).free < MIN_FREE:
            raise ValueError("archive preparation resource limit exceeded")

    try:
        for name in names:
            guard()
            package = packages[name]
            row = {"name": package["name"], "version": package["version"], "sha256": package["checksum"], "redirects": []}
            rows.append(row)
            target = destination_root / name
            if target.exists():
                if target.is_symlink() or target.stat().st_nlink != 1 or digest(target) != package["checksum"]:
                    raise ValueError("existing owned archive does not match its lock")
                row.update({"source": "owned-cache", "bytes": target.stat().st_size})
                continue
            candidate = args.from_cache / "registry/cache" / REGISTRY / name
            if candidate.is_file() and not candidate.is_symlink():
                with candidate.open("rb") as stream:
                    size = save_archive(stream, target, package["checksum"], guard)
                row.update({"source": str(candidate), "bytes": size})
            else:
                url = canonical("https://static.crates.io/crates/" + quote(package["name"], safe="") + "/" + quote(name, safe=""))
                row["url"] = url
                opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), Redirects(row["redirects"]))
                with opener.open(url, timeout=min(15, MAX_SECONDS - (time.monotonic() - started))) as response:
                    row.update({"final_url": canonical(response.url), "http_status": response.status})
                    if response.status != 200:
                        raise ValueError("canonical archive HTTP response is not 200")
                    size = save_archive(response, target, package["checksum"], guard)
                row.update({"source": "canonical-https", "bytes": size})
            total += size
        report["exit_code"] = 0
    except BaseException as error:
        report.update({"exit_code": 1, "error": type(error).__name__ + ": " + str(error)})
        raise
    finally:
        report.update({"elapsed_seconds": time.monotonic() - started, "prepared_bytes": total, "final_lock_sha256": digest(lock)})
        receipt_path.write_text(json.dumps(report, indent=2) + "\n")
        if report["final_lock_sha256"] != lock_hash:
            raise ValueError("archive preparation changed the source lock")
    print(json.dumps({"exit_code": 0, "archives": len(rows), "prepared_bytes": total, "receipt": str(receipt_path)}))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--from-cache", type=Path, required=True)
    parser.add_argument("--receipt", default="archive-preparation.json")
    prepare(parser.parse_args())
