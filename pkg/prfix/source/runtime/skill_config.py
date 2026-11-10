"""Skill-owned options and native assignment definitions; never an agent runner.

Schemas/defaults live with each maintained skill. Scoped overrides retain the
existing private workflow files and domain stores. Runtime validation implements
only the deliberately small schema vocabulary used by these option schemas.
"""
from copy import deepcopy
import json
from pathlib import Path
import os
import re

ROOT = Path(__file__).resolve().parents[1]
DIRECTORY = ROOT/'codex/skills'


def directory(skill):
    import invocation
    invocation.skill_name(skill)
    plugin, name = skill.split(':')
    target = DIRECTORY/name
    return target if plugin == 'dk-workflows' and (target/'SKILL.md').is_file() else None


def catalog():
    return ['dk-workflows:' + path.parent.name for path in sorted(DIRECTORY.glob('*/config.json'))]


def _read(path):
    try:
        return json.loads(Path(path).read_text())
    except (OSError, ValueError) as exc:
        raise ValueError('Cannot read skill configuration: ' + str(path)) from exc


def _validate(value, schema, location='options'):
    """Validate our closed option-schema vocabulary without runtime dependencies."""
    import invocation
    supported = {'$schema', '$id', 'title', 'description', 'default', '$ref', 'anyOf',
                 'type', 'enum', 'minimum', 'maximum', 'minLength', 'pattern', 'items',
                 'minItems', 'uniqueItems', 'properties', 'additionalProperties', 'required'}
    if set(schema) - supported:
        raise ValueError(location + ': unsupported option schema keywords: ' + ', '.join(sorted(set(schema) - supported)))
    if '$ref' in schema:
        reference = schema['$ref']
        if reference.endswith('invocation.schema.json#/$defs/plan'):
            groups(value)
        elif reference.endswith('invocation.schema.json'):
            invocation.validate(value)
        elif reference.endswith('commit-policy.schema.json'):
            if isinstance(value, dict) and set(value) == {'prompt'}:
                if not invocation.text(value['prompt']):
                    raise ValueError(location + ': policy prompt must be nonempty')
            else:
                invocation.validate(value)
        else:
            raise ValueError('Unsupported skill schema reference: ' + reference)
        return
    if 'anyOf' in schema:
        for candidate in schema['anyOf']:
            try:
                _validate(value, candidate, location)
                return
            except ValueError:
                pass
        raise ValueError(location + ': no supported alternative matches')
    types = schema.get('type', [])
    types = [types] if isinstance(types, str) else types
    actual = ('null' if value is None else 'boolean' if type(value) is bool else
              'integer' if type(value) is int else 'string' if isinstance(value, str) else
              'object' if isinstance(value, dict) else 'array' if isinstance(value, list) else 'unsupported')
    if types and actual not in types:
        raise ValueError(location + ': expected ' + ' or '.join(types))
    if 'enum' in schema and value not in schema['enum']:
        raise ValueError(location + ': unsupported value')
    if actual == 'integer':
        if value < schema.get('minimum', value) or value > schema.get('maximum', value):
            raise ValueError(location + ': outside supported limits')
    elif actual == 'string':
        if (len(value) < schema.get('minLength', 0) or '\x00' in value
                or ('pattern' in schema and re.fullmatch(schema['pattern'], value) is None)):
            raise ValueError(location + ': invalid text')
    elif actual == 'array':
        if len(value) < schema.get('minItems', 0):
            raise ValueError(location + ': too few entries')
        if schema.get('uniqueItems') and len({json.dumps(x, sort_keys=True) for x in value}) != len(value):
            raise ValueError(location + ': duplicate entries')
        for index, item in enumerate(value):
            _validate(item, schema.get('items', {}), location + '/' + str(index))
    elif actual == 'object':
        properties = schema.get('properties', {})
        if schema.get('additionalProperties') is False and set(value) - properties.keys():
            raise ValueError(location + ': unknown settings: ' + ', '.join(sorted(set(value) - properties.keys())))
        if set(schema.get('required', [])) - value.keys():
            raise ValueError(location + ': missing required fields')
        for key, item in value.items():
            _validate(item, properties.get(key, {}), location + '/' + key)


