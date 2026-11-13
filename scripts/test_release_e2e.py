"""Release E2E orchestration rejection tests. All providers/APIs/runtimes are fake."""
import copy
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import unittest
from unittest.mock import patch

import release_e2e as e2e
import release_certification as cert
import release_runtime as runtime
from test_release import module

environments = module('release-environments')
gate = module('validate-release')

class CertificationTests(unittest.TestCase):
    def setUp(self):
        self.policy = json.loads((e2e.ROOT / 'release/policy.json').read_text())
        self.commit, self.fingerprint = 'a'*40, 'b'*64
        self.env = {'GITHUB_RUN_ID': '100', 'GITHUB_RUN_ATTEMPT': '2'}
        self.report = {'candidate': self.commit, 'source_fingerprint': self.fingerprint, 'reviewed': False,
                       'origin': {'repository': e2e.REPO, 'workflow': '.github/workflows/release.yml', 'ref': 'refs/tags/v0.2.0', 'run_id': '100', 'run_attempt': '1'},
                       'records': [], 'runtime_recovery': [], 'images': {'amd64': 'sha256:'+'1'*64, 'arm64': 'sha256:'+'2'*64}}
        for row in self.policy['required_evidence']:
            if row['scenario'] == 'disposable-github-publication':
                continue
            scenario = json.loads((e2e.ROOT / 'conformance/scenarios' / (row['scenario']+'.json')).read_text())
            provider = row['provider']
            versions = {provider: 'claude-agent-sdk@0.3.319' if provider == 'claude-sdk' else 'codex-cli 0.167.0' if provider.startswith('codex') else '2.1.323 (Claude Code)'}
            if row['scenario'].startswith('mixed-development-'):
                versions['codex-app-server'] = 'codex_cli_rs/0.167.0 (Linux 6.11.0; x86_64)'
            record = dict(row, engine={'commit': self.commit, 'source_fingerprint': self.fingerprint, 'version': '0.2.0', 'dirty': 'false'},
                          assertions={a: True for a in scenario['assertions']}, provider_versions=versions,
                          invocations=[{'id': 'invocation-'+p, 'provider': p, 'version': v} for p,v in versions.items()],
                          configuration_fingerprint='c'*64, started='2026-11-13T00:00:00Z', finished='2026-11-13T00:01:00Z')
            if row.get('runtime'):
                record.update(image_digest=self.report['images']['amd64'], runtime_version='1.0.0')
            self.report['records'].append(record)
        for row in self.policy['required_runtime_recovery']:
            self.report['runtime_recovery'].append(dict(row, candidate=self.commit, source_fingerprint=self.fingerprint,
                                                      image_digest=self.report['images']['amd64'], runtime_version='1.0.0', assertions={a: True for a in e2e.RECOVERY}))
        self.kwargs = dict(policy=self.policy, commit=self.commit, fingerprint=self.fingerprint, environment=self.env)

    def validate(self, report=None):
        return cert.validate_automated(report or self.report, **self.kwargs)

    def test_complete_coverage_and_certification_retry_reuses_original_attempt(self):
        self.validate()
        signed = copy.deepcopy(self.report)
        signed['reviewed'] = True
        signed['records'].append({'scenario': 'disposable-github-publication'})
        cert.match_signed(signed, self.report, **self.kwargs)
        self.env['GITHUB_RUN_ATTEMPT'] = '3'
        cert.match_signed(signed, self.report, **self.kwargs)

    def test_reject_foreign_candidate_run_future_attempt_and_changed_image(self):
        mutations = [lambda r: r.update(candidate='f'*40), lambda r: r.update(source_fingerprint='f'*64),
                     lambda r: r['origin'].update(run_id='99'), lambda r: r['origin'].update(run_attempt='3'),
                     lambda r: r['origin'].update(workflow='test.yml'), lambda r: r['images'].update(amd64='sha256:'+'3'*64),
                     lambda r: r['records'].pop(), lambda r: r['runtime_recovery'].pop(),
                     lambda r: r['records'][0].update(error='failed'), lambda r: r['records'][0].update(invocations=[]),
                     lambda r: r['records'][0]['assertions'].update(usage_observed=False)]
        for mutate in mutations:
            value=copy.deepcopy(self.report)
            mutate(value)
            with self.subTest(mutate=mutate), self.assertRaises(ValueError):
                self.validate(value)

    def test_signed_report_cannot_change_automated_records_or_origin(self):
        for key in ['origin', 'records', 'runtime_recovery', 'images', 'candidate', 'source_fingerprint', 'reviewed']:
            signed=copy.deepcopy(self.report)
            signed['reviewed']=True
            signed[key]=False
            with self.subTest(key=key), self.assertRaises((ValueError, TypeError)):
                cert.match_signed(signed, self.report, **self.kwargs)

    def test_sanitization_removes_private_transcripts_paths_and_rejects_secret_fields(self):
        record=copy.deepcopy(self.report['records'][0])
        record.update(artifacts=['/private/state'], stdout='SECRET transcript', configuration={'token':'SECRET'})
        cleaned=e2e.sanitized(record, ['SECRET'])
        self.assertNotIn('artifacts',cleaned)
        self.assertNotIn('SECRET',json.dumps(cleaned))
        record['provider_versions']['codex-cli']='SECRET'
        with self.assertRaises(ValueError):
            e2e.sanitized(record,['SECRET'])
        record = next(r for r in self.report['records'] if r['provider'] == 'claude-cli')
        self.assertEqual(e2e.sanitized(record, [])['provider_versions']['claude-cli'], '2.1.323 (Claude Code)')

