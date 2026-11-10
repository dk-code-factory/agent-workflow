"""Bound transient retries to known read-only GitHub CLI operations."""
import json
import os
from pathlib import Path
import random
import re
import selectors
import subprocess
import time

from shared.ci_deadline import ACTIVE_BUDGET


def _bounded_run(argv, *, stdout_limit, timeout, **kwargs):
    """Read binary output with a byte cap and the caller's subprocess deadline."""
    if kwargs != {'capture_output': True}:
        raise ValueError('bounded GitHub reads require binary captured output')
    deadline = time.monotonic() + timeout
    output, errors = bytearray(), bytearray()
    with subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE) as process:
        try:
            with selectors.DefaultSelector() as ready:
                ready.register(process.stdout, selectors.EVENT_READ, output)
                ready.register(process.stderr, selectors.EVENT_READ, errors)
                while ready.get_map():
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        raise subprocess.TimeoutExpired(argv, timeout)
                    for key, _ in ready.select(remaining):
                        chunk = os.read(key.fd, 65536)
                        if not chunk:
                            ready.unregister(key.fileobj)
                            continue
                        destination = key.data
                        if destination is output:
                            if len(output) + len(chunk) > stdout_limit:
                                raise RuntimeError('GitHub output exceeds collection limit')
                            output.extend(chunk)
                        else:
                            # Keep diagnostics bounded even if the transport is noisy.
                            errors.extend(chunk[:max(0, 65536-len(errors))])
            process.wait(timeout=max(0, deadline-time.monotonic()))
        except BaseException:
            process.kill()
            process.wait()
            raise
    return subprocess.CompletedProcess(argv, process.returncode, bytes(output), bytes(errors))


def read_only(argv, input_text=None):
    if len(argv) < 3 or Path(argv[0]).name != 'gh':
        return False
    if argv[1] in ('pr', 'repo', 'issue'):
        return argv[2] in ('view', 'list', 'checks', 'diff')
    if argv[1] != 'api':
        return False
    endpoint, methods, fields, body_file = None, [], [], None
    values = {'--hostname', '--method', '-X', '-f', '-F', '--field', '--raw-field',
              '--input', '--jq', '-q', '--template', '-t', '--header', '-H', '--cache'}
    flags = {'--paginate', '--slurp', '--silent', '--include', '-i', '--verbose'}
    args = iter(argv[2:])
    for arg in args:
        option, separator, inline = arg.partition('=')
        if option in values:
            value = inline if separator else next(args, None)
            if value is None:
                return False
            if option in ('--method', '-X'):
                methods.append(value.upper())
            elif option in ('-f', '-F', '--field', '--raw-field'):
                fields.append(value)
            elif option == '--input':
                body_file = value
        elif arg in flags:
            continue
        elif arg.startswith('-') or endpoint is not None:
            return False
        else:
            endpoint = arg
    if endpoint == 'graphql':
        queries = [field.split('=', 1)[1] for field in fields if field.startswith('query=')]
        if body_file == '-':
            try:
                payload = json.loads(input_text)
                queries.append(payload['query'])
            except (ValueError, TypeError, KeyError):
                return False
        if len(queries) != 1 or any(method not in ('GET', 'POST') for method in methods):
            return False
        query = queries[0]
        return (isinstance(query, str) and bool(re.match(r'\s*(?:query\b|\{)', query))
                and not re.search(r'\b(?:mutation|subscription)\b', query))
    return bool(endpoint) and (all(method == 'GET' for method in methods) if methods
                               else not fields and body_file is None)


def transient(result):
    detail = result.stderr or ''
    if isinstance(detail, bytes):
        detail = detail.decode(errors='replace')
    if re.search(r'HTTP (?:401|403)|not authenticated|authentication failed|bad credentials', detail, re.I):
        return False
    return bool(re.search(r'HTTP 5\d\d|error connecting to|TLS handshake timeout|'
                          r'connection reset|connection refused|unexpected EOF|no such host|'
                          r'context deadline exceeded|net/http:.*timeout', detail, re.I))


def run(argv, **kwargs):
    """Keep original result/exception semantics; never retry publication or schema errors."""
    stdout_limit = kwargs.pop('stdout_limit', None)
    runner = subprocess.run if stdout_limit is None else lambda argv, **options: _bounded_run(
        argv, stdout_limit=stdout_limit, **options)
    if not read_only(argv, kwargs.get('input')):
        return runner(argv, **kwargs)
    classification = ACTIVE_BUDGET.get()
    if classification is not None:
        kwargs['timeout'] = min(kwargs.get('timeout') or 60, classification.remaining())
    budget = kwargs.get('timeout')
    budget = 60 if budget is None else min(60, budget)
    deadline = time.monotonic() + budget
    for attempt in range(3):
        remaining = max(0, deadline - time.monotonic())
        if classification is not None:
            remaining = min(remaining, classification.remaining())
        options = dict(kwargs, timeout=remaining)
        failure = None
        try:
            result = runner(argv, **options)
        except subprocess.TimeoutExpired as exc:
            failure = exc
        else:
            if not result.returncode or not transient(result):
                return result
        remaining = deadline - time.monotonic()
        if classification is not None:
            remaining = min(remaining, classification.remaining())
        delay = 0.25 * (2 ** attempt) + random.uniform(0, 0.25)
        if attempt == 2 or remaining <= delay:
            if failure is not None:
                raise failure
            return result
        time.sleep(delay)
