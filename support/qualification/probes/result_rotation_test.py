import copy
import json
import pathlib
import unittest

from result_rotation import verify_evidence


class RotationEvidenceTest(unittest.TestCase):
    def setUp(self):
        self.value = {
            'phaseBefore': 'stable', 'phaseInterrupted': 'leaf', 'phaseAfter': 'stable',
            'gateRefused': True, 'projectionHeld': True,
            'rotatorUIDBefore': 'old-rotator', 'rotatorUIDAfter': 'new-rotator',
            'candidateBeforeRestart': 'new-leaf', 'candidateAfterRestart': 'new-leaf',
            'leafBefore': 'old-leaf', 'leafAfter': 'new-leaf',
            'keyBefore': 'old-key', 'keyAfter': 'new-key',
            'serverCABefore': 'server-root', 'serverCAAfter': 'server-root',
            'clientCABefore': 'client-root', 'clientCAAfter': 'client-root',
            'managerUIDsBefore': ['manager-one', 'manager-two'],
            'managerUIDsAfter': ['manager-one', 'manager-two'],
            'projectionUIDBefore': 'projection', 'projectionUIDAfter': 'projection',
            'journalUIDBefore': 'journal', 'journalUIDAfter': 'journal',
            'policyBefore': {'policy': 'unchanged'}, 'policyAfter': {'policy': 'unchanged'},
            'rotatorReadyAfter': True, 'restoredArguments': True,
        }

    def test_retained_installed_reading(self):
        source = pathlib.Path(__file__).parents[1] / 'evidence/result-leaf-rotation-2026-10-02/rotation.json'
        evidence = json.loads(source.read_text())
        self.assertEqual(verify_evidence(evidence), evidence['verification'])
        evidence['candidateAfterRestart'] = evidence['leafBefore']
        with self.assertRaises(ValueError):
            verify_evidence(evidence)

    def test_accepts_interrupted_renewal(self):
        self.assertEqual(verify_evidence(self.value)['servingReplicas'], 2)

    def test_refuses_missing_fault_or_recovery(self):
        for field, value in [
            ('phaseInterrupted', 'stable'), ('gateRefused', False),
            ('projectionHeld', False), ('rotatorUIDAfter', 'old-rotator'),
            ('candidateAfterRestart', 'replacement-candidate'),
            ('leafAfter', 'old-leaf'), ('keyAfter', 'old-key'),
            ('serverCAAfter', 'new-root'), ('clientCAAfter', 'new-root'),
            ('managerUIDsBefore', []), ('managerUIDsBefore', ['manager-one', 'manager-one']), ('managerUIDsAfter', ['replacement', 'manager-two']),
            ('projectionUIDAfter', 'replacement'), ('journalUIDAfter', 'replacement'),
            ('policyAfter', {}), ('rotatorReadyAfter', False), ('restoredArguments', False),
        ]:
            with self.subTest(field=field):
                evidence = copy.deepcopy(self.value)
                evidence[field] = value
                with self.assertRaises(ValueError):
                    verify_evidence(evidence)


if __name__ == '__main__':
    unittest.main()
