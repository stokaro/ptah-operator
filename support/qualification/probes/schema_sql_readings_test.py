import itertools
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest import mock

import schema_sql_readings
from schema_sql_readings import LINE_WITNESSES, ROOT, Lab, added_postgresql_lines, contract, contract_readings, received

READINGS = ROOT / 'testdata/e2e/readings'
CONTRACT = ROOT / 'test/e2e/schema-sql-contract.json'


class SchemaSQLReadingsTest(unittest.TestCase):
    def test_the_contract_is_exactly_what_the_checked_in_readings_witness(self):
        checked_in = json.loads(CONTRACT.read_text())
        self.assertEqual(contract(READINGS, checked_in['ptahCommit'], checked_in['scope']), checked_in)

    def test_every_added_postgresql_line_witnesses_every_operation(self):
        lines = added_postgresql_lines()
        self.assertTrue(lines, 'the catalog adds no PostgreSQL line, so this checks nothing')
        readings = contract_readings('postgresql')
        for major, _ in lines:
            witnessed = {operation for name, operation in readings if name.startswith(f'postgresql-{major}-')}
            self.assertEqual(witnessed, {operation for _, operation in LINE_WITNESSES})
            for name, _ in LINE_WITNESSES:
                self.assertTrue((READINGS / f'postgresql-{major}-schema-{name}.jsonl').is_file())

    def test_statements_are_read_the_way_the_audit_reads_them(self):
        with tempfile.TemporaryDirectory() as directory:
            postgres = Path(directory) / 'postgresql.jsonl'
            postgres.write_text('\n'.join(json.dumps(row) for row in [
                {'dbname': 'ptah_audit_schema', 'remote_host': '172.19.0.3', 'message': 'statement: -- ping'},
                {'dbname': 'ptah_audit_schema', 'remote_host': '172.19.0.3', 'message': 'execute stmtcache_1: SELECT $1',
                 'detail': "Parameters: $1 = 'public'"},
                {'dbname': 'ptah_audit_schema', 'remote_host': '172.19.0.3', 'message': 'connection authorized'},
            ]) + '\n')
            self.assertEqual(list(received('postgresql', postgres)),
                             [('query', '-- ping', ''), ('query', 'SELECT $1', "Parameters: $1 = 'public'")])
            mysql = Path(directory) / 'mysql.jsonl'
            mysql.write_text('\n'.join(json.dumps({'time': '2026-10-10T00:00:00.000000', 'type': kind, 'client': 'x', 'thread': 1,
                                                   'argumentHex': text.encode().hex().upper()}) for kind, text in [
                ('Connect', 'ptah_audit@172.19.0.3 on ptah_audit_schema using TCP/IP'),
                ('Prepare', 'SELECT ? FROM t WHERE s = ?'),
                ('Execute', "SELECT 1 FROM t WHERE s = 'ptah_audit_schema'"),
                ('Quit', ''),
            ]) + '\n')
            self.assertEqual(list(received('mysql', mysql)),
                             [('Prepare', 'SELECT ? FROM t WHERE s = ?', ''),
                              ('Execute', "SELECT 1 FROM t WHERE s = '{{database}}'", '')])


class LabCleanupTest(unittest.TestCase):
    """What a Lab leaves on the daemon, read from the commands it sends one."""

    def setUp(self):
        self.calls = []
        self.refuse = lambda verb: False
        patcher = mock.patch.object(schema_sql_readings, 'run', self.docker)
        patcher.start()
        self.addCleanup(patcher.stop)
        # Each reading of the clock is a minute later, so a wait ends at once.
        clock = mock.patch.object(schema_sql_readings, 'time')
        self.time = clock.start()
        self.addCleanup(clock.stop)
        self.time.time.side_effect = itertools.count(step=60)

    def docker(self, argv, data=None, check=True):
        verb = argv[3:]
        self.calls.append(verb)
        refused = self.refuse(verb)
        if refused and check:
            raise RuntimeError(f'{verb[0]} refused')
        return SimpleNamespace(returncode=1 if refused else 0, stdout=b'', stderr=b'')

    def called(self, *verb):
        return [call for call in self.calls if call[:len(verb)] == list(verb)]

    def server(self):
        """The container the Lab asked the daemon to run, by the name it sent."""
        started = self.called('run', '-d')
        self.assertEqual(len(started), 1, self.calls)
        return started[0][started[0].index('--name') + 1]

    def assert_removed_everything(self, lab):
        self.assertEqual(self.calls[-2:], [['rm', '-f', '-v', self.server()], ['network', 'rm', lab.name]])

    def test_a_start_that_fails_removes_what_it_created(self):
        probe = lambda verb: verb[0] == 'exec' and '-i' not in verb
        truncate = lambda verb: verb[:2] == ['exec', '-i']
        run_detached = lambda verb: verb[:2] == ['run', '-d']
        for name, engine, refuse, error in [
            ('postgresql never answers', 'postgresql', probe, 'did not start'),
            ('mysql never answers', 'mysql', probe, 'did not start'),
            ('mysql answers and refuses the first statement', 'mysql', truncate, 'exec refused'),
            ('a docker run that fails after creating the container', 'postgresql', run_detached, 'run refused'),
        ]:
            with self.subTest(name):
                self.calls.clear()
                self.refuse = refuse
                lab = Lab('ctx', engine, 'executor')
                with self.assertRaisesRegex(RuntimeError, error):
                    with lab:
                        self.fail('the block ran after a failed start')
                self.assert_removed_everything(lab)

    def test_an_interrupted_wait_removes_what_it_created(self):
        self.refuse = lambda verb: verb[0] == 'exec'
        self.time.sleep.side_effect = KeyboardInterrupt
        lab = Lab('ctx', 'postgresql', 'executor')
        with self.assertRaises(KeyboardInterrupt):
            with lab:
                self.fail('the block ran after a failed start')
        self.assert_removed_everything(lab)

    def test_a_network_that_failed_leaves_no_container_to_remove(self):
        self.refuse = lambda verb: verb[:2] == ['network', 'create']
        lab = Lab('ctx', 'postgresql', 'executor')
        with self.assertRaisesRegex(RuntimeError, 'network refused'):
            with lab:
                self.fail('the block ran after a failed start')
        self.assertEqual(self.called('run'), [])
        self.assertEqual(self.called('rm'), [])

    def test_a_start_that_works_removes_nothing_until_the_block_ends(self):
        lab = Lab('ctx', 'postgresql', 'executor')
        with lab:
            self.assertEqual(self.called('rm'), [])
            self.assertEqual(self.called('network', 'rm'), [])
        self.assert_removed_everything(lab)


if __name__ == '__main__':
    unittest.main()
