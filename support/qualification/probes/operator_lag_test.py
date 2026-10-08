import copy
import base64
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
        reading = json.loads((Path(__file__).parent / 'testdata/workload-history.json').read_text())
        job, pod = copy.deepcopy((reading['jobs'][0], reading['pods'][0]))
        later_job, later_pod = copy.deepcopy((job, pod))
        later_job['metadata']['uid'] = 'completed-intervening'
        later_pod['metadata']['uid'] = 'intervening-pod'
        later_pod['metadata']['ownerReferences'][0]['uid'] = later_job['metadata']['uid']
        resource = {'metadata': {'uid': reading['identity']['uid']}, 'spec': copy.deepcopy(spec)}
        self.old = {'resource': resource, 'jobs': [job]}
        self.later = {'sourceField': self.field, 'reference': self.new_source['ociRef'],
                      'suspended': {'metadata': copy.deepcopy(resource['metadata']),
                                    'spec': {**copy.deepcopy(spec), self.field: self.new_source}},
                      'jobs': [job, later_job], 'pods': [pod, later_pod],
                      'plan': {'metadata': {'uid': 'intervening-plan'}, 'spec': {'artifactDigest': 'new'}},
                      'approval': {'metadata': {'uid': 'intervening-approval'}, 'spec': {'planUID': 'intervening-plan'},
                                   'status': {'conditions': [{'type': 'Consumed', 'status': 'True'}]}}}
        dependencies = [{'kind': kind, 'apiVersion': 'v1', 'metadata': {'name': name, 'uid': name + '-uid'},
                         'data': {'fixture': 'original'}}
                        for kind, name in [('Secret', 'restore-target'), ('Secret', 'restore-registry'),
                            ('Secret', 'restore-pull'), ('ConfigMap', 'restore-policy'),
                            ('Service', 'restore-database'), ('Service', 'restore-registry'),
                            ('EndpointSlice', 'restore-database-endpoint'), ('EndpointSlice', 'restore-registry-endpoint')]]
        self.old['namespaceState'] = {'namespace': {'metadata': {'uid': 'original-namespace'}},
            'dependencies': dependencies, 'plans': [], 'approvals': [], 'chunks': [], 'jobs': [job], 'pods': [pod],
            'preservedContract': [{'kind': 'CustomResourceDefinition', 'uid': 'contract'}], 'durableResults': {'enabled': False}}
        fresh = copy.deepcopy(self.old['namespaceState'])
        fresh.update(plans=[self.later['plan']], approvals=[self.later['approval']], jobs=[later_job], pods=[later_pod])
        self.update = {'startedAt': '2026-10-08T19:05:00+00:00',
                       'resource': copy.deepcopy(self.later['suspended']), 'namespaceState': fresh}
        self.payloads = {}
        self.probe.report = {'backups': {}, 'checks': {}}
        self.seal_archives()
        self.probe.command = lambda action, args: SimpleNamespace(stdout=self.payloads[Path(args[-1]).stem])
        self.probe.persist = lambda: None
        self.probe.settled = lambda phase: copy.deepcopy(self.live)
        self.probe.source_spec = lambda reference: {'ociRef': reference}
        self.probe.barrier = lambda name: name
        self.jobs = {j['metadata']['uid'] for j in self.later['jobs']}
        self.probe.watched_apply_jobs = lambda barrier: self.jobs
        self.patches = []

        def patch(value):
            self.patches.append(copy.deepcopy(value))
            self.live['spec'].update(value['spec'])

        self.probe.patch = patch

    def seal_archives(self):
        for name, obj in (('operator-checkpoint', self.old), ('intervening-execution', self.later)):
            payload = json.dumps(obj).encode()
            self.payloads[name] = payload
            self.probe.report['backups'][name] = {'plaintextSHA256': db.digest(payload)}
        self.update.update(baseSHA256=db.digest(self.payloads['operator-checkpoint']),
                           executionSHA256=db.digest(self.payloads['intervening-execution']))
        payload = json.dumps(self.update).encode()
        self.payloads['operator-update'] = payload
        self.probe.report['backups']['operator-update'] = {'plaintextSHA256': db.digest(payload)}

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
        for defect in ('old-backup', 'later-backup', 'update-backup', 'already-new', 'not-suspended', 'automatic', 'replay', 'missing-history'):
            with self.subTest(defect=defect):
                self.setUp()
                if defect in ('old-backup', 'later-backup', 'update-backup'):
                    name = {'old-backup': 'operator-checkpoint', 'later-backup': 'intervening-execution',
                            'update-backup': 'operator-update'}[defect]
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

    def test_valid_checksums_do_not_hide_an_incomplete_update(self):
        for defect in ('missing-dependency', 'changed-secret', 'duplicate-dependency', 'changed-contract',
                       'missing-inventory', 'different-namespace', 'different-resource', 'active-operation',
                       'unresolved-run', 'unresolved-copy', 'automatic-update', 'changed-source', 'missing-plan',
                       'changed-approval', 'unconsumed-approval', 'missing-job', 'missing-pod', 'running-pod',
                       'missing-finish', 'different-delivery', 'naive-time'):
            with self.subTest(defect=defect):
                self.setUp()
                fresh = self.update['namespaceState']
                if defect == 'missing-dependency': fresh['dependencies'].pop()
                elif defect == 'changed-secret': fresh['dependencies'][0]['data']['fixture'] = 'rotated'
                elif defect == 'duplicate-dependency': fresh['dependencies'][-1] = fresh['dependencies'][0]
                elif defect == 'changed-contract': fresh['preservedContract'][0]['uid'] = 'changed'
                elif defect == 'missing-inventory': fresh.pop('durableResults')
                elif defect == 'different-namespace': fresh['namespace']['metadata']['uid'] = 'another'
                elif defect == 'different-resource': self.update['resource']['metadata']['uid'] = 'another'
                elif defect == 'active-operation': self.update['resource']['status'] = {'activeOperation': {'id': 'running'}}
                elif defect == 'unresolved-run': self.update['resource']['status'] = {'unresolvedRun': {'operationID': 'unknown'}}
                elif defect == 'unresolved-copy': self.update['resource']['metadata']['annotations'] = {'operator.ptah.run/unresolved-run': '{}'}
                elif defect == 'automatic-update': self.update['resource']['spec']['policy']['apply'] = 'Always'
                elif defect == 'changed-source': self.update['resource']['spec'][self.field] = self.old_source
                elif defect == 'missing-plan': fresh['plans'] = []
                elif defect == 'changed-approval': fresh['approvals'] = [copy.deepcopy(self.later['approval'])]; fresh['approvals'][0]['spec']['planUID'] = 'other'
                elif defect == 'unconsumed-approval': self.later['approval']['status']['conditions'] = []
                elif defect == 'missing-job': fresh['jobs'] = []
                elif defect == 'missing-pod': fresh['pods'] = []
                elif defect == 'running-pod': self.later['pods'][1]['status']['phase'] = 'Running'
                elif defect == 'missing-finish': self.later['pods'][1]['status']['containerStatuses'][0]['state']['terminated'].pop('finishedAt')
                elif defect == 'different-delivery': fresh['durableResults']['enabled'] = True
                elif defect == 'naive-time': self.update['startedAt'] = '2026-10-08T19:05:00'
                self.seal_archives()
                with self.assertRaises((RuntimeError, KeyError)):
                    self.diagnose()
                self.assertEqual(self.patches, [])

    def test_individually_valid_archives_must_belong_to_the_same_kit(self):
        for field in ('baseSHA256', 'executionSHA256'):
            with self.subTest(field=field):
                self.setUp()
                self.update[field] = 'f' * 64
                payload = json.dumps(self.update).encode()
                self.payloads['operator-update'] = payload
                self.probe.report['backups']['operator-update']['plaintextSHA256'] = db.digest(payload)
                with self.assertRaisesRegex(RuntimeError, 'checkpoint or execution binding differs'):
                    self.diagnose()
                self.assertEqual(self.patches, [])

    def test_a_fresh_recovery_kit_preserves_the_failed_base_age(self):
        self.diagnose()
        report = self.probe.report
        report.update(status='PASS', functionalRestore='PASS', cleanupSucceeded=True, timing='operator-lag',
                      operatorBaseBackupAgeUpperBoundSeconds=380,
                      operatorUpdateCompletedAt='2026-10-08T19:05:10+00:00',
                      databaseBackupStartedAt='2026-10-08T19:05:11+00:00', lossInjectedAt='2026-10-08T19:05:20+00:00',
                      profileRPO={'database': 'PASS', 'operatorBase': 'FAIL', 'combinedOperatorRecoveryKit': 'PASS'})
        self.assertEqual(OperatorProbe.lagged_recovery_point(report), 20)
        OperatorProbe.finalize_result(report)
        self.assertEqual(report['status'], 'PASS')
        self.assertEqual(report['profileRPO']['operatorBase'], 'FAIL')
        self.assertEqual(report['operatorBaseBackupAgeUpperBoundSeconds'], 380)
        for defect in ('missing-update', 'different-update', 'different-base', 'missing-diagnosis', 'after-loss',
                       'after-database', 'old-point', 'naive-point', 'not-lag'):
            with self.subTest(defect=defect):
                bad = copy.deepcopy(report)
                if defect == 'missing-update': bad['backups'].pop('operator-update')
                elif defect == 'different-update': bad['lagDiagnosis']['operatorUpdateSHA256'] = 'f' * 64
                elif defect == 'different-base': bad['lagDiagnosis']['operatorCheckpointSHA256'] = 'f' * 64
                elif defect == 'missing-diagnosis': bad.pop('lagDiagnosis')
                elif defect == 'after-loss': bad['operatorUpdateCompletedAt'] = '2026-10-08T19:05:21+00:00'
                elif defect == 'after-database': bad['operatorUpdateCompletedAt'] = '2026-10-08T19:05:12+00:00'
                elif defect == 'old-point': bad['lagDiagnosis']['recoveryPointStartedAt'] = '2026-10-08T19:00:19+00:00'
                elif defect == 'naive-point': bad['lagDiagnosis']['recoveryPointStartedAt'] = '2026-10-08T19:05:00'
                elif defect == 'not-lag': bad['timing'] = 'idle'
                OperatorProbe.finalize_result(bad)
                self.assertEqual(bad['status'], 'FAIL')
                self.assertEqual(bad['functionalRestore'], 'PASS')

    def test_later_recovery_inventory_reconstructs_the_actual_durable_apply(self):
        path = Path(__file__).parent / 'testdata/results/result-network-2026-10-02/ptah-result-network-pg-migration-workflow.json'
        reading = json.loads(path.read_text())
        job_uid = reading['verification']['publications']['migration-apply']['jobUID']
        for defect in ('none', 'missing-receipt', 'wrong-resource', 'missing-trust', 'different-journal'):
            with self.subTest(defect=defect):
                self.setUp()
                for resource in (self.old['resource'], self.later['suspended'], self.update['resource']):
                    resource['metadata'].update(uid=reading['resource']['metadata']['uid'],
                                                namespace=reading['resource']['metadata']['namespace'])
                    resource['kind'] = 'PtahMigration'
                self.later['jobs'][1]['metadata']['uid'] = job_uid
                self.later['pods'][1]['metadata']['ownerReferences'][0]['uid'] = job_uid
                self.jobs = {j['metadata']['uid'] for j in self.later['jobs']}
                self.old['namespaceState']['durableResults'] = {'enabled': True}
                material = {'enabled': True, 'records': copy.deepcopy(list(reading['publications'].values())),
                    'trust': {'metadata': {'uid': 'trust'}, 'data': {k: 'eA==' for k in
                        ('tls.crt', 'tls.key', 'ca.crt', 'client-ca.crt', 'client-ca.key', 'client-trust.crt')}},
                    'enrollmentPolicy': {'metadata': {'uid': 'policy'}, 'data': {'enrollment.json': '{}'}},
                    'journal': {'data': {'rotation.json': base64.b64encode(json.dumps(
                        {'projectionUID': 'trust', 'policyUID': 'policy'}).encode()).decode()}}}
                self.update['namespaceState']['durableResults'] = material
                if defect == 'missing-receipt': material['records'] = [r for r in material['records'] if r['spec']['type'] != 'complete']
                elif defect == 'wrong-resource':
                    for resource in (self.old['resource'], self.later['suspended'], self.update['resource']):
                        resource['metadata']['uid'] = 'foreign-resource'
                elif defect == 'missing-trust': material['trust']['data'].pop('client-ca.key')
                elif defect == 'different-journal': material['trust']['metadata']['uid'] = 'replacement'
                self.seal_archives()
                if defect == 'none': self.diagnose()
                else:
                    with self.assertRaises((RuntimeError, ValueError, KeyError)):
                        self.diagnose()
                    self.assertEqual(self.patches, [])


if __name__ == '__main__':
    unittest.main()
