"""Process configuration and shared native invocation definitions."""
from copy import deepcopy
from pathlib import Path
import os
import re

import invocation
from state import digest, make_spec, read
from shared.storage_paths import location

# Compatibility process views are derived from the owning skill defaults.
# New callers use skill_config.effective/select and ordered invocation plans.
import skill_config

CYCLE_PHASES = ('before_cycle', 'after_cycle')
_FIX = skill_config.defaults('dk-workflows:dk-pr-fix')
_REVIEW = skill_config.defaults('dk-workflows:dk-pr-review')
LIMITS = _FIX['limits']
PERMISSIONS = tuple(key for key in _FIX['permissions'] if key != 'merge_pr')
FINALIZER_DEFAULTS = _FIX['finalizers']
_LEGACY_TASK_NAMES = ('read_feedback', 'collect_decisions', 'address_feedback', 'split_scope',
                      'review_changes', 'organize_commits', 'draft_commit_message',
                      'update_description', 'reply_to_feedback', 'check_ci',
                      'classify_ci_failures', 'publish_pr_actions')
_LEGACY_SKILLS = ('dk-pr-feedback-read', 'dk-pr-decisions', 'dk-pr-feedback-address', 'dk-pr-split',
                  'dk-pr-reviewers-run', 'dk-commit-split', 'dk-commit-message', 'dk-pr-description',
                  'dk-pr-feedback-reply', 'dk-ci', 'dk-ci-flakes', 'dk-pr-publish')
FIX_TASKS = {task: {'skill': 'dk-workflows:' + name, 'options': {}, 'required': True}
             for task, name in zip(_LEGACY_TASK_NAMES, _LEGACY_SKILLS)}

_REVIEW_TASKS = dict(zip(('read_feedback', 'review_changes', 'verify_findings', 'post_review'), _REVIEW['tasks']))
_REVIEW_TASKS.update(dict(zip(('approve_fork_ci', 'check_ci', 'publish_pr_actions'), _REVIEW['finalizers'])))
_REVIEW_TASKS['classify_ci_failures'] = {'skill': 'dk-workflows:dk-ci-flakes', 'options': {}, 'required': True}
DEFAULTS = {
    'schema_version': 1, 'accounts': {'personal': []}, 'skill_defaults': {},
    'processes': {
        'pr-iter': {'coordinator': 'dk-workflows:dk-pr-iter', 'routes': {'own_pr': 'pr-fix', 'other_pr': 'pr-review'}},
        'pr-fix': {'coordinator': 'dk-workflows:dk-pr-fix', 'tasks': FIX_TASKS,
                   'limits': deepcopy(LIMITS), 'permissions': _FIX['permissions'],
                   'finalizers': FINALIZER_DEFAULTS, 'hooks': {phase: [] for phase in CYCLE_PHASES}},
        'pr-review': {'coordinator': 'dk-workflows:dk-pr-review', 'tasks': _REVIEW_TASKS,
                      'limits': {**LIMITS, **_REVIEW['limits']}, 'permissions': _REVIEW['permissions'],
                      'hooks': {phase: [] for phase in CYCLE_PHASES}},
        'commit-message': {'coordinator': 'dk-workflows:dk-commit-message'},
        'issue': {'coordinator': 'dk-workflows:dk-issue'},
        'pr-reviewers-run': {'coordinator': 'dk-workflows:dk-pr-reviewers-run'},
    }, 'extensions': {},
}


def finalizer_groups(finalizers):
    if not isinstance(finalizers, list):
        raise ValueError('Finalizers must be an ordered invocation list')
    groups = []
    for entry in finalizers:
        group = entry['parallel'] if isinstance(entry, dict) and set(entry) == {'parallel'} else entry if isinstance(entry, list) else [entry]
        if not group:
            raise ValueError('Parallel finalizer groups must not be empty')
        for value in group:
            call = invocation.normalize(value)
        groups.append(deepcopy(group))
    return groups


