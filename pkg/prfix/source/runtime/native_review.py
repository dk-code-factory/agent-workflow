"""Validate coordinator-captured native host observations, never reviewer assertions."""
import hashlib
import json
import re


def required(components):
    task = components['task']['id']
    skill = components['skill']['identity'].split('@')[0].split(':')[-1]
    return ('native_review' in components or task == 'finding-verification'
            or task.startswith('finding-verification:')
            or re.search(r'(^|[-_:])review($|[-_:])', task) is not None
            or skill.startswith('code-review'))


def contract(value):
    if not isinstance(value, dict) or value.get('schema') != 1:
        raise ValueError('native_review requires schema 1')
    if not isinstance(value.get('skill_version'), str) or not value['skill_version'].strip():
        raise ValueError('native_review requires the resolved skill version')
    passes = value.get('passes')
    if not isinstance(passes, list):
        raise ValueError('native_review requires the complete applicable passes list')
    roles = set()
    for item in passes:
        if not isinstance(item, dict) or not isinstance(item.get('role'), str) or not item['role'].strip():
            raise ValueError('native_review requires nonempty role names')
        if item['role'] in roles:
            raise ValueError('native_review roles must be unique')
        roles.add(item['role'])
        if not isinstance(item.get('drivers'), list):
            raise ValueError('native_review requires each role driver argv list')
        for argv in item['drivers']:
            if not isinstance(argv, list) or not argv or any(not isinstance(arg, str) or not arg for arg in argv):
                raise ValueError('native_review driver argv must contain nonempty strings')
        if len({tuple(argv) for argv in item['drivers']}) != len(item['drivers']):
            raise ValueError('native_review driver commands must be unique per role')
    return value


def binding(components, claim):
    """Expected identities for a collector; never synthesizes observed events."""
    from state import digest
    expected = components.get('native_review')
    if expected is None:
        raise ValueError('Reviewer execution requirements were not captured before invocation')
    contract(expected)
    return {'schema': 1, 'source': 'native-host', 'claim': claim['id'],
            'fingerprint': claim['fingerprint'], 'skill_identity': components['skill']['identity'],
            'skill_version': expected['skill_version'], 'resources': digest(components['skill']['resources']),
            'pins': claim['provenance']}


def assess(components, claim, original, evidence):
    """Return a diagnostic instead of manufacturing execution from a complete report.

    Evidence is collected by the trusted host coordinator from native tool
    observations. This checks its structure and binding, not authenticity of a
    supplied transcript or semantic adherence to every skill instruction.
    """
    expected = components.get('native_review')
    if expected is None:
        return {'code': 'missing_contract', 'reason': 'Reviewer execution requirements were not captured before invocation'}
    try:
        wanted_binding = binding(components, claim)
        if evidence is None:
            return {'code': 'unavailable_provenance', 'reason': 'Native host observations were not supplied; execution is unestablished'}
        observed = json.loads(evidence)
        if not isinstance(observed, dict):
            raise ValueError('Native evidence must be an object')
        if observed.get('availability') != 'available':
            return {'code': 'unavailable_provenance', 'reason': 'Native host transport is unavailable or unsupported; execution is unestablished'}
        if any(observed.get(key) != value for key, value in wanted_binding.items()):
            return {'code': 'binding_mismatch', 'reason': 'Native observations do not identify this claim, skill resources/version and pinned inputs'}
        root = observed.get('invocation', {})
        if not isinstance(root, dict) or not isinstance(root.get('handle'), str) or not root['handle']:
            raise ValueError('Native invocation handle is missing')
        if root.get('state') != 'completed' or root.get('output_sha256') != hashlib.sha256(original).hexdigest():
            return {'code': 'incomplete_invocation', 'reason': 'Root native invocation did not successfully terminate with this original output'}
        if not root.get('receipt'):
            raise ValueError('Root native terminal observation is missing')
        # A complete inventory distinguishes missing work from missing telemetry.
        if observed.get('inventory_complete') is not True:
            return {'code': 'unavailable_provenance', 'reason': 'Native child/tool inventory is incomplete; execution is unestablished'}
        passes = observed.get('passes')
        if not isinstance(passes, list) or any(not isinstance(item, dict) for item in passes):
            raise ValueError('Native pass observations are missing')
        roles = [item.get('role') for item in passes]
        wanted = [item['role'] for item in expected['passes']]
        if len(roles) != len(set(roles)) or set(roles) != set(wanted):
            return {'code': 'missing_pass', 'reason': 'Observed independent passes do not match the captured applicable requirements'}
        handles = [item.get('handle') for item in passes]
        if (any(not isinstance(handle, str) or not handle for handle in handles)
                or len(set(handles)) != len(handles) or root['handle'] in handles):
            return {'code': 'nonindependent_pass', 'reason': 'Required roles lack distinct native child handles under the selected invocation'}
        handles, calls = {root['handle']}, set()
        for requirement in expected['passes']:
            item = passes[roles.index(requirement['role'])]
            handle = item.get('handle')
            if not isinstance(handle, str) or not handle or handle in handles or item.get('parent') != root['handle']:
                return {'code': 'nonindependent_pass', 'reason': 'Required roles lack distinct native child handles under the selected invocation'}
            handles.add(handle)
            if item.get('state') != 'completed' or not isinstance(item.get('output'), str) or not item['output'].strip():
                return {'code': 'incomplete_pass', 'reason': 'A required native role has no successful terminal result and separate original output'}
            if not item.get('spawn_receipt') or not item.get('terminal_receipt'):
                raise ValueError('Native child spawn/terminal observations are missing')
            drivers = item.get('drivers')
            if not isinstance(drivers, list) or any(not isinstance(driver, dict) for driver in drivers):
                raise ValueError('Native driver observations are missing')
            if [driver.get('argv') for driver in drivers] != requirement['drivers']:
                return {'code': 'missing_driver', 'reason': 'Required deeper-look driver executions are absent or differ from captured commands'}
            for driver in drivers:
                call = driver.get('call_id')
                if (not isinstance(call, str) or not call or call in calls or driver.get('handle') != handle
                        or driver.get('state') != 'completed' or type(driver.get('exit_code')) is not int
                        or driver['exit_code'] != 0 or not isinstance(driver.get('output'), str)
                        or not driver['output'].strip() or not driver.get('receipt')):
                    return {'code': 'incomplete_driver', 'reason': 'A required driver lacks a distinct successful native tool observation in its role session'}
                calls.add(call)
        return None
    except (ValueError, TypeError, KeyError, AttributeError, RecursionError):
        return {'code': 'malformed_provenance', 'reason': 'Native execution observations or captured requirements are malformed; execution is unestablished'}
