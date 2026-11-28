#!/usr/bin/env python3
"""Linux build-only syscall filter; it is not a provider isolation profile."""

import argparse
import ctypes
import ctypes.util
import errno
import hashlib
import json
import os
from pathlib import Path
import socket
import stat
import sys
import tempfile

# libseccomp 2.6.0 public ABI:
# https://github.com/seccomp/libseccomp/blob/v2.6.0/include/seccomp.h.in
class Comparison(ctypes.Structure):
    _fields_ = [("arg", ctypes.c_uint), ("op", ctypes.c_uint), ("a", ctypes.c_uint64), ("b", ctypes.c_uint64)]


class Version(ctypes.Structure):
    _fields_ = [("major", ctypes.c_uint), ("minor", ctypes.c_uint), ("micro", ctypes.c_uint)]


def install():
    if sys.platform != "linux":
        raise RuntimeError("offline source check requires Linux syscall enforcement")
    library = ctypes.util.find_library("seccomp")
    if not library:
        raise RuntimeError("libseccomp prerequisite is unavailable")
    lib = ctypes.CDLL(library, use_errno=True)
    lib.seccomp_version.restype = ctypes.POINTER(Version)
    version = lib.seccomp_version().contents
    if (version.major, version.minor) < (2, 5):
        raise RuntimeError("unsupported libseccomp ABI")
    lib.seccomp_init.argtypes = [ctypes.c_uint32]
    lib.seccomp_init.restype = ctypes.c_void_p
    lib.seccomp_release.argtypes = [ctypes.c_void_p]
    lib.seccomp_attr_set.argtypes = [ctypes.c_void_p, ctypes.c_uint, ctypes.c_uint32]
    lib.seccomp_syscall_resolve_name.argtypes = [ctypes.c_char_p]
    lib.seccomp_syscall_resolve_name.restype = ctypes.c_int
    lib.seccomp_rule_add_array.argtypes = [ctypes.c_void_p, ctypes.c_uint32, ctypes.c_int, ctypes.c_uint, ctypes.POINTER(Comparison)]
    lib.seccomp_export_bpf.argtypes = [ctypes.c_void_p, ctypes.c_int]
    lib.seccomp_load.argtypes = [ctypes.c_void_p]
    context = lib.seccomp_init(0x7FFF0000)  # SCMP_ACT_ALLOW
    if not context:
        raise RuntimeError("cannot create offline syscall filter")

    def require(result):
        if result != 0:
            raise RuntimeError("offline syscall filter operation failed: " + str(result))

    try:
        require(lib.seccomp_attr_set(context, 3, 1))  # CTL_NNP
        require(lib.seccomp_attr_set(context, 2, 0x80000000))  # bad ABI: KILL_PROCESS
        deny = 0x00050000 | errno.EPERM  # SCMP_ACT_ERRNO
        for name in ("socket", "socketpair", "io_uring_setup", "io_uring_register", "io_uring_enter"):
            number = lib.seccomp_syscall_resolve_name(name.encode())
            if number < 0:
                raise RuntimeError("required syscall is unknown: " + name)
            if name in ("socket", "socketpair"):
                comparison = Comparison(0, 1, socket.AF_UNIX, 0)  # SCMP_CMP_NE
                require(lib.seccomp_rule_add_array(context, deny, number, 1, ctypes.byref(comparison)))
            else:
                require(lib.seccomp_rule_add_array(context, deny, number, 0, None))
        with tempfile.TemporaryFile() as binary:
            require(lib.seccomp_export_bpf(context, binary.fileno()))
            binary.seek(0)
            bpf_sha256 = hashlib.sha256(binary.read()).hexdigest()
        require(lib.seccomp_load(context))
        return {"pid": os.getpid(), "libseccomp": [version.major, version.minor, version.micro], "bpf_sha256": bpf_sha256, "installed": True, "wrapper_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest()}
    finally:
        lib.seccomp_release(context)


def close_inherited_descriptors():
    for descriptor in (0, 1, 2):
        try:
            metadata = os.fstat(descriptor)
        except OSError as error:
            if error.errno == errno.EBADF:
                continue
            raise
        if stat.S_ISSOCK(metadata.st_mode):
            with socket.socket(fileno=os.dup(descriptor)) as stream:
                if stream.family != socket.AF_UNIX:
                    raise RuntimeError("offline command cannot inherit Internet standard streams")
    os.closerange(3, os.sysconf("SC_OPEN_MAX"))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--receipt", type=Path, required=True)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command
    if command[:1] == ["--"]:
        command = command[1:]
    if not command or not Path(command[0]).is_absolute():
        parser.error("an absolute executable path is required")
    if args.receipt.exists() or args.receipt.is_symlink():
        parser.error("offline filter receipt must be new")
    allowed = {
        "PATH", "LANG", "LC_ALL", "HOME", "TMPDIR", "CARGO_HOME", "CARGO_TARGET_DIR",
        "RUSTC", "CARGO_BUILD_JOBS", "CARGO_INCREMENTAL", "CARGO_PROFILE_DEV_DEBUG",
        "CARGO_PROFILE_TEST_DEBUG", "AGENT_WORKFLOW_CODEX_BUILD_ID",
    }
    environment = {key: value for key, value in os.environ.items() if key in allowed}
    close_inherited_descriptors()
    receipt = install()
    with args.receipt.open("x") as output:
        output.write(json.dumps(receipt, indent=2) + "\n")
    os.execve(command[0], command, environment)


if __name__ == "__main__":
    main()