def validate(skill, options):
    target = directory(skill)
    if not isinstance(options, dict):
        raise ValueError('Skill options must be an object')
    if target:
        _validate(options, _read(target/'config.schema.json'), skill)
    return deepcopy({key: value for key, value in options.items() if key != '$schema'})


def defaults(skill):
    target = directory(skill)
    return validate(skill, _read(target/'config.json')) if target else {}


def scope_path(repository=None):
    from shared.storage_paths import location
    from state import digest
    name = digest(repository.lower()) if repository else 'user'
    return location('config')/(name + '.json')


def _scope_record(path):
    # Apply the same migration and permission validation at every entry point.
    import config
    return config.validate(_read(path))


def repository_key(cwd=None, pr=None):
    from urllib.parse import urlsplit
    if pr and '://' in pr:
        parsed = urlsplit(pr)
        parts = parsed.path.strip('/').split('/')
        if parsed.scheme == 'https' and parsed.hostname and len(parts) == 4 and parts[2] == 'pull':
            return parsed.hostname.lower() + '/' + '/'.join(parts[:2]).lower()
        raise ValueError('Canonical PR URL required for skill configuration')
    from shared.repository_scope import repository
    return repository(cwd or Path.cwd()).removeprefix('remote:')


def _domain_layer(skill, repository):
    """Read existing private domain records at their original scope and path."""
    identity = (repository if repository.startswith(('remote:', 'path:')) else 'remote:' + repository) if repository else 'user'
    if skill == 'dk-workflows:dk-pr-split':
        from shared import pr_size
        path = pr_size.path(identity)
        if path.exists():
            record = _read(path)
            if (record.get('schema_version') != 1 or record.get('identity') != identity
                    or set(record) != {'schema_version', 'identity', 'pr_size'}):
                raise ValueError('Malformed saved PR size policy: ' + str(path))
            return pr_size.validate(record['pr_size']), path
        return {}, path
    if skill == 'dk-workflows:dk-commit-split':
        import commit_policy
        path = commit_policy.path(identity)
        return ({'policy': commit_policy.validate(_read(path))} if path.exists() else {}), path
    if skill == 'dk-workflows:dk-pr-fix':
        from settings import domain_path
        path = domain_path('completion', identity)
        if path.exists():
            prompt = path.read_text()
            import invocation
            if not invocation.text(prompt):
                raise ValueError('Malformed saved completion instructions: ' + str(path))
            return {'completion_instructions': prompt}, path
        return {}, path
    return {}, None


