import copy
import unittest

from result_lost_ack import verify_evidence


class LostAcknowledgmentEvidenceTests(unittest.TestCase):
    def fixture(self):
        receipt = {'Name': 'attempt-complete', 'UID': 'receipt',
                   'Digest': 'sha256:' + 'a' * 64, 'Size': 120}
        return {
            'commit': 'b' * 40, 'procedureSHA256': 'c' * 64,
            'namespace': 'acceptance', 'resourceUID': 'resource',
            'jobUID': 'job', 'podUID': 'pod', 'intentUID': 'intent',
            'receiptUID': 'receipt', 'receiptName': 'attempt-complete',
            'payloadDigest': receipt['Digest'], 'payloadBytes': 120,
            'credentialCertificateDigest': 'sha256:' + 'd' * 64,
            'binding': {'kind': 'PtahMigration', 'operation': 'migration-apply',
                        'namespace': 'acceptance', 'uid': 'resource',
                        'jobUID': 'job', 'podUID': 'pod', 'generation': 1},
            'proxy': {'clientCertificateDigest': 'sha256:' + 'd' * 64,
                      'preflights': 1, 'dropped': True, 'released': True,
                      'attempts': [
                          {'receivedAt': '2026-10-02T11:00:01Z',
                           'receipt': copy.deepcopy(receipt)},
                          {'receivedAt': '2026-10-02T11:00:02Z',
                           'receipt': copy.deepcopy(receipt)}]},
            'converged': True, 'applyJobs': 1, 'podRestarts': 0,
            'executionPods': 1, 'runnerAPICredentials': False,
            'podPhase': 'Succeeded', 'databaseBeforeExecution': '0:1:false',
            'databaseBeforeRelease': '1:1:true', 'databaseAfterRelease': '1:1:true',
            'completedAt': '2026-10-02T11:00:03+00:00',
            'conditions': [{'type': 'Ready', 'status': 'True',
                            'reason': 'HistoryMatched', 'observedGeneration': 1}],
        }

    def test_accepts_one_execution_and_identical_receipts(self):
        self.assertEqual(verify_evidence(self.fixture()), {
            'deliveries': 2, 'sqlExecutions': 1, 'receiptUID': 'receipt'})

    def test_requires_calibrated_witness_for_each_explicit_engine(self):
        for engine, expected, storage in [
                ('PostgreSQL', '0:1:true', 'PostgreSQL sequence'),
                ('MySQL', '0:1:true', 'InnoDB')]:
            with self.subTest(engine=engine):
                value = self.fixture()
                value.update(evidenceVersion=2, engine=engine,
                             rollbackCalibration={'afterRollback': expected,
                                                  'afterReset': '0:1:false',
                                                  'storageEngine': storage})
                self.assertEqual(verify_evidence(value)['sqlExecutions'], 1)
                for field, replacement in [('afterRollback', '0:1:false'),
                                           ('afterReset', expected)]:
                    bad = copy.deepcopy(value)
                    bad['rollbackCalibration'][field] = replacement
                    with self.assertRaises(ValueError):
                        verify_evidence(bad)
                for bad_engine in ['', 'SQLite']:
                    bad = copy.deepcopy(value)
                    bad['engine'] = bad_engine
                    with self.assertRaises(ValueError):
                        verify_evidence(bad)
                if engine == 'MySQL':
                    value['rollbackCalibration']['storageEngine'] = 'MyISAM'
                    with self.assertRaises(ValueError):
                        verify_evidence(value)

    def test_receiver_restart_requires_new_processes_before_redelivery(self):
        value = self.fixture()
        value.update(evidenceVersion=3, engine='MySQL',
                     rollbackCalibration={'afterRollback': '0:1:true', 'afterReset': '0:1:false',
                                          'storageEngine': 'InnoDB'},
                     receiverRestart={'before': ['old-a', 'old-b'], 'after': ['new-a', 'new-b'],
                                      'oldPodsAbsent': True, 'newPodsReady': True,
                                      'oldPodsAbsentAt': '2026-10-02T11:00:01.1+00:00',
                                      'newPodsReadyAt': '2026-10-02T11:00:01.2+00:00',
                                      'receiptUIDBeforeRestart': 'receipt', 'databaseBeforeRestart': '1:1:true'})
        value['proxy'].update(retryGateEnabled=True, retryWaits=1,
                              retryResumedAt='2026-10-02T11:00:01.3Z')
        self.assertEqual(verify_evidence(value)['sqlExecutions'], 1)
        mutations = [
            ('receiverRestart', 'before', []),
            ('receiverRestart', 'after', ['new-a', 'new-a']),
            ('receiverRestart', 'after', ['old-a', 'new-b']),
            ('receiverRestart', 'after', ['', 'new-b']),
            ('receiverRestart', 'oldPodsAbsent', False),
            ('receiverRestart', 'newPodsReady', False),
            ('receiverRestart', 'receiptUIDBeforeRestart', 'other'),
            ('receiverRestart', 'databaseBeforeRestart', '0:1:false'),
            ('receiverRestart', 'oldPodsAbsentAt', '2026-10-02T11:00:00+00:00'),
            ('receiverRestart', 'newPodsReadyAt', '2026-10-02T11:00:01.4+00:00'),
            ('proxy', 'retryGateEnabled', False),
            ('proxy', 'retryWaits', 0),
            ('proxy', 'retryResumedAt', '2026-10-02T11:00:02.1Z'),
        ]
        for section, field, replacement in mutations:
            with self.subTest(section=section, field=field, value=replacement):
                bad = copy.deepcopy(value)
                bad[section][field] = replacement
                with self.assertRaises(ValueError):
                    verify_evidence(bad)

    def test_refuses_absent_fault_replay_or_changed_identity(self):
        mutations = [
            ('absent commit', ['commit'], ''),
            ('absent procedure', ['procedureSHA256'], ''),
            ('wrong family', ['binding', 'kind'], 'PtahSchema'),
            ('read-only result', ['binding', 'operation'], 'migration-status'),
            ('different namespace', ['binding', 'namespace'], 'other'),
            ('different resource', ['binding', 'uid'], 'other'),
            ('different Job', ['binding', 'jobUID'], 'other'),
            ('different Pod', ['binding', 'podUID'], 'other'),
            ('missing intent', ['intentUID'], ''),
            ('missing convergence', ['converged'], False),
            ('replacement Apply', ['applyJobs'], 2),
            ('replacement Pod', ['executionPods'], 2),
            ('runner API credentials', ['runnerAPICredentials'], True),
            ('restarted runner', ['podRestarts'], 1),
            ('unfinished runner', ['podPhase'], 'Running'),
            ('preexisting SQL', ['databaseBeforeExecution'], '1:1:true'),
            ('no SQL', ['databaseBeforeRelease'], '0:1:false'),
            ('replayed SQL', ['databaseAfterRelease'], '2:2:true'),
            ('rolled-back replay', ['databaseAfterRelease'], '1:2:true'),
            ('no lost ACK', ['proxy', 'dropped'], False),
            ('no release', ['proxy', 'released'], False),
            ('no preflight', ['proxy', 'preflights'], 0),
            ('another execution preflight', ['proxy', 'preflights'], 2),
            ('missing client', ['proxy', 'clientCertificateDigest'], ''),
            ('changed client', ['credentialCertificateDigest'], 'sha256:' + 'e' * 64),
            ('no deliveries', ['proxy', 'attempts'], []),
            ('changed receipt', ['proxy', 'attempts', 1, 'receipt', 'UID'], 'other'),
            ('another stored receipt', ['receiptUID'], 'other'),
            ('another stored payload', ['payloadDigest'], 'sha256:' + 'e' * 64),
            ('wrong stored size', ['payloadBytes'], 0),
            ('no retry interval', ['proxy', 'attempts', 1, 'receivedAt'],
             '2026-10-02T11:00:01Z'),
            ('unfinished acceptance', ['completedAt'], '2026-10-02T11:00:01+00:00'),
            ('no conditions', ['conditions'], []),
            ('stale convergence', ['conditions', 0, 'observedGeneration'], 2),
        ]
        for name, path, replacement in mutations:
            with self.subTest(name=name):
                value = self.fixture()
                target = value
                for key in path[:-1]:
                    target = target[key]
                target[path[-1]] = replacement
                with self.assertRaises(ValueError):
                    verify_evidence(value)


if __name__ == '__main__':
    unittest.main()
