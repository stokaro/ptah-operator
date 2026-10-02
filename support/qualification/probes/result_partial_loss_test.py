import copy
import json
import unittest
from result_partial_loss import verify_evidence


class PartialLossEvidenceTests(unittest.TestCase):
    def fixture(self):
        unknown = {'operationID': 'operation', 'jobUID': 'original-job', 'outcome': 'Unknown'}
        original = {'metadata': {'uid': 'approval-one'}, 'spec': {'planRef': {'uid': 'plan-one'}, 'planFingerprint': 'fingerprint-one'}, 'status': {'conditions': [{'type': 'Consumed', 'status': 'True'}]}}
        roots = {'engine': 'PostgreSQL', 'policy': 'OnApproval', 'resourceUID': 'resource', 'operationID': 'operation', 'jobUID': 'original-job',
                 'calibration': {'afterRollback': '0:1:true', 'afterReset': '0:1:false'},
                 'beforeLoss': {'witness': '1:1:true', 'dirtyRevisions': 1, 'podPhase': 'Running', 'publications': 0},
                 'originalJobAbsent': True, 'originalPodAbsent': True,
                 'unknown': {'metadata': {'annotations': {'operator.ptah.run/unresolved-run': json.dumps(unknown)}}, 'status': {'unresolvedRun': unknown}},
                 'afterManualRepair': '0:1:true',
                 'initial': {'approval': original, 'identity': {'person': 'approver'}},
                 'acknowledgment': {'metadata': {'uid': 'ack'}, 'spec': {'operationID': 'operation', 'migrationRef': {'uid': 'resource'}}, 'status': {'conditions': [{'type': 'Consumed', 'status': 'True'}]}},
                 'acknowledged': {'status': {'resolvedRun': {'resolution': 'Acknowledged', 'operationID': 'operation', 'acknowledgmentRef': {'uid': 'ack'}, 'acknowledgedBy': {'username': 'approver'}}}},
                 'freshApproval': {'metadata': {'uid': 'approval-two', 'creationTimestamp': '2026-10-02T20:00:00Z'}, 'spec': {'planRef': {'uid': 'plan-two'}, 'planFingerprint': 'fingerprint-two'}},
                 'freshJob': {'metadata': {'creationTimestamp': '2026-10-02T20:00:01Z'}},
                 'finalWitness': '1:2:true', 'originalPublications': 0, 'freshApplyJobs': 1,
                 'final': {'metadata': {'uid': 'resource'}, 'status': {'conditions': [{'type': 'Ready', 'status': 'True', 'reason': 'HistoryMatched'}]}}}
        for key, witness, dirty in [('beforeAcknowledgment', '1:1:true', True), ('beforeFreshApproval', '0:1:true', False)]:
            roots[key] = [{'at': t, 'witness': witness, 'applyJobUIDs': [], 'unresolvedOperation': 'operation' if dirty else None, 'dirty': dirty} for t in range(0, 91, 5)]
        return roots

    def test_accepts_two_separately_authorized_executions(self):
        self.assertEqual(verify_evidence(self.fixture())['unapprovedExecutions'], 0)

    def test_refuses_missing_fault_or_authorization(self):
        for path, value in [
            (['policy'], 'Always'), (['beforeLoss', 'dirtyRevisions'], 0), (['beforeLoss', 'publications'], 1),
            (['originalJobAbsent'], False), (['originalPodAbsent'], False),
            (['unknown', 'status', 'unresolvedRun', 'outcome'], 'Failed'),
            (['beforeAcknowledgment'], []), (['beforeFreshApproval'], []),
            (['afterManualRepair'], '0:1:false'),
            (['acknowledged', 'status', 'resolvedRun', 'resolution'], 'HistoryRead'),
            (['freshApproval', 'metadata', 'uid'], 'approval-one'),
            (['freshApproval', 'spec', 'planRef', 'uid'], 'plan-one'),
            (['finalWitness'], '1:3:true'), (['freshApplyJobs'], 2),
        ]:
            with self.subTest(path=path):
                evidence = self.fixture()
                target = evidence
                for key in path[:-1]:
                    target = target[key]
                target[path[-1]] = value
                with self.assertRaises(ValueError):
                    verify_evidence(evidence)
        for key in ('beforeAcknowledgment', 'beforeFreshApproval'):
            for field, value in [('witness', '2:2:true'), ('applyJobUIDs', ['unapproved-job'])]:
                evidence = self.fixture()
                evidence[key][1][field] = value
                with self.assertRaises(ValueError):
                    verify_evidence(evidence)
