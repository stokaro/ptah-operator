"""Refuse mismatched database targets and incomplete workload preparation."""

import base64
import copy
from contextlib import contextmanager
import json
import hashlib
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from capacity_fixtures import row_inventory
from capacity_workload import Workload, generate


class WorkloadTests(unittest.TestCase):
    def state(self, directory, engine='PostgreSQL'):
        rows = []
        for slot in range(20):
            rows.append({'index': slot, 'family': 'schema' if slot < 10 else 'migration',
                         'namespace': 'owned-' + str(slot % 2), 'username': f'capacity_user_{slot:03d}',
                         'database': f'capacity{slot:03d}' if engine == 'MySQL' else f'capacity_{slot:03d}'})
        value = {'engine': engine, 'schemas': 10, 'migrations': 10, 'databases': rows,
                 'fixtureNamespace': 'owned-fixture', 'runID': 'test'}
        path = Path(directory) / 'state.json'
        path.write_text(json.dumps(value))
        return path

    def test_signer_refuses_wrong_digest_or_failed_cryptographic_verification(self):
        with tempfile.TemporaryDirectory() as parent:
            root = Path(parent) / 'inputs'
            root.mkdir()
            public = Path(parent) / 'signing.pub'
            public.write_text('public key fixture')
            workload = Workload(self.state(parent), root)
            identity = 'sha256:' + 'a' * 64
            reference = 'oci://localhost:1234/schemas/capacity-inputs@' + identity
            claims = json.dumps([{'critical': {'image': {'docker-manifest-digest': identity}}}]).encode()
            with patch.dict('os.environ', CAPACITY_SIGNING_KEY='/signing.key', CAPACITY_SIGNING_PUBLIC_KEY=str(public),
                            PTAH_OCI_USERNAME='fixture', PTAH_OCI_PASSWORD='private'):
                for reading in (b'[]', claims.replace(b'a' * 64, b'b' * 64), RuntimeError('signature refused')):
                    with self.subTest(reading=reading), patch.object(workload, 'command', side_effect=[b'', reading]):
                        with self.assertRaises((ValueError, RuntimeError)):
                            workload.sign_artifact(reference)
                with patch.object(workload, 'command', side_effect=[b'', claims]) as command:
                    result = workload.sign_artifact(reference)
                    self.assertEqual(result['manifestDigest'], identity)
                    calls = command.call_args_list
                    self.assertIn('--registry-referrers-mode=oci-1-1', calls[0].args[1])
                    self.assertIn('--use-signing-config=false', calls[0].args[1])
                    self.assertIn(str(public), calls[1].args[1])
                    self.assertEqual(calls[0].args[2]['COSIGN_EXPERIMENTAL'], '1')
                self.assertFalse(list(root.glob('signer-auth-*')))

    def test_generation_covers_every_slot_and_declared_update(self):
        with tempfile.TemporaryDirectory() as parent:
            for engine in ('PostgreSQL', 'MySQL'):
                root = Path(parent) / engine
                bundle = generate(root, engine)
                self.assertEqual(len(bundle['artifacts']), 51)
                self.assertEqual(len(bundle['schemas']), 10)
                self.assertEqual(len(bundle['migrations']), 10)
                self.assertEqual([x['historyLength'] for x in bundle['migrations']], [2]*4 + [32]*3 + [128]*3)
                for family in ('schemas', 'migrations'):
                    for row in bundle[family]:
                        self.assertEqual(len(row['artifacts']), 10)
                        self.assertEqual(len(set(row['artifacts'])), 10 if row['slot'] < 5 else 1)
                        for name in row['artifacts']:
                            artifact = bundle['artifacts'][name]
                            path = root / artifact['path']
                            self.assertTrue(path.exists())
                            if artifact['kind'] == 'migration':
                                self.assertEqual(len(list(path.glob('*.up.sql'))), artifact['versions'])
                with self.assertRaises(FileExistsError):
                    generate(root, engine)

    def test_credentials_bind_engine_host_port_database_and_login(self):
        with tempfile.TemporaryDirectory() as parent:
            for engine in ('PostgreSQL', 'MySQL'):
                workload = Workload(self.state(parent, engine), Path(parent) / 'inputs')
                row = workload.databases[0]
                password = 'a' * 48
                host = workload.deployment + '.owned-fixture.svc.cluster.local'
                url = (f'mysql://{row["username"]}:{password}@tcp({host}:3306)/{row["database"]}' if engine == 'MySQL' else
                       f'postgres://{row["username"]}:{password}@{host}:5432/{row["database"]}?sslmode=disable')
                def read(raw):
                    return patch.object(workload.bootstrap, 'read', return_value={'data': {'url': base64.b64encode(raw.encode()).decode()}})
                with read(url):
                    self.assertEqual(workload.credential(row), password)
                for bad in (url.replace('owned-fixture', 'another'), url.replace('5432', '5433') if engine == 'PostgreSQL' else url.replace('3306', '3307'),
                            url.replace(row['database'], row['database'] + '1'), url.replace('user_000', 'user_001'),
                            url + '&other=true', url.replace('postgres://', 'mysql://') if engine == 'PostgreSQL' else url.replace('mysql://', 'postgres://')):
                    with self.subTest(engine=engine, bad=bad), read(bad), self.assertRaises(ValueError):
                        workload.credential(row)

    def test_prepare_never_emits_catalog_for_failed_population(self):
        with tempfile.TemporaryDirectory() as parent:
            root = Path(parent) / 'inputs'
            workload = Workload(self.state(parent), root)
            def sql(row, query):
                if query.startswith(b'SELECT'):
                    return row_inventory(row['index']) if row['index'] != 19 else b''
                return b''
            with patch.dict('os.environ', PTAH_OCI_REGISTRY='localhost:1234', E2E_REGISTRY_HOST='registry:5000', PTAH_OCI_USERNAME='test', PTAH_OCI_PASSWORD='test'), \
                    patch('capacity_workload.ArtifactReader') as reader, \
                    patch.object(workload, 'command', return_value=('Digest: sha256:' + 'a'*64 + '\n').encode()), \
                    patch.object(workload, 'forwarded', side_effect=lambda action: action()), \
                    patch.object(workload, 'ptah', return_value=b''), patch.object(workload, 'sql', side_effect=sql):
                reader.return_value.read.return_value = {'test': 'isolated population refusal'}
                with self.assertRaisesRegex(ValueError, 'database rows differ'):
                    workload.prepare()
            self.assertEqual(reader.return_value.read.call_count, 51)
            self.assertTrue((root / 'initial-rows-19.tsv').exists())
            self.assertFalse((root / 'catalog.json').exists())

    def test_failed_readback_stops_before_database_population(self):
        with tempfile.TemporaryDirectory() as parent:
            root = Path(parent) / 'inputs'
            workload = Workload(self.state(parent), root)
            with patch.dict('os.environ', PTAH_OCI_REGISTRY='localhost:1234', E2E_REGISTRY_HOST='registry:5000', PTAH_OCI_USERNAME='test', PTAH_OCI_PASSWORD='test'), \
                    patch('capacity_workload.ArtifactReader') as reader, \
                    patch.object(workload, 'command', return_value=('Digest: sha256:' + 'a'*64 + '\n').encode()), \
                    patch.object(workload, 'forwarded') as forwarded, patch.object(workload, 'sql') as sql:
                reader.return_value.read.side_effect = ValueError('blob digest mismatch')
                with self.assertRaisesRegex(ValueError, 'blob digest mismatch'):
                    workload.prepare()
                forwarded.assert_not_called()
                sql.assert_not_called()
                self.assertFalse((root / 'catalog.json').exists())

    @contextmanager
    def readings(self, workload, engine, version, failure=None):
        visited = set()

        def sql(row, query):
            slot = row['index']
            if query.startswith(b'SELECT id, payload'):
                visited.add(slot)
                return b'' if failure == 'rows' and slot == 19 else row_inventory(slot)
            if query.startswith(b'BEGIN;'):
                if failure == 'schema-default' and slot == 9:
                    return b'0\t0\twrong\n'
                tables = (1, 4, 16)[slot % 3]
                current = version if slot < 5 else 0
                # Independently pinned outputs from native calibration. Returning
                # round one when round nine was requested must fail this verifier.
                repeated = {0: (241, 21013, 84133), 1: (121, 10506, 42063),
                            9: (181, 14446, 57839)}[current][slot % 3]
                if engine == 'MySQL':
                    repeated = (498, 42088, 168510)[slot % 3]
                result = []
                for table in range(tables):
                    # The stale-reading control deliberately returns an older
                    # result even though the query names the new round's table.
                    if failure != 'stale':
                        suffix = f'_r{current:02d}' if engine == 'MySQL' else ''
                        self.assertIn(f'INSERT INTO capacity_payload_{table:03d}{suffix} (id)'.encode(), query)
                    value = '<' * (repeated // tables + (table < repeated % tables)) + f'r{current:02d}'
                    result.append(f'{table}\t{len(value)}\t{hashlib.md5(value.encode(), usedforsecurity=False).hexdigest()}\n')
                return ''.join(result).encode()
            if query.startswith(b'SELECT COUNT(*)'):
                return b'1\n' if failure == 'migration-default' and slot == 19 else b'0\n'
            raise AssertionError('unexpected SQL')

        def ptah(row, args):
            slot = row['index'] - 10
            count = 2 + version if slot < 4 else 32 + version if slot == 4 else 32 if slot < 7 else 128
            if failure == 'history' and slot == 9:
                count -= 1
            pending = [count + 1] if failure == 'pending' and slot == 9 else []
            return json.dumps({'applied_migrations': list(range(1, count + 1)), 'pending_migrations': pending}).encode()

        with patch.object(workload, 'sql', side_effect=sql), patch.object(workload, 'ptah', side_effect=ptah), \
                patch.object(workload, 'forwarded', side_effect=lambda action: action()):
            yield visited

    def test_final_database_checks_refuse_loss_wrong_defaults_and_history(self):
        for engine in ('PostgreSQL', 'MySQL'):
            for version in (0, 1, 9):
                for failure in (None, 'rows', 'schema-default', 'history', 'pending', 'migration-default'):
                    with self.subTest(engine=engine, version=version, failure=failure), tempfile.TemporaryDirectory() as parent:
                        root = Path(parent) / 'inputs'
                        workload = Workload(self.state(parent, engine), root)
                        bundle = generate(root, engine)
                        (root / 'bundle.json').write_text(json.dumps(bundle))
                        checkpoint = f'round-{version}' if version != 1 else None
                        destination = root / 'checkpoints' / checkpoint if checkpoint else root
                        with self.readings(workload, engine, version, failure) as visited:
                            if failure:
                                with self.assertRaises(ValueError):
                                    workload.verify(5, version, checkpoint)
                                self.assertFalse((destination / 'database-verification.json').exists())
                            else:
                                result = workload.verify(5, version, checkpoint)
                                self.assertEqual(visited, set(range(20)))
                                self.assertEqual(len(result['slots']), 20)
                                self.assertEqual(len({r['sha256'] for r in result['slots']}), 20)
                                self.assertEqual(result['round'], version)
                                self.assertEqual(result['bundleSHA256'], hashlib.sha256((root / 'bundle.json').read_bytes()).hexdigest())
                                self.assertEqual([r['round'] for r in result['slots']], ([version]*5 + [0]*5)*2)
                                self.assertEqual(len(list(destination.glob('final-*'))), 50)
                                self.assertTrue((destination / 'database-verification.json').exists())

    def test_checkpoints_preserve_prior_results_and_refuse_stale_rounds(self):
        for engine in ('PostgreSQL', 'MySQL'):
            with self.subTest(engine=engine), tempfile.TemporaryDirectory() as parent:
                root = Path(parent) / 'inputs'
                workload = Workload(self.state(parent, engine), root)
                (root / 'bundle.json').write_text(json.dumps(generate(root, engine)))
                for version in (0, 1, 9):
                    with self.readings(workload, engine, version):
                        workload.verify(5, version, f'round-{version}')
                before = {str(p.relative_to(root)): p.read_bytes() for p in root.glob('checkpoints/**/*') if p.is_file()}
                with patch.object(workload, 'sql') as sql, patch.object(workload, 'forwarded') as forwarded:
                    with self.assertRaises(FileExistsError):
                        workload.verify(5, 9, 'round-9')
                    sql.assert_not_called()
                    forwarded.assert_not_called()
                after = {str(p.relative_to(root)): p.read_bytes() for p in root.glob('checkpoints/**/*') if p.is_file()}
                self.assertEqual(before, after)
                with self.readings(workload, engine, 1, 'stale'):
                    with self.assertRaisesRegex(ValueError, 'incorrect executable defaults'):
                        workload.verify(5, 9, 'stale')
                self.assertFalse((root / 'checkpoints/stale/database-verification.json').exists())
                self.assertTrue((root / 'checkpoints/stale/final-defaults-00.tsv').exists())

    def test_legacy_retry_cannot_overwrite_partial_or_complete_evidence(self):
        with tempfile.TemporaryDirectory() as parent:
            root = Path(parent) / 'inputs'
            workload = Workload(self.state(parent), root)
            (root / 'bundle.json').write_text(json.dumps(generate(root, 'PostgreSQL')))
            for filename in ('final-rows-00.tsv', 'database-verification.json'):
                original = root / filename
                original.write_bytes(b'previous evidence')
                with patch.object(workload, 'forwarded') as forwarded, self.assertRaises(FileExistsError):
                    workload.verify(5)
                forwarded.assert_not_called()
                self.assertEqual(original.read_bytes(), b'previous evidence')
                original.unlink()

    def test_unchanged_batch_requires_initial_version_even_at_round_nine(self):
        with tempfile.TemporaryDirectory() as parent:
            root = Path(parent) / 'inputs'
            workload = Workload(self.state(parent), root)
            (root / 'bundle.json').write_text(json.dumps(generate(root, 'PostgreSQL')))
            with self.readings(workload, 'PostgreSQL', 0):
                result = workload.verify(0, 9, 'unchanged')
            self.assertEqual([r['round'] for r in result['slots']], [0]*20)

    def test_repeated_port_forwards_keep_distinct_logs_and_stop_on_failure(self):
        with tempfile.TemporaryDirectory() as parent:
            root = Path(parent) / 'inputs'
            root.mkdir()
            workload = Workload(self.state(parent), root)
            workload.bootstrap.env['E2E_KUBECONFIG'] = 'test-config'
            processes = []

            class Forward:
                def __init__(self, args, stdout, stderr):
                    self.stopped = False
                    os.write(stdout.fileno(), b'Forwarding from 127.0.0.1:12345 -> 5432\n')
                    processes.append(self)

                def poll(self):
                    return 0 if self.stopped else None

                def terminate(self):
                    self.stopped = True

                def wait(self, timeout):
                    return 0

            def fail():
                raise ValueError('database check failed')

            with patch('capacity_workload.subprocess.Popen', Forward):
                self.assertEqual(workload.forwarded(lambda: workload.port), 12345)
                with self.assertRaisesRegex(ValueError, 'database check failed'):
                    workload.forwarded(fail)
            self.assertIsNone(workload.port)
            self.assertEqual(len(processes), 2)
            self.assertTrue(all(p.stopped for p in processes))
            self.assertEqual(len(list(root.glob('database-forward-*.private.log'))), 2)

    def test_invalid_checkpoint_and_round_fail_before_any_database_command(self):
        with tempfile.TemporaryDirectory() as parent:
            root = Path(parent) / 'inputs'
            workload = Workload(self.state(parent), root)
            (root / 'bundle.json').write_text(json.dumps(generate(root, 'PostgreSQL')))
            with patch.object(workload, 'forwarded') as forwarded:
                for version in (-1, 10, True, '9', 1.5):
                    with self.subTest(version=version), self.assertRaises(ValueError):
                        workload.verify(5, version, 'invalid')
                for checkpoint in ('', '../escape', 'sub/dir', 'A', 'a'*81, 42):
                    with self.subTest(checkpoint=checkpoint), self.assertRaises(ValueError):
                        workload.verify(5, 9, checkpoint)
                for changed in (False, 1, 5.0):
                    with self.subTest(changed=changed), self.assertRaises(ValueError):
                        workload.verify(changed, 9, 'invalid')
                forwarded.assert_not_called()
            self.assertFalse((root / 'checkpoints').exists())

    def test_missing_reordered_or_duplicate_database_slots_are_refused(self):
        with tempfile.TemporaryDirectory() as parent:
            path = self.state(parent)
            valid = json.loads(path.read_text())
            for change in (lambda v: v['databases'].pop(), lambda v: v['databases'].reverse(),
                           lambda v: v['databases'].__setitem__(19, v['databases'][0])):
                bad = copy.deepcopy(valid)
                change(bad)
                path.write_text(json.dumps(bad))
                with self.assertRaises(ValueError):
                    Workload(path, Path(parent) / 'inputs')


if __name__ == '__main__':
    unittest.main()
