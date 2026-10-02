import base64
import copy
import hashlib
import json
import pathlib
import unittest

from result_upload_budget import SECOND, EXPECTED_SCHEMA, EMPTY_SCHEMA, verify_evidence
from result_first_harvest import publication


class UploadBudgetTests(unittest.TestCase):
    def fixture(self):
        def stamp(seconds):
            return int(seconds * SECOND)
        checks = []
        for phase, index, status, second in [
                ('initial', 0, 204, -2), ('initial', 1, 204, -1),
                ('leader-busy', 0, 503, 2), ('follower-free', 1, 204, 3),
                ('both-busy', 0, 503, 42), ('both-busy', 1, 503, 43),
                ('leader-recovered', 0, 204, 122), ('follower-still-busy', 1, 503, 123),
                ('recovered', 0, 204, 162), ('recovered', 1, 204, 163)]:
            checks.append({'phase': phase, 'index': index, 'status': status, 'receiverUID': 'receiver-' + str(index),
                           'sentNs': stamp(second), 'responseNs': stamp(second + 0.1), 'retryAfter': '1' if status == 503 else None})
        samples = []
        for second in range(166):
            samples.append({'observedNs': stamp(second), 'uid': 'lease', 'holder': 'operation', 'epoch': 'epoch',
                            'duration': 1020, 'renewTime': '2026-10-02T12:%02d:%02dZ' % divmod(second // 5 * 5, 60)})
        execution = {'resourceUID': 'original', 'generation': 1, 'jobUID': 'job', 'podUID': 'pod', 'receiptUID': 'receipt',
                     'databaseWitness': '1:1:true', 'applyJobs': 1, 'executionPods': 1, 'podRestarts': 0,
                     'podPhase': 'Succeeded', 'runnerAPICredentials': False,
                     'conditions': [{'type': 'Ready', 'status': 'True', 'reason': 'HistoryMatched', 'observedGeneration': 1}]}
        peer = dict(execution, resourceUID='peer', jobUID='peer-job', podUID='peer-pod', receiptUID='peer-receipt')
        return {'evidenceVersion': 1, 'engine': 'PostgreSQL', 'leaderUID': 'receiver-0', 'leaderUnchanged': True,
                'budgets': {'uploadSeconds': 120, 'responseSeconds': 5, 'renewalSeconds': 15},
                'slowUploads': [{'receiverUID': 'receiver-' + str(i), 'sentNs': stamp(start), 'responseNs': stamp(start + 120),
                                 'status': 408, 'declaredBytes': 4096, 'sentBytes': 1} for i, start in enumerate([1, 41])],
                'checks': checks, 'leaseSamples': samples, 'leaseEpoch': 'epoch', 'peerCreatedNs': stamp(4),
                'peerConvergedNs': stamp(40), 'publicationsBeforeRelease': 0, 'serviceRestored': True,
                'calibration': {'afterRollback': '0:1:true', 'afterReset': '0:1:false'},
                'peerCalibration': {'afterRollback': '0:1:true', 'afterReset': '0:1:false'},
                'executions': {'original': execution, 'peer': peer}}

    def test_accepts_bounded_uploads_progress_and_renewal(self):
        self.assertEqual(verify_evidence(self.fixture()),
                         {'timedOutUploads': 2, 'renewals': 34, 'independentSQLExecutions': 2})

    def schema_fixture(self):
        value = self.fixture()
        value.update(evidenceVersion=2, family='PtahSchema')
        value['calibration'] = {'before': copy.deepcopy(EMPTY_SCHEMA)}
        value['peerCalibration'] = {'before': copy.deepcopy(EMPTY_SCHEMA)}
        for row in value['executions'].values():
            row['databaseWitness'] = copy.deepcopy(EXPECTED_SCHEMA)
            row['conditions'][0]['reason'] = 'InSync'
        return value

    def test_accepts_schema_convergence_without_claiming_sql_counters(self):
        self.assertEqual(verify_evidence(self.schema_fixture()),
                         {'timedOutUploads': 2, 'renewals': 34, 'independentSchemasConverged': 2})

    def test_refuses_schema_status_without_actual_database_effects(self):
        mutations = [
            (['calibration', 'before'], EXPECTED_SCHEMA),
            (['peerCalibration', 'before'], EXPECTED_SCHEMA),
            (['executions', 'peer', 'databaseWitness'], EMPTY_SCHEMA),
            (['executions', 'original', 'databaseWitness', 'columns'], ['id:bigint:NO']),
            (['executions', 'original', 'databaseWitness', 'columns'], ['id:bigint:YES', 'email:text:NO']),
            (['executions', 'original', 'databaseWitness', 'primaryKeyColumns'], []),
            (['executions', 'peer', 'databaseWitness', 'primaryKeyColumns'], ['email']),
            (['executions', 'peer', 'conditions', 0, 'reason'], 'HistoryMatched'),
            (['family'], 'PtahMigration'),
        ]
        for path, replacement in mutations:
            with self.subTest(path=path):
                value = self.schema_fixture()
                target = value
                for key in path[:-1]:
                    target = target[key]
                target[path[-1]] = replacement
                with self.assertRaises((ValueError, KeyError)):
                    verify_evidence(value)

    def test_refuses_vacuous_or_unbounded_evidence(self):
        mutations = [
            (['slowUploads'], []),
            (['slowUploads', 1, 'receiverUID'], 'receiver-0'),
            (['slowUploads', 0, 'status'], 400),
            (['slowUploads', 0, 'responseNs'], 20 * SECOND),
            (['slowUploads', 1, 'responseNs'], 170 * SECOND),
            (['slowUploads', 0, 'sentBytes'], 4096),
            (['leaderUID'], 'receiver-1'),
            (['peerConvergedNs'], 130 * SECOND),
            (['checks'], []),
            (['checks', 2, 'retryAfter'], None),
            (['checks', 2, 'responseNs'], 10 * SECOND),
            (['checks', 5, 'sentNs'], 39 * SECOND),
            (['checks', 6, 'sentNs'], 119 * SECOND),
            (['leaseSamples'], []),
            (['leaseSamples', 0, 'observedNs'], 2 * SECOND),
            (['leaseSamples', 5, 'holder'], 'other-holder'),
            (['leaseSamples', 5, 'observedNs'], 12 * SECOND),
            (['leaseEpoch'], 'other-epoch'),
            (['leaderUnchanged'], False),
            (['publicationsBeforeRelease'], 1),
            (['serviceRestored'], False),
            (['calibration', 'afterRollback'], '0:1:false'),
            (['peerCalibration', 'afterReset'], '0:1:true'),
            (['executions', 'peer', 'resourceUID'], 'original'),
            (['executions', 'peer', 'databaseWitness'], '1:2:true'),
            (['executions', 'original', 'applyJobs'], 2),
            (['executions', 'original', 'runnerAPICredentials'], True),
            (['executions', 'peer', 'conditions', 0, 'observedGeneration'], 0),
        ]
        for path, replacement in mutations:
            with self.subTest(path=path):
                value = self.fixture()
                target = value
                for key in path[:-1]:
                    target = target[key]
                target[path[-1]] = replacement
                with self.assertRaises(ValueError):
                    verify_evidence(value)

    def test_refuses_a_lease_that_stops_after_many_renewals(self):
        value = self.fixture()
        for row in value['leaseSamples'][100:]:
            row['renewTime'] = value['leaseSamples'][99]['renewTime']
        with self.assertRaisesRegex(ValueError, 'stopped renewing'):
            verify_evidence(value)

    def test_refuses_a_long_renewal_gap(self):
        value = self.fixture()
        for row in value['leaseSamples'][50:70]:
            row['renewTime'] = value['leaseSamples'][49]['renewTime']
        with self.assertRaisesRegex(ValueError, 'renewal exceeded'):
            verify_evidence(value)

    def test_replays_native_engine_evidence_and_actual_publications(self):
        root = pathlib.Path(__file__).resolve().parents[1] / 'evidence/result-upload-budget-2026-10-02'
        summary = json.loads((root / 'summary.json').read_text())
        self.assertEqual(set(summary['engines']), {'PostgreSQL', 'MySQL'})
        for engine, entry in summary['engines'].items():
            with self.subTest(engine=engine):
                directory = root / entry['directory']
                self.assertEqual(set(entry['files']), {'upload-budget.json', 'original-publication.json',
                                                      'peer-publication.json', 'installation.json'})
                for name, expected in entry['files'].items():
                    self.assertEqual(hashlib.sha256((directory / name).read_bytes()).hexdigest(), expected)
                proof = json.loads((directory / 'upload-budget.json').read_text())
                install = json.loads((directory / 'installation.json').read_text())
                self.assertEqual(proof['engine'], engine)
                self.assertEqual(verify_evidence(proof), entry['verification'])
                self.assertEqual(proof['procedureSHA256'], summary['procedures'])
                self.assertEqual(proof['commit'], install['operatorRevision'])
                self.assertEqual(proof['commit'], summary['operatorRevision'])
                self.assertEqual(set(install['receivers']), {row['receiverUID'] for row in proof['slowUploads']})
                self.assertTrue(all(install[k] for k in ['serviceRestored', 'temporaryLabelRemoved', 'credentialGateRemoved']))
                self.assertEqual(len(install['nodes']), 4)
                self.assertTrue(all(n['containerLogMaxSize'] == '10Mi' for n in install['nodes']))
                for key, execution in proof['executions'].items():
                    records = json.loads((directory / (key + '-publication.json')).read_text())
                    self.assertTrue(records)
                    self.assertTrue(all(r['spec']['type'] in ('intent', 'chunk', 'complete') for r in records.values()))
                    intent, complete, payload = publication(records, execution['jobUID'])
                    binding = json.loads(base64.b64decode(intent['spec']['data']))['binding']
                    self.assertEqual(binding['uid'], execution['resourceUID'])
                    self.assertEqual(binding['podUID'], execution['podUID'])
                    self.assertEqual(binding['generation'], execution['generation'])
                    self.assertEqual(complete['metadata']['uid'], execution['receiptUID'])
                    self.assertEqual(json.loads(payload)['migrationRun']['outcome'], 'applied')

    def test_replays_both_schema_engine_cases(self):
        root = pathlib.Path(__file__).resolve().parents[1] / 'evidence/result-schema-upload-budget-2026-10-02'
        summary = json.loads((root / 'summary.json').read_text())
        self.assertEqual(summary['family'], 'PtahSchema')
        self.assertEqual(set(summary['engines']), {'PostgreSQL', 'MySQL'})
        inventory_bytes = (root / 'operation-inventory.json').read_bytes()
        self.assertEqual(hashlib.sha256(inventory_bytes).hexdigest(), summary['operationInventorySHA256'])
        inventory = json.loads(inventory_bytes)
        self.assertEqual(inventory['operatorRevision'], summary['operatorRevision'])
        self.assertEqual(set(inventory['engines']), {'PostgreSQL', 'MySQL'})
        expected_operations = {'PtahSchema/' + op for op in ('resolve', 'verify', 'observe', 'plan', 'apply')}
        expected_operations |= {'PtahMigration/' + op for op in ('resolve', 'verify', 'migration-history', 'migration-apply')}
        for item in inventory['engines'].values():
            self.assertEqual(set(item['operations']), expected_operations)
            for operation, row in item['operations'].items():
                self.assertEqual(row['childExitCode'], 0)
                self.assertGreater(row['payloadBytes'], 0)
                self.assertGreaterEqual(row['memberCount'], 3)
                self.assertTrue(all(row[k] for k in ('resourceUID', 'jobUID', 'podUID', 'receiptUID', 'intentUID')))
                intent, complete, payload = publication(row['publication'], row['jobUID'])
                binding = json.loads(base64.b64decode(intent['spec']['data']))['binding']
                self.assertEqual(binding['kind'] + '/' + binding['operation'], operation)
                self.assertEqual(binding['uid'], row['resourceUID'])
                self.assertEqual(binding['podUID'], row['podUID'])
                self.assertEqual(intent['metadata']['uid'], row['intentUID'])
                self.assertEqual(complete['metadata']['uid'], row['receiptUID'])
                self.assertEqual(len(payload), row['payloadBytes'])
                self.assertEqual('sha256:' + hashlib.sha256(payload).hexdigest(), row['payloadDigest'])
                self.assertEqual(json.loads(payload)['childExitCode'], 0)
            self.assertEqual(len(item['access']), 4)
            for row in item['access']:
                attributes = row['request']['resourceAttributes']
                self.assertEqual(attributes['namespace'], row['namespace'])
                self.assertEqual(attributes['verb'], 'get')
                self.assertEqual(row['request']['user'], inventory['managerIdentity'])
                self.assertEqual(row['status']['allowed'], row['allowed'])
                self.assertFalse(row['status'].get('evaluationError'))
                if row['resource'] == 'pods/log':
                    self.assertEqual(attributes['resource'], 'pods')
                    self.assertEqual(attributes['subresource'], 'log')
                    self.assertFalse(attributes.get('name'))
                else:
                    self.assertEqual(attributes['resource'], 'ptahresultrecords')
                    self.assertEqual(attributes['group'], 'operator.ptah.run')
            namespaces = {row['namespace'] for row in item['access']}
            self.assertEqual(len(namespaces), 2)
            for namespace in namespaces:
                self.assertEqual({(row['verb'], row['resource'], row['allowed']) for row in item['access']
                                  if row['namespace'] == namespace},
                                 {('get', 'pods/log', False), ('get', 'ptahresultrecords.operator.ptah.run', True)})
        for engine, entry in summary['engines'].items():
            with self.subTest(engine=engine):
                directory = root / entry['directory']
                self.assertEqual(set(entry['files']), {'upload-budget.json', 'original-publication.json',
                                                      'peer-publication.json', 'installation.json'})
                for name, expected in entry['files'].items():
                    self.assertEqual(hashlib.sha256((directory / name).read_bytes()).hexdigest(), expected)
                proof = json.loads((directory / 'upload-budget.json').read_text())
                install = json.loads((directory / 'installation.json').read_text())
                self.assertEqual(proof['family'], 'PtahSchema')
                self.assertEqual(proof['engine'], engine)
                self.assertEqual(verify_evidence(proof), entry['verification'])
                self.assertEqual(proof['procedureSHA256'], summary['procedures'])
                self.assertEqual(proof['commit'], install['operatorRevision'])
                self.assertEqual(proof['commit'], summary['operatorRevision'])
                self.assertEqual(set(install['receivers']), {row['receiverUID'] for row in proof['slowUploads']})
                self.assertTrue(all(install[k] for k in ['serviceRestored', 'temporaryLabelRemoved', 'credentialGateRemoved']))
                self.assertTrue(install['nodes'])
                self.assertTrue(all(n['containerLogMaxSize'] == '10Mi' for n in install['nodes']))
                for execution in ('original', 'peer'):
                    row = proof['executions'][execution]
                    records = json.loads((directory / (execution + '-publication.json')).read_text())
                    self.assertTrue(records)
                    self.assertTrue(all(r['spec']['type'] in ('intent', 'chunk', 'complete') for r in records.values()))
                    intent, complete, payload = publication(records, row['jobUID'])
                    binding = json.loads(base64.b64decode(intent['spec']['data']))['binding']
                    self.assertEqual(binding['kind'], 'PtahSchema')
                    self.assertEqual(binding['operation'], 'apply')
                    self.assertEqual(binding['uid'], row['resourceUID'])
                    self.assertEqual(binding['podUID'], row['podUID'])
                    self.assertEqual(binding['generation'], row['generation'])
                    self.assertEqual(complete['metadata']['uid'], row['receiptUID'])
                    self.assertEqual(json.loads(payload)['childExitCode'], 0)


if __name__ == '__main__':
    unittest.main()