def validate(settings):
    if not isinstance(settings, dict) or set(settings) - (DEFAULTS.keys() | {'$schema'}):
        raise ValueError('Unknown workflow setting; use the process configuration format')
    result = deepcopy(settings)
    if '$schema' in result and not invocation.text(result['$schema']):
        raise ValueError('$schema must be a nonempty schema reference')
    if 'schema_version' in result and (type(result['schema_version']) is not int or result['schema_version'] != 1):
        raise ValueError('Unsupported configuration schema_version')
    accounts = result.get('accounts', {})
    if not isinstance(accounts, dict) or set(accounts) - {'personal'}:
        raise ValueError('Accounts accept a personal account list')
    personal = accounts.get('personal', [])
    if not isinstance(personal, list):
        raise ValueError('Personal accounts must be a list')
    seen = set()
    for account in personal:
        if (not isinstance(account, dict) or set(account) != {'host', 'id'}
                or not isinstance(account['host'], str)
                or not re.fullmatch(r'[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?', account['host'])
                or type(account['id']) is not int or account['id'] <= 0):
            raise ValueError('Personal accounts require host and positive numeric id')
        account['host'] = account['host'].lower()
        key = (account['host'], account['id'])
        if key in seen:
            raise ValueError('Personal accounts must be unique per host/account')
        seen.add(key)
    defaults = result.get('skill_defaults', {})
    if not isinstance(defaults, dict):
        raise ValueError('skill_defaults must be a skill-to-settings object')
    for skill, value in defaults.items():
        invocation.skill_name(skill)
        invocation.validate_settings(value)
        skill_config.validate(skill, value.get('options', {}))
    processes = result.get('processes', {})
    if not isinstance(processes, dict):
        raise ValueError('Processes must be an object')
    for name, process in processes.items():
        if not isinstance(name, str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]*', name):
            raise ValueError('Invalid process name')
        if not isinstance(process, dict) or set(process) - {'coordinator', 'tasks', 'finalizers', 'limits', 'permissions', 'hooks', 'routes'}:
            raise ValueError('Unknown process setting')
        if 'coordinator' in process:
            invocation.validate(process['coordinator'])
        if 'finalizers' in process:
            finalizer_groups(process['finalizers'])
        if not isinstance(process.get('tasks', {}), dict):
            raise ValueError('Process tasks must map purpose names to invocations')
        for task, call in process.get('tasks', {}).items():
            if not isinstance(task, str) or not re.fullmatch(r'[a-z][a-z0-9]*(?:_[a-z0-9]+)*', task):
                raise ValueError('Task names must describe their purpose in snake_case')
            invocation.validate(call)
        limits = process.get('limits', {})
        if not isinstance(limits, dict) or set(limits) - LIMITS.keys() or any(
                type(value) is not int or value < (1 if key == 'review_cycles' else 0)
                or value > LIMITS[key] for key, value in limits.items()):
            raise ValueError('Process limits must be integers within the retained runtime ceilings; review_cycles must be positive')
        permissions = process.get('permissions', {})
        if (not isinstance(permissions, dict) or set(permissions) - (set(PERMISSIONS) | {'merge_pr'})
                or any(type(value) is not bool for key, value in permissions.items() if key != 'merge_pr')):
            raise ValueError('Process permissions must be known boolean permissions')
        merging = permissions.get('merge_pr', {})
        if (not isinstance(merging, dict) or set(merging) - {'enabled', 'method'}
                or type(merging.get('enabled', False)) is not bool
                or merging.get('method', 'squash') not in {'squash', 'merge'}):
            raise ValueError('Merge policy accepts enabled and squash or merge method')
        hooks = process.get('hooks', {})
        if not isinstance(hooks, dict) or set(hooks) - set(CYCLE_PHASES):
            raise ValueError('Hooks accept before_cycle and after_cycle lists')
        for entries in hooks.values():
            if not isinstance(entries, list):
                raise ValueError('Hooks must be ordered invocation lists')
            for call in entries:
                invocation.validate(call)
        routes = process.get('routes', {})
        if not isinstance(routes, dict) or set(routes) - {'own_pr', 'other_pr'} or any(
                not isinstance(route, str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]*', route)
                for route in routes.values()):
            raise ValueError('Routes must name configured processes')
    if not isinstance(result.get('extensions', {}), dict):
        raise ValueError('Extensions must be an object')
    return result


def path(repository=None):
    return location('config')/((digest(repository.lower()) if repository else 'user') + '.json')


def _merge_settings(base, layer):
    result = invocation.merge(base, layer)
    for skill, settings in layer.get('skill_defaults', {}).items():
        if 'policy' in settings.get('options', {}):
            result['skill_defaults'][skill]['options']['policy'] = deepcopy(settings['options']['policy'])
    return result


