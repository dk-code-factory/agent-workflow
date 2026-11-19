"""Test-only migration adapter for existing ledger/evidence regression fixtures."""
from pathlib import Path
from prfix.prepare import prepare as prepare_data, _definition
from prfix.core import digest


DEFAULT_ADAPTERS = {'dk-workflows:dk-pr-feedback-read': {'kind': 'inspection',
                                      'writes': False,
                                      'assignment': 'Capture all actionable inline threads, review '
                                                    'bodies and general comments; preserve source '
                                                    'identities and obligations.',
                                      'definition': 'leaf'},
 'dk-workflows:dk-pr-decisions': {'kind': 'decisions',
                                  'writes': False,
                                  'assignment': 'Reconcile settled decisions from original '
                                                'sources, exact request and repository guidance; '
                                                'do not invent user authority.',
                                  'definition': 'leaf'},
 'dk-workflows:dk-pr-feedback-address': {'kind': 'fix',
                                         'writes': True,
                                         'assignment': 'Verify actionable feedback and implement '
                                                       'focused changes. Account for original '
                                                       'findings and every changed file; preserve '
                                                       'unrelated work.',
                                         'definition': 'leaf'},
 'dk-workflows:dk-pr-split': {'kind': 'scope',
                              'writes': False,
                              'assignment': 'Assess the complete diff against the selected '
                                            'size/scope policies; report concrete boundaries and '
                                            'ownership evidence. Splitting publication requires '
                                            'separately supplied explicit authority.',
                              'definition': 'leaf'},
 'dk-workflows:dk-pr-description': {'kind': 'description',
                                    'writes': False,
                                    'assignment': 'Prepare a repository-native PR title and '
                                                  'description from the actual final diff and '
                                                  'accepted issue; propose exact authorized '
                                                  'publication operations only.',
                                    'definition': 'leaf'},
 'dk-workflows:dk-commit-split': {'kind': 'cleanup',
                                  'writes': False,
                                  'assignment': 'Inspect the entire commit series against selected '
                                                'commit policy and final tree. Report blockers and '
                                                'exact justified history proposals; do not rewrite '
                                                'Git directly.',
                                  'definition': 'leaf'},
 'dk-workflows:dk-preferences': {'kind': 'synchronization',
                                 'writes': True,
                                 'assignment': 'Synchronize governing repository content only '
                                               'where the exact original request and selected '
                                               'policy require changes; account for every changed '
                                               'path.',
                                 'definition': 'leaf'}}


def prepare(root, request, provider=None):
    controls = {**request.get('options', {}), **{key: request.get(key, {} if key != 'resources' else [])
                for key in ('settings', 'adapters', 'resources')}}
    controls['adapters'] = {**DEFAULT_ADAPTERS, **controls['adapters']}
    controls['turns'] = request.get('runtime', {}).get('limits', {}).get('max-turns', 8)
    definitions = {key: _definition(Path(root), value) for key, value in request['roles'].items()}
    original = {key: value for key, value in request.items()
                if key in {'pr', 'logical_run', 'instructions', 'authorizations', 'maintain_other'}}
    return prepare_data(root, original, controls, digest(['fixture-execution', request.get('runtime', {})]), provider, definitions)
