import copy
import unittest

import result_concurrent_test as concurrent_tests
from result_first_publication import verify_evidence


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
