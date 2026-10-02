import copy
import json
import pathlib
import unittest

from result_concurrent import changed_payload, digest, verify_evidence
from result_first_harvest import publication


class ConcurrentDeliveryTests(unittest.TestCase):
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

    def test_refuses_serial_requests_wrong_errors_and_replacement(self):
        mutations = [
            (['evidenceVersion'], 2), (['receipt', 'UID'], ''),
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

    def test_mutation_preserves_native_result_structure(self):
        root = pathlib.Path(__file__).resolve().parents[1] / 'evidence/result-receiver-restart-2026-10-02'
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