def effective(repository, upstream=None, *, expanded=True):
    result, sources = deepcopy(DEFAULTS) if expanded else {}, []
    for repo in [None] + ([upstream] if upstream else []) + [repository]:
        source = path(repo)
        if source.exists():
            settings = validate(read(source))
            result = _merge_settings(result, {key: value for key, value in settings.items() if key != '$schema'})
            sources.append(str(source))
    if expanded:
        # Preserve the existing process API while new settings live with skills.
        for process in ('pr-fix', 'pr-review'):
            owned = skill_config.effective('dk-workflows:dk-' + process, repository, upstream, domain=False)['options']
            for key in ('permissions', 'limits', 'finalizers'):
                if key in owned and (key != 'finalizers' or process == 'pr-fix'):
                    result['processes'][process][key] = deepcopy(owned[key])
        result['accounts']['personal'] = skill_config.effective('dk-workflows:dk-pr-iter', repository, upstream, domain=False)['options']['personal_accounts']
        for name, process in result['processes'].items():
            if 'coordinator' not in process:
                raise ValueError('Custom process requires a coordinator invocation: ' + name)
            for route in process.get('routes', {}).values():
                if route not in result['processes']:
                    raise ValueError('Route selects an unknown process: ' + route)
    return {'settings': result, 'sources': sources}


def calls(settings, process=None, available=None):
    def visit(value, location, ancestors=()):
        raw = invocation.normalize(value)
        reference = raw.get('skill')
        candidates = [name for name in (available or []) if reference and invocation.matches(reference, name)]
        identity = candidates[0] if len(candidates) == 1 else None
        call = invocation.configured(raw, settings['skill_defaults'], identity=identity)
        yield location, call
        reviewers = call.get('options', {}).get('reviewers', [])
        signature = digest(call)
        if reviewers and signature in ancestors:
            raise ValueError('Recursive reviewer selection: ' + str(reference))
        for index, reviewer in enumerate(reviewers):
            yield from visit(reviewer, location + '/options/reviewers/' + str(index), (*ancestors, signature))
    for name, entry in settings['processes'].items():
        if process is not None and name != process:
            continue
        prefix = '/processes/' + name
        yield from visit(entry['coordinator'], prefix + '/coordinator')
        for task, call in entry.get('tasks', {}).items():
            yield from visit(call, prefix + '/tasks/' + task)
        for group_index, group in enumerate(finalizer_groups(entry.get('finalizers', []))):
            for index, call in enumerate(group):
                yield from visit(call, prefix + '/finalizers/' + str(group_index) + '/' + str(index))
        for phase, hooks in entry.get('hooks', {}).items():
            for index, call in enumerate(hooks):
                yield from visit(call, prefix + '/hooks/' + phase + '/' + str(index))


def preflight(settings, available, capabilities=None, *, process=None, native_reviews=None):
    if process is not None and process not in settings['processes']:
        raise ValueError('Unknown process')
    if not isinstance(available, list) or any(not isinstance(item, str) for item in available):
        raise ValueError('Available skill identities must be a list of strings')
    return invocation.preflight(list(calls(settings, process, available)), available, capabilities,
                                native_reviews=native_reviews)


def reviewers(settings, process):
    entry = settings['processes'][process]
    coordinator = entry.get('tasks', {}).get('review_changes', entry['coordinator'])
    call = invocation.configured(coordinator, settings['skill_defaults'])
    return call.get('options', {}).get('reviewers', skill_config.defaults('dk-workflows:dk-pr-reviewers-run')['reviewers'])


def select(settings, process, kind='coordinator', index=None, phase=None, task=None):
    if process not in settings['processes']:
        raise ValueError('Unknown process')
    entry = settings['processes'][process]
    if kind == 'coordinator':
        if task is not None or index is not None or phase is not None:
            raise ValueError('Coordinator selection has no task, index or phase')
        value = entry['coordinator']
    elif kind == 'task':
        if not isinstance(task, str) or task not in entry.get('tasks', {}):
            raise ValueError('No configured task with this purpose name')
        if index is not None or phase is not None:
            raise ValueError('Named task selection has no index or phase')
        value = entry['tasks'][task]
    else:
        if task is not None:
            raise ValueError('Task name is only valid for named task selection')
        if type(index) is not int or index < 0:
            raise ValueError('Invocation selection requires a nonnegative index')
        if kind == 'finalizer':
            values = [value for group in finalizer_groups(entry.get('finalizers', [])) for value in group]
        elif kind == 'hook' and phase in CYCLE_PHASES:
            values = entry.get('hooks', {}).get(phase, [])
        elif kind == 'reviewer':
            values = reviewers(settings, process)
        else:
            raise ValueError('Unknown invocation selection')
        if index >= len(values):
            raise ValueError('No configured invocation at this index')
        value = values[index]
    return invocation.configured(value, settings['skill_defaults'])


