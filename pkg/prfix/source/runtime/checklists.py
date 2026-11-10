"""Deterministic completion contracts, separate from analysis fingerprints."""
import base64
from copy import deepcopy
import re
import uuid

from state import digest, read, write


def contract(spec, definition):
    custom = definition.get('checklist', {})
    if not isinstance(custom, dict) or set(custom) - {'items', 'max_checks', 'required'}:
        raise ValueError('Checklist accepts items, max_checks and required')
    required = definition.get('required', spec.get('options', {}).get('invocation', {}).get('required', True))
    if type(required) is not bool:
        raise ValueError('Task required must be boolean')
    required = custom.get('required', required)
    maximum = custom.get('max_checks', 3)
    if type(required) is not bool or type(maximum) is not int or maximum < 1:
        raise ValueError('Checklist requires boolean required and positive integer max_checks')
    items = [
        {'id': 'scope', 'text': 'Cover the entire assigned scope under current governing instructions and permissions.'},
        {'id': 'evidence', 'text': 'Support conclusions with retained source or execution evidence; preserve disagreements and blockers.'},
        {'id': 'validation', 'text': 'Perform the validation required by the assignment and report its results or limitations.'},
        {'id': 'deliverables', 'text': 'Supply every assigned deliverable and an explicit complete primary report.'},
    ]
    role = spec['task']['id'].split(':', 1)[0]
    if ('fix' in role or role in {'feedback', 'ci-repair'}
            or spec['skill']['identity'].split(':')[-1] == 'dk-pr-feedback-address'):
        items += [
            {'id': 'dispositions', 'text': 'Account for every assigned finding with a sourced disposition and remaining blockers.'},
            {'id': 'tests', 'text': 'Run relevant tests for the fixes and retain results; finish missing required checks.'},
        ]
    if role == 'finding-verification':
        items.append({'id': 'verification', 'text': 'Challenge every candidate independently and return confirmed, rejected or uncertain with evidence and required locations.'})
    if 'native_review' in spec:
        items.append({'id': 'native-evidence', 'text': 'Retain actual terminal native observations for every required role and driver; never synthesize missing evidence.'})
    options = spec.get('options', {})
    execution = options.get('execution', options)
    if not isinstance(execution, dict):
        execution = {}
    publication = execution.get('publication', {})
    if isinstance(publication, dict) and publication.get('assigned') is True and publication.get('authorized') is True:
        items.append({'id': 'publication', 'text': 'Reconcile assigned authorized publications and verify current checks and operation receipts.'})
    if role == 'workflow-completion':
        from skill_config import groups
        # The selected plan is an explicit governing input, including [];
        # omitted default duties must never be restored by this checklist.
        if 'completion_plan' not in options:
            raise ValueError('Workflow completion requires the selected completion_plan')
        plan = options['completion_plan']
        if not isinstance(plan, list):
            raise ValueError('Workflow completion_plan must be a list')
        selected_groups = groups(plan)
        items += [
            {'id': 'live-inputs', 'text': 'Refresh live scope, instructions, discussion, Decisions, head and target and affected coverage.'},
            {'id': 'coverage', 'text': 'Verify required reviews and stages, finding dispositions or explicit acceptances, and reconciled publication receipts.'},
            {'id': 'report', 'text': 'Report selected-plan completion separately from PR readiness, skipped duties, optional omissions and CI exceptions.'},
        ]
        for index, call in enumerate(call for group in selected_groups for call in group):
            items.append({'id': 'finalizer-' + str(index) + '-' + digest(call)[:16],
                          'text': 'Verify selected finalizer completion and evidence: ' + str(call)})
    supplied = custom.get('items', [])
    if not isinstance(supplied, list):
        raise ValueError('Checklist items must be a list')
    ids = {item['id'] for item in items}
    for item in supplied:
        if (not isinstance(item, dict) or set(item) != {'id', 'text'}
                or not isinstance(item['id'], str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]*', item['id'])
                or item['id'] in ids or not isinstance(item['text'], str) or not item['text'].strip()):
            raise ValueError('Custom checklist items need unique stable IDs and nonempty text')
        ids.add(item['id'])
        items.append(deepcopy(item))
    return {'items': items, 'max_checks': maximum, 'required': required}


