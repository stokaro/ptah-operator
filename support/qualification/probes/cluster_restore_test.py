import copy
import json
import os
from pathlib import Path
import runpy
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch

from cluster_restore import ClusterRestoreProbe, db
from operator_restore import OPERATOR_CRDS, OperatorProbe


class SurvivingSourceTest(unittest.TestCase):
    def test_only_database_lag_uses_the_surviving_source_rebuild(self):
        for timing in ('idle', 'during-apply', 'operator-lag'):
            with self.subTest(timing=timing):
                def initialize(probe, engine, family, environment, root, loss, selected):
                    probe.loss, probe.timing = loss, selected
                    probe.envs = {'E2E_KIND_CLUSTER_NAME': 'ptah-e2e-owned', 'E2E_CONTROLLER_REVISION': 'same'}
                    probe.report = {'sourceCommit': 'same'}
                with patch.object(OperatorProbe, '__init__', initialize):
                    if timing == 'operator-lag':
                        probe = ClusterRestoreProbe('mysql', 'migration', Path('source'), Path('output'), 'database', timing)
                        self.assertTrue(probe.rebuilds_operator)
                        self.assertFalse(probe.source_watches)
                    else:
                        with self.assertRaisesRegex(ValueError, 'older operator checkpoint'):
                            ClusterRestoreProbe('mysql', 'migration', Path('source'), Path('output'), 'database', timing)
        pilot = object.__new__(OperatorProbe); pilot.loss = 'database'
        self.assertFalse(pilot.rebuilds_operator)

    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        p = self.probe = object.__new__(ClusterRestoreProbe)
        p.root = Path(self.directory.name); p.report = {'checks': {}}
        p.loss, p.timing = 'database', 'operator-lag'
        p.envs = {'cluster': 'source'}; p.source_envs = p.envs
        p.target_active = False; p.source_watches = {}; p.watches = {}
        p.source_cluster = {'uid': 'original-cluster'}
        p.archived_workloads = {'jobs': [], 'pods': []}
        self.resource = {'metadata': {'uid': 'original-resource'},
                         'spec': {'suspend': True, 'policy': {'apply': 'OnApproval'}, 'artifact': {'ociRef': 'committed'}},
                         'status': {'executionBinding': {'epoch': 'original-epoch'}}}
        self.secret = {'kind': 'Secret', 'type': 'Opaque', 'data': {'url': 'fixture'},
                       'metadata': {'name': 'restore-target', 'uid': 'original-secret'}}
        self.cluster = {'metadata': {'uid': 'original-cluster'}}
        p.recovery_update = {'resource': copy.deepcopy(self.resource), 'namespaceState': {'dependencies': [self.secret]}}
        p.read = lambda kind=None, name=None: copy.deepcopy(self.secret if kind == 'secrets' else self.cluster if kind == 'namespaces' else self.resource)
        p.retained_resource = copy.deepcopy(self.resource); p.retained_secret = copy.deepcopy(self.secret)
        for kind in ('jobs', 'pods'):
            path = p.root / (kind + '-watch.private.json')
            stream = path.open('w'); stream.write('original evidence'); stream.flush()
            self.addCleanup(stream.close)
            (p.root / (kind + '-watch.stderr.private')).write_text('original diagnostics')
            process = Mock(); process.poll.return_value = None
            p.watches[kind] = (process, stream, path, [])

    def test_database_lag_retains_the_source_and_live_evidence(self):
        p = self.probe; original = dict(p.watches)
        with patch.object(p, 'command') as command:
            p.lose_namespace()
        command.assert_not_called()
        self.assertFalse(p.report['operatorLossInjected'])
        self.assertNotIn('sourceDestroyedAt', p.report)
        self.assertEqual(set(p.source_watches), {'jobs', 'pods'})
        self.assertFalse(p.watches)
        for kind, (process, stream, path, _) in original.items():
            process.terminate.assert_not_called(); self.assertFalse(stream.closed)
            self.assertFalse(path.exists())
            stream.write(' still live'); stream.flush()
            self.assertEqual(p.source_watches[kind][2].read_text(), 'original evidence still live')
            self.assertEqual((p.root / ('source-' + kind + '-watch.stderr.private')).read_text(), 'original diagnostics')

    def test_changed_source_stops_recovery_and_restores_target_context(self):
        for defect in ('cluster', 'resource', 'secret', 'secret-data', 'secret-type', 'spec', 'automatic', 'resumed', 'execution', 'binding', 'dead-watch'):
            with self.subTest(defect=defect):
                p = self.probe
                resource, secret, cluster = copy.deepcopy((self.resource, self.secret, self.cluster))
                if defect == 'cluster': cluster['metadata']['uid'] = 'new-cluster'
                elif defect == 'resource': resource['metadata']['uid'] = 'new-resource'
                elif defect == 'secret': secret['metadata']['uid'] = 'new-secret'
                elif defect == 'secret-data': secret['data']['url'] = 'changed'
                elif defect == 'secret-type': secret['type'] = 'changed'
                elif defect == 'spec': resource['spec']['artifact']['ociRef'] = 'another'
                elif defect == 'automatic': resource['spec']['policy']['apply'] = 'Always'
                elif defect == 'resumed': resource['spec']['suspend'] = False
                elif defect == 'execution': resource['status']['activeOperation'] = {'type': 'Apply'}
                elif defect == 'binding': resource['status']['executionBinding']['epoch'] = 'replaced'
                p.source_watches = p.watches if not p.source_watches else p.source_watches
                target_watches = {}; target_environment = {'cluster': 'replacement'}
                p.envs, p.watches, p.target_active = target_environment, target_watches, True
                p.source_watches['jobs'][0].poll.return_value = 0 if defect == 'dead-watch' else None
                def read(kind=None, name=None):
                    self.assertIs(p.envs, p.source_envs); self.assertFalse(p.target_active)
                    return secret if kind == 'secrets' else cluster if kind == 'namespaces' else resource
                with patch.object(p, 'read', side_effect=read), self.assertRaises(RuntimeError):
                    p.check_retained_source()
                self.assertIs(p.envs, target_environment); self.assertIs(p.watches, target_watches)
                self.assertTrue(p.target_active)

    def test_replacement_requires_a_retained_and_live_source(self):
        with patch('cluster_restore.subprocess.Popen') as start:
            with self.assertRaisesRegex(RuntimeError, 'retains and watches'):
                self.probe.provision_target()
        start.assert_not_called()

    def test_a_barrier_is_created_in_each_cluster(self):
        p = self.probe; p.source_watches = p.watches
        p.watches = {'target': 'watch'}; p.envs = {'cluster': 'replacement'}; p.target_active = True
        observed = []
        def barrier(_, name):
            observed.append((p.envs['cluster'], p.target_active, p.watches))
            return {kind: p.envs['cluster'] + '-' + kind for kind in ('jobs', 'pods')}
        with patch.object(OperatorProbe, 'barrier', barrier):
            result = p.barrier('proof')
        self.assertEqual([(c, active) for c, active, _ in observed], [('source', False), ('replacement', True)])
        self.assertIs(observed[0][2], p.source_watches)
        self.assertEqual(result, {kind: {'source': 'source-' + kind, 'replacement': 'replacement-' + kind} for kind in ('jobs', 'pods')})

    def test_both_clusters_supply_history_and_a_dead_source_cannot_pass(self):
        p = self.probe
        reading = json.loads((Path(__file__).parent / 'testdata/workload-history.json').read_text())
        p.family, p.kind = 'migration', 'PtahMigration'
        p.name, p.namespace = reading['identity']['name'], reading['identity']['namespace']
        p.uid = 'replacement-resource'; p.resource_uids = {p.uid, reading['identity']['uid']}
        for _, stream, _, _ in p.watches.values(): stream.close()
        p.watches = {}; p.source_watches = {}
        target_job, target_pod = copy.deepcopy((reading['jobs'][0], reading['pods'][0]))
        target_job['metadata']['uid'] = 'replacement-job'; target_job['metadata']['ownerReferences'][0]['uid'] = p.uid
        target_pod['metadata']['uid'] = 'replacement-pod'; target_pod['metadata']['ownerReferences'][0]['uid'] = 'replacement-job'
        for label, collection, baseline in (('source', p.source_watches, reading),
                                             ('replacement', p.watches, {'jobs': [target_job], 'pods': [target_pod]})):
            for kind in ('jobs', 'pods'):
                path = p.root / (label + '-' + kind + '.json')
                path.write_text(json.dumps({'type': 'ADDED', 'object': {'metadata': {'uid': label + '-' + kind}}}))
                stream = path.open('a'); self.addCleanup(stream.close)
                process = Mock(); process.poll.return_value = None
                collection[kind] = (process, stream, path, baseline[kind])
        p.envs = {'cluster': 'replacement'}; p.target_active = True
        boundaries = {kind: {label: label + '-' + kind for label in ('source', 'replacement')} for kind in ('jobs', 'pods')}
        original_uid = reading['jobs'][0]['metadata']['uid']
        self.assertEqual(p.watched_apply_jobs(boundaries), {original_uid, 'replacement-job'})
        unexpected_job, unexpected_pod = copy.deepcopy((reading['jobs'][0], reading['pods'][0]))
        unexpected_job['metadata']['uid'] = 'unexpected-source-job'
        unexpected_pod['metadata']['uid'] = 'unexpected-source-pod'
        unexpected_pod['metadata']['ownerReferences'][0]['uid'] = 'unexpected-source-job'
        p.source_watches['jobs'][3].append(unexpected_job); p.source_watches['pods'][3].append(unexpected_pod)
        self.assertEqual(p.watched_apply_jobs(boundaries), {original_uid, 'replacement-job', 'unexpected-source-job'})
        with self.assertRaisesRegex(RuntimeError, 'both clusters'):
            p.watched_objects('jobs', 'replacement-jobs')
        p.source_watches['jobs'][0].poll.return_value = 0
        with self.assertRaisesRegex(RuntimeError, 'watch remains live'):
            p.watched_apply_jobs(boundaries)

    def test_source_watch_cleanup_closes_streams_on_timeout(self):
        p = self.probe; p.source_watches = p.watches; p.watches = {}
        process = p.source_watches['jobs'][0]
        process.wait.side_effect = [subprocess.TimeoutExpired('watch', 10), None]
        self.assertFalse(p.stop_source_watches())
        process.kill.assert_called_once()
        self.assertTrue(all(stream.closed for _, stream, _, _ in p.source_watches.values()))


