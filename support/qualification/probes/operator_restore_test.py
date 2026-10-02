import copy
import base64
import json
from pathlib import Path
import unittest
import tempfile

from operator_restore import OperatorProbe


class ResultBackupInventoryTest(unittest.TestCase):
    def test_requires_keys_and_original_journal_bindings(self):
        trust = {'metadata': {'uid': 'trust'}, 'data': {k: 'eA==' for k in
            ('tls.crt', 'tls.key', 'ca.crt', 'client-ca.crt', 'client-ca.key', 'client-trust.crt')}}
        policy = {'metadata': {'uid': 'policy'}, 'data': {'enrollment.json': '{}'}}
        journal = {'data': {'rotation.json': base64.b64encode(json.dumps(
            {'projectionUID': 'trust', 'policyUID': 'policy'}).encode()).decode()}}
        OperatorProbe.validate_result_material(trust, journal, policy)
        for fault in ('missing key', 'empty journal', 'missing policy', 'replaced trust', 'replaced policy'):
            with self.subTest(fault=fault):
                t, j, p = copy.deepcopy((trust, journal, policy))
                if fault == 'missing key': t['data'].pop('client-ca.key')
                if fault == 'empty journal': j['data'] = {}
                if fault == 'missing policy': p['data'] = {}
                if fault == 'replaced trust': t['metadata']['uid'] = 'replacement'
                if fault == 'replaced policy': p['metadata']['uid'] = 'replacement'
                with self.assertRaises(RuntimeError):
                    OperatorProbe.validate_result_material(t, j, p)


class RecoveredDeliveryTest(unittest.TestCase):
    def test_fresh_apply_requires_receipt_and_declared_authority(self):
        path = Path(__file__).resolve().parents[1] / 'evidence/result-network-2026-10-02/ptah-result-network-pg-migration-workflow.json'
        reading = json.loads(path.read_text())
        job_uid = reading['verification']['publications']['migration-apply']['jobUID']
        for replacement in (True, False):
            for defect in ('none', 'missing receipt', 'wrong resource', 'wrong authority'):
                with self.subTest(replacement=replacement, defect=defect), tempfile.TemporaryDirectory() as directory:
                    probe = object.__new__(OperatorProbe)
                    probe.root = Path(directory); probe.report = {'checks': {}}
                    if replacement: probe.report['targetCluster'] = {'uid': 'replacement-cluster'}
                    probe.family = 'migration'; probe.namespace = reading['resource']['metadata']['namespace']
                    probe.uid = reading['resource']['metadata']['uid']
                    records = copy.deepcopy(list(reading['publications'].values()))
                    if defect == 'missing receipt': records = [r for r in records if r['spec']['type'] != 'complete']
                    if defect == 'wrong resource': probe.uid = 'foreign-resource'
                    original = {'enabled': True, 'records': records}
                    restored = {'enabled': True, 'records': records}
                    for key in ('trust', 'journal', 'enrollmentPolicy'):
                        original[key] = {'metadata': {'uid': 'old-' + key}}
                        restored[key] = {'metadata': {'uid': ('new-' if replacement else 'old-') + key}}
                    if defect == 'wrong authority':
                        restored['trust']['metadata']['uid'] = original['trust']['metadata']['uid'] if replacement else 'changed'
                    probe.result_backup = lambda: restored
                    if defect == 'none':
                        probe.verify_recovered_results({'durableResults': original}, {job_uid})
                        self.assertEqual(len(probe.report['recoveredDelivery']['receipts']), 1)
                    else:
                        with self.assertRaises((RuntimeError, ValueError, KeyError)):
                            probe.verify_recovered_results({'durableResults': original}, {job_uid})


class WorkloadHistoryTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.reading = json.loads((Path(__file__).parent / 'testdata/workload-history.json').read_text())

    def setUp(self):
        self.probe = object.__new__(OperatorProbe)
        self.probe.family = 'migration'
        self.probe.kind = 'PtahMigration'
        for key in ('name', 'namespace', 'uid'):
            setattr(self.probe, key, self.reading['identity'][key])
        self.objects = {kind: copy.deepcopy(self.reading[kind]) for kind in ('jobs', 'pods')}
        self.probe.watched_objects = lambda resource, barrier: self.objects[resource]

    def history(self):
        return self.probe.watched_apply_jobs({'jobs': 'job-sentinel', 'pods': 'pod-sentinel'})

    def test_native_history_has_one_accounted_apply(self):
        self.assertEqual(len(self.objects['jobs']), 1)
        self.assertEqual(len(self.objects['pods']), 1)
        self.assertEqual(self.history(), {self.objects['jobs'][0]['metadata']['uid']})

    def test_missing_pod_cannot_pass_as_no_replay(self):
        self.objects['pods'] = []
        with self.assertRaisesRegex(RuntimeError, 'missing or replacement Pod'):
            self.history()

    def test_replacement_pod_is_detected_under_the_original_job(self):
        replacement = copy.deepcopy(self.objects['pods'][0])
        replacement['metadata']['uid'] = 'replacement-pod'
        self.objects['pods'].append(replacement)
        with self.assertRaisesRegex(RuntimeError, 'missing or replacement Pod'):
            self.history()

    def test_labels_cannot_hide_an_owned_apply(self):
        for value in (None, 'history'):
            with self.subTest(operation=value):
                self.objects['jobs'] = copy.deepcopy(self.reading['jobs'])
                labels = self.objects['jobs'][0]['metadata']['labels']
                if value is None:
                    labels.pop('operator.ptah.run/operation')
                else:
                    labels['operator.ptah.run/operation'] = value
                with self.assertRaises(RuntimeError):
                    self.history()

    def test_missing_ownership_does_not_hide_a_named_workload(self):
        for resource in ('jobs', 'pods'):
            with self.subTest(resource=resource):
                self.objects = {kind: copy.deepcopy(self.reading[kind]) for kind in ('jobs', 'pods')}
                self.objects[resource][0]['metadata']['ownerReferences'] = []
                with self.assertRaises(RuntimeError):
                    self.history()

    def test_a_later_correct_template_cannot_hide_a_changed_one(self):
        changed = copy.deepcopy(self.objects['jobs'][0])
        changed['spec']['template']['spec']['containers'][0]['image'] = 'different-executor'
        self.objects['jobs'].insert(0, changed)
        with self.assertRaisesRegex(RuntimeError, 'template changed'):
            self.history()

    def test_native_operations_require_the_exact_runner_command(self):
        readings = self.reading['operationPods']
        self.assertGreaterEqual(len(readings), 4)
        observed = set()
        for pod in readings:
            operation = self.probe.checked_operation(pod['metadata'], pod['spec'])
            observed.add(operation)
            for defect in ('command', 'duplicate-operation', 'inline-operation'):
                with self.subTest(operation=operation, defect=defect):
                    changed = copy.deepcopy(pod['spec'])
                    container = changed['containers'][0]
                    if defect == 'command':
                        container['command'] = ['/bin/sh']
                    elif defect == 'duplicate-operation':
                        container['args'] += ['--operation', 'migration-history']
                    else:
                        container['args'] += ['--operation=migration-history']
                    with self.assertRaises(RuntimeError):
                        self.probe.checked_operation(pod['metadata'], changed)
        self.assertEqual(observed, {'apply', 'history', 'resolve', 'verify'})


class RecoveryStateTest(unittest.TestCase):
    def test_native_ready_states_reject_stale_and_unsettled_status(self):
        readings = json.loads((Path(__file__).parent / 'testdata/recovery-states.json').read_text())
        self.assertEqual(set(readings), {'schema', 'migration'})
        for family, states in readings.items():
            with self.subTest(family=family):
                ready = states['initialApplied']
                self.assertTrue(OperatorProbe.is_settled(ready, 'InSync'))
                self.assertTrue(OperatorProbe.is_settled(states['resource'], 'Suspended'))
                self.assertFalse(OperatorProbe.is_settled(states['resource'], 'InSync'))
                for defect in ('generation', 'condition-generation', 'not-ready', 'active-operation', 'missing-generation'):
                    with self.subTest(defect=defect):
                        changed = copy.deepcopy(ready)
                        if defect == 'generation':
                            changed['metadata']['generation'] += 1
                        elif defect == 'missing-generation':
                            changed['metadata'].pop('generation')
                            changed['status'].pop('observedGeneration')
                        elif defect == 'active-operation':
                            changed['status']['activeOperation'] = {'id': 'still-running'}
                        else:
                            for condition in changed['status']['conditions']:
                                if condition['type'] == 'Ready':
                                    if defect == 'condition-generation':
                                        condition['observedGeneration'] -= 1
                                    else:
                                        condition['status'] = 'False'
                        self.assertFalse(OperatorProbe.is_settled(changed, 'InSync'))