class SetupTests(unittest.TestCase):
    def test_missing_invalid_expired_and_api_credentials_fail_without_login(self):
        import base64
        encode=lambda x: base64.urlsafe_b64encode(json.dumps(x).encode()).decode().rstrip('=')
        jwt='header.'+encode({'exp':time.time()-1})+'.signature'
        cases=[('', ''), ('{}','token'), ('not JSON','token'), (json.dumps({'tokens':{'access_token':jwt,'account_id':'account'}}),'token'),
               (json.dumps({'OPENAI_API_KEY':'api-key','tokens':{'access_token':'opaque','account_id':'account'}}),'token')]
        for codex,claude in cases:
            with self.subTest(codex=codex),self.assertRaises(ValueError):
                e2e.credentials(codex,claude)
        self.assertEqual(e2e.credentials('{"tokens":{"access_token":"opaque","account_id":"account"}}','token')[1], 'token')

    def test_every_required_scenario_has_setup_and_phase(self):
        policy=json.loads((e2e.ROOT/'release/policy.json').read_text())
        rows=e2e.rows(policy,'smoke')+e2e.rows(policy,'matrix')
        self.assertEqual(len(rows),16)
        self.assertEqual(len(e2e.rows(policy,'smoke')),7)
        self.assertFalse(any(r['scenario']=='disposable-github-publication' for r in rows))
        for row in rows:
            cfg=e2e.configuration(Path('/private'),Path('/package'),row.get('runtime'),'sha256:'+'1'*64)
            agent=next(a for a in cfg['agents'].values() if a['provider']==row['provider'])
            self.assertEqual(agent['authentication']['mode'],'native')
            if row.get('runtime'):
                self.assertEqual(agent['execution']['image'],'sha256:'+'1'*64)
                self.assertEqual(agent['execution']['runtime'],row['runtime'])
        pins=json.loads((e2e.ROOT/'images/pins.json').read_text())
        lock=json.loads((e2e.ROOT/'conformance/providers/package-lock.json').read_text())
        self.assertEqual(lock['packages']['node_modules/@anthropic-ai/claude-code']['version'],pins['claude_cli'])

    def test_private_credentials_not_forwarded_to_controller_environment(self):
        with tempfile.TemporaryDirectory() as d:
            state=Path(d)
            (state/'claude-token').write_text('claude-token')
            with patch.dict(os.environ,GH_TOKEN='github-token',CODEX_AUTH_JSON='codex-json',OPENAI_API_KEY='api-token'):
                env=e2e.provider_env(state)
            self.assertFalse({'GH_TOKEN','CODEX_AUTH_JSON','OPENAI_API_KEY'} & set(env))
            self.assertEqual(env['CLAUDE_CODE_OAUTH_TOKEN'],'claude-token')

    def test_environment_setup_preserves_existing_reviewers_and_requires_explicit_replacement(self):
        current={'can_admins_bypass':False,'deployment_branch_policy':{'custom_branch_policies':True},
                 'protection_rules':[{'type':'wait_timer','wait_timer':5},{'type':'required_reviewers','prevent_self_review':True,
                  'reviewers':[{'type':'Team','reviewer':{'id':7}}]}]}
        calls=[]
        def api(method,path,data=None,**kwargs):
            calls.append((method,path,data))
            if method=='GET' and path.endswith('release-certification'):
                return current
            if method=='GET':
                return {'branch_policies':[{'name':'v*','type':'tag'}]}
            return {}
        with patch.object(environments,'api',side_effect=api):
            environments.configure('release-certification',123)
        body=next(data for method,path,data in calls if method=='PUT')
        self.assertEqual(body['wait_timer'],5)
        self.assertEqual(body['reviewers'],[{'type':'Team','id':7},{'type':'User','id':123}])
        self.assertFalse(body['prevent_self_review'])
        self.assertFalse(any(method=='POST' for method,path,data in calls))

    def test_secret_upload_uses_stdin_and_suppresses_service_diagnostics(self):
        with tempfile.TemporaryDirectory() as d:
            path=Path(d)/'credential'
            path.write_bytes(b'private-token')
            with patch.object(environments.subprocess,'run',return_value=subprocess.CompletedProcess([],0)) as run:
                environments.upload('CLAUDE_CODE_OAUTH_TOKEN',path)
                args,kwargs=run.call_args
                self.assertNotIn('private-token',repr(args))
                self.assertEqual(kwargs['input'],b'private-token')
                self.assertTrue(kwargs['capture_output'])

    def test_disposable_runtime_targets_only_private_daemons_and_storage(self):
        for name in ['docker','podman']:
            with self.subTest(runtime=name),tempfile.TemporaryDirectory() as d:
                commands=[]
                with patch.object(runtime,'control',side_effect=lambda p,a: commands.append((json.loads(p.read_text()),a))), \
                     patch.object(runtime.shutil,'which',side_effect=lambda x:'/usr/bin/'+x), \
                     patch.object(runtime.subprocess,'run'),patch.object(runtime.os,'getuid',return_value=1001):
                    with runtime.DisposableRuntime(Path(d),name,{'PATH':'/usr/bin'}) as fixture:
                        spec=json.loads((fixture.root/'service.json').read_text())
                        command=spec['command']
                        for option in (['--data-root','--exec-root','--pidfile','--host'] if name=='docker' else ['--root','--runroot']):
                            self.assertIn(str(fixture.root),command[command.index(option)+1])
                        self.assertNotIn('build',command)
                        self.assertNotIn('login',command)
                    self.assertEqual([a for s,a in commands],['start','stop'])