class RestoreDockerContextTest(unittest.TestCase):
    def test_database_commands_and_report_use_the_selected_context(self):
        for selected in ('diabolocom', '', None):
            with self.subTest(selected=selected), tempfile.TemporaryDirectory() as directory:
                environment = {} if selected is None else {'DOCKER_CONTEXT': selected}
                with patch.dict(os.environ, environment, clear=True):
                    module = runpy.run_path(str(Path(__file__).with_name('database_restore.py')))
                with patch.object(db.subprocess, 'check_output', return_value=b'fixture-commit'):
                    probe = module['Probe']('postgresql', Path(directory) / 'proof')
                expected = selected or 'remote-dev-container'
                with patch.object(probe, 'command') as command:
                    probe.sql('owned-database', 'SELECT 1')
                self.assertEqual(command.call_args.args[1][:3], ['docker', '--context', expected])
                self.assertEqual(probe.report['context'], expected)

    def test_source_teardown_keeps_the_selected_context(self):
        with tempfile.TemporaryDirectory() as directory:
            probe = object.__new__(ClusterRestoreProbe)
            probe.root = Path(directory); probe.report = {'checks': {}}
            probe.source_envs = {'E2E_KIND_CLUSTER_NAME': 'ptah-e2e-owned-restore'}
            probe.source_cluster = {'nodeNames': ['node-' + str(i) for i in range(4)]}
            probe.archived_workloads = {}; probe.watches = {}
            containers = '\n'.join(probe.source_cluster['nodeNames'] + ['load-balancer']).encode()
            responses = [subprocess.CompletedProcess([], 0, containers),
                         subprocess.CompletedProcess([], 0), subprocess.CompletedProcess([], 0, b'')]
            with patch.object(db, 'DOCKER_CONTEXT', 'diabolocom'), patch.object(
                    db, 'DOCKER', ['docker', '--context', 'diabolocom']), patch.object(
                    probe, 'barrier', return_value={'jobs': '', 'pods': ''}), patch.object(
                    OperatorProbe, 'watched_objects', return_value=[]), patch.object(
                    probe, 'command', side_effect=responses) as command:
                probe.lose_namespace()
            argv = [call.args[1] for call in command.call_args_list]
            self.assertEqual(argv[0][:3], ['docker', '--context', 'diabolocom'])
            self.assertIn('DOCKER_CONTEXT=diabolocom', argv[1])
            self.assertEqual(argv[2][:3], ['docker', '--context', 'diabolocom'])

    def test_replacement_bootstrap_keeps_the_selected_context(self):
        with tempfile.TemporaryDirectory() as directory:
            probe = object.__new__(ClusterRestoreProbe)
            probe.timing = 'idle'
            probe.root = Path(directory); probe.report = {'steps': []}
            probe.source_cluster = {'kubernetesVersion': '1.37.0'}
            probe.prefix = 'ptah-020-restore-owned'
            probe.target_environment = probe.root / 'target-environment'
            process = Mock(); process.wait.return_value = 1
            with patch.object(db, 'DOCKER_CONTEXT', 'diabolocom'), patch.dict(
                    os.environ, {'DOCKER_CONTEXT': 'ambient-wrong-context'}), patch(
                    'cluster_restore.subprocess.Popen', return_value=process) as start:
                with self.assertRaisesRegex(RuntimeError, 'provisioning failed'):
                    probe.provision_target()
            self.assertEqual(start.call_args.kwargs['env']['DOCKER_CONTEXT'], 'diabolocom')

    def test_in_flight_replacement_never_starts_without_the_execution_fence(self):
        with tempfile.TemporaryDirectory() as directory:
            probe = object.__new__(ClusterRestoreProbe)
            probe.root = Path(directory); probe.report = {'checks': {}}
            probe.timing = 'during-apply'
            with patch('cluster_restore.subprocess.Popen') as start:
                with self.assertRaisesRegex(RuntimeError, 'fenced before replacement'):
                    probe.provision_target()
            start.assert_not_called()

    def test_endpoint_mismatch_stops_before_any_resource_is_created(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            environment = root / 'environment'
            environment.write_text('E2E_DOCKER_ENDPOINT=ssh://recorded-host\n')
            with patch.object(db.subprocess, 'check_output', return_value=b'fixture-commit'), patch.object(
                    OperatorProbe, 'command', return_value=subprocess.CompletedProcess(
                        [], 0, b'ssh://different-host\n')) as command:
                with self.assertRaisesRegex(RuntimeError, 'different Docker daemons'):
                    OperatorProbe('postgresql', 'schema', environment, root / 'proof')
            self.assertEqual(command.call_count, 1)
            self.assertIn('inspect', command.call_args.args[1])


class ColdRestoreContractTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.reading = json.loads((Path(__file__).parent / 'testdata/recovery-webhooks.json').read_text())

    def setUp(self):
        self.namespace = self.reading['source']['operatorNamespace']
        self.controller = self.reading['source']['controllerName']
        # CRD specifications are compared as opaque full objects; the native
        # webhook documents exercise the installation-specific normalization.
        self.original = [{'kind': 'CustomResourceDefinition', 'name': name,
                          'uid': 'source-crd-' + str(i), 'spec': {'contract': i}}
                         for i, name in enumerate(sorted(OPERATOR_CRDS))] + copy.deepcopy(self.reading['webhooks'])

    def compare(self, values, namespace=None, controller=None):
        return ClusterRestoreProbe.comparable_contract(values, namespace or self.namespace, controller or self.controller)

    def test_only_declared_installation_identities_can_change(self):
        original = copy.deepcopy(self.original)
        moved = copy.deepcopy(original)
        namespace, controller = 'replacement-namespace', 'replacement-manager'
        old_user = 'system:serviceaccount:' + self.namespace + ':' + self.controller
        new_user = 'system:serviceaccount:' + namespace + ':' + controller
        self.assertEqual(sum(old_user in c['expression'] for r in original for w in r.get('webhooks', []) for c in w.get('matchConditions', [])), 1)
        for item in moved:
            item['uid'] = 'replacement-' + item['uid']
            for webhook in item.get('webhooks', []):
                webhook['clientConfig']['service'].update(namespace=namespace, name=controller + '-webhook')
                for condition in webhook.get('matchConditions', []):
                    condition['expression'] = condition['expression'].replace(old_user, new_user)
        self.assertEqual(self.compare(original), self.compare(moved, namespace, controller))
        self.assertEqual(original, self.original)

    def test_missing_contracts_or_invalid_certificates_are_refused(self):
        for defect in ('missing-crd', 'missing-results-crd', 'duplicate-crd', 'missing-webhook', 'empty-webhooks', 'missing-uid', 'wrong-service', 'invalid-ca'):
            with self.subTest(defect=defect):
                changed = copy.deepcopy(self.original)
                if defect == 'missing-crd':
                    changed.pop(0)
                elif defect == 'missing-results-crd':
                    changed = [r for r in changed if r['name'] != 'ptahresultrecords.operator.ptah.run']
                elif defect == 'duplicate-crd':
                    changed[1] = copy.deepcopy(changed[0])
                elif defect == 'missing-webhook':
                    changed.pop()
                elif defect == 'empty-webhooks':
                    changed[-1]['webhooks'] = []
                elif defect == 'missing-uid':
                    changed[0].pop('uid')
                elif defect == 'wrong-service':
                    changed[-1]['webhooks'][0]['clientConfig']['service']['name'] = 'other-service'
                else:
                    changed[-1]['webhooks'][0]['clientConfig']['caBundle'] = 'bm90LWEtY2VydGlmaWNhdGU='
                with self.assertRaises(RuntimeError):
                    self.compare(changed)

    def test_contract_changes_cannot_hide_behind_new_installation_names(self):
        expected = self.compare(self.original)
        for defect in ('crd-schema', 'failure-policy', 'rules', 'match-condition'):
            with self.subTest(defect=defect):
                changed = copy.deepcopy(self.original)
                if defect == 'crd-schema':
                    changed[0]['spec']['contract'] = 'different'
                elif defect == 'failure-policy':
                    changed[-1]['webhooks'][0]['failurePolicy'] = 'Ignore'
                elif defect == 'rules':
                    changed[-1]['webhooks'][0]['rules'] = []
                else:
                    changed[-1]['webhooks'][0]['matchConditions'] = [{'name': 'bypass', 'expression': 'false'}]
                self.assertNotEqual(self.compare(changed), expected)


class ColdRestoreCleanupTest(unittest.TestCase):
    def test_pre_backup_failure_is_terminal_and_stops_owned_tunnel(self):
        with tempfile.TemporaryDirectory() as directory:
            probe = object.__new__(ClusterRestoreProbe)
            probe.root = Path(directory)
            probe.source_envs = {}
            probe.source_environment = probe.root / 'absent-source'
            probe.target_environment = probe.root / 'absent-target'
            process = Mock()
            process.poll.return_value = None
            probe.api_tunnels = [process]
            probe.report = {'status': 'RUNNING'}
            with patch.object(probe, 'ensure_api_connection', side_effect=RuntimeError('connection lost')):
                with self.assertRaisesRegex(RuntimeError, 'connection lost'):
                    probe.run()
            process.terminate.assert_called_once()
            process.wait.assert_called_once_with(timeout=10)
            report = json.loads((probe.root / 'result.json').read_text())
            self.assertEqual(report['status'], 'FAIL')
            self.assertIn('connection lost', report['failure'])

    def test_durable_mode_requires_effective_helm_readback(self):
        environment = {'E2E_KUBECONFIG': '/target/kubeconfig', 'E2E_OPERATOR_NAMESPACE': 'target-system',
                       'E2E_HELM_RELEASE': 'target-release', 'E2E_CHART_PACKAGE': '/target/operator.tgz'}
        for enabled in (True, False):
            with self.subTest(enabled=enabled), tempfile.TemporaryDirectory() as directory:
                probe = object.__new__(ClusterRestoreProbe)
                probe.root = Path(directory); probe.report = {'checks': {}}
                responses = [subprocess.CompletedProcess(['helm'], 0), subprocess.CompletedProcess(
                    ['helm'], 0, stdout=json.dumps({'resultDelivery': {'enabled': enabled}}).encode())]
                with patch.object(probe, 'command', side_effect=responses) as command:
                    if enabled:
                        probe.enable_result_delivery(environment)
                    else:
                        with self.assertRaisesRegex(RuntimeError, 'uses durable delivery'):
                            probe.enable_result_delivery(environment)
                argv = command.call_args_list[0].args[1]
                self.assertIn('/target/operator.tgz', argv)
                self.assertEqual(argv[:5], ['helm', '--kubeconfig', '/target/kubeconfig', '--namespace', 'target-system'])
                self.assertIn('resultDelivery.enabled=true', argv)

    def test_replacement_cleanup_failure_still_cleans_the_source(self):
        for failure in (subprocess.TimeoutExpired('lab', 180), OSError('unavailable'),
                        subprocess.CompletedProcess(['lab'], 1)):
            with self.subTest(failure=type(failure).__name__), tempfile.TemporaryDirectory() as directory:
                probe = object.__new__(ClusterRestoreProbe)
                probe.root = Path(directory)
                probe.target_environment = probe.root / 'target-environment'
                probe.source_environment = probe.root / 'source-environment'
                probe.target_environment.touch()
                probe.source_environment.touch()
                probe.source_envs = {}
                probe.report = {'status': 'PASS', 'cleanupSucceeded': True}
                with patch.object(probe, 'ensure_api_connection'), patch.object(OperatorProbe, 'run'), patch('cluster_restore.subprocess.run',
                        side_effect=[failure, subprocess.CompletedProcess(['lab'], 0)]) as cleanup:
                    with self.assertRaisesRegex(RuntimeError, 'Owned cluster cleanup failed'):
                        probe.run()
                self.assertEqual(cleanup.call_count, 2)
                self.assertEqual([call.kwargs['env']['LAB_ENVIRONMENT'] for call in cleanup.call_args_list],
                                 [str(probe.target_environment), str(probe.source_environment)])
                persisted = json.loads((probe.root / 'result.json').read_text())
                self.assertEqual(persisted['status'], 'FAIL')
                self.assertFalse(persisted['cleanupSucceeded'])
                self.assertNotEqual(persisted['clusterCleanup'][0]['exitCode'], 0)
                self.assertEqual(persisted['clusterCleanup'][1], {'cluster': 'source', 'exitCode': 0})


class ColdRestoreTunnelTest(unittest.TestCase):
    def test_reconnect_uses_exact_recorded_forward_and_owns_only_new_process(self):
        with tempfile.TemporaryDirectory() as directory:
            probe = object.__new__(ClusterRestoreProbe)
            probe.root = Path(directory); probe.report = {}; probe.api_tunnels = []
            environment = {'E2E_KUBECONFIG': '/recorded/config',
                           'E2E_DOCKER_ENDPOINT': 'ssh://buster@remote-dev:2222',
                           'E2E_TUNNEL_FORWARD': '127.0.0.1:31817:127.0.0.1:31817'}
            config = {'clusters': [{'cluster': {'server': 'https://127.0.0.1:31817'}}]}
            process = Mock(); process.poll.return_value = None
            with patch.object(probe, 'api_ready', side_effect=[False, True, True]), patch.object(
                    probe, 'command', return_value=subprocess.CompletedProcess([], 0, json.dumps(config).encode())), patch(
                    'cluster_restore.subprocess.Popen', return_value=process) as start:
                probe.ensure_api_connection(environment, 'source')
                probe.ensure_api_connection(environment, 'source')
            start.assert_called_once()
            argv = start.call_args.args[0]
            self.assertEqual(argv[argv.index('-L') + 1], environment['E2E_TUNNEL_FORWARD'])
            self.assertEqual(argv[-4:], ['-p', '2222', '--', 'buster@remote-dev'])
            self.assertEqual(probe.api_tunnels, [process])
            self.assertEqual(probe.report['reopenedAPITunnels'], ['source'])
            probe.close_api_tunnels()
            process.terminate.assert_called_once()

    def test_mismatched_forward_cannot_connect_to_another_cluster(self):
        probe = object.__new__(ClusterRestoreProbe)
        environment = {'E2E_KUBECONFIG': '/recorded/config', 'E2E_DOCKER_ENDPOINT': 'ssh://buster@remote-dev',
                       'E2E_TUNNEL_FORWARD': '127.0.0.1:31818:127.0.0.1:31818'}
        config = {'clusters': [{'cluster': {'server': 'https://127.0.0.1:31817'}}]}
        with patch.object(probe, 'api_ready', return_value=False), patch.object(
                probe, 'command', return_value=subprocess.CompletedProcess([], 0, json.dumps(config).encode())), patch(
                'cluster_restore.subprocess.Popen') as start:
            with self.assertRaisesRegex(RuntimeError, 'recorded loopback'):
                probe.ensure_api_connection(environment, 'source')
            start.assert_not_called()


if __name__ == '__main__':
    unittest.main()
