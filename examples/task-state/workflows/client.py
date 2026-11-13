"""Scoped task-state-v1 client for an action-v1 helper; standard library only."""

import http.client
import json
import socket


class TaskStateError(RuntimeError):
    """An RPC failed; this alone does not establish whether its write committed."""


class _UnixConnection(http.client.HTTPConnection):
    def __init__(self, path, timeout):
        super().__init__("localhost", timeout=timeout)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        try:
            self.sock.connect(self.path)
        except BaseException:
            self.sock.close()
            self.sock = None
            raise


class TaskState:
    def __init__(self, request, timeout=15):
        """Use only the current hook's descriptor; never persist its token."""
        descriptor = request.get("task_state")
        if not isinstance(descriptor, dict):
            raise TaskStateError("run requires an admitted task group")
        self._path = descriptor["socket"]
        self._token = descriptor["token"]
        self.group = descriptor["group"]
        self.timeout = timeout

    def call(self, operation, **fields):
        """Send once. Callers must retain operation/request IDs before any retry."""
        body = json.dumps({**fields, "operation": operation}, allow_nan=False).encode()
        if len(body) > 64 * 1024:
            raise ValueError("task-state request exceeds 64 KiB")
        connection = _UnixConnection(self._path, self.timeout)
        try:
            connection.request("POST", "/v1", body=body, headers={
                "Authorization": "Bearer " + self._token,
                "Content-Type": "application/json",
            })
            response = connection.getresponse()
            raw = response.read(1024 * 1024 + 1)
            if len(raw) > 1024 * 1024:
                raise TaskStateError("task-state response exceeds 1 MiB")
            if response.status != 200:
                raise TaskStateError("task-state RPC failed (%d): %s" % (
                    response.status, raw.decode("utf-8", errors="replace")))
            return json.loads(raw)
        finally:
            connection.close()