def attach(store, entry, captured, value):
    """Refresh acceptance only; never mutate the original analysis or attempts."""
    previous = entry.get('checklist', {})
    result = entry.get('result', {}).get('hash')
    if (previous.get('contract') != value or previous.get('fingerprint') != captured['fingerprint']
            or previous.get('result') != result):
        prior = []
        if previous.get('fingerprint') == captured['fingerprint'] and previous.get('result') == result:
            references = previous.get('prior_findings', []) + [r['response'] for r in previous['rounds']
                                                             if 'response' in r]
            prior = [reference for reference in references
                     if load_response(store, reference)['report'].get('findings')]
            prior = list({reference['hash']: reference for reference in prior}.values())
        entry['checklist'] = {'contract': deepcopy(value), 'fingerprint': captured['fingerprint'],
                              'result': result, 'rounds': [], 'outcome': None,
                              **({'prior_findings': prior} if prior else {})}


def summary(store, entry, captured):
    gate = entry.get('checklist')
    if gate is None:
        return None
    current = (gate['fingerprint'] == captured['fingerprint']
               and gate['result'] == entry.get('result', {}).get('hash'))
    rounds = gate['rounds']
    if current and gate['outcome'] is not None:
        try:
            supplemental(store, entry)
            if len(rounds) > gate['contract']['max_checks']:
                raise ValueError('Invalid checklist rounds')
            if rounds:
                report = load_response(store, rounds[-1]['response'])['report']
            else:
                saved, problem = store._load_saved(entry)
                if problem or saved['status'] != 'complete':
                    raise ValueError('Missing complete primary checklist evidence')
                report = saved['normalized']
            validate_report(gate, report)
            expected = ('complete' if report['complete'] else
                        'failed' if gate['contract']['required'] else 'best_effort')
            if (expected != gate['outcome'] or
                    (not report['complete'] and len(rounds) != gate['contract']['max_checks'])):
                raise ValueError('Invalid checklist outcome')
        except (OSError, ValueError, KeyError, TypeError):
            current = False
    return {'complete': current and gate['outcome'] == 'complete',
            'accepted': current and gate['outcome'] in {'complete', 'best_effort'},
            'outcome': gate['outcome'] if current else 'stale',
            'required': gate['contract']['required'], 'max_checks': gate['contract']['max_checks'],
            'checks_used': len(rounds),
            'pending_round': rounds[-1]['id'] if current and rounds and 'response' not in rounds[-1] else None,
            'responses': [r['response'] for r in rounds if 'response' in r]}


def reserve(gate):
    if gate['outcome'] is None and (not gate['rounds'] or 'response' in gate['rounds'][-1]):
        if len(gate['rounds']) >= gate['contract']['max_checks']:
            raise ValueError('Checklist check allowance exhausted')
        gate['rounds'].append({'id': uuid.uuid4().hex})


def load_response(store, reference):
    path = store.root/'results'/reference['file']
    if path.parent.resolve() != (store.root/'results').resolve():
        raise ValueError('Invalid checklist response reference')
    value = read(path)
    if digest(value) != reference['hash']:
        raise ValueError('Damaged checklist response')
    return value


def supplemental(store, entry):
    """Read retained follow-up findings without changing the primary output."""
    gate = entry.get('checklist', {})
    references = gate.get('prior_findings', []) + [r['response'] for r in gate.get('rounds', [])
                                                if 'response' in r]
    findings, sources, seen = [], [], set()
    for reference in references:
        report = load_response(store, reference)['report']
        added = report.get('findings', [])
        if not isinstance(added, list):
            raise ValueError('Checklist findings must be a list')
        if added:
            sources.append(str(store.root/'results'/reference['file']))
        for finding in added:
            key = digest(finding)
            if key not in seen:
                findings.append(deepcopy(finding))
                seen.add(key)
    return findings, list(dict.fromkeys(sources))


def require_responses(store, entry):
    """Protect submissions and every completion/replay response alike."""
    try:
        supplemental(store, entry)
    except (OSError, ValueError, KeyError, TypeError) as exc:
        raise ValueError('Checklist requires intact earlier response evidence') from exc


