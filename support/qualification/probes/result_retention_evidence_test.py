import copy
import unittest

from result_retention_evidence import verify


class RetentionEvidenceTests(unittest.TestCase):
    def fixture(self):
        binding = {'namespace': 'work', 'jobUID': 'retired-job'}
        created, due, deleted = '2026-10-02T09:00:00Z', '2026-10-02T10:00:00Z', '2026-10-02T10:00:01Z'
        def record(name, role, bound=None, owners=None):
            value = {'name': name, 'uid': name + '-uid', 'type': role,
                     'createdAt': created, 'owners': owners or [], 'dataDigest': name}
            if bound:
                value['binding'] = bound
            return value
        rows = [record('credential', 'credential'), record('intent', 'intent', binding),
                record('chunk', 'chunk', owners=[{'uid': 'intent-uid'}]),
                record('complete', 'complete', owners=[{'uid': 'intent-uid'}]),
                record('retired', 'retired', binding), record('pinned', 'credential')]
        eligible = [r['name'] for r in rows[:-1]]
        secrets = {name: {'name': name, 'namespace': 'work', 'uid': name + '-secret-uid',
                         'ownerReferences': [{'uid': name + '-uid', 'blockOwnerDeletion': False}],
                         'annotations': {'operator.ptah.run/result-job-uid': 'pinned-job' if name == 'pinned' else 'retired-job'}}
                   for name in ['credential', 'pinned']}
        before = {'records': rows, 'eligibleNames': eligible, 'pinnedNames': ['pinned'],
                  'secretOwners': secrets, 'planDigests': {'plan-uid': 'plan-hash'},
                  'deadlines': {name: due for name in eligible}}
        after = {'remainingPinned': [copy.deepcopy(rows[-1])],
                 'eligibleRecordsCollected': 5, 'eligibleSecretProjectionsCollected': 1,
                 'planDigests': {'plan-uid': 'plan-hash'}}
        marker = {'metadata': {'name': 'retired', 'uid': 'retired-uid', 'creationTimestamp': created},
                  'spec': {'value': {'binding': binding, 'retentionSeconds': 3600,
                                    'source': {'name': 'credential', 'uid': 'credential-uid', 'type': 'credential'}}}}
        def event(resource, name, uid, actor):
            return {'verb': 'delete', 'stage': 'ResponseComplete', 'auditID': name,
                    'responseStatus': {'code': 200}, 'requestReceivedTimestamp': deleted,
                    'objectRef': {'resource': resource, 'namespace': 'work', 'name': name},
                    'requestObject': {'preconditions': {'uid': uid}}, 'user': {'username': actor}}
        audits = [event('ptahresultrecords', name, name + '-uid', 'manager') for name in eligible]
        audits.append(event('secrets', 'credential', 'credential-secret-uid', 'system:kube-controller-manager'))
        return before, after, [marker], audits

    def test_accepts_api_timed_gc_and_unchanged_pins(self):
        report = verify(*self.fixture())
        self.assertEqual(report['eligibleRecords'], 5)
        self.assertEqual(report['pinnedRecords'], 1)
        self.assertEqual(report['collectedSecretProjections'], 1)
        self.assertEqual(len(report['recordDeletes']), 5)

    def test_refuses_incomplete_or_misbound_evidence(self):
        for fault in ('empty denominator', 'duplicate denominator', 'missing pin', 'changed pin',
                      'changed plan', 'missing marker', 'marker UID', 'source UID', 'short retention',
                      'early record', 'early Secret', 'failed delete', 'wrong UID', 'wrong namespace',
                      'missing delete', 'manager deleted Secret', 'missing projection', 'forged deadline'):
            with self.subTest(fault=fault):
                before, after, markers, audits = self.fixture()
                if fault == 'empty denominator': before['eligibleNames'] = []
                if fault == 'duplicate denominator': before['eligibleNames'].append('intent')
                if fault == 'missing pin': after['remainingPinned'] = []
                if fault == 'changed pin': after['remainingPinned'][0]['uid'] = 'replacement'
                if fault == 'changed plan': after['planDigests'] = {'plan-uid': 'other'}
                if fault == 'missing marker': markers.clear()
                if fault == 'marker UID': markers[0]['metadata']['uid'] = 'replacement'
                if fault == 'source UID': markers[0]['spec']['value']['source']['uid'] = 'replacement'
                if fault == 'short retention': markers[0]['spec']['value']['retentionSeconds'] = 3599
                if fault == 'early record': audits[0]['requestReceivedTimestamp'] = '2026-10-02T09:59:59Z'
                if fault == 'early Secret': audits[-1]['requestReceivedTimestamp'] = '2026-10-02T09:59:59Z'
                if fault == 'failed delete': audits[0]['responseStatus']['code'] = 403
                if fault == 'wrong UID': audits[0]['requestObject']['preconditions']['uid'] = 'replacement'
                if fault == 'wrong namespace': audits[0]['objectRef']['namespace'] = 'other'
                if fault == 'missing delete': audits.pop(0)
                if fault == 'manager deleted Secret': audits[-1]['user']['username'] = 'manager'
                if fault == 'missing projection': after['eligibleSecretProjectionsCollected'] = 0
                if fault == 'forged deadline': before['deadlines']['intent'] = '2026-10-02T09:30:00Z'
                with self.assertRaises(ValueError):
                    verify(before, after, markers, audits)

    def test_late_record_gets_its_own_full_window(self):
        before, after, markers, audits = self.fixture()
        next(r for r in before['records'] if r['name'] == 'chunk')['createdAt'] = '2026-10-02T09:30:00Z'
        with self.assertRaisesRegex(ValueError, 'Frozen deadline'):
            verify(before, after, markers, audits)
        before['deadlines']['chunk'] = '2026-10-02T10:30:00Z'
        with self.assertRaisesRegex(ValueError, 'before its retention'):
            verify(before, after, markers, audits)
        next(a for a in audits if a['objectRef']['name'] == 'chunk')['requestReceivedTimestamp'] = '2026-10-02T10:30:01Z'
        self.assertEqual(verify(before, after, markers, audits)['status'], 'passed')


if __name__ == '__main__':
    unittest.main()