class ReleasePhaseTests(unittest.TestCase):
    def test_candidate_does_not_read_conformance_and_never_authorizes_publication(self):
        import contextlib
        import io
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)
            (root/'release/keys').mkdir(parents=True)
            (root/'internal/buildinfo').mkdir(parents=True)
            (root/'release/policy.json').write_text(json.dumps({'version':'0.2.0','signers':['fixture']}))
            (root/'internal/buildinfo/info.go').write_text('const Version = "0.2.0"')
            def git(*args):
                if args[0]=='status': return ''
                if args[0]=='cat-file': return 'tag' if args[1]=='-t' else 'tag v0.2.0\ntype commit\n\nfixture'
                if args[0]=='rev-parse': return 'a'*40
                self.fail(args)
            output=io.StringIO()
            with patch.object(gate,'ROOT',root),patch.object(gate,'git',side_effect=git), \
                 patch.object(gate,'source_fingerprint',return_value='b'*64),patch.object(gate,'trusted_signature',return_value='fixture') as signatures, \
                 patch.object(gate,'validate_evidence',side_effect=AssertionError('candidate read evidence')), \
                 patch.object(gate,'validate_remote') as remote,patch.object(gate.subprocess,'run'), \
                 patch('sys.argv',['validate-release.py','v0.2.0','--phase','candidate']),contextlib.redirect_stdout(output):
                gate.main()
                self.assertEqual(signatures.call_count,1)
                remote.assert_called_once()
            self.assertFalse(json.loads(output.getvalue())['publication_authorized'])

    def test_publisher_refuses_another_workflow_before_external_effects(self):
        publisher=module('publish-release')
        with patch.dict(os.environ,GITHUB_WORKFLOW_REF='dk-code-factory/agent-workflow/.github/workflows/other.yml@refs/tags/v0.2.0'), \
             patch.object(publisher,'run',side_effect=AssertionError('unexpected subprocess')):
            with self.assertRaises(ValueError):
                publisher.validate_certified(Path('/unused'),'v0.2.0')

    def test_publisher_checks_both_image_identities_before_registry_mutation(self):
        publisher=module('publish-release')
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)
            (root/'conformance-evidence.json').write_text(json.dumps({'images':{'amd64':'sha256:a','arm64':'sha256:b'}}))
            for arch in ['amd64','arm64']:
                (root/(arch+'.tar')).write_bytes(b'archive')
                (root/('worker-linux-'+arch+'.json')).write_text(json.dumps({'image':'sha256:a', 'archive':arch+'.tar','archive_sha256':e2e.sha256(root/(arch+'.tar'))}))
            with patch.object(publisher,'validate_certified'),patch.object(publisher,'manifest',side_effect=AssertionError('registry read before image validation')):
                with self.assertRaisesRegex(ValueError,'distribution image differs'):
                    publisher.registry(root,'v0.2.0')