def effective(skill, repository=None, upstream=None, *, options=None, domain=True):
    import invocation
    options = {} if options is None else validate(skill, options)
    def merge_options(base, layer):
        merged = invocation.merge(base, layer)
        if 'policy' in layer:
            merged['policy'] = deepcopy(layer['policy'])
        return merged
    result = defaults(skill)
    target = directory(skill)
    sources = [str(target/'config.json'), str(target/'config.schema.json')] if target else []
    checked = list(sources)
    provenance = {key: sources[0] if sources else 'default' for key in result}
    process = skill.removeprefix('dk-workflows:dk-')
    routes = {}
    if skill == 'dk-workflows:dk-pr-iter':
        routes = {name.removeprefix('dk-workflows:dk-'): {'skill': name, 'options': {}, 'required': True}
                  for name in catalog()}
        for repo in [None] + ([upstream] if upstream else []) + ([repository] if repository else []):
            source = scope_path(repo)
            if source.exists():
                raw = _scope_record(source)
                if not isinstance(raw, dict) or not isinstance(raw.get('processes', {}), dict):
                    raise ValueError('Workflow process settings must be an object')
                for name, entry in raw.get('processes', {}).items():
                    if not isinstance(entry, dict):
                        raise ValueError('Workflow process settings must be an object')
                    if 'coordinator' in entry:
                        routes[name] = entry['coordinator']
    for repo in [None] + ([upstream] if upstream else []) + ([repository] if repository else []):
        if domain:
            layer, path = _domain_layer(skill, repo)
            if path:
                checked.append(str(path))
                if path.exists():
                    sources.append(str(path))
                    result = merge_options(result, validate(skill, layer))
                    provenance.update({key: str(path) for key in layer})
        source = scope_path(repo)
        checked.append(str(source))
        if not source.exists():
            continue
        raw = _scope_record(source)
        if not isinstance(raw, dict):
            raise ValueError('Workflow settings must be an object')
        if any(not isinstance(raw.get(key, {}), dict) for key in ('processes', 'skill_defaults', 'accounts')):
            raise ValueError('Workflow settings sections must be objects')
        sources.append(str(source))
        # Compatibility reads do not rewrite or relocate saved process settings.
        legacy = raw.get('processes', {}).get(process, {})
        layer = {key: legacy[key] for key in ('limits', 'permissions', 'finalizers') if key in legacy and key in result}
        if skill == 'dk-workflows:dk-pr-iter' and 'accounts' in raw:
            layer['personal_accounts'] = raw['accounts'].get('personal', [])
        if skill == 'dk-workflows:dk-pr-iter' and 'routes' in legacy:
            if not isinstance(legacy['routes'], dict) or set(legacy['routes']) - {'own_pr', 'other_pr'}:
                raise ValueError('Routing accepts own_pr and other_pr')
            if any(target not in routes for target in legacy['routes'].values()):
                raise ValueError('Legacy route selects an unknown process')
            layer['routes'] = {name: routes[target] for name, target in legacy['routes'].items()}
        result = merge_options(result, validate(skill, layer))
        provenance.update({key: str(source) for key in layer})
        shared = raw.get('skill_defaults', {}).get(skill, {})
        invocation.validate_settings(shared)
        result = merge_options(result, validate(skill, shared.get('options', {})))
        provenance.update({key: str(source) for key in shared.get('options', {})})
    result = merge_options(result, options)
    provenance.update({key: 'invocation' for key in options})
    validate(skill, result)
    if skill == 'dk-workflows:dk-pr-iter':
        seen = set()
        for account in result['personal_accounts']:
            account['host'] = account['host'].lower()
            key = account['host'], account['id']
            if key in seen:
                raise ValueError('Personal accounts must be unique per host/account')
            seen.add(key)
    return {'skill': skill, 'options': result, 'sources': list(dict.fromkeys(sources)),
            'checked_paths': list(dict.fromkeys(checked)), 'provenance': provenance}


def for_context(skill, *, cwd=None, pr=None, repository=None, upstream=None, options=None):
    key = repository or repository_key(cwd, pr)
    if key.startswith('remote:'):
        key = key.removeprefix('remote:')
    if len(key.split('/')) == 2:
        key = os.environ.get('GH_HOST', 'github.com') + '/' + key
    return effective(skill, key, upstream, options=options)


def bind_options(spec, call, repository, upstream=None, *, raw_call=None):
    """Bind effective leaf options to legacy/provider specs without executing."""
    if 'skill' not in call:
        return
    binding = {'skill': call['skill'], 'repository': repository, 'upstream': upstream,
               'options': call.get('options', {})}
    spec['options']['configured'] = effective(**binding)['options']
    if raw_call is not None:
        binding['call'] = deepcopy(raw_call)
    spec['skill_options_configuration'] = binding
    target = directory(call['skill'])
    if target:
        resources = spec['skill']['resources']
        for label, filename in [('selected_config_defaults', 'config.json'), ('selected_config_schema', 'config.schema.json')]:
            if label in resources:
                raise ValueError('Reserved skill configuration resource label')
            resources[label] = str(target/filename)


def check_options(spec):
    binding = spec.get('skill_options_configuration')
    if binding is None:
        return
    if (not isinstance(binding, dict) or set(binding) not in (
            {'skill', 'repository', 'upstream', 'options'}, {'skill', 'repository', 'upstream', 'options', 'call'})
            or not isinstance(binding['repository'], str)
            or binding['repository'].lower() != spec['repository'].lower()
            or binding['upstream'] is not None and not isinstance(binding['upstream'], str)):
        raise ValueError('Malformed skill option configuration binding')
    if 'call' in binding and configured_call(binding['call'], binding['repository'], binding['upstream']) != spec['options'].get('invocation'):
        raise ValueError('Inputs changed: selected skill instructions or native controls changed')
    parameters = {key: value for key, value in binding.items() if key != 'call'}
    if effective(**parameters)['options'] != spec['options'].get('configured'):
        raise ValueError('Inputs changed: selected skill options changed')