def make_invocation_spec(*, repository, pr, process, definition, kind='coordinator', index=None,
                         phase=None, task=None, upstream=None):
    """Bind the common invocation to explicit native resources without executing it."""
    loaded = effective(repository, upstream)
    call = select(loaded['settings'], process, kind, index, phase, task)
    if not isinstance(definition, dict):
        raise ValueError('Invocation definition must be an object')
    runner = definition.get('skill_identity')
    if call.get('skill') and (not isinstance(runner, str) or not invocation.matches(call['skill'], runner)):
        raise ValueError('Resolve and supply the configured invocation skill identity')
    if not call.get('skill'):
        owner = invocation.normalize(loaded['settings']['processes'][process]['coordinator']).get('skill')
        if not isinstance(runner, str) or (owner and not invocation.matches(owner, runner)):
            raise ValueError('Instruction-only invocations use the owning process identity')
    call = invocation.configured(call, loaded['settings']['skill_defaults'], identity=runner)
    execution = deepcopy(definition)
    if kind == 'task':
        execution.setdefault('responsibility', 'finding-verification' if task == 'verify_findings' else task)
    instructions = execution.get('instructions', {})
    if not isinstance(instructions, dict):
        raise ValueError('Instructions must be a label-to-resource-path map')
    instructions = dict(instructions)
    resources = execution.get('resources', {})
    if not isinstance(resources, dict):
        raise ValueError('Resources must be a label-to-resource-path map')
    resources = dict(resources)
    for label, resource in {
            'invocation_contract': Path(__file__).with_name('invocation.py'),
            'invocation_schema': Path(__file__).parents[1]/'schemas/invocation.schema.json',
            'configuration_contract': Path(__file__),
            'configuration_schema': Path(__file__).parents[1]/'schemas/config.schema.json'}.items():
        if label in resources:
            raise ValueError('Reserved invocation resource label')
        resources[label] = str(resource)
    if not isinstance(execution.get('options', {}), dict):
        raise ValueError('Invocation execution options must be an object')
    execution.update(instructions=instructions, resources=resources,
        options={'invocation': call, 'configured': call.get('options', {}),
                 'process_limits': loaded['settings']['processes'][process].get('limits', dict(LIMITS)),
                 'execution': execution.get('options', {})})
    if 'instructions' in call:
        execution['assigned_instructions'] = invocation.additional_instructions(
            execution.get('assigned_instructions', ''), call['instructions'])
    if kind == 'task':
        execution['options']['configured_task'] = task
    spec = make_spec(repository=repository, pr=pr, **execution)
    skill_config.bind_options(spec, call, repository, upstream)
    spec['configuration'] = {
        'sources': [str(path(repo)) for repo in [None] + ([upstream] if upstream else []) + [repository]],
        'process': process, 'kind': kind, 'task': task, 'index': index, 'phase': phase,
        'upstream': upstream}
    if kind != 'reviewer' and not (kind == 'task' and task == 'verify_findings'):
        spec['options']['process_permissions'] = loaded['settings']['processes'][process].get('permissions', {})
    return spec


def check_selection(spec):
    """Check only explicit live configuration inputs, preserving unrelated reuse."""
    selection = spec.get('configuration')
    if selection is None:
        return
    if (not isinstance(selection, dict)
            or set(selection) not in ({'sources', 'process', 'kind', 'task', 'index', 'phase'},
                                       {'sources', 'process', 'kind', 'task', 'index', 'phase', 'upstream'})
            or not isinstance(selection['sources'], list)
            or any(not isinstance(source, str) or not Path(source).is_absolute() for source in selection['sources'])):
        raise ValueError('Malformed explicit configuration selection')
    settings = deepcopy(DEFAULTS)
    for source in selection['sources']:
        if Path(source).exists():
            value = validate(read(source))
            settings = _merge_settings(settings, {key: item for key, item in value.items() if key != '$schema'})
    if 'upstream' in selection:
        settings = effective(spec['repository'], selection['upstream'])['settings']
    try:
        call = select(settings, selection['process'], selection['kind'], selection['index'], selection['phase'], selection['task'])
        call = invocation.configured(call, settings['skill_defaults'], identity=spec['skill']['identity'])
        process = settings['processes'][selection['process']]
    except (ValueError, KeyError) as exc:
        raise ValueError('Inputs changed: configured invocation no longer resolves') from exc
    options = spec['options']
    changed = (call != options.get('invocation')
               or process.get('limits', dict(LIMITS)) != options.get('process_limits'))
    if 'process_permissions' in options:
        changed = changed or process.get('permissions', {}) != options['process_permissions']
    if 'hook_phase' in options:
        occurrence = options['hook_phase']['occurrence']
        changed = changed or hook_phase(settings, selection['process'], occurrence['run'],
                                       occurrence['cycle'], selection['phase']) != options['hook_phase']
    if 'finalization' in options:
        changed = changed or process.get('finalizers', []) != options['finalization']['plan']
    if changed:
        raise ValueError('Inputs changed: configured invocation, policy or hook list changed')


