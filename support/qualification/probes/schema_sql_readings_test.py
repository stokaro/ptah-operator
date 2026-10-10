import json
from pathlib import Path
import tempfile
import unittest

from schema_sql_readings import ROOT, contract, received

READINGS = ROOT / 'testdata/e2e/readings'
CONTRACT = ROOT / 'test/e2e/schema-sql-contract.json'


class SchemaSQLReadingsTest(unittest.TestCase):
    def test_the_contract_is_exactly_what_the_checked_in_readings_witness(self):
        checked_in = json.loads(CONTRACT.read_text())
        self.assertEqual(contract(READINGS, checked_in['ptahCommit'], checked_in['scope']), checked_in)

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


if __name__ == '__main__':
    unittest.main()