def groups(plan):
    """Ordered leaves with explicit parallel groups; no implicit task registry."""
    import invocation
    if not isinstance(plan, list):
        raise ValueError('Plan must be an ordered invocation list')
    result = []
    for value in plan:
        parallel = isinstance(value, dict) and set(value) == {'parallel'}
        group = value['parallel'] if parallel else value if isinstance(value, list) else [value]
        if not isinstance(group, list) or not group:
            raise ValueError('Parallel groups must be nonempty invocation lists')
        for item in group:
            invocation.validate(item)
        result.append(deepcopy(group))
    return result


def configured_call(value, repository=None, upstream=None):
    import invocation
    call = invocation.normalize(value)
    skill = call.get('skill')
    if skill is None:
        return call
    shared = {}
    for repo in [None] + ([upstream] if upstream else []) + ([repository] if repository else []):
        source = scope_path(repo)
        if source.exists():
            saved = _scope_record(source)
            if not isinstance(saved, dict) or not isinstance(saved.get('skill_defaults', {}), dict):
                raise ValueError('Workflow skill defaults must be an object')
            settings = saved.get('skill_defaults', {}).get(skill, {})
            invocation.validate_settings(settings)
            shared = invocation.configured({'skill': skill, **settings}, {skill: shared})
            shared.pop('skill', None)
    return invocation.validate(invocation.configured(call, {skill: shared} if skill else {}))


def select(skill, kind, index, repository=None, upstream=None, *, options=None):
    if kind not in {'tasks', 'finalizers', 'reviewers'}:
        raise ValueError('Selection must be tasks, finalizers or reviewers')
    loaded = effective(skill, repository, upstream, options=options)
    values = [item for group in groups(loaded['options'].get(kind, [])) for item in group]
    if type(index) is not int or not 0 <= index < len(values):
        raise ValueError('No configured invocation at this index')
    return configured_call(values[index], repository, upstream), loaded


def plan(skill, kind, repository=None, upstream=None, *, options=None, resolved=False):
    """Read a live plan, optionally resolving each occurrence without saving it."""
    import invocation
    if kind not in {'tasks', 'finalizers', 'reviewers'}:
        raise ValueError('Selection must be tasks, finalizers or reviewers')
    loaded = effective(skill, repository, upstream, options=options)
    loaded['groups'] = groups(loaded['options'].get(kind, []))
    if resolved:
        index = 0
        loaded['resolved_groups'] = []
        for group in loaded['groups']:
            occurrences = []
            for value in group:
                call = configured_call(value, repository, upstream)
                # Reapply only occurrence-local options: configured_call already
                # includes scoped values, which must retain their actual sources.
                configuration = (effective(call['skill'], repository, upstream,
                                           options=invocation.normalize(value).get('options', {}))
                                 if 'skill' in call else None)
                occurrences.append({'index': index, 'invocation': call, 'configuration': configuration})
                index += 1
            loaded['resolved_groups'].append(occurrences)
    return loaded


def calls(skill, repository=None, upstream=None, *, options=None):
    """Enumerate selected native calls for availability checks, without launching."""
    import invocation
    def visit(call, location, ancestors=()):
        call = configured_call(call, repository, upstream)
        identity = call.get('skill')
        settings = effective(identity, repository, upstream, options=call.get('options', {}))['options'] if identity else call.get('options', {})
        signature = json.dumps([identity, settings], sort_keys=True)
        if signature in ancestors:
            raise ValueError('Recursive skill configuration at ' + location)
        yield location, call
        nested = (*ancestors, signature)
        keys = ('tasks', 'finalizers', 'reviewers') if identity and directory(identity) else ('reviewers',)
        for key in keys:
            for group_index, group in enumerate(groups(settings.get(key, []))):
                for index, child in enumerate(group):
                    yield from visit(child, location + '/' + key + '/' + str(group_index) + '/' + str(index), nested)
        if identity == 'dk-workflows:dk-pr-iter':
            for route, child in settings['routes'].items():
                yield from visit(child, location + '/routes/' + route, nested)
        if identity == 'dk-workflows:dk-commit-split' and settings['policy'] is not None:
            policy = settings['policy']
            if not (isinstance(policy, dict) and set(policy) == {'prompt'}):
                yield from visit(policy, location + '/policy', nested)
    yield from visit({'skill': skill, 'options': {} if options is None else options, 'required': True}, skill)


