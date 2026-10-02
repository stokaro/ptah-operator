import copy
import hashlib
import json
import pathlib
import unittest

import result_concurrent_test as concurrent_tests
from result_first_publication import verify_evidence
from result_lost_ack import verify_evidence as verify_ack
from result_first_harvest import publication


class FirstPublicationTests(unittest.TestCase):
    def fixture(self):
        concurrent = concurrent_tests.ConcurrentDeliveryTests().fixture()
        return {'evidenceVersion': 1, 'intentName': 'intent', 'intentUID': 'intent-uid',
                'concurrent': concurrent,
                'before': {'readCompletedNs': 5, 'records': [
                    {'name': 'original-credential', 'uid': 'credential-uid', 'type': 'credential'}]},
                'barrier': {'firstGateEnabled': True, 'firstWaits': 1, 'preflights': 1,
                            'attempts': [], 'dropped': False, 'firstAdmissions': [
                                {'requestUID': 'one', 'managerPodUID': 'receiver-0', 'intentName': 'intent',
                                 'payloadDigest': concurrent['originalDigest'],
                                 'arrivedAt': '2026-10-02T14:00:01Z', 'releasedAt': '2026-10-02T14:00:03Z'},
                                {'requestUID': 'two', 'managerPodUID': 'receiver-1', 'intentName': 'intent',
                                 'payloadDigest': concurrent['originalDigest'],
                                 'arrivedAt': '2026-10-02T14:00:02Z', 'releasedAt': '2026-10-02T14:00:03.1Z'}]}}

    def test_requires_both_installed_creates_before_either_release(self):
        self.assertEqual(verify_evidence(self.fixture()),
                         {'simultaneousIntentCreates': 2, 'receiptUID': 'receipt'})

    def test_replays_both_installed_first_publications(self):
        root = pathlib.Path(__file__).resolve().parents[1] / 'evidence/result-first-publication-2026-10-02'
        summary = json.loads((root / 'summary.json').read_text())
        self.assertEqual(set(summary['engines']), {'PostgreSQL', 'MySQL'})
        for engine, entry in summary['engines'].items():
            with self.subTest(engine=engine):
                directory = root / entry['directory']
                self.assertEqual(set(entry['files']), {'first-publication.json', 'before-first-publication.json',
                    'concurrent.json', 'lost-ack.json', 'held-retry.json', 'publication.json', 'installation.json'})
                for name, expected in entry['files'].items():
                    self.assertEqual(hashlib.sha256((directory / name).read_bytes()).hexdigest(), expected)
                proof = json.loads((directory / 'first-publication.json').read_text())
                ack = json.loads((directory / 'lost-ack.json').read_text())
                self.assertEqual(verify_evidence(proof), entry['verification'])
                self.assertEqual(verify_ack(ack)['sqlExecutions'], 1)
                self.assertEqual(proof['concurrent']['receipt'], ack['proxy']['attempts'][0]['receipt'])
                self.assertEqual(proof['barrier']['firstAdmissions'], ack['proxy']['firstAdmissions'])
                records = json.loads((directory / 'publication.json').read_text())
                intent, complete, payload = publication(records, ack['jobUID'])
                self.assertEqual(intent['metadata']['uid'], proof['intentUID'])
                self.assertEqual(complete['metadata']['uid'], proof['concurrent']['receipt']['UID'])
                self.assertEqual('sha256:' + hashlib.sha256(payload).hexdigest(), proof['concurrent']['originalDigest'])

    def test_refuses_prior_publication_missing_or_serial_admissions(self):
        mutations = [
            (['before', 'records'], []),
            (['before', 'records', 0, 'name'], 'intent'),
            (['before', 'records', 0, 'name'], 'intent-complete'),
            (['before', 'readCompletedNs'], 15),
            (['barrier', 'firstGateEnabled'], False),
            (['barrier', 'firstWaits'], 0),
            (['barrier', 'firstResumedAt'], '2026-10-02T14:00:04Z'),
            (['barrier', 'attempts'], [{'receipt': 'already stored'}]),
            (['barrier', 'dropped'], True),
            (['barrier', 'firstAdmissions'], []),
            (['barrier', 'firstAdmissions', 1, 'requestUID'], 'one'),
            (['barrier', 'firstAdmissions', 1, 'managerPodUID'], 'receiver-0'),
            (['barrier', 'firstAdmissions', 1, 'payloadDigest'], 'different'),
            (['barrier', 'firstAdmissions', 0, 'releasedAt'], None),
            (['barrier', 'firstAdmissions', 0, 'releasedAt'], '2026-10-02T14:00:01.5Z'),
        ]
        for path, replacement in mutations:
            with self.subTest(path=path):
                value = self.fixture()
                target = value
                for key in path[:-1]:
                    target = target[key]
                target[path[-1]] = replacement
                with self.assertRaises(ValueError):
                    verify_evidence(value)


if __name__ == '__main__':
    unittest.main()
