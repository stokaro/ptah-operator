import base64
import copy
import json
import pathlib
import unittest

from result_runner_loss import snapshot_migration, verify_evidence
from result_first_harvest import publication


class RunnerLossEvidenceTests(unittest.TestCase):
    def test_snapshot_accepts_removed_last_annotation(self):
        value = {'metadata': {'name': 'migration', 'namespace': 'app', 'uid': 'uid', 'generation': 1},
                 'status': {'resolvedRun': {'resolution': 'HistoryRead'}}}
        result = snapshot_migration(value)
        self.assertEqual(result['metadata']['annotations'], {})
        self.assertEqual(result['status'], value['status'])
        self.assertNotIn('annotations', value['metadata'])

    def test_replays_both_retained_native_recovery_readings(self):
        root = pathlib.Path(__file__).parent / 'testdata' / 'results' / 'result-runner-loss-2026-10-02'
        for name in ['pg', 'mysql']:
            with self.subTest(engine=name):
                proof = json.loads((root / name / 'runner-loss.json').read_text())
                self.assertEqual(verify_evidence(proof)['resolution'], 'HistoryRead')
                records = json.loads((root / name / 'history-publication.json').read_text())
                intent = next(r for r in records.values() if r['spec']['type'] == 'intent')
                binding = json.loads(base64.b64decode(intent['spec']['data']))['binding']
                self.assertEqual(binding['uid'], proof['resourceUID'])
                self.assertNotEqual(binding['jobUID'], proof['jobUID'])
                _, _, payload = publication(records, binding['jobUID'])
                data = json.loads(payload)
                self.assertEqual(data['operation'], 'migration-history')
                self.assertEqual(data['childExitCode'], 0)
                self.assertEqual(data['migrationHistory']['current_version'], 1)
                self.assertFalse(data['migrationHistory']['has_pending_changes'])
                # The exact operator reading must also refuse a replayed SQL counter.
                proof['afterRecovery']['databaseWitness'] = '1:2:true'
                with self.assertRaises(ValueError):
                    verify_evidence(proof)

    def fixture(self):
        unknown = {'outcome': 'Unknown', 'operationID': 'operation', 'jobUID': 'job',
                   'recordedAt': '2026-10-02T12:00:00Z'}
        return {'engine': 'PostgreSQL', 'commit': 'a' * 40,
                'procedureSHA256': {'result_lost_ack.py': 'b' * 64, 'result_runner_loss.py': 'c' * 64},
                'resourceUID': 'resource', 'jobUID': 'job', 'podUID': 'pod', 'operationID': 'operation',
                'namespace': 'app',
                'calibration': {'afterRollback': '0:1:true', 'afterReset': '0:1:false'},
                'beforeLoss': {'databaseWitness': '1:1:true', 'podPhase': 'Running', 'podRestarts': 0, 'publications': 0},
                'quota': {'uid': 'quota', 'hard': 4, 'used': 4},
                'originalJobAbsent': True, 'originalPodAbsent': True,
                'unknown': {'metadata': {'uid': 'resource', 'namespace': 'app', 'annotations': {
                    'operator.ptah.run/unresolved-run': json.dumps(unknown)}}, 'status': {'unresolvedRun': unknown}},
                'afterRecovery': {'databaseWitness': '1:1:true', 'publications': 0, 'replacementApplyJobs': 0,
                                  'migration': {'metadata': {'uid': 'resource', 'generation': 1},
                                                'status': {'resolvedRun': {'operationID': 'operation', 'outcome': 'Unknown',
                                                                          'resolution': 'HistoryRead', 'resolvedAt': '2026-10-02T12:00:01Z'},
                                                           'conditions': [{'type': 'Ready', 'status': 'True', 'reason': 'HistoryMatched', 'observedGeneration': 1}]}}}}

    def test_accepts_history_recovery_without_reexecution(self):
        self.assertEqual(verify_evidence(self.fixture()), {
            'sqlExecutions': 1, 'originalResultPublished': False, 'resolution': 'HistoryRead'})

    def test_refuses_missing_fault_replayed_sql_and_stale_recovery(self):
        mutations = [
            (['podUID'], ''), (['commit'], ''), (['engine'], 'SQLite'),
            (['beforeLoss', 'podPhase'], 'Succeeded'), (['beforeLoss', 'podRestarts'], 1),
            (['beforeLoss', 'publications'], 1), (['beforeLoss', 'databaseWitness'], '0:1:false'),
            (['quota', 'used'], 3), (['quota', 'uid'], ''),
            (['originalJobAbsent'], False), (['originalPodAbsent'], False),
            (['unknown', 'metadata', 'uid'], 'other'),
            (['unknown', 'status', 'unresolvedRun', 'outcome'], 'Applied'),
            (['unknown', 'metadata', 'annotations', 'operator.ptah.run/unresolved-run'], '{}'),
            (['afterRecovery', 'databaseWitness'], '1:2:true'),
            (['afterRecovery', 'publications'], 1), (['afterRecovery', 'replacementApplyJobs'], 1),
            (['afterRecovery', 'migration', 'status', 'unresolvedRun'], {'outcome': 'Unknown'}),
            (['afterRecovery', 'migration', 'status', 'resolvedRun', 'operationID'], 'other'),
            (['afterRecovery', 'migration', 'status', 'resolvedRun', 'resolution'], 'Acknowledged'),
            (['afterRecovery', 'migration', 'status', 'resolvedRun', 'resolvedAt'], '2026-10-02T11:59:59Z'),
            (['afterRecovery', 'migration', 'status', 'conditions', 0, 'observedGeneration'], 0),
        ]
        for path, replacement in mutations:
            with self.subTest(path=path):
                value = copy.deepcopy(self.fixture())
                target = value
                for key in path[:-1]:
                    target = target[key]
                target[path[-1]] = replacement
                with self.assertRaises(ValueError):
                    verify_evidence(value)