def reply(store, entry, *, round_index=None):
    require_responses(store, entry)
    gate = entry['checklist']
    rounds = gate['rounds']
    if not rounds and gate['outcome'] == 'complete':
        saved, problem = store._load_saved(entry)
        if problem or saved['status'] != 'complete':
            raise ValueError('Missing complete primary checklist evidence')
        validate_report(gate, saved['normalized'])
        return {'action': 'complete', 'status': 'complete', 'key': entry['key'],
                'result': saved['path'], 'checklist_complete': True, 'omissions': []}
    index = len(rounds) - 1 if round_index is None else round_index
    round_ = rounds[index]
    if 'response' in round_:
        report = load_response(store, round_['response'])['report']
        omissions = [item for item in report['checklist'] if item['status'] == 'unfinished']
        if not omissions or index + 1 == gate['contract']['max_checks']:
            return {'action': 'complete' if not omissions or not gate['contract']['required'] else 'failed',
                    'status': 'complete' if not omissions or not gate['contract']['required'] else 'failed',
                    'key': entry['key'], 'result': str(store.root/'results'/entry['result']['file']),
                    'checklist_complete': not omissions, 'omissions': omissions}
        round_ = rounds[index + 1]
        index += 1
    return {'action': 'recheck', 'status': 'checking', 'key': entry['key'],
            'claim': load_primary_claim(store, entry), 'round': round_['id'], 'check_number': index + 1,
            'checklist': gate['contract'], 'checklist_complete': False,
            'result': str(store.root/'results'/entry['result']['file']),
            'prompt': 'Verify every checklist item against the retained assignment and original evidence. '
                      'Finish missing work within permission. Return every item as completed with evidence references '
                      'or unfinished with a reason. Preserve completed analysis and original native evidence.'}


def load_primary_claim(store, entry):
    saved, problem = store._load_saved(entry)
    if problem:
        raise ValueError('Checklist requires intact primary evidence: ' + problem)
    return saved['claim']


def validate_report(gate, report):
    outcomes = report.get('checklist') if isinstance(report, dict) else None
    if (not isinstance(outcomes, list) or type(report.get('complete')) is not bool
            or len(outcomes) != len(gate['contract']['items'])
            or not isinstance(report.get('findings', []), list)):
        raise ValueError('Checklist report requires explicit completion and every item outcome')
    ids = set()
    for item in outcomes:
        if not isinstance(item, dict) or not isinstance(item.get('id'), str) or item['id'] in ids:
            raise ValueError('Checklist outcomes require unique item IDs')
        ids.add(item['id'])
        if item.get('status') == 'completed':
            refs = item.get('evidence')
            if not isinstance(refs, list) or not refs or any(not isinstance(ref, str) or not ref.strip() for ref in refs):
                raise ValueError('Completed checklist items require evidence references')
        elif item.get('status') == 'unfinished':
            if not isinstance(item.get('reason'), str) or not item['reason'].strip():
                raise ValueError('Unfinished checklist items require a reason')
        else:
            raise ValueError('Checklist outcome must be completed or unfinished')
    if ids != {item['id'] for item in gate['contract']['items']}:
        raise ValueError('Checklist report must cover exactly the assigned items')
    complete = all(item['status'] == 'completed' for item in outcomes)
    if report['complete'] != complete:
        raise ValueError('Checklist complete must agree with per-item outcomes')


def submit(store, entry, round_id, original, report):
    gate = entry['checklist']
    validate_report(gate, report)
    require_responses(store, entry)
    response = {'original': base64.b64encode(original).decode(), 'report': report}
    fingerprint = digest(response)
    index = next((i for i, r in enumerate(gate['rounds']) if r['id'] == round_id), None)
    if index is None:
        raise ValueError('Unknown checklist round')
    round_ = gate['rounds'][index]
    if 'response' in round_:
        if round_['response']['hash'] != fingerprint:
            raise ValueError('Checklist round already has a different submission')
        return reply(store, entry, round_index=index)
    if index != len(gate['rounds']) - 1 or gate['outcome'] is not None:
        raise ValueError('Checklist round is not pending')
    reference = {'file': fingerprint + '.json', 'hash': fingerprint}
    write(store.root/'results'/reference['file'], response)
    round_['response'] = reference
    if report['complete']:
        gate['outcome'] = 'complete'
    elif len(gate['rounds']) == gate['contract']['max_checks']:
        gate['outcome'] = 'failed' if gate['contract']['required'] else 'best_effort'
    else:
        reserve(gate)
    return reply(store, entry, round_index=index)