def make_spec(*, repository, pr, skill, kind, index, definition, upstream=None, run=None, options=None):
    import invocation
    from state import make_spec as task_spec, digest
    selected, owner = select(skill, kind, index, repository, upstream, options=options)
    call = invocation.normalize(selected)
    runner = call.get('skill', skill)
    if not isinstance(definition, dict) or definition.get('skill_identity') != runner:
        raise ValueError('Definition must resolve the exact configured skill identity')
    execution = deepcopy(definition)
    configured = effective(runner, repository, upstream, options=call.get('options', {})) if 'skill' in call else {'options': call.get('options', {})}
    assigned = execution.get('assigned_instructions', '')
    if 'instructions' in call:
        execution['assigned_instructions'] = invocation.additional_instructions(assigned, call['instructions'])
    if kind == 'reviewers':
        # Stable across workflow/run/order changes, while repeated identical
        # reviewers still own independent claims and evidence.
        earlier = [item for group in groups(owner['options'][kind]) for item in group][:index]
        repeat = sum(configured_call(item, repository, upstream) == call for item in earlier)
        occurrence = {'call': call, 'repeat': repeat}
        responsibility = execution.get('responsibility', 'content-review')
    else:
        occurrence = {'owner': skill, 'kind': kind, 'index': index}
        if kind == 'finalizers':
            if not isinstance(run, str) or not run.strip():
                raise ValueError('Finalizers require the retained run id')
            occurrence['run'] = run
        responsibility = execution.get('responsibility', 'skill-task')
    execution['responsibility'] = responsibility + ':' + digest(occurrence)
    resources = execution.setdefault('resources', {})
    for label, path in {'skill_config_runtime': Path(__file__),
                        'invocation_contract': Path(__file__).with_name('invocation.py'),
                        'invocation_schema': ROOT/'schemas/invocation.schema.json'}.items():
        if label in resources:
            raise ValueError('Reserved skill configuration resource label')
        resources[label] = str(path)
    for prefix, identity in [('owner', skill), ('selected', runner)]:
        target = directory(identity)
        if target:
            if any(prefix + suffix in resources for suffix in ('_config_defaults', '_config_schema', '_skill_contract')):
                raise ValueError('Reserved skill configuration resource label')
            resources[prefix + '_config_defaults'] = str(target/'config.json')
            resources[prefix + '_config_schema'] = str(target/'config.schema.json')
            resources[prefix + '_skill_contract'] = str(target/'SKILL.md')
    execution['options'] = {'invocation': call, 'configured': configured['options'],
                            'execution': execution.get('options', {}),
                            'process_limits': owner['options'].get('limits', {}),
                            'process_permissions': owner['options'].get('permissions', {})}
    spec = task_spec(repository=repository, pr=pr, **execution)
    spec['skill_configuration'] = {'skill': skill, 'kind': kind, 'index': index,
                                   'repository': repository, 'upstream': upstream,
                                   'options': options or {}, 'owner_limits': owner['options'].get('limits', {}),
                                   'owner_permissions': owner['options'].get('permissions', {})}
    return spec


def check_selection(spec):
    import invocation
    selection = spec.get('skill_configuration')
    if selection is None:
        return
    expected = {'skill', 'kind', 'index', 'repository', 'upstream', 'options', 'owner_limits', 'owner_permissions'}
    if not isinstance(selection, dict) or set(selection) != expected:
        raise ValueError('Malformed skill configuration selection')
    if (not isinstance(selection['repository'], str)
            or selection['repository'].lower() != spec['repository'].lower()
            or selection['upstream'] is not None and not isinstance(selection['upstream'], str)):
        raise ValueError('Skill configuration must retain its original repository scope')
    value, owner = select(selection['skill'], selection['kind'], selection['index'], selection['repository'],
                          selection['upstream'], options=selection['options'])
    call = invocation.normalize(value)
    actual = effective(call['skill'], selection['repository'], selection['upstream'], options=call.get('options', {}))['options'] if 'skill' in call else call.get('options', {})
    if (call != spec['options'].get('invocation') or actual != spec['options'].get('configured')
            or owner['options'].get('limits', {}) != selection['owner_limits']
            or owner['options'].get('permissions', {}) != selection['owner_permissions']):
        raise ValueError('Inputs changed: skill configuration, assignment or policy changed')
