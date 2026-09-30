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
