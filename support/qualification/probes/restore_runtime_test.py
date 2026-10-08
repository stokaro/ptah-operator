import copy
import base64
import hashlib
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch

from operator_restore import OperatorProbe
from restore_runtime import ReleaseRuntime, REPOSITORY, TAG


class ReleaseRuntimeTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.probe = Mock(root=self.root, report={'sourceCommit': 'a' * 40})
        self.probe.command.side_effect = self.command
        self.calls = []
        self.index = json.dumps({'manifests': [
            {'digest': 'sha256:' + 'c' * 64, 'platform': {'os': 'linux', 'architecture': 'amd64'}},
            {'digest': 'sha256:' + 'd' * 64, 'platform': {'os': 'linux', 'architecture': 'arm64'}}]}).encode()
        self.digest = 'sha256:' + hashlib.sha256(self.index).hexdigest()
        self.image = 'ghcr.io/' + REPOSITORY + '@' + self.digest
        self.executor = 'ghcr.io/' + REPOSITORY + '-executor@' + self.digest
        self.chart = self.root / 'ptah-operator-0.2.0.tgz'
        self.chart.write_bytes(b'chart bytes; the external release verifier is mocked in these orchestration tests')
        self.fields = {'version': '0.2.0', 'source-repository': REPOSITORY, 'source-ref': 'refs/tags/' + TAG,
                       'source-sha': 'a' * 40, 'image': self.image, 'executor': self.executor,
                       'chart-asset': self.chart.name, 'chart-asset-sha256': hashlib.sha256(self.chart.read_bytes()).hexdigest(),
                       'executor-ptah-commit': 'b' * 40, 'executor-ptah-version': 'pinned-ptah'}
        self.write_manifest()
        (self.root / 'SHA256SUMS').write_text('fixture checksums; external verifier is mocked\n')
        self.original = {'replicaCount': 2, 'image': {'repository': 'fixture', 'digest': 'old'},
                         'execution': {'runnerImage': 'old', 'executorImage': 'old'},
                         'imagePullSecrets': [{'name': 'private-fixture-registry'}],
                         'approvals': {'requireDistinctApprover': True}, 'resources': {'limits': {'memory': '256Mi'}}}
        self.runtime = ReleaseRuntime(self.probe, self.root)
        self.installed = False

    def write_manifest(self):
        (self.root / 'release-manifest.txt').write_text(''.join(k + '=' + v + '\n' for k, v in self.fields.items()))

    def pods(self):
        pods = []
        for i, component in enumerate(('controller', 'controller', 'certificate-rotation')):
            pods.append({'metadata': {'uid': str(i), 'name': 'runtime-' + str(i),
                                     'labels': {'app.kubernetes.io/component': component}},
                         'spec': {'containers': [{'name': 'runtime', 'image': self.image}],
                                  'initContainers': [{'name': 'verify', 'image': self.image}]},
                         'status': {'phase': 'Running', 'containerStatuses': [
                             {'name': 'runtime', 'imageID': self.image, 'restartCount': 0, 'ready': True}],
                             'initContainerStatuses': [{'name': 'verify', 'imageID': self.image, 'restartCount': 0,
                                                        'state': {'terminated': {'exitCode': 0}}}]}})
        return pods

    def command(self, action, args):
        self.calls.append(args)
        result = b''
        if args[:2] == ['gh', 'api']:
            result = ('a' * 40).encode()
        elif 'imagetools' in args:
            result = self.index
        elif args[0] == 'helm' and 'upgrade' in args:
            self.installed = True
        elif args[0] == 'helm' and 'values' in args:
            result = json.dumps(self.runtime.values(self.original) if self.installed else self.original).encode()
        elif args[0] == 'kubectl':
            result = json.dumps({'items': self.pods()}).encode()
        return subprocess.CompletedProcess(args, 0, result)

    def test_manifest_refusals_happen_before_any_command(self):
        for key, value in [('source-sha', 'e' * 40), ('source-ref', 'refs/heads/master'),
                           ('version', 'dev'), ('image', 'ghcr.io/stokaro/ptah-operator:latest'),
                           ('executor', 'example.com/executor@' + self.digest),
                           ('chart-asset-sha256', 'f' * 64)]:
            with self.subTest(key=key):
                original = self.fields[key]; self.fields[key] = value; self.write_manifest()
                with self.assertRaises(ValueError):
                    ReleaseRuntime(self.probe, self.root)
                self.fields[key] = original
        self.assertEqual(self.calls, [])

    def test_duplicate_or_changed_assets_are_refused(self):
        path = self.root / 'release-manifest.txt'
        path.write_text(path.read_text() + 'source-sha=' + 'a' * 40 + '\n')
        with self.assertRaises(ValueError):
            ReleaseRuntime(self.probe, self.root)
        with self.assertRaisesRegex(RuntimeError, 'changed'):
            self.runtime.authenticate()
        self.assertEqual(self.calls, [])

    def test_complete_consumer_identity_is_required(self):
        self.runtime.authenticate()
        checks = [a for a in self.calls if a[:3] == ['gh', 'attestation', 'verify']]
        self.assertEqual(len(checks), 5)
        for command in checks:
            for flag, value in [('--repo', REPOSITORY), ('--source-ref', 'refs/tags/' + TAG),
                                ('--source-digest', 'a' * 40), ('--signer-workflow', REPOSITORY + '/.github/workflows/release.yml')]:
                self.assertEqual(command[command.index(flag) + 1], value)
        self.assertEqual(len([a for a in self.calls if a[:2] == ['cosign', 'verify']]), 2)
        self.assertTrue(self.runtime.authenticated)

    def test_each_failed_trust_command_prevents_installation(self):
        self.runtime.authenticate(); count = len(self.calls)
        for failure in range(count):
            with self.subTest(failure=failure):
                runtime = ReleaseRuntime(self.probe, self.root)
                current = 0
                def command(action, args):
                    nonlocal current
                    current += 1
                    if current == failure + 1:
                        raise RuntimeError('verification failed')
                    return self.command(action, args)
                with patch.object(self.probe, 'command', side_effect=command):
                    with self.assertRaises(RuntimeError):
                        runtime.authenticate()
                    with self.assertRaisesRegex(RuntimeError, 'authentication must precede'):
                        runtime.install({}, 'source')
                self.assertFalse(runtime.authenticated)

    def test_registry_cannot_supply_another_index(self):
        self.index += b' '
        with self.assertRaisesRegex(RuntimeError, 'Registry image index differs'):
            self.runtime.authenticate()
        self.assertFalse(self.runtime.authenticated)

    def test_same_assets_install_in_both_clusters_and_preserve_other_values(self):
        self.runtime.authenticate()
        environments = []
        for name in ('source', 'replacement'):
            self.installed = False
            env = {'E2E_KUBECONFIG': '/' + name, 'E2E_OPERATOR_NAMESPACE': name, 'E2E_HELM_RELEASE': name}
            self.runtime.install(env, name); environments.append(env)
            self.assertEqual(env['E2E_CONTROLLER_IMAGE'], self.image)
            self.assertEqual(env['E2E_RUNNER_IMAGE'], self.image)
            self.assertEqual(env['E2E_EXECUTOR_IMAGE'], self.executor)
            self.assertEqual(env['E2E_CHART_PACKAGE'], str(self.chart.resolve()))
            values = json.loads((self.root / (name + '-release-values.private.json')).read_text())
            self.assertEqual(values['approvals'], self.original['approvals'])
            self.assertEqual(values['resources'], self.original['resources'])
            self.assertEqual(values['imagePullSecrets'], [])
        installations = self.probe.report['releaseRuntime']['installations']
        self.assertEqual([r['cluster'] for r in installations], ['source', 'replacement'])
        self.assertEqual(installations[0]['chartSHA256'], installations[1]['chartSHA256'])
        self.chart.write_bytes(b'changed after source loss')
        calls = len(self.calls)
        with self.assertRaisesRegex(RuntimeError, 'changed'):
            self.runtime.install(environments[1], 'replacement')
        self.assertEqual(len(self.calls), calls)

    def test_runtime_readback_refuses_missing_stale_or_wrong_images(self):
        accepted = {self.digest, 'sha256:' + 'c' * 64}
        for defect in ('none', 'empty', 'missing replica', 'duplicate replica', 'old image', 'wrong imageID',
                       'missing status', 'restart', 'unready', 'terminating', 'failed init'):
            with self.subTest(defect=defect):
                pods = self.pods()
                if defect == 'empty': pods = []
                elif defect == 'missing replica': pods.pop()
                elif defect == 'duplicate replica': pods.append(copy.deepcopy(pods[0]))
                elif defect == 'old image': pods[0]['spec']['containers'][0]['image'] = 'dev:latest'
                elif defect == 'wrong imageID': pods[0]['status']['containerStatuses'][0]['imageID'] = 'sha256:' + 'f' * 64
                elif defect == 'missing status': pods[0]['status']['containerStatuses'] = []
                elif defect == 'restart': pods[0]['status']['containerStatuses'][0]['restartCount'] = 1
                elif defect == 'unready': pods[0]['status']['containerStatuses'][0]['ready'] = False
                elif defect == 'terminating': pods[0]['metadata']['deletionTimestamp'] = 'now'
                elif defect == 'failed init': pods[0]['status']['initContainerStatuses'][0]['state']['terminated']['exitCode'] = 1
                if defect == 'none': self.assertEqual(len(self.runtime.runtime_pods(pods, self.original, accepted)), 3)
                else:
                    with self.assertRaises(RuntimeError): self.runtime.runtime_pods(pods, self.original, accepted)

    def test_actual_apply_reading_is_rebound_and_wrong_bytes_are_refused(self):
        self.runtime.authenticate()
        pod = json.loads((Path(__file__).parent / 'testdata/workload-history.json').read_text())['pods'][0]
        for collection in ('containers', 'initContainers'):
            for c in pod['spec'][collection]: c['image'] = self.image if c['name'] == 'install-runner' else self.executor
        for collection in ('containerStatuses', 'initContainerStatuses'):
            for c in pod['status'][collection]: c['imageID'] = self.image if c['name'] == 'install-runner' else self.executor
        self.runtime.verify_apply_pods([pod])
        self.assertEqual(self.probe.report['releaseRuntime']['applyPods'][0]['uid'], pod['metadata']['uid'])
        for defect in ('missing', 'runner', 'executor', 'runtime ID', 'status'):
            with self.subTest(defect=defect):
                changed = copy.deepcopy(pod)
                if defect == 'runner': changed['spec']['initContainers'][0]['image'] = 'dev:runner'
                elif defect == 'executor': changed['spec']['containers'][0]['image'] = 'dev:executor'
                elif defect == 'runtime ID': changed['status']['containerStatuses'][0]['imageID'] = 'sha256:' + 'f' * 64
                elif defect == 'status': changed['status']['initContainerStatuses'] = []
                with self.assertRaises(RuntimeError): self.runtime.verify_apply_pods([] if defect == 'missing' else [changed])

    def test_source_preparation_is_shared_by_database_and_cold_recovery(self):
        p = object.__new__(OperatorProbe)
        p.release_assets = self.root; p.release_runtime = None; p.envs = {}; p.report = {'scope': 'Recovery'}
        def install(env, name): env.update(E2E_CONTROLLER_IMAGE=self.image, E2E_EXECUTOR_IMAGE=self.executor)
        runtime = Mock(); runtime.install.side_effect = install
        with patch('operator_restore.ReleaseRuntime', return_value=runtime):
            p.prepare_runtime(); p.prepare_runtime()
        runtime.authenticate.assert_called_once_with()
        runtime.install.assert_called_once_with(p.envs, 'source')
        self.assertEqual(p.report['operatorImage'], self.image)

    def test_fixture_pull_credentials_do_not_follow_the_release_executor_to_ghcr(self):
        p = object.__new__(OperatorProbe)
        credentials = self.root / 'fixture-credentials.private.json'
        credentials.write_text(json.dumps({'username': 'fixture', 'password': 'disposable'}))
        p.envs = {'E2E_REGISTRY_IP': '192.0.2.10', 'E2E_REGISTRY_HOST': 'fixture-registry:5000',
                  'E2E_REGISTRY_CREDENTIALS_FILE': str(credentials), 'E2E_EXECUTOR_IMAGE': self.executor}
        p.family, p.namespace = 'schema', 'isolated-recovery'
        p.kubectl = Mock(); p.route = Mock(); p.secret = Mock(); p.create = Mock()
        p.prepare_namespace()
        pull = next(call.args[1] for call in p.secret.call_args_list if call.args[0] == 'restore-pull')
        self.assertEqual(set(json.loads(pull['.dockerconfigjson'])['auths']), {'fixture-registry:5000'})

    def test_replacement_pull_credentials_stay_in_the_replacement_fixture_registry(self):
        from cluster_restore import ClusterRestoreProbe
        p = object.__new__(ClusterRestoreProbe)
        credentials = self.root / 'replacement-credentials.private.json'
        credentials.write_text(json.dumps({'username': 'replacement', 'password': 'disposable'}))
        p.envs = {'E2E_REGISTRY_HOST': 'replacement-registry:5000',
                  'E2E_REGISTRY_CREDENTIALS_FILE': str(credentials), 'E2E_EXECUTOR_IMAGE': self.executor}
        p.kind, p.namespace = 'PtahSchema', 'isolated-recovery'
        p.target_active = True; p.report = {}; p.api = 'operator.ptah.run/v1alpha1'
        p.watches = {}; p.begin_watch = Mock()
        with patch.object(OperatorProbe, 'create', side_effect=lambda obj: {**obj, 'metadata': {**obj['metadata'], 'uid': 'created'}}) as create:
            p.create({'kind': p.kind, 'metadata': {'name': 'fixture'}, 'spec': {}})
        secret = create.call_args_list[0].args[0]
        config = json.loads(base64.b64decode(secret['data']['.dockerconfigjson']))
        self.assertEqual(set(config['auths']), {'replacement-registry:5000'})

    def test_cold_bootstrap_identity_gate_precedes_release_installation(self):
        from cluster_restore import ClusterRestoreProbe
        for mismatch in (False, True):
            with self.subTest(mismatch=mismatch):
                p = object.__new__(ClusterRestoreProbe)
                p.root = self.root; p.report = {'steps': [], 'checks': {}}
                p.prefix = 'ptah-020-restore-runtime'; p.timing = 'idle'; p.loss = 'operator'
                p.target_environment = self.root / 'target-environment'
                p.source_cluster = {'kubernetesVersion': '1.37.0'}
                p.source_envs = {'E2E_DOCKER_ENDPOINT': 'ssh://selected',
                                 'E2E_CONTROLLER_REVISION': 'a' * 40, 'E2E_PTAH_REVISION': 'b' * 40}
                target = {**p.source_envs, 'E2E_DOCKER_ENDPOINT': 'ssh://wrong' if mismatch else 'ssh://selected'}
                p.target_environment.write_text(''.join(k + '=' + v + '\n' for k, v in target.items()))
                p.ensure_api_connection = Mock(); p.release_runtime = Mock()
                p.release_runtime.install.side_effect = RuntimeError('reached release installation')
                process = Mock(); process.wait.return_value = 0
                with patch('cluster_restore.subprocess.Popen', return_value=process):
                    with self.assertRaisesRegex(RuntimeError, 'same explicit Docker endpoint' if mismatch else 'reached release installation'):
                        p.provision_target()
                if mismatch:
                    p.ensure_api_connection.assert_not_called()
                    p.release_runtime.install.assert_not_called()
                else:
                    p.release_runtime.install.assert_called_once_with(target, 'replacement')


if __name__ == '__main__':
    unittest.main()
