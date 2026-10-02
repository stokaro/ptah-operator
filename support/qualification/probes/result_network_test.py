import copy
import json
import pathlib
import shutil
import tempfile
import unittest
from result_network import OPERATIONS, EXPECTED_SCHEMA, objects_from_output, selected, verify, verify_archive


class NetworkEvidenceTests(unittest.TestCase):
    def test_reads_kubectl_multi_document_output(self):
        first = '{"kind":"Service"}'
        second = '{"kind":"NetworkPolicy"}'
        self.assertEqual(objects_from_output('\n' + first + '\n' + second),
                         [{'kind': 'Service'}, {'kind': 'NetworkPolicy'}])
        self.assertEqual(objects_from_output('{"kind":"List","items":[' + second + ']}'),
                         [{'kind': 'NetworkPolicy'}])
        for value in ('', first + '\ninvalid'):
            with self.assertRaises(ValueError):
                objects_from_output(value)

    def fixture(self):
        value = {'nodes': [{'containerLogMaxSize': '10Mi'} for _ in range(4)], 'receiverUIDs': ['one', 'two'],
                 'beforeIngress': 'open open open', 'afterIngress': 'closed closed closed', 'afterRemoval': 'open open open',
                 'policiesRemoved': True, 'probesRemoved': True, 'workflows': []}
        for family, operations in OPERATIONS.items():
            for engine in ('PostgreSQL', 'MySQL'):
                value['workflows'].append({'family': family, 'engine': engine, 'publications': dict.fromkeys(operations, {}),
                                          'database': copy.deepcopy(EXPECTED_SCHEMA) if family == 'PtahSchema' else 1,
                                          'applyJobs': 1, 'resultPolicySelectedEveryJob': True, 'ingressSelectedReceivers': True,
                                          'generation': 1, 'conditions': [{'type': 'Ready', 'status': 'True', 'observedGeneration': 1,
                                                                         'reason': 'InSync' if family == 'PtahSchema' else 'HistoryMatched'}]})
        return value

    def test_accepts_both_families_and_native_engines(self):
        self.assertEqual(verify(self.fixture())['familyOperationEnginePairs'], 18)

    def test_refuses_weak_or_incomplete_network_proof(self):
        for fault in ('closed baseline', 'open backend', 'open Service', 'no restoration', 'missing engine',
                      'missing operation', 'wrong database', 'unselected workload', 'stale generation', 'extra Apply', 'left policy'):
            with self.subTest(fault=fault):
                e = self.fixture()
                if fault == 'closed baseline': e['beforeIngress'] = 'closed closed closed'
                if fault == 'open backend': e['afterIngress'] = 'open closed closed'
                if fault == 'open Service': e['afterIngress'] = 'closed closed open'
                if fault == 'no restoration': e['afterRemoval'] = 'closed closed closed'
                if fault == 'missing engine': e['workflows'].pop()
                if fault == 'missing operation': e['workflows'][0]['publications'].pop('apply')
                if fault == 'wrong database': e['workflows'][2]['database'] = 0
                if fault == 'unselected workload': e['workflows'][0]['resultPolicySelectedEveryJob'] = False
                if fault == 'stale generation': e['workflows'][0]['generation'] = 2
                if fault == 'extra Apply': e['workflows'][0]['applyJobs'] = 2
                if fault == 'left policy': e['policiesRemoved'] = False
                with self.assertRaises(ValueError):
                    verify(e)

    def test_selectors_require_both_manager_and_family(self):
        selector = {'matchLabels': {'manager': 'operator'}, 'matchExpressions': [{'key': 'family', 'operator': 'In', 'values': ['schema', 'migration']}]}
        self.assertTrue(selected({'manager': 'operator', 'family': 'schema'}, selector))
        self.assertTrue(selected({'manager': 'operator', 'family': 'migration'}, selector))
        self.assertFalse(selected({'manager': 'probe', 'family': 'schema'}, selector))
        self.assertFalse(selected({'manager': 'operator'}, selector))

    def test_replays_installed_network_evidence(self):
        root = pathlib.Path(__file__).resolve().parents[1] / 'evidence/result-network-2026-10-02'
        self.assertEqual(verify_archive(root)['familyOperationEnginePairs'], 18)

    def test_refuses_changed_api_documents(self):
        original = pathlib.Path(__file__).resolve().parents[1] / 'evidence/result-network-2026-10-02'
        for fault in ('receiver selector', 'probe reading', 'resource UID', 'Job selector', 'chunk bytes'):
            with self.subTest(fault=fault), tempfile.TemporaryDirectory() as temp:
                root = pathlib.Path(temp) / 'evidence'
                shutil.copytree(original, root)
                name = 'ptah-result-network-pg-schema-workflow.json'
                if fault == 'receiver selector': name = 'receiver-policy.json'
                if fault == 'probe reading': name = 'network-after-removal.json'
                data = json.loads((root / name).read_text())
                if fault == 'receiver selector': data['spec']['podSelector'] = {'matchLabels': {'app': 'foreign'}}
                if fault == 'probe reading': data['reading'] = ['closed'] * 3
                if fault == 'resource UID': data['resource']['metadata']['uid'] = 'replacement'
                if fault == 'Job selector': data['jobs'][0]['spec']['template']['metadata']['labels'] = {}
                if fault == 'chunk bytes':
                    chunk = next(r for r in data['publications'].values() if r['spec']['type'] == 'chunk')
                    chunk['spec']['data'] = 'eA=='
                (root / name).write_text(json.dumps(data))
                with self.assertRaises(ValueError):
                    verify_archive(root)
