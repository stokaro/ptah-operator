import copy
import json
from pathlib import Path
import subprocess
import unittest
from unittest.mock import Mock, patch

from operator_restore import OperatorProbe
from restore_during_apply import ApplyLoss, executor_node_address, fixture_waiter, running_apply, terminal_apply_pods


class ApplyLossBoundaryTest(unittest.TestCase):
    def setUp(self):
        self.reading = json.loads((Path(__file__).parent / 'testdata/workload-history.json').read_text())
        self.job, self.pod = copy.deepcopy((self.reading['jobs'][0], self.reading['pods'][0]))
        meta = self.job['metadata']
        self.resource = {'metadata': {'uid': meta['ownerReferences'][0]['uid'], 'namespace': meta['namespace']},
                         'status': {'activeOperation': {'type': 'Apply', 'jobUID': meta['uid'], 'jobName': meta['name'],
                             'id': meta['annotations']['operator.ptah.run/operation-id']}}}

    def live_pod(self):
        pod = copy.deepcopy(self.pod)
        pod['status'].update(phase='Running', podIP='10.244.1.5')
        pod['status']['containerStatuses'][0]['state'] = {'running': {'startedAt': '2026-10-08T00:00:00Z'}}
        return pod

    def test_finished_native_pod_cannot_supply_an_in_flight_boundary(self):
        self.assertFalse(running_apply(self.resource, self.job, self.pod))
        self.assertTrue(running_apply(self.resource, self.job, self.live_pod()))

    def test_live_boundary_refuses_another_claim_or_executor(self):
        for defect in ('claim', 'job', 'owner', 'namespace', 'deleting', 'restarted', 'missing-status', 'stopped'):
            with self.subTest(defect=defect):
                resource, job, pod = copy.deepcopy((self.resource, self.job, self.live_pod()))
                if defect == 'claim': resource['status']['activeOperation']['id'] = 'another-operation'
                if defect == 'job': job['metadata']['uid'] = 'another-job'
                if defect == 'owner': pod['metadata']['ownerReferences'][0]['uid'] = 'another-job'
                if defect == 'namespace': pod['metadata']['namespace'] = 'another-namespace'
                if defect == 'deleting': pod['metadata']['deletionTimestamp'] = '2026-10-08T00:00:00Z'
                if defect == 'restarted': pod['status']['containerStatuses'][0]['restartCount'] = 1
                if defect == 'missing-status': pod['status']['containerStatuses'] = []
                if defect == 'stopped': pod['status']['containerStatuses'][0]['state'] = self.pod['status']['containerStatuses'][0]['state']
                self.assertFalse(running_apply(resource, job, pod))

    def test_server_wait_must_be_the_fixture_ddl_from_the_exact_pod(self):
        for engine in ('postgresql', 'mysql'):
            row = {'session': 42, 'query': 'ALTER TABLE "public"."recovery_canary" ADD COLUMN "recovered" INTEGER NOT NULL DEFAULT 1;',
                   'client': '10.244.1.5', 'blockers': [17], 'state': 'Waiting for table metadata lock'}
            self.assertTrue(fixture_waiter(row, '10.244.1.5', 17, engine))
            for defect in ('client', 'read-only', 'table', 'column', 'second-statement', 'holder', 'missing-session', 'not-blocked'):
                with self.subTest(engine=engine, defect=defect):
                    changed = copy.deepcopy(row)
                    if defect == 'client': changed['client'] = '10.244.1.6'
                    if defect == 'read-only': changed['query'] = 'SELECT * FROM recovery_canary'
                    if defect == 'table': changed['query'] = changed['query'].replace('recovery_canary', 'other_table')
                    if defect == 'column': changed['query'] = changed['query'].replace('recovered', 'other_column')
                    if defect == 'second-statement': changed['query'] += ' SELECT 1'
                    if defect == 'holder': changed['session'] = 17
                    if defect == 'missing-session': changed.pop('session')
                    if defect == 'not-blocked': changed.update(blockers=[], state='executing')
                    self.assertFalse(fixture_waiter(changed, '10.244.1.5', 17, engine))

    def test_stop_proof_requires_every_native_container_and_each_original_job(self):
        jobs = {self.job['metadata']['uid']}
        self.assertTrue(terminal_apply_pods([self.pod], jobs))
        for defect in ('empty', 'duplicate-pod', 'another-job', 'missing-init', 'missing-main', 'restart', 'running-init', 'missing-finish', 'exit-code'):
            with self.subTest(defect=defect):
                pod = copy.deepcopy(self.pod); pods = [pod]
                if defect == 'empty': pods = []
                if defect == 'duplicate-pod': pods.append(copy.deepcopy(pod))
                if defect == 'another-job': pod['metadata']['ownerReferences'][0]['uid'] = 'other-job'
                if defect == 'missing-init': pod['status']['initContainerStatuses'].pop()
                if defect == 'missing-main': pod['status']['containerStatuses'] = []
                if defect == 'restart': pod['status']['containerStatuses'][0]['restartCount'] = 1
                if defect == 'running-init': pod['status']['initContainerStatuses'][0]['state'] = {'running': {}}
                if defect == 'missing-finish': pod['status']['containerStatuses'][0]['state']['terminated'].pop('finishedAt')
                if defect == 'exit-code': pod['status']['containerStatuses'][0]['state']['terminated']['exitCode'] = 1
                self.assertFalse(terminal_apply_pods(pods, jobs))
        failed = copy.deepcopy(self.pod)
        failed['status']['phase'] = 'Failed'
        failed['status']['containerStatuses'][0]['state']['terminated']['exitCode'] = 1
        self.assertFalse(terminal_apply_pods([failed], jobs))
        self.assertTrue(terminal_apply_pods([failed], jobs, require_success=False))
        failed['status']['initContainerStatuses'].pop()
        self.assertFalse(terminal_apply_pods([failed], jobs, require_success=False))

    def test_external_database_source_can_be_the_exact_executor_node(self):
        pod = {'spec': {'nodeName': 'worker'}, 'status': {'podIP': '10.244.1.5', 'hostIP': '172.19.0.4'}}
        node = {'metadata': {'name': 'worker', 'uid': 'node-uid'},
                'status': {'addresses': [{'type': 'InternalIP', 'address': '172.19.0.4'}]}}
        self.assertEqual(executor_node_address(pod, node), '172.19.0.4')
        row = {'session': 42, 'query': 'ALTER TABLE recovery_canary ADD COLUMN recovered INTEGER;',
               'client': '172.19.0.4', 'blockers': [17], 'state': 'Waiting for table metadata lock'}
        for engine in ('postgresql', 'mysql'):
            self.assertFalse(fixture_waiter(row, pod['status']['podIP'], 17, engine))
            self.assertTrue(fixture_waiter(row, pod['status']['podIP'], 17, engine, executor_node_address(pod, node)))
            self.assertFalse(fixture_waiter(row, pod['status']['podIP'], 17, engine, '172.19.0.5'))
        for defect in ('different-node', 'missing-uid', 'different-address', 'external-address', 'unscheduled'):
            with self.subTest(defect=defect):
                wrong_pod, wrong_node = copy.deepcopy((pod, node))
                if defect == 'different-node': wrong_node['metadata']['name'] = 'another-worker'
                if defect == 'missing-uid': wrong_node['metadata'].pop('uid')
                if defect == 'different-address': wrong_pod['status']['hostIP'] = '172.19.0.5'
                if defect == 'external-address': wrong_node['status']['addresses'][0]['type'] = 'ExternalIP'
                if defect == 'unscheduled': wrong_pod['spec'].pop('nodeName')
                with self.assertRaises(RuntimeError): executor_node_address(wrong_pod, wrong_node)

    def test_existing_namespace_rebuild_cannot_claim_during_apply_coverage(self):
        for loss in ('operator', 'combined'):
            with self.subTest(loss=loss), self.assertRaisesRegex(ValueError, 'database loss only'):
                OperatorProbe('postgresql', 'migration', None, None, loss, 'during-apply')


