import base64
import copy
import datetime as dt
import hashlib
import pathlib
import subprocess
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
                 'final': {'metadata': {'uid': 'resource', 'generation': 2}, 'status': {'conditions': [{'type': 'Ready', 'status': 'True', 'reason': 'HistoryMatched', 'observedGeneration': 2}]}}}
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
            (['finalWitness'], '1:3:true'), (['final', 'metadata', 'generation'], 3), (['freshApplyJobs'], 2),
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

    def test_replays_retained_native_engine_results(self):
        from result_first_harvest import publication
        root = pathlib.Path(__file__).resolve().parents[1] / 'evidence/result-partial-loss-2026-10-02'
        for suffix in ('pg', 'mysql'):
            with self.subTest(engine=suffix):
                e = json.loads((root / suffix / 'partial-loss.json').read_text())
                self.assertEqual(verify_evidence(e)['unapprovedExecutions'], 0)
                install = json.loads((root / suffix / 'installation.json').read_text())
                self.assertEqual(len(install['nodes']), 4)
                self.assertTrue(all(n['containerLogMaxSize'] == '10Mi' for n in install['nodes']))
                for label in ('history-before-approval', 'fresh-apply'):
                    records = json.loads((root / suffix / (label + '-publication.json')).read_text())
                    identity = install['publications'][label]
                    intent, receipt, payload = publication(records, identity['jobUID'])
                    manifest = json.loads(base64.b64decode(intent['spec']['data']))
                    document = json.loads(payload)
                    self.assertEqual(manifest['binding']['uid'], e['resourceUID'])
                    self.assertEqual(manifest['digest'], identity['digest'])
                    self.assertEqual(receipt['metadata']['uid'], identity['receiptUID'])
                    self.assertEqual(document['childExitCode'], 0)
                    if label == 'history-before-approval':
                        history = document['migrationHistory']
                        self.assertEqual(history['current_version'], 0)
                        self.assertTrue(history['has_pending_changes'])
                        self.assertEqual(len(history['pending_migrations']), 1)
                        self.assertFalse(any(m.get('state') in ('partial', 'failed', 'applying') for m in history.get('migrations', [])))
                        self.assertLess(install['acknowledgedAt'], receipt['metadata']['creationTimestamp'])
                        self.assertLess(receipt['metadata']['creationTimestamp'], install['freshApprovalCreatedAt'])
                        if identity['jobCreatedAt'] is not None:
                            self.assertLess(install['acknowledgedAt'], identity['jobCreatedAt'])
                        else:
                            credential = install['historyCredential']
                            dates = subprocess.check_output(['openssl', 'x509', '-noout', '-startdate'], input=credential['certificatePEM'].encode()).decode()
                            start = dt.datetime.strptime(dates.strip().split('=', 1)[1], '%b %d %H:%M:%S %Y %Z').replace(tzinfo=dt.timezone.utc)
                            issued = (start + dt.timedelta(seconds=60)).isoformat().replace('+00:00', 'Z')
                            self.assertEqual(issued, credential['issuedAt'])
                            self.assertIn(identity['jobUID'], credential['metadata']['annotations'].values())
                            self.assertLess(install['acknowledgedAt'], issued)
                            self.assertLess(issued, install['freshApprovalCreatedAt'])
                if 'resume' in e:
                    checkpoint = (root / suffix / 'interrupted-checkpoint.json').read_bytes()
                    self.assertEqual(hashlib.sha256(checkpoint).hexdigest(), e['resume']['checkpointSHA256'])
                    old = json.loads(checkpoint)
                    for key in ('resourceUID', 'operationID', 'jobUID', 'beforeAcknowledgment'):
                        self.assertEqual(e[key], old[key])
