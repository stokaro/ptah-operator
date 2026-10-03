import copy
import base64
import hashlib
import json
import pathlib
import unittest

from result_concurrent import (attempt_name, authority_cases, changed_payload, digest,
                               pod_token_headers, verify_authority_evidence, verify_evidence)
from result_first_harvest import publication


class ConcurrentDeliveryTests(unittest.TestCase):
    def authority_fixture(self):
        identity = {'binding': {'namespace': 'probe', 'name': 'migration', 'uid': 'resource', 'generation': 1,
                    'executionBindingID': 'v1-' + '1' * 32, 'operationID': 'sha256:' + '2' * 64,
                    'jobName': 'original-job', 'jobUID': 'job', 'podUID': 'pod'}, 'engine': 'postgresql'}
        receipt = {'Name': attempt_name(identity) + '-complete', 'UID': 'receipt'}
        receivers = ['receiver-0', 'receiver-1']

        def requests(claim, refused=False, untrusted=False):
            rows = []
            for uid in receivers:
                for method in ('HEAD', 'PUT'):
                    row = {'receiverUID': uid, 'method': method, 'status': 403 if refused else (204 if method == 'HEAD' else 200),
                           'path': '/v1/results/' + attempt_name(claim),
                           'identityDigest': digest(json.dumps(claim, separators=(',', ':')).encode()),
                           'credential': 'foreign-pod-token' if untrusted else 'original-pod-token',
                           'bodyWithheld': refused and method == 'PUT'}
                    if not refused and method == 'PUT':
                        row['receipt'] = copy.deepcopy(receipt)
                    rows.append(row)
            return rows

        return {'evidenceVersion': 1, 'authentication': 'pod-token', 'identity': identity, 'receipt': receipt,
                'receivers': receivers, 'foreignPod': {'uid': 'publisher-pod', 'namespace': 'probe',
                    'tokenReviewAuthenticated': True, 'audiences': ['operator.ptah.run/results']}, 'publicationUnchanged': True, 'before': requests(identity), 'after': requests(identity),
                'cases': [{'name': label, 'identity': claim, 'requests': requests(claim, True, label == 'foreign-pod-token')}
                          for label, claim in authority_cases(identity)]}

    def test_authority_refusals_require_each_claim_on_its_own_route(self):
        value = self.authority_fixture()
        self.assertEqual(verify_authority_evidence(value), {'refusedRequests': 32, 'positiveControls': 8})
        changes = [(['cases'], []), (['cases', 0, 'requests'], []),
                   (['foreignPod', 'uid'], 'pod'), (['foreignPod', 'tokenReviewAuthenticated'], False),
                   (['cases', 0, 'requests', 0, 'credential'], 'original-pod-token'),
                   (['cases', 0, 'requests', 1, 'bodyWithheld'], False),
                   (['cases', 1, 'requests', 0, 'path'], value['before'][0]['path']),
                   (['cases', 1, 'requests', 0, 'identityDigest'], value['before'][0]['identityDigest']),
                   (['cases', 1, 'requests', 0, 'status'], 401),
                   (['cases', 1, 'requests', 0, 'status'], 422),
                   (['cases', 1, 'requests', 0, 'status'], 503),
                   (['cases', 1, 'requests', 0, 'status'], 204),
                   (['before'], []), (['after', 0, 'status'], 403),
                   (['after', 1, 'receipt', 'UID'], 'replacement'), (['publicationUnchanged'], False)]
        for path, replacement in changes:
            bad = copy.deepcopy(value)
            target = bad
            for key in path[:-1]:
                target = target[key]
            target[path[-1]] = replacement
            with self.subTest(path=path, replacement=replacement), self.assertRaises(ValueError):
                verify_authority_evidence(bad)

    def fixture(self):
        receipt = {'Name': 'intent-complete', 'UID': 'receipt',
                   'Size': 12, 'Digest': 'sha256:' + 'a' * 64}
        rounds = []
        for label, statuses in [('identical', [200, 200]),
                                ('conflicting', [409, 409]), ('mixed', [200, 409])]:
            rows = []
            for i, status in enumerate(statuses):
                row = {'receiverUID': 'receiver-' + str(i), 'status': status,
                       'digest': receipt['Digest'] if status == 200 else 'sha256:' + 'b' * 64,
                       'headersSentNs': 10 + i, 'bodySentNs': 20 + i,
                       'responseReadNs': 30 + i}
                if status == 200:
                    row['receipt'] = copy.deepcopy(receipt)
                rows.append(row)
            rounds.append({'name': label, 'requests': rows})
        return {'evidenceVersion': 1, 'receipt': receipt, 'originalDigest': receipt['Digest'],
                'changedDigest': 'sha256:' + 'b' * 64, 'receivers': ['receiver-0', 'receiver-1'],
                'rounds': rounds, 'publicationUnchanged': True}

    def test_requires_identical_receipts_and_content_conflicts(self):
        self.assertEqual(verify_evidence(self.fixture()),
                         {'requests': 6, 'identicalReceipts': 3, 'conflicts': 3})

    def test_pod_token_mode_requires_original_public_binding(self):
        value = self.fixture()
        value.update(evidenceVersion=2, authentication='pod-token', identityDigest='sha256:' + 'c' * 64,
                     binding={'namespace': 'probe', 'jobUID': 'job', 'podUID': 'pod'})
        self.assertEqual(verify_evidence(value)['conflicts'], 3)
        for field, bad in [('authentication', 'certificate'), ('identityDigest', ''), ('binding', {})]:
            changed = copy.deepcopy(value)
            changed[field] = bad
            with self.subTest(field=field), self.assertRaises(ValueError):
                verify_evidence(changed)

    def test_token_headers_carry_exact_identity_and_reject_injection(self):
        identity = b'{"binding":{"namespace":"probe","jobUID":"job","podUID":"pod"}}'
        token = 'original.pod-token.signature'
        headers = pod_token_headers(token, identity)
        self.assertIn('\r\nAuthorization: Bearer ' + token, headers)
        encoded = headers.split('\r\nX-Ptah-Result-Identity: ')[1]
        self.assertEqual(base64.urlsafe_b64decode(encoded + '=' * (-len(encoded) % 4)), identity)
        for bad in ('', token + '\n', token + '\r\nInjected: yes', 'a' * 8193):
            with self.subTest(token_length=len(bad)), self.assertRaises(ValueError):
                pod_token_headers(bad, identity)
        for bad in (b'{}', b'{"binding":{"jobUID":"job","podUID":"pod"}}', identity * 100):
            with self.assertRaises(ValueError):
                pod_token_headers(token, bad)

    def test_refuses_serial_requests_wrong_errors_and_replacement(self):
        mutations = [
            (['evidenceVersion'], 3), (['receipt', 'UID'], ''),
            (['changedDigest'], 'sha256:' + 'a' * 64),
            (['receivers'], ['receiver-0', 'receiver-0']),
            (['rounds'], []), (['rounds', 0, 'requests'], []),
            (['rounds', 0, 'requests', 1, 'receiverUID'], 'receiver-0'),
            (['rounds', 0, 'requests', 1, 'headersSentNs'], 35),
            (['rounds', 0, 'requests', 1, 'bodySentNs'], 35),
            (['rounds', 0, 'requests', 0, 'receipt', 'UID'], 'replacement'),
            (['rounds', 0, 'requests', 0, 'digest'], 'sha256:' + 'b' * 64),
            (['rounds', 1, 'requests', 0, 'status'], 422),
            (['rounds', 1, 'requests', 1, 'status'], 403),
            (['rounds', 1, 'requests', 0, 'status'], 503),
            (['rounds', 2, 'requests', 1, 'status'], 200),
            (['publicationUnchanged'], False),
        ]
        for path, replacement in mutations:
            with self.subTest(path=path, replacement=replacement):
                value = self.fixture()
                target = value
                for key in path[:-1]:
                    target = target[key]
                target[path[-1]] = replacement
                with self.assertRaises(ValueError):
                    verify_evidence(value)

    def test_replays_installed_native_results_and_previous_defect(self):
        root = pathlib.Path(__file__).resolve().parent / 'testdata/results/result-concurrent-2026-10-02'
        summary = json.loads((root / 'summary.json').read_text())
        self.assertEqual(set(summary['engines']), {'PostgreSQL', 'MySQL'})
        for engine, entry in summary['engines'].items():
            with self.subTest(engine=engine):
                directory = root / entry['directory']
                for filename, expected in entry['files'].items():
                    self.assertEqual(hashlib.sha256((directory / filename).read_bytes()).hexdigest(), expected)
                proof = json.loads((directory / 'concurrent.json').read_text())
                ack = json.loads((directory / 'lost-ack.json').read_text())
                self.assertEqual(verify_evidence(proof), entry['verification'])
                self.assertEqual(proof['receipt'], ack['proxy']['attempts'][0]['receipt'])
                records = json.loads((directory / 'publication.json').read_text())
                _, complete, payload = publication(records, ack['jobUID'])
                self.assertEqual(complete['metadata']['uid'], proof['receipt']['UID'])
                self.assertEqual(digest(payload), proof['originalDigest'])
                self.assertEqual(digest(changed_payload(payload)), proof['changedDigest'])
        before = (root / summary['regression']['file']).read_bytes()
        self.assertEqual(hashlib.sha256(before).hexdigest(), summary['regression']['sha256'])
        with self.assertRaisesRegex(ValueError, 'expected 409, received 503'):
            verify_evidence(json.loads(before))

    def test_mutation_preserves_native_result_structure(self):
        root = pathlib.Path(__file__).resolve().parent / 'testdata/results/result-receiver-restart-2026-10-02'
        for engine in ('pg', 'mysql'):
            with self.subTest(engine=engine):
                acknowledgment = json.loads((root / engine / 'lost-ack.json').read_text())
                records = json.loads((root / engine / 'publication.json').read_text())
                payload = publication(records, acknowledgment['jobUID'])[2]
                changed = changed_payload(payload)
                self.assertNotEqual(digest(payload), digest(changed))
                original, other = json.loads(payload), json.loads(changed)
                other['migrationRun']['status']['migrations'][0]['description'] = 'Record Delivery'
                self.assertEqual(original, other)
        for payload in [b'{}', b'"description":"Record Delivery"' * 2]:
            with self.assertRaises(ValueError):
                changed_payload(payload)


if __name__ == '__main__':
    unittest.main()
