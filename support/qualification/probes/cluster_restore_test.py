import copy
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from cluster_restore import ClusterRestoreProbe
from operator_restore import OPERATOR_CRDS, OperatorProbe


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
                probe.report = {'status': 'PASS', 'cleanupSucceeded': True}
                with patch.object(OperatorProbe, 'run'), patch('cluster_restore.subprocess.run',
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


if __name__ == '__main__':
    unittest.main()