class RebuildTest(unittest.TestCase):
    def test_rebuild_keeps_uncertain_evidence_without_reusing_authority(self):
        for kind in ('PtahSchema', 'PtahMigration'):
            with self.subTest(kind=kind):
                original = {'apiVersion': 'operator.ptah.run/v1alpha1', 'kind': kind,
                            'metadata': {'name': 'source', 'namespace': 'restore', 'uid': 'old-uid',
                                         'resourceVersion': '123', 'generation': 4, 'finalizers': ['protect-old-run'],
                                         'ownerReferences': [{'uid': 'old-owner'}],
                                         'annotations': {'operator.ptah.run/unresolved-run': 'original-uncertain-record'}},
                            'spec': {'suspend': False, 'policy': {'apply': 'Always', 'transactionMode': 'file'}},
                            'status': {'activeOperation': {'id': 'original-run'}}}
                saved = copy.deepcopy(original)
                rebuilt = OperatorProbe.rebuild_object(original)
                self.assertEqual(original, saved)
                self.assertNotIn('status', rebuilt)
                self.assertEqual(rebuilt['metadata'], {'name': 'source', 'namespace': 'restore',
                                 'annotations': {'operator.ptah.run/unresolved-run': 'original-uncertain-record'}})
                self.assertEqual(rebuilt['spec'], {'suspend': True, 'policy': {'apply': 'OnApproval', 'transactionMode': 'file'}})

    def test_rebuild_restores_secret_bytes_without_old_runtime_identity(self):
        original = {'apiVersion': 'v1', 'kind': 'Secret',
                    'metadata': {'name': 'target', 'namespace': 'restore', 'uid': 'old-secret'},
                    'type': 'Opaque', 'immutable': True, 'data': {'url': 'Zml4dHVyZQ=='}}
        rebuilt = OperatorProbe.rebuild_object(original)
        self.assertEqual(rebuilt['data'], original['data'])
        self.assertEqual(rebuilt['type'], 'Opaque')
        self.assertTrue(rebuilt['immutable'])
        self.assertNotIn('uid', rebuilt['metadata'])

    def test_both_original_and_rebuilt_workloads_remain_in_the_watch_proof(self):
        reading = json.loads((Path(__file__).parent / 'testdata/workload-history.json').read_text())
        probe = object.__new__(OperatorProbe)
        probe.family, probe.kind = 'migration', 'PtahMigration'
        probe.name, probe.namespace = reading['identity']['name'], reading['identity']['namespace']
        original_uid = reading['identity']['uid']
        probe.uid = 'rebuilt-uid'
        probe.resource_uids = {original_uid, probe.uid}
        jobs, pods = copy.deepcopy(reading['jobs']), copy.deepcopy(reading['pods'])
        new_job, new_pod = copy.deepcopy(jobs[0]), copy.deepcopy(pods[0])
        new_job['metadata']['uid'] = 'fresh-job'
        new_job['metadata']['name'] = 'fresh-job'
        new_job['metadata']['ownerReferences'][0]['uid'] = probe.uid
        new_pod['metadata']['uid'] = 'fresh-pod'
        new_pod['metadata']['ownerReferences'][0].update(uid='fresh-job', name='fresh-job')
        jobs.append(new_job); pods.append(new_pod)
        probe.watched_objects = lambda resource, boundary: jobs if resource == 'jobs' else pods
        self.assertEqual(probe.watched_apply_jobs({'jobs': '', 'pods': ''}),
                         {reading['jobs'][0]['metadata']['uid'], 'fresh-job'})
        replacement = copy.deepcopy(pods[0]); replacement['metadata']['uid'] = 'old-job-replacement-pod'
        pods.append(replacement)
        with self.assertRaisesRegex(RuntimeError, 'missing or replacement Pod'):
            probe.watched_apply_jobs({'jobs': '', 'pods': ''})


if __name__ == '__main__':
    unittest.main()
