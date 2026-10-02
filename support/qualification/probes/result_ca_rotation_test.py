import copy
import json
import pathlib
import unittest
from result_ca_rotation import verify_evidence


class CARecoveryEvidenceTests(unittest.TestCase):
    def setUp(self):
        roots = lambda prefix: {key: prefix + key for key in ('ServerCA', 'ServerKey', 'ClientCA', 'ClientKey')}
        self.value = dict(phaseBefore='stable', seededPhase='prepare', heldPhase='prepare', phaseAfter='stable',
                          expiredNotAfter={str(i): 100 for i in range(6)}, seededAt=1000,
                          gateRefused=True, projectionHeld=True, managersUnavailableWithExpiredTrust=True,
                          candidateBeforeRestart=roots('new'), candidateAfterRestart=roots('new'), currentAfter=roots('new'),
                          expiredCandidate=roots('expired'), currentBefore=roots('old'),
                          fenceBeforeRestart='persisted-fence', fenceAfterRestart='persisted-fence',
                          rotatorUIDBefore='old', rotatorUIDAfter='new',
                          managerUIDsBefore=['old-a', 'old-b'], managerUIDsAfter=['new-a', 'new-b'],
                          projectionUIDBefore='projection', projectionUIDAfter='projection', journalUIDBefore='journal', journalUIDAfter='journal',
                          receiversReady=True, rotatorReady=True, temporaryGateRemoved=True, replicasRestored=True, argumentsUnchanged=True)

    def test_retained_installed_reading(self):
        path = pathlib.Path(__file__).parents[1] / 'evidence/result-ca-rotation-2026-10-02/rotation.json'
        evidence = json.loads(path.read_text())
        self.assertEqual(verify_evidence(evidence), evidence['verification'])
        evidence['candidateAfterRestart']['ClientKey'] = evidence['expiredCandidate']['ClientKey']
        with self.assertRaises(ValueError):
            verify_evidence(evidence)

    def test_accepts_recovery(self):
        self.assertTrue(verify_evidence(self.value)['newAuthoritiesInstalled'])

    def test_refuses_incomplete_fault_and_recovery(self):
        for key, value in [('heldPhase', 'stable'), ('expiredNotAfter', {}), ('seededAt', 399),
                           ('gateRefused', False), ('projectionHeld', False), ('managersUnavailableWithExpiredTrust', False),
                           ('fenceAfterRestart', 'another-fence'), ('rotatorUIDAfter', 'old'),
                           ('managerUIDsAfter', ['new-a', 'new-a']), ('managerUIDsAfter', ['old-a', 'new-b']),
                           ('projectionUIDAfter', 'another-projection'), ('journalUIDAfter', 'another-journal'),
                           ('receiversReady', False), ('rotatorReady', False), ('temporaryGateRemoved', False),
                           ('replicasRestored', False), ('argumentsUnchanged', False)]:
            with self.subTest(key=key):
                evidence = copy.deepcopy(self.value)
                evidence[key] = value
                with self.assertRaises(ValueError):
                    verify_evidence(evidence)
        for key in ('ServerCA', 'ServerKey', 'ClientCA', 'ClientKey'):
            with self.subTest(reused=key):
                evidence = copy.deepcopy(self.value)
                evidence['currentBefore'][key] = evidence['currentAfter'][key]
                with self.assertRaises(ValueError):
                    verify_evidence(evidence)
