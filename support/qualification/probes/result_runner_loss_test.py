import base64
import copy
import datetime as dt
import json
import pathlib
import unittest

from result_runner_loss import seconds_to_lease_term, snapshot_migration, verify_evidence
from result_first_harvest import publication


class LeaseTermTests(unittest.TestCase):
    """The claim the operator persisted, read by the e2e row that held it to its term."""

    def claim(self):
        root = pathlib.Path(__file__).resolve().parents[3]
        reading = json.loads((root / 'testdata' / 'e2e' / 'readings' / 'apply-pod-gone-live.json').read_text())
        return reading['claim']

    def at(self, value):
        return dt.datetime.fromisoformat(value.replace('Z', '+00:00'))

    def test_waits_from_the_start_to_the_end_of_the_lease_term(self):
        claim = self.claim()
        started = self.at(claim['startedAt'])
        self.assertEqual(seconds_to_lease_term(claim, started), claim['leaseDurationSeconds'])
        self.assertEqual(seconds_to_lease_term(claim, started + dt.timedelta(seconds=100)),
                         claim['leaseDurationSeconds'] - 100)

    def test_the_term_is_the_leases_not_the_dispatch_windows(self):
        claim = self.claim()
        window = self.at(claim['dispatchNotAfter'])
        # The Lease outlives the window the run was authorized for, so a wait
        # that ended at the window would end before the hold does.
        self.assertGreater(seconds_to_lease_term(claim, window), 0)

    def test_a_term_already_over_waits_for_nothing(self):
        claim = self.claim()
        past = self.at(claim['startedAt']) + dt.timedelta(seconds=claim['leaseDurationSeconds'] + 60)
        self.assertEqual(seconds_to_lease_term(claim, past), 0)

    def test_refuses_a_claim_without_its_term(self):
        now = dt.datetime.now(dt.timezone.utc)
        for name, change in [('no start', {'startedAt': None}), ('empty start', {'startedAt': ''}),
                             ('no duration', {'leaseDurationSeconds': None}), ('zero duration', {'leaseDurationSeconds': 0}),
                             ('a flag for a duration', {'leaseDurationSeconds': True}),
                             ('a string duration', {'leaseDurationSeconds': '420'})]:
            with self.subTest(name):
                claim = {**self.claim(), **change}
                # The refusal names what the claim lacks, rather than the
                # parser's complaint about an empty time.
                with self.assertRaisesRegex(ValueError, '^The Apply claim records no '):
                    seconds_to_lease_term(claim, now)


class StopAtWait(Exception):
    """Raised by the fake lab at the wait a row is about."""


class FakeLab:
    """The calls a loss probe makes up to its first history wait, answered.

    wait records each bound it is given and returns the next canned value; at
    the wait numbered stop it raises StopAtWait, so the probe goes no further.
    """

    def __init__(self, operation, unknown, stop):
        self.operation, self.unknown, self.stop = operation, unknown, stop
        self.waits = []
        self.canned = [{'status': {'hard': {QUOTA_KEY: '1'}, 'used': {QUOTA_KEY: '1'}}}, True, unknown]

    def wait(self, fn, seconds=240):
        self.waits.append(seconds)
        if len(self.waits) == self.stop:
            raise StopAtWait()
        return self.canned[len(self.waits) - 1]

    def get(self, kind, name=None):
        if kind == 'pod':
            return {'metadata': {'uid': 'pod-uid'}, 'status': {'phase': 'Running', 'containerStatuses': [{'restartCount': 0}]}}
        if kind == 'job':
            return {'metadata': {'uid': self.operation['jobUID']}}
        if kind == 'jobs':
            return {'items': []}
        raise AssertionError('unexpected read of %s %s' % (kind, name))

    def records(self):
        return [{'spec': {'type': 'credential', 'data': ''}}]

    def arguments(self):
        return dict(k=lambda *args, **kwargs: '', get=self.get, create=lambda o: {'metadata': {'uid': 'quota-uid'}},
                    wait=self.wait, save=lambda name, value: None, witness=lambda: '1:1:true', records=self.records,
                    open_gate=lambda: None,
                    resource={'metadata': {'namespace': 'lab', 'name': 'lost-ack', 'uid': 'resource-uid'},
                              'spec': {'policy': {'apply': 'OnApproval'}}},
                    pod={'metadata': {'name': 'apply-pod', 'uid': 'pod-uid'}}, operation=self.operation,
                    engine='PostgreSQL', environment={'E2E_CONTROLLER_REVISION': 'a' * 40},
                    calibration={'afterRollback': '0:1:true', 'afterReset': '0:1:false'})


QUOTA_KEY = 'count/ptahresultrecords.operator.ptah.run'


def fresh_claim(started_ago):
    """The operator's own claim, as the e2e row read it, dated started_ago before now."""
    root = pathlib.Path(__file__).resolve().parents[3]
    claim = json.loads((root / 'testdata' / 'e2e' / 'readings' / 'apply-pod-gone-live.json').read_text())['claim']
    started = dt.datetime.now(dt.timezone.utc) - dt.timedelta(seconds=started_ago)
    return {**claim, 'startedAt': started.strftime('%Y-%m-%dT%H:%M:%SZ')}


def unknown_reading(operation):
    unknown = {'outcome': 'Unknown', 'operationID': operation['id'], 'jobUID': operation['jobUID'],
               'recordedAt': '2026-11-12T06:54:10Z'}
    return {'metadata': {'name': 'lost-ack', 'namespace': 'lab', 'uid': 'resource-uid', 'generation': 1,
                         'annotations': {'operator.ptah.run/unresolved-run': json.dumps(unknown)}},
            'status': {'unresolvedRun': unknown}}


class RunnerLossWaitTests(unittest.TestCase):
    def test_the_recovery_wait_starts_at_the_lease_term(self):
        from result_runner_loss import run
        operation = fresh_claim(60)
        lab = FakeLab(operation, unknown_reading(operation), stop=4)
        with self.assertRaises(StopAtWait):
            run(**lab.arguments())
        expected = operation['leaseDurationSeconds'] - 60 + 240
        # A fixed 240 seconds from the deletion would end before the hold does.
        self.assertAlmostEqual(lab.waits[3], expected, delta=5)


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
