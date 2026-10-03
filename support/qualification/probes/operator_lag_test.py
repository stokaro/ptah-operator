import copy
import json
from pathlib import Path
from types import SimpleNamespace
import unittest

from operator_restore import OperatorProbe, db


class LagWaitTest(unittest.TestCase):
    def test_wait_measures_the_full_gap_from_backup_completion(self):
        for now in (100, 170, 399.5, 400, 420):
            with self.subTest(now=now):
                clock, waits = [now], []

                def sleep(seconds):
                    self.assertGreater(seconds, 0)
                    self.assertLessEqual(seconds, 30)
                    waits.append(seconds)
                    clock[0] += seconds

                OperatorProbe.wait_for_backup_lag(100, lambda: clock[0], sleep)
                self.assertGreaterEqual(clock[0], 400)
                self.assertEqual(sum(waits), max(400 - now, 0))

    def test_database_only_loss_cannot_claim_an_older_operator_restore(self):
        with self.assertRaisesRegex(ValueError, 'restore the older operator backup'):
            OperatorProbe('mysql', 'migration', Path('unused'), Path('unused'), 'database', 'operator-lag')

    def test_job_expiry_during_the_lag_preserves_the_verified_original_execution(self):
        probe = object.__new__(OperatorProbe)
        probe.family, probe.kind = 'migration', 'PtahMigration'
        probe.operator_backup_completed = 100
        probe.report = {'checks': {}, 'artifacts': {'2': {'digest': 'sha256:fixture'}}}
        probe.persist = lambda: None
        probe.publish = lambda revision: 'oci://fixture/new'
        probe.source_spec = lambda reference: {'ociRef': reference}
        probe.patch = lambda value: None
        probe.settled = lambda phase: {'status': {'plan': {'name': 'later-plan'}}}
        plan = {'metadata': {'uid': 'later-plan-uid'}}
        probe.read = lambda kind, name: plan
        probe.approve = lambda *args: SimpleNamespace(stdout=b'{"metadata":{"uid":"later-approval"}}')
        waits = []
        probe.wait_for_backup_lag = lambda completed: waits.append(completed)
        old_job = {'metadata': {'uid': 'original-job'}, 'status': {'succeeded': 1}}
        later_job = {'metadata': {'uid': 'later-job'}, 'status': {'succeeded': 1}}
        old_pod = {'metadata': {'uid': 'original-pod'}}
        later_pod = {'metadata': {'uid': 'later-pod'}}
        # Kubernetes has already removed the original Job and Pod. Only the
        # watch and immutable pre-lag evidence retain that completed execution.
        probe.apply_jobs = lambda: {'later-job': later_job}
        polled = []

        def stopped(uids):
            polled.append(uids)
            self.assertEqual(uids, {'later-job'})
            return [later_pod]

        probe.stopped_apply_pods = stopped
        probe.barrier = lambda name: name
        probe.watched_apply_jobs = lambda barrier: ({'original-job'} if barrier == 'lag-unapproved-boundary' else {'original-job', 'later-job'})
        probe.sql = lambda source, query: SimpleNamespace(stdout=b'1\n2' if 'schema_migrations' in query else b'2')
        archives = []
        probe.encrypted = lambda name, payload, *args: archives.append(json.loads(payload))
        probe.inventories = lambda source: {'rows': b'recorded'}
        before, jobs, pods = probe.advance_lagged_database('source', 'artifact', {'original-job': old_job}, [old_pod], 'recipient', 'key', 'wrong')
        self.assertEqual(waits, [100])
        self.assertEqual(polled, [{'later-job'}])
        self.assertEqual(before, {'rows': b'recorded'})
        self.assertEqual(jobs, {'original-job': old_job, 'later-job': later_job})
        self.assertEqual(pods, [old_pod, later_pod])
        self.assertEqual(archives[0]['pods'], pods)
        self.assertEqual({j['metadata']['uid'] for j in archives[0]['jobs']}, set(jobs))


class RecoveryArtifactTest(unittest.TestCase):
    def test_revision_three_preserves_committed_migrations_and_adds_real_work(self):
        initial = OperatorProbe.artifact_files('migration', 1)
        intervening = OperatorProbe.artifact_files('migration', 2)
        fresh = OperatorProbe.artifact_files('migration', 3)
        self.assertEqual(len(initial), 2)
        self.assertEqual(len(intervening), 4)
        self.assertEqual(len(fresh), 6)
        for name, content in initial.items():
            self.assertEqual(content, intervening[name])
        for name, content in intervening.items():
            self.assertEqual(content, fresh[name])
        self.assertIn('ADD COLUMN resumed INTEGER NOT NULL DEFAULT 1', fresh['0000000003_resume.up.sql'])
        self.assertIn('DROP COLUMN resumed', fresh['0000000003_resume.down.sql'])

    def test_schema_after_lag_has_a_new_change_without_removing_the_committed_one(self):
        for revision in (1, 2, 3):
            sql = OperatorProbe.artifact_files('schema', revision)['schema.sql']
            self.assertEqual(sql.count('recovered INTEGER'), int(revision >= 2))
            self.assertEqual(sql.count('resumed INTEGER'), int(revision == 3))
            self.assertIn('id INTEGER PRIMARY KEY, value TEXT NOT NULL', sql)

    def test_an_undeclared_revision_cannot_silently_reuse_old_work(self):
        for family, revision in (('schema', 0), ('migration', 4), ('unknown', 1)):
            with self.subTest(family=family, revision=revision), self.assertRaises(ValueError):
                OperatorProbe.artifact_files(family, revision)