def make_completion_spec(*, repository, pr, run, definition, upstream=None, options=None):
    """Bind the coordinator's completion check to its live skill-owned plan."""
    if not invocation.text(run):
        raise ValueError('Completion requires the retained run id')
    owner = 'dk-workflows:dk-pr-fix'
    if definition.get('skill_identity') != owner:
        raise ValueError('Workflow completion requires the fix coordinator identity')
    loaded = skill_config.effective(owner, repository, upstream, options=options)
    execution = deepcopy(definition)
    execution.update(cacheable=False, responsibility='workflow-completion:' + digest({'run': run}),
                     options={'completion_plan': loaded['options']['finalizers'],
                              'execution': definition.get('options', {})})
    spec = make_spec(repository=repository, pr=pr, **execution)
    skill_config.bind_options(spec, {'skill': owner, 'options': options or {}}, repository, upstream)
    return spec


def make_finalizer_spec(*, repository, pr, run, definition, finalizer=None, index=None,
                        process='pr-fix', upstream=None):
    if not invocation.text(run):
        raise ValueError('Finalizer specs require the retained run id')
    if not isinstance(definition, dict):
        raise ValueError('Invocation definition must be an object')
    loaded = effective(repository, upstream)
    plan = loaded['settings']['processes'][process].get('finalizers', [])
    flat = [value for group in finalizer_groups(plan) for value in group]
    if finalizer is not None:
        matches = [position for position, value in enumerate(flat)
                   if invocation.normalize(value).get('skill') == finalizer]
        if len(matches) != 1:
            raise ValueError('No configured finalizer with this reference')
        index = matches[0]
    call = select(loaded['settings'], process, 'finalizer', index)
    runner = definition.get('skill_identity') if isinstance(definition, dict) else None
    occurrence = {'run': run, 'skill': runner or call.get('skill') or digest(call)}
    references = [invocation.normalize(item).get('skill') for item in flat]
    if call.get('skill') and references.count(call['skill']) > 1:
        occurrence['ordinal'] = references[:index].count(call['skill'])
    if not call.get('skill'):
        occurrence['index'] = index
    spec = make_invocation_spec(repository=repository, pr=pr, process=process, kind='finalizer',
        index=index, upstream=upstream, definition={**definition, 'responsibility': 'finalizer:' + digest(occurrence)})
    spec['options']['finalization'] = {**occurrence, 'plan': deepcopy(plan)}
    return spec


def hook_phase(settings, process, run, cycle, phase):
    if phase not in CYCLE_PHASES or not invocation.text(run) or not invocation.text(cycle):
        raise ValueError('Hook phase requires retained run and cycle ids')
    if process not in settings['processes']:
        raise ValueError('Unknown process')
    entries = settings['processes'][process].get('hooks', {}).get(phase, [])
    return {'occurrence': {'process': process, 'run': run, 'cycle': cycle, 'phase': phase},
            'hooks': [select(settings, process, 'hook', position, phase) for position in range(len(entries))]}


def make_hook_spec(*, repository, pr, run, cycle, phase, index, definition,
                   process='pr-fix', upstream=None):
    if phase not in CYCLE_PHASES or not invocation.text(run) or not invocation.text(cycle):
        raise ValueError('Hook specs require a phase and retained run and cycle ids')
    if not isinstance(definition, dict) or set(definition) - {
            'skill_identity', 'resources', 'instructions', 'inputs', 'options', 'git',
            'governing_inputs_complete', 'cacheable', 'native_review'}:
        raise ValueError('Hook definitions supply execution resources and inputs')
    loaded = effective(repository, upstream)
    call = select(loaded['settings'], process, 'hook', index, phase)
    occurrence = {'process': process, 'run': run, 'cycle': cycle, 'phase': phase, 'index': index}
    spec = make_invocation_spec(
        repository=repository, pr=pr, process=process, kind='hook', index=index, phase=phase,
        upstream=upstream, definition={**definition, 'responsibility': 'cycle-hook:' + digest(occurrence),
                                     'assigned_instructions': call.get('instructions', 'Execute the configured hook skill and return original evidence and explicit completion.')})
    spec['options']['cycle_hook'] = occurrence
    spec['options']['hook_phase'] = hook_phase(loaded['settings'], process, run, cycle, phase)
    return spec
