import base64
import copy
import json
import unittest
from result_retention_evidence import verify_abandoned
import result_retention_evidence_test as retention_tests


class AbandonedEvidenceTests(unittest.TestCase):
    def fixture(self):
        before, after, markers, audits = retention_tests.RetentionEvidenceTests().fixture()
        binding = {'namespace': 'work', 'uid': 'resource', 'operation': 'resolve',
                   'operationID': 'operation', 'jobUID': 'retired-job', 'podUID': 'original-pod'}
        before['records'] = [r for r in before['records'] if r['type'] not in ('chunk', 'complete')]
        before['eligibleNames'] = ['credential', 'intent', 'retired']
        before['deadlines'] = {k: v for k, v in before['deadlines'].items() if k in before['eligibleNames']}
        for r in before['records']:
            if 'binding' in r:
                r['binding'] = binding
        markers[0]['spec']['value']['binding'] = binding
        after['eligibleRecordsCollected'] = 3
        audits = [a for a in audits if a['objectRef']['name'] not in ('chunk', 'complete')]
        interrupted = {'intent': {'metadata': {'name': 'intent', 'uid': 'intent-uid'},
                                  'spec': {'data': base64.b64encode(json.dumps({'binding': binding, 'chunks': [{'size': 1}]}).encode()).decode()}},
                       'resource': {'metadata': {'uid': 'resource'}, 'status': {'activeOperation': {'id': 'operation'}}},
                       'job': {'metadata': {'uid': 'retired-job'}}, 'pod': {'metadata': {'uid': 'original-pod'}},
                       'records': copy.deepcopy(before['records'][:2]),
                       'quota': {'metadata': {'name': 'hold-partial'}, 'status': {k: {'count/ptahresultrecords.operator.ptah.run': '2'} for k in ('hard', 'used')}}}
        refusal = {'verb': 'create', 'stage': 'ResponseComplete', 'responseStatus': {'code': 403, 'message': 'exceeded quota: hold-partial, requested: one chunk'},
                   'objectRef': {'namespace': 'work', 'resource': 'ptahresultrecords', 'name': 'intent-000'}, 'auditID': 'refused-chunk'}
        return interrupted, before, after, markers, audits, [refusal]

    def test_accepts_abandoned_intent_with_pinned_control(self):
        report = verify_abandoned(*self.fixture())
        self.assertEqual(report['eligibleRecords'], 3)
        self.assertEqual(report['abandonedPublication']['recordsBeforeRetirement'], 2)

    def test_refuses_wrong_fault_or_collection(self):
        for fault in ('complete', 'wrong Pod', 'changed intent', 'missing cohort', 'quota available',
                      'missing refusal', 'wrong refused namespace', 'early delete', 'lost pin'):
            with self.subTest(fault=fault):
                args = list(self.fixture())
                interrupted, before, after, markers, audits, refusals = args
                if fault == 'complete': interrupted['records'].append({'type': 'complete'})
                if fault == 'wrong Pod': interrupted['pod']['metadata']['uid'] = 'replacement'
                if fault == 'changed intent': interrupted['intent']['metadata']['uid'] = 'replacement'
                if fault == 'missing cohort': before['eligibleNames'].remove('intent')
                if fault == 'quota available': interrupted['quota']['status']['hard']['count/ptahresultrecords.operator.ptah.run'] = '3'
                if fault == 'missing refusal': refusals.clear()
                if fault == 'wrong refused namespace': refusals[0]['objectRef']['namespace'] = 'foreign'
                if fault == 'early delete': audits[0]['requestReceivedTimestamp'] = '2026-10-02T09:59:59Z'
                if fault == 'lost pin': after['remainingPinned'] = []
                with self.assertRaises(ValueError):
                    verify_abandoned(*args)
