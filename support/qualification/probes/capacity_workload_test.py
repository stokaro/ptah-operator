"""Refuse mismatched database targets and incomplete workload preparation."""

import base64
import copy
import json
import hashlib
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
            with patch.dict('os.environ', PTAH_OCI_REGISTRY='localhost:1234', E2E_REGISTRY_HOST='registry:5000'), \
                    patch.object(workload, 'command', return_value=('Digest: sha256:' + 'a'*64 + '\n').encode()), \
                    patch.object(workload, 'forwarded', side_effect=lambda action: action()), \
                    patch.object(workload, 'ptah', return_value=b''), patch.object(workload, 'sql', side_effect=sql):
                with self.assertRaisesRegex(ValueError, 'database rows differ'):
                    workload.prepare()
            self.assertTrue((root / 'initial-rows-19.tsv').exists())
            self.assertFalse((root / 'catalog.json').exists())

    def test_final_database_checks_refuse_loss_wrong_defaults_and_history(self):
        for failure in (None, 'rows', 'schema-default', 'history', 'migration-default'):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as parent:
                root = Path(parent) / 'inputs'
                workload = Workload(self.state(parent), root)
                bundle = generate(root, 'PostgreSQL')
                (root / 'bundle.json').write_text(json.dumps(bundle))
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
                        # Independently pinned outputs from the native input
                        # calibration: changed slots use round one.
                        repeated = ((121, 10506, 42063) if slot < 5 else (241, 21013, 84133))[slot % 3]
                        result = []
                        for table in range(tables):
                            value = '<' * (repeated // tables + (table < repeated % tables)) + ('r01' if slot < 5 else 'r00')
                            result.append(f'{table}\t{len(value)}\t{hashlib.md5(value.encode(), usedforsecurity=False).hexdigest()}\n')
                        return ''.join(result).encode()
                    if query.startswith(b'SELECT COUNT(*)'):
                        return b'1\n' if failure == 'migration-default' and slot == 19 else b'0\n'
                    raise AssertionError('unexpected SQL')
                def ptah(row, args):
                    slot = row['index'] - 10
                    count = 3 if slot < 4 else 33 if slot == 4 else 32 if slot < 7 else 128
                    if failure == 'history' and slot == 9:
                        count -= 1
                    return json.dumps({'applied_migrations': list(range(1, count + 1)), 'pending_migrations': []}).encode()
                with patch.object(workload, 'sql', side_effect=sql), patch.object(workload, 'ptah', side_effect=ptah), \
                        patch.object(workload, 'forwarded', side_effect=lambda action: action()):
                    if failure:
                        with self.assertRaises(ValueError):
                            workload.verify(5)
                        self.assertFalse((root / 'database-verification.json').exists())
                    else:
                        result = workload.verify(5)
                        self.assertEqual(visited, set(range(20)))
                        self.assertEqual(len(result['slots']), 20)
                        self.assertEqual(len({r['sha256'] for r in result['slots']}), 20)
                        self.assertTrue((root / 'database-verification.json').exists())

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
