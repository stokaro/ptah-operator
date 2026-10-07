import base64
import copy
import json
import pathlib
import unittest
from result_retention_evidence import verify_abandoned
from result_abandoned import prior_retirements_complete
import result_retention_evidence_test as retention_tests


class AbandonedEvidenceTests(unittest.TestCase):
    def test_quota_waits_for_all_preceding_retirement_records(self):
        rows = [{'type': kind, 'binding': {'jobUID': job}}
                for job in ('resolve', 'verify', 'observe') for kind in ('intent', 'credential')]
        self.assertFalse(prior_retirements_complete([]))
        self.assertFalse(prior_retirements_complete(rows))
        for job in ('resolve', 'verify'):
            rows.append({'type': 'retired', 'binding': {'jobUID': job}})
            self.assertFalse(prior_retirements_complete(rows))
        rows.append({'type': 'retired', 'binding': {'jobUID': 'observe'}})
        self.assertTrue(prior_retirements_complete(rows))
        rows.append({'type': 'credential', 'binding': {'jobUID': 'unfinished'}})
        self.assertFalse(prior_retirements_complete(rows))

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

    def pod_token_fixture(self):
        interrupted, before, after, markers, audits, refusals = self.fixture()
        before['authentication'] = after['authentication'] = 'pod-token'
        before['collectorIdentity'] = 'system:serviceaccount:operator:manager'
        before['secretOwners'] = {}
        for rows in (before['records'], interrupted['records'], after['remainingPinned']):
            for r in rows:
                if r['type'] == 'credential':
                    r['binding'] = {'jobUID': 'pinned-job' if r['name'] == 'pinned' else 'retired-job'}
                elif 'binding' in r:
                    r['binding']['operation'] = 'plan'
        for reading in (before, after):
            reading['credentialSecretCensus'] = {'expectedNames': ['credential', 'pinned'], 'presentNames': []}
        after['eligibleSecretProjectionsCollected'] = 0
        audits = [a for a in audits if a['objectRef']['resource'] != 'secrets']
        for event in audits:
            event['user']['username'] = before['collectorIdentity']
        manifest = json.loads(base64.b64decode(interrupted['intent']['spec']['data']))
        manifest['binding']['operation'] = 'plan'
        manifest.update(size=300000, chunks=[{'size': 300000}])
        interrupted['intent']['spec']['data'] = base64.b64encode(json.dumps(manifest).encode()).decode()
        interrupted['precedingRecords'] = [{'name': 'earlier-result', 'uid': 'earlier-result-uid'}]
        for field in ('hard', 'used'):
            interrupted['quota']['status'][field]['count/ptahresultrecords.operator.ptah.run'] = '3'
        return interrupted, before, after, markers, audits, refusals

    def test_accepts_large_plan_without_credential_secret_projections(self):
        report = verify_abandoned(*self.pod_token_fixture())
        self.assertEqual(report['eligibleRecords'], 3)
        self.assertEqual(report['collectedSecretProjections'], 0)

    def test_refuses_false_partial_plan_or_missing_token_census(self):
        for fault in ('inline', 'small', 'bad chunk sizes', 'missing census', 'Secret exists',
                      'missing credential binding', 'wrong authentication', 'unfilled quota',
                      'missing preceding records', 'repeated preceding record', 'early delete', 'lost pin',
                      'foreign collector'):
            with self.subTest(fault=fault):
                args = self.pod_token_fixture()
                interrupted, before, after, _, audits, _ = args
                manifest = json.loads(base64.b64decode(interrupted['intent']['spec']['data']))
                if fault == 'inline': manifest['inline'] = 'YQ=='
                if fault == 'small': manifest.update(size=1, chunks=[{'size': 1}])
                if fault == 'bad chunk sizes': manifest['chunks'][0]['size'] = 1
                if fault == 'missing census': before.pop('credentialSecretCensus')
                if fault == 'Secret exists': after['credentialSecretCensus']['presentNames'] = ['credential']
                if fault == 'missing credential binding': before['records'][0].pop('binding')
                if fault == 'wrong authentication': after['authentication'] = 'certificate'
                if fault == 'unfilled quota': interrupted['quota']['status']['used']['count/ptahresultrecords.operator.ptah.run'] = '2'
                if fault == 'missing preceding records': interrupted['precedingRecords'] = []
                if fault == 'repeated preceding record': interrupted['precedingRecords'] *= 2
                if fault == 'early delete': audits[0]['requestReceivedTimestamp'] = '2026-10-02T09:59:59Z'
                if fault == 'lost pin': after['remainingPinned'] = []
                if fault == 'foreign collector': audits[0]['user']['username'] = 'administrator'
                interrupted['intent']['spec']['data'] = base64.b64encode(json.dumps(manifest).encode()).decode()
                with self.assertRaises(ValueError):
                    verify_abandoned(*args)

    def test_refuses_wrong_fault_or_collection(self):
        for fault in ('complete', 'wrong Pod', 'changed intent', 'missing cohort', 'quota available',
                      'missing refusal', 'wrong refusal cause', 'wrong refused namespace', 'early delete', 'lost pin'):
            with self.subTest(fault=fault):
                args = list(self.fixture())
                interrupted, before, after, markers, audits, refusals = args
                if fault == 'complete': interrupted['records'].append({'type': 'complete'})
                if fault == 'wrong Pod': interrupted['pod']['metadata']['uid'] = 'replacement'
                if fault == 'changed intent': interrupted['intent']['metadata']['uid'] = 'replacement'
                if fault == 'missing cohort': before['eligibleNames'].remove('intent')
                if fault == 'quota available': interrupted['quota']['status']['hard']['count/ptahresultrecords.operator.ptah.run'] = '3'
                if fault == 'missing refusal': refusals.clear()
                if fault == 'wrong refusal cause': refusals[0]['responseStatus']['message'] = 'forbidden by another guard'
                if fault == 'wrong refused namespace': refusals[0]['objectRef']['namespace'] = 'foreign'
                if fault == 'early delete': audits[0]['requestReceivedTimestamp'] = '2026-10-02T09:59:59Z'
                if fault == 'lost pin': after['remainingPinned'] = []
                with self.assertRaises(ValueError):
                    verify_abandoned(*args)

    def test_replays_retained_installed_cleanup(self):
        root = pathlib.Path(__file__).resolve().parent / 'testdata/results/result-abandoned-2026-10-02'
        names = ('interrupted.json', 'retention-before.json', 'retention-after.json',
                 'retention-markers.json', 'retention-delete-audit.json', 'quota-refusal-audit.json')
        report = verify_abandoned(*[json.loads((root / n).read_text()) for n in names])
        self.assertEqual(report['eligibleRecords'], 3)
        self.assertEqual(report['pinnedRecords'], 4)
        self.assertEqual(report['collectedSecretProjections'], 1)
        self.assertEqual(report['preservedPlanObjects'], 2)
