"""One native invocation contract for processes, skills, reviewers and hooks."""
from copy import deepcopy
import re

SKILL_PATTERN = r'[A-Za-z0-9][A-Za-z0-9_.-]*:[A-Za-z0-9][A-Za-z0-9_.-]*'
EFFORTS = ('none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'ultra')
FIELDS = {'skill', 'instructions', 'options', 'agent', 'required'}


def text(value):
    return isinstance(value, str) and bool(value.strip()) and '\x00' not in value


def additional_instructions(assignment, prompt):
    if not assignment or assignment == prompt:
        return prompt
    return assignment + '\n\nAdditional configured instructions:\n' + prompt


def skill_name(value):
    if not isinstance(value, str) or not re.fullmatch(SKILL_PATTERN, value):
        raise ValueError('Skill must be fully qualified as plugin:skill, without a /skills/ prefix')
    return value


def validate_settings(value):
    if not isinstance(value, dict) or set(value) - (FIELDS - {'skill'}):
        raise ValueError('Unknown invocation setting')
    if 'instructions' in value and not text(value['instructions']):
        raise ValueError('Invocation instructions must be nonempty text without NUL bytes')
    if not isinstance(value.get('options', {}), dict):
        raise ValueError('Invocation options must be an object')
    if 'reviewers' in value.get('options', {}):
        reviewers = value['options']['reviewers']
        if not isinstance(reviewers, list):
            raise ValueError('Reviewers must be an ordered invocation list')
        for reviewer in reviewers:
            validate(reviewer)
    if type(value.get('required', True)) is not bool:
        raise ValueError('Invocation required must be boolean')
    if 'agent' in value:
        agent = value['agent']
        if not isinstance(agent, dict) or set(agent) - {'model', 'reasoning_effort'}:
            raise ValueError('Agent accepts model and reasoning_effort')
        if 'model' in agent and not text(agent['model']):
            raise ValueError('Agent model must be nonempty text without NUL bytes')
        if 'reasoning_effort' in agent and agent['reasoning_effort'] not in EFFORTS:
            raise ValueError('Unsupported reasoning effort')
    return deepcopy(value)


def validate(value):
    if isinstance(value, str):
        skill_name(value)
    elif isinstance(value, dict):
        if set(value) - FIELDS or not {'skill', 'instructions'} & value.keys():
            raise ValueError('Invocation requires a skill or instructions')
        if 'skill' in value:
            skill_name(value['skill'])
        validate_settings({key: item for key, item in value.items() if key != 'skill'})
        if 'skill' in value:
            from skill_config import validate as validate_options
            validate_options(value['skill'], value.get('options', {}))
    else:
        raise ValueError('Invocation must be a skill name or invocation object')
    return deepcopy(value)


def normalize(value):
    value = validate(value)
    return {'skill': value} if isinstance(value, str) else value


def merge(base, override):
    """Objects merge recursively; lists and scalars replace."""
    result = deepcopy(base)
    for key, value in override.items():
        result[key] = (merge(result[key], value) if isinstance(value, dict)
                       and isinstance(result.get(key), dict) else deepcopy(value))
    return result


def matches(reference, identity):
    return reference == identity


def configured(value, defaults, *, identity=None):
    call = normalize(value)
    reference = call.get('skill')
    choices = [settings for name, settings in defaults.items()
               if reference and (matches(name, identity) if identity else
                                 (matches(name, reference) or matches(reference, name)))]
    if len(choices) > 1:
        raise ValueError('Multiple shared settings select the same resolved skill')
    result = merge(choices[0] if choices else {}, call)
    if 'policy' in call.get('options', {}):
        result['options']['policy'] = deepcopy(call['options']['policy'])
    return result


def preflight(calls, available, capabilities=None, *, native_reviews=None):
    """Check current native controls and evidence transport before claiming work.

    Native tools resolve occurrence-specific review contracts from installed skills;
    this helper neither discovers installations nor probes a host by launching work.
    """
    from native_review import contract
    calls = list(calls)
    native_reviews = {} if native_reviews is None else native_reviews
    if (not isinstance(native_reviews, dict)
            or any(not text(location) for location in native_reviews)
            or set(native_reviews) - {location for location, _ in calls}):
        raise ValueError('Native review requirements must map selected occurrence locations to contracts')
    for requirements in native_reviews.values():
        contract(requirements)
    if not isinstance(available, list) or any(not isinstance(item, str) for item in available):
        raise ValueError('Available skill identities must be a list of strings')
    if capabilities is not None and (
            not isinstance(capabilities, dict) or set(capabilities) - {'delegation', 'models', 'reasoning_efforts', 'native_evidence'}
            or type(capabilities.get('delegation', False)) is not bool
            or not isinstance(capabilities.get('models', {}), dict)
            or any(not text(model) or not isinstance(efforts, list)
                   or any(effort not in EFFORTS for effort in efforts)
                   for model, efforts in capabilities.get('models', {}).items())
            or not isinstance(capabilities.get('reasoning_efforts', []), list)
            or any(effort not in EFFORTS for effort in capabilities.get('reasoning_efforts', []))):
        raise ValueError('Malformed native agent capabilities')
    support = capabilities or {}
    transport = support.get('native_evidence', {})
    if (not isinstance(transport, dict)
            or set(transport) - {'availability', 'inventory_complete', 'invocation_results',
                                  'child_results', 'tool_observations'}
            or transport.get('availability', 'unknown') not in ('available', 'unavailable', 'unknown')
            or any(type(value) is not bool for key, value in transport.items() if key != 'availability')):
        raise ValueError('Malformed native evidence capabilities')
    missing, ambiguous, unsupported, resolved, evidence = set(), {}, [], {}, []
    for location, call in calls:
        reference = call.get('skill')
        if reference:
            candidates = sorted({identity for identity in available if matches(reference, identity)})
            if not candidates:
                missing.add(reference)
            elif len(candidates) != 1:
                ambiguous[reference] = candidates
            else:
                resolved[reference] = candidates[0]
        reviewer = (re.search(r'/(?:options/reviewers/[0-9]+|reviewers/[0-9]+/[0-9]+)$', location)
                    is not None or location == 'reviewer'
                    or (reference or '').split(':')[-1].startswith('code-review'))
        if reviewer or location in native_reviews:
            requirements = native_reviews.get(location)
            gaps = []
            if requirements is None:
                gaps.append('resolved native review requirements')
            else:
                needed = ['inventory_complete', 'invocation_results']
                if requirements['passes']:
                    needed.append('child_results')
                if any(item['drivers'] for item in requirements['passes']):
                    needed.append('tool_observations')
                if not support.get('delegation'):
                    gaps.append('native delegation')
                if transport.get('availability') != 'available':
                    gaps.append('available native evidence transport')
                gaps.extend(name for name in needed if transport.get(name) is not True)
            if gaps:
                evidence.append({'location': location, 'skill': reference,
                    'missing': gaps,
                    'reason': 'Cannot establish review execution before launch: ' + ', '.join(gaps),
                    'supported_path': ('Resolve the installed skill and applicable pass/driver requirements, '
                        'then use a native host exposing the required identities, terminal results and '
                        'tool observations. If unavailable, keep review blocked; a reviewer report '
                        'or model subprocess cannot replace host evidence.')})
        if 'agent' in call:
            agent, support = call['agent'], capabilities or {}
            reason = None
            if not support.get('delegation'):
                reason = 'Native delegation support must be verified'
            elif 'model' in agent and agent['model'] not in support.get('models', {}):
                reason = 'Requested model is unavailable'
            elif 'reasoning_effort' in agent:
                efforts = (support.get('models', {}).get(agent['model'], []) if 'model' in agent
                           else support.get('reasoning_efforts', []))
                if agent['reasoning_effort'] not in efforts:
                    reason = 'Requested reasoning effort is unsupported'
            if reason:
                unsupported.append({'location': location, 'reason': reason, 'agent': deepcopy(agent)})
    return {'ready': not (missing or ambiguous or unsupported or evidence),
            'missing_selected_skills': sorted(missing), 'ambiguous_selected_skills': ambiguous,
            'unsupported_agents': unsupported, 'resolved_skills': resolved,
            'unsupported_native_reviews': evidence}