class RecoveryResultTest(unittest.TestCase):
    def test_success_requires_both_recovery_points_and_cleanup(self):
        good = {'status': 'PASS', 'functionalRestore': 'PASS', 'cleanupSucceeded': True,
                'profileRPO': {'database': 'PASS', 'operatorBase': 'PASS'}}
        result = copy.deepcopy(good)
        OperatorProbe.finalize_result(result)
        self.assertEqual(result, good)
        for defect in ('database', 'operatorBase', 'missing-rpo', 'unaccepted-kit', 'cleanup', 'missing-cleanup'):
            with self.subTest(defect=defect):
                result = copy.deepcopy(good)
                if defect in ('database', 'operatorBase'):
                    result['profileRPO'][defect] = 'FAIL'
                elif defect == 'missing-rpo':
                    result.pop('profileRPO')
                elif defect == 'unaccepted-kit':
                    result['profileRPO']['combinedOperatorRecoveryKit'] = 'Not assessed'
                elif defect == 'cleanup':
                    result['cleanupSucceeded'] = False
                else:
                    result.pop('cleanupSucceeded')
                OperatorProbe.finalize_result(result)
                self.assertEqual(result['status'], 'FAIL')
                self.assertEqual(result['functionalRestore'], 'PASS')
                self.assertTrue(result['failure'])

    def test_finalization_preserves_an_earlier_failure(self):
        report = {'status': 'FAIL', 'failure': 'Exact original approval replayed',
                  'cleanupSucceeded': True, 'profileRPO': {'database': 'PASS', 'operatorBase': 'PASS'}}
        original = copy.deepcopy(report)
        OperatorProbe.finalize_result(report)
        self.assertEqual(report, original)


class LagDiagnosisTest(unittest.TestCase):
    def setUp(self):
        self.probe = object.__new__(OperatorProbe)
        self.probe.root = Path('/unused-lag-fixture')
        self.field = 'artifact'
        self.old_source = {'ociRef': 'oci://fixture/old@sha256:' + '1' * 64}
        self.new_source = {'ociRef': 'oci://fixture/new@sha256:' + '2' * 64}
        spec = {'suspend': True, 'policy': {'apply': 'OnApproval'}, self.field: self.old_source}
        self.live = {'spec': copy.deepcopy(spec)}
        self.old = {'resource': {'spec': copy.deepcopy(spec)}}
        self.later = {'sourceField': self.field, 'reference': self.new_source['ociRef'],
                      'suspended': {'spec': {**copy.deepcopy(spec), self.field: self.new_source}},
                      'jobs': [{'metadata': {'uid': 'completed-original'}}, {'metadata': {'uid': 'completed-intervening'}}]}
        self.payloads = {}
        self.probe.report = {'backups': {}, 'checks': {}}
        for name, obj in (('operator-checkpoint', self.old), ('intervening-execution', self.later)):
            payload = json.dumps(obj).encode()
            self.payloads[name] = payload
            self.probe.report['backups'][name] = {'plaintextSHA256': db.digest(payload)}
        self.probe.command = lambda action, args: SimpleNamespace(stdout=self.payloads[Path(args[-1]).stem])
        self.probe.persist = lambda: None
        self.probe.settled = lambda phase: copy.deepcopy(self.live)
        self.probe.source_spec = lambda reference: {'ociRef': reference}
        self.probe.barrier = lambda name: name
        self.jobs = {'completed-original', 'completed-intervening'}
        self.probe.watched_apply_jobs = lambda barrier: self.jobs
        self.patches = []

        def patch(value):
            self.patches.append(copy.deepcopy(value))
            self.live['spec'].update(value['spec'])

        self.probe.patch = patch

    def diagnose(self):
        self.probe.select_diagnosed_source(Path('operator-checkpoint.age'), Path('key'), self.field)

    def test_only_the_source_changes_after_diagnosis(self):
        self.diagnose()
        self.assertEqual(self.patches, [{'spec': {self.field: self.new_source}}])
        self.assertTrue(self.live['spec']['suspend'])
        self.assertEqual(self.live['spec']['policy']['apply'], 'OnApproval')
        self.assertEqual(self.probe.report['lagDiagnosis']['controlledSourceChange'],
                         {'from': self.old_source['ociRef'], 'to': self.new_source['ociRef']})
        self.assertEqual(self.probe.lag_execution, self.later)

    def test_bad_backup_or_changed_runtime_never_enables_a_source_change(self):
        for defect in ('old-backup', 'later-backup', 'already-new', 'not-suspended', 'automatic', 'replay', 'missing-history'):
            with self.subTest(defect=defect):
                self.setUp()
                if defect in ('old-backup', 'later-backup'):
                    name = 'operator-checkpoint' if defect == 'old-backup' else 'intervening-execution'
                    self.payloads[name] += b' '
                elif defect == 'already-new':
                    self.live['spec'][self.field] = self.new_source
                elif defect == 'not-suspended':
                    self.live['spec']['suspend'] = False
                elif defect == 'automatic':
                    self.live['spec']['policy']['apply'] = 'Always'
                elif defect == 'replay':
                    self.jobs.add('unauthorized-apply')
                else:
                    self.jobs.clear()
                with self.assertRaises(RuntimeError):
                    self.diagnose()
                self.assertEqual(self.patches, [])


if __name__ == '__main__':
    unittest.main()
