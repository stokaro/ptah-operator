import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from capacity_fixtures import row_inventory
from capacity_retention_fault import RetentionFault
from capacity_workload import Workload, generate, sha
import capacity_workload_test as workload_test


class RetentionFaultTests(unittest.TestCase):
    def fixture(self, parent, engine='PostgreSQL'):
        root = Path(parent) / 'inputs'
        bundle = generate(root, engine)
        for a in bundle['artifacts'].values():
            source = root / a['path']
            files = sorted(source.iterdir()) if source.is_dir() else [source]
            a['sourceFiles'] = [{'path': p.relative_to(root).as_posix(), 'sha256': sha(p.read_bytes())} for p in files]
            a['reference'] = 'oci://fixture/artifact@sha256:' + sha(a['path'].encode())
        (root / 'bundle.json').write_text(json.dumps(bundle))
        workload = Workload(workload_test.WorkloadTests().state(parent, engine), root)
        identities = {family: {'name': f'capacity-{family}-000', 'namespace': 'owned-0', 'uid': family, 'generation': 2}
                      for family in ('schema', 'migration')}
        f = RetentionFault(workload, Path(parent) / 'fault', 1, identities)
        def read(kind, name, ns):
            family = 'schema' if kind == 'ptahschemas' else 'migration'
            field = 'desired' if family == 'schema' else 'artifact'
            return {'metadata': {'name': name, 'namespace': ns, 'uid': family, 'generation': 2}, 'status': {'observedGeneration': 2},
                    'spec': {'policy': {'apply': 'OnApproval'}, field: {'ociRef': bundle['artifacts'][bundle[family+'s'][0]['artifacts'][1]]['reference']},
                             'target': {'urlFrom': {'name': 'capacity-db-' + ('0' if family == 'schema' else '10')}}}}
        return f, read

    def test_native_fault_preserves_rows_and_uses_only_the_last_fixture_column(self):
        for engine in ('PostgreSQL', 'MySQL'):
            with self.subTest(engine=engine), tempfile.TemporaryDirectory() as parent:
                f, read = self.fixture(parent, engine)
                calls = []
                def ptah(row, argv):
                    calls.append(argv)
                    if argv[1] == 'down':
                        self.assertEqual(argv[-3:], ['--target', '2', '--confirm'])
                        return b''
                    applied = [1, 2, 3] if len(calls) == 1 else [1, 2]
                    return json.dumps({'applied_migrations': applied, 'pending_migrations': [] if len(calls) == 1 else [3]}).encode()
                def sql(row, query):
                    return row_inventory(row['index']) if query.startswith(b'SELECT id') else b'0\n'
                with patch.object(f.workload.bootstrap, 'read', side_effect=read), \
                        patch.object(f.workload, 'ptah', side_effect=ptah), \
                        patch.object(f.workload, 'sql', side_effect=sql) as sql_call, \
                        patch.object(f.workload, 'forwarded', side_effect=lambda action: action()):
                    f.prepare()
                self.assertTrue((f.root / 'prepared.json').exists())
                drop = [c.args[1] for c in sql_call.call_args_list if c.args[1].startswith(b'DROP')]
                self.assertEqual(drop, [b'DROP TABLE capacity_payload_000_r01;' if engine == 'MySQL' else b'DROP TABLE capacity_payload_000;'])
                self.assertEqual(len(calls), 3)

    def test_changed_target_refuses_before_any_sql(self):
        for mode in ('UID', 'name', 'namespace', 'generation', 'policy', 'artifact', 'target', 'suspended', 'deleting'):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as parent:
                f, read = self.fixture(parent)
                def changed(*args):
                    o = read(*args)
                    if mode == 'name': o['metadata']['name'] = 'other'
                    if mode == 'namespace': o['metadata']['namespace'] = 'other'
                    if mode == 'UID': o['metadata']['uid'] = 'other'
                    if mode == 'generation': o['metadata']['generation'] += 1
                    if mode == 'policy': o['spec']['policy']['apply'] = 'Always'
                    if mode == 'artifact': o['spec']['desired' if args[0] == 'ptahschemas' else 'artifact']['ociRef'] = 'other'
                    if mode == 'target': o['spec']['target']['urlFrom']['name'] = 'other'
                    if mode == 'suspended': o['spec']['suspend'] = True
                    if mode == 'deleting': o['metadata']['deletionTimestamp'] = 'now'
                    return o
                with patch.object(f.workload.bootstrap, 'read', side_effect=changed), patch.object(f.workload, 'sql') as sql, self.assertRaises(ValueError):
                    f.prepare()
                sql.assert_not_called()

    def test_unknown_migration_bytes_cannot_be_rolled_back(self):
        with tempfile.TemporaryDirectory() as parent:
            f, _ = self.fixture(parent)
            f.down.write_text('DROP TABLE capacity_rows;\n')
            with self.assertRaisesRegex(ValueError, 'reversible fixture'):
                RetentionFault(f.workload, f.root, f.round, f.identities)

    def test_waiting_requires_the_exact_native_statement_and_holder(self):
        for engine in ('PostgreSQL', 'MySQL'):
            for mode in ('valid', 'unrelated statement', 'not waiting', 'another database', 'duplicate', 'same session'):
                with self.subTest(engine=engine, mode=mode), tempfile.TemporaryDirectory() as parent:
                    f, _ = self.fixture(parent, engine)
                    f.root.mkdir()
                    lock = {'session': 1, 'database': f.migration['database'], 'username': f.migration['username'], 'seconds': 30}
                    if mode == 'another database': lock['database'] = 'other'
                    (f.root / 'lock-ready.json').write_text(json.dumps(lock))
                    row = {'session': 2, 'query': f.up.read_text(), 'state': 'Waiting for table metadata lock', 'blockers': [1]}
                    if mode == 'unrelated statement': row['query'] = 'ALTER TABLE another ADD COLUMN x INT;'
                    if mode == 'not waiting': row.update(state='executing', blockers=[])
                    if mode == 'same session': row['session'] = 1
                    raw = (json.dumps(row) + '\n') * (2 if mode == 'duplicate' else 1)
                    with patch.object(f.workload, 'sql', return_value=raw.encode()):
                        if mode in ('another database', 'duplicate'):
                            with self.assertRaises(ValueError): f.waiting()
                        else:
                            self.assertEqual(f.waiting(), mode == 'valid')
                    self.assertEqual((f.root / 'apply-waiting.json').exists(), mode == 'valid')

    def test_lock_is_bounded_on_the_database_server(self):
        for engine in ('PostgreSQL', 'MySQL'):
            with tempfile.TemporaryDirectory() as parent:
                f, _ = self.fixture(parent, engine)
                sql = f.lock_sql()
                self.assertIn('capacity_rows', sql)
                self.assertIn('SLEEP(30)' if engine == 'MySQL' else 'pg_sleep(30)', sql)
                self.assertIn('UNLOCK TABLES' if engine == 'MySQL' else 'COMMIT', sql)
                self.assertIn('CONNECTION_ID()' if engine == 'MySQL' else 'pg_backend_pid()', sql)


if __name__ == '__main__':
    unittest.main()