class ApplyLossRecoveryTest(unittest.TestCase):
    @staticmethod
    def checked(name, condition):
        if not condition:
            raise RuntimeError(name)

    def test_database_fence_cannot_address_an_unowned_or_replaced_container(self):
        for defect in ('none', 'unowned', 'label', 'replacement'):
            with self.subTest(defect=defect):
                probe = Mock(); probe.check.side_effect = self.checked
                probe.containers = ['original']; probe.prefix = 'owned-run'
                fault = ApplyLoss(probe, ['docker', '--context', 'fixture'])
                fault.source_id = 'a' * 64
                identity = ('b' if defect == 'replacement' else 'a') * 64
                label = 'another-run' if defect == 'label' else probe.prefix
                probe.command.return_value = subprocess.CompletedProcess([], 0, (identity + ' ' + label).encode())
                if defect == 'none':
                    fault.bind_source('original')
                    self.assertEqual(fault.source_id, identity)
                else:
                    with self.assertRaises(RuntimeError):
                        fault.bind_source('unowned' if defect == 'unowned' else 'original')
                probe.sql.assert_not_called()

    def test_replacement_requires_both_node_loss_and_a_later_database_fence(self):
        original = {'sourceDestroyedAt': '2026-10-08T18:00:00+00:00',
            'checks': {'the original control plane and workload nodes were destroyed': True},
            'inFlightLoss': {'sourceContainerID': 'original', 'databaseFence': {'containerID': 'original',
                'writer': 'operator_writer', 'loginRefused': True, 'remainingWriterSessions': [],
                'endedAt': '2026-10-08T18:00:01+00:00'}}}
        self.assertTrue(ApplyLoss.replacement_is_fenced(original))
        for defect in ('nodes', 'source', 'login', 'writer', 'sessions', 'missing-sessions', 'old-time', 'naive-time'):
            with self.subTest(defect=defect):
                report = copy.deepcopy(original); fence = report['inFlightLoss']['databaseFence']
                if defect == 'nodes': report['checks'].clear()
                if defect == 'source': fence['containerID'] = 'replacement'
                if defect == 'login': fence['loginRefused'] = False
                if defect == 'writer': fence['writer'] = 'another-account'
                if defect == 'sessions': fence['remainingWriterSessions'] = [{'session': 7}]
                if defect == 'missing-sessions': fence.pop('remainingWriterSessions')
                if defect == 'old-time': fence['endedAt'] = '2026-10-08T17:59:59+00:00'
                if defect == 'naive-time': fence['endedAt'] = '2026-10-08T18:00:01'
                self.assertFalse(ApplyLoss.replacement_is_fenced(report))

    def test_writer_access_stays_closed_until_safe_replacement_state(self):
        for loss in ('operator', 'combined'):
            for defect in ('none', 'not-fenced', 'old-uid', 'automatic', 'running', 'active-operation'):
                with self.subTest(loss=loss, defect=defect):
                    probe = Mock(); probe.check.side_effect = self.checked
                    probe.loss = loss; probe.engine = 'postgresql'; probe.report = {'inFlightLoss': {}}
                    resource = {'metadata': {'uid': 'new'}, 'spec': {'suspend': True, 'policy': {'apply': 'OnApproval'}}}
                    if defect == 'old-uid': resource['metadata']['uid'] = 'old'
                    if defect == 'automatic': resource['spec']['policy']['apply'] = 'Always'
                    if defect == 'running': resource['spec']['suspend'] = False
                    if defect == 'active-operation': resource['status'] = {'activeOperation': {'type': 'Apply'}}
                    probe.read.return_value = resource
                    fault = ApplyLoss(probe, [])
                    fault.resource = {'metadata': {'uid': 'old'}}; fault.fenced = defect != 'not-fenced'
                    with patch.object(fault, 'bind_source'), patch.object(fault, 'writer_connection',
                            return_value=subprocess.CompletedProcess([], 0, b'1\n')) as login:
                        if defect == 'none':
                            fault.allow_recovered_writer('database', 'fixture-password')
                            self.assertTrue(probe.report['inFlightLoss']['writerAccessRestored'])
                            self.assertEqual(probe.sql.call_count, 1 if loss == 'operator' else 0)
                        else:
                            with self.assertRaises(RuntimeError): fault.allow_recovered_writer('database', 'fixture-password')
                            probe.sql.assert_not_called(); login.assert_not_called()

    def test_replacement_pod_stops_recovery_before_spec_changes(self):
        probe = Mock()
        probe.check.side_effect = self.checked
        probe.stopped_apply_pods.return_value = [{'metadata': {'uid': 'replacement'}}]
        fault = ApplyLoss(probe, [])
        fault.job = {'metadata': {'uid': 'job'}}; fault.pod = {'metadata': {'uid': 'original'}}
        with self.assertRaisesRegex(RuntimeError, 'without replacement'):
            fault.stopped('artifact', {'ociRef': 'original'})
        probe.patch.assert_not_called()

    def test_acknowledgment_requires_the_exact_original_operation_and_consumption(self):
        for defect in ('none', 'operation', 'reference', 'unresolved', 'history-read'):
            with self.subTest(defect=defect):
                probe = Mock()
                probe.check.side_effect = self.checked
                probe.report = {'inFlightLoss': {'acknowledgmentUID': 'ack-uid'}}
                fault = ApplyLoss(probe, []); fault.operation = 'original-operation'
                resource = {'status': {'resolvedRun': {'operationID': fault.operation, 'resolution': 'Acknowledged',
                                                      'acknowledgmentRef': {'uid': 'ack-uid'}}}}
                if defect == 'operation': resource['status']['resolvedRun']['operationID'] = 'old-operation'
                if defect == 'reference': resource['status']['resolvedRun']['acknowledgmentRef']['uid'] = 'other-ack'
                if defect == 'unresolved': resource['status']['unresolvedRun'] = {'operationID': fault.operation}
                if defect == 'history-read': resource['status']['resolvedRun']['resolution'] = 'HistoryRead'
                if defect == 'none':
                    fault.verify_resolution(resource)
                    self.assertEqual(probe.report['inFlightLoss']['resolution'], resource['status']['resolvedRun'])
                else:
                    with self.assertRaises(RuntimeError): fault.verify_resolution(resource)


if __name__ == '__main__':
    unittest.main()
