import copy
import json
from pathlib import Path
import unittest

from operator_restore import OperatorProbe


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


if __name__ == '__main__':
    unittest.main()
