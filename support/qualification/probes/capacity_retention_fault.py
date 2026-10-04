#!/usr/bin/env python3
"""Inject a reversible retention fault into two owned capacity databases.

No Kubernetes status or migration journal is fabricated. Native Ptah rolls back
one fixture migration; the operator must apply it again under a real approval.
"""

import argparse
import datetime
import json
import os
from pathlib import Path
import subprocess
import sys

from capacity_fixtures import payload_table, verify_row_inventory
from capacity_workload import Workload, sha


def retain(path, value):
    with Path(path).open('x') as stream:
        json.dump(value, stream, indent=2)
        stream.write('\n')


def statement(value):
    return ' '.join(value.strip().rstrip(';').split())


class RetentionFault:
    def __init__(self, workload, directory, round_number, identities):
        self.workload = workload
        self.root = Path(directory)
        self.round = round_number
        self.identities = identities
        if type(round_number) is not int or not 0 <= round_number <= 9:
            raise ValueError('fault round is outside the populated catalog')
        bundle_raw = (workload.root / 'bundle.json').read_bytes()
        self.bundle = json.loads(bundle_raw)
        if self.bundle['engine'] != workload.engine:
            raise ValueError('fault engine differs from its bundle')
        self.bundle_digest = sha(bundle_raw)
        self.schema = workload.databases[0]
        self.migration = workload.databases[10]
        if (self.schema['index'], self.migration['index']) != (0, 10):
            raise ValueError('fault must use the declared fleet slots')
        self.artifact = self.bundle['artifacts'][self.bundle['migrations'][0]['artifacts'][round_number]]
        self.version = self.artifact['versions']
        if self.version < 2:
            raise ValueError('fault would roll back the row table')
        self.migrations = workload.root / self.artifact['path']
        self.up = self.migrations / f'{self.version:010d}_capacity_{self.version:03d}.up.sql'
        self.down = self.migrations / f'{self.version:010d}_capacity_{self.version:03d}.down.sql'
        if (self.up.read_text() != f'ALTER TABLE capacity_rows ADD COLUMN history_{self.version:03d} INTEGER NOT NULL DEFAULT {self.version};\n' or
                self.down.read_text() != f'ALTER TABLE capacity_rows DROP COLUMN history_{self.version:03d};\n'):
            raise ValueError('fault migration is not the reversible fixture column')
        sources = {row['path']: row['sha256'] for row in self.artifact['sourceFiles']}
        for path in (self.up, self.down):
            if sources.get(path.relative_to(workload.root).as_posix()) != sha(path.read_bytes()):
                raise ValueError('fault migration differs from the published bytes')

    def check_consumers(self):
        for family, row in (('schema', self.schema), ('migration', self.migration)):
            wanted = self.identities[family]
            name = 'capacity-' + family + '-000'
            obj = self.workload.bootstrap.read('ptah' + family + 's', name, row['namespace'])
            meta, spec = obj['metadata'], obj['spec']
            source = 'desired' if family == 'schema' else 'artifact'
            reference = self.bundle['artifacts'][self.bundle[family + 's'][0]['artifacts'][self.round]]['reference']
            if (wanted['name'] != name or wanted['namespace'] != row['namespace'] or
                    meta['name'] != name or meta['namespace'] != row['namespace'] or meta['uid'] != wanted['uid'] or meta['generation'] != wanted['generation'] or
                    obj['status']['observedGeneration'] != meta['generation'] or meta.get('deletionTimestamp') or
                    spec['policy']['apply'] != 'OnApproval' or spec.get('suspend', False) or
                    spec[source]['ociRef'] != reference or spec['target']['urlFrom']['name'] != 'capacity-db-' + str(row['index'])):
                raise ValueError('fault target identity, policy or artifact changed')

    def history(self):
        raw = self.workload.ptah(self.migration, ['migrations', 'status', '--migrations-dir', str(self.migrations),
                                                 '--dir-format', 'ptah', '--verify-sum', '--json'])
        return json.loads(raw)

    def prepare(self):
        self.root.mkdir(mode=0o700)
        self.check_consumers()
        # Capture every row in each affected database before changing only the
        # empty payload table and the last fixture-generated default column.
        for row in (self.schema, self.migration):
            raw = self.workload.sql(row, b'SELECT id, payload FROM capacity_rows ORDER BY id;')
            verify_row_inventory(raw, row['index'])
            (self.root / f'before-rows-{row["index"]:02d}.tsv').write_bytes(raw)
        table = payload_table('mysql' if self.workload.mysql else 'postgresql', 0, self.round)
        if self.workload.sql(self.schema, f'SELECT COUNT(*) FROM {table};'.encode()) != b'0\n':
            raise ValueError('refusing to remove a nonempty payload fixture')
        def rollback():
            before = self.history()
            if before.get('applied_migrations') != list(range(1, self.version + 1)) or before.get('pending_migrations') not in (None, []):
                raise ValueError('fault requires the complete original history')
            retain(self.root / 'before-history.json', before)
            self.workload.ptah(self.migration, ['migrations', 'down', '--migrations-dir', str(self.migrations),
                                                '--dir-format', 'ptah', '--verify-sum', '--target', str(self.version-1), '--confirm'])
            after = self.history()
            retain(self.root / 'after-down-history.json', after)
            if after.get('applied_migrations') != list(range(1, self.version)) or after.get('pending_migrations') != [self.version]:
                raise ValueError('native rollback did not leave exactly one pending migration')
        self.workload.forwarded(rollback)
        self.workload.sql(self.schema, f'DROP TABLE {table};'.encode())
        retain(self.root / 'prepared.json', {'bundleSHA256': self.bundle_digest, 'round': self.round,
                                           'schemaTable': table, 'migrationVersion': self.version, 'identities': self.identities})

    def lock_sql(self):
        if self.workload.mysql:
            return ("SET SESSION lock_wait_timeout=10; LOCK TABLES capacity_rows WRITE; "
                    "SELECT CONNECTION_ID(), UTC_TIMESTAMP(6); DO SLEEP(30); "
                    "SELECT UTC_TIMESTAMP(6); UNLOCK TABLES;\n")
        return ("BEGIN; SET LOCAL lock_timeout='10s'; SET LOCAL statement_timeout='45s'; "
                "LOCK TABLE capacity_rows IN ACCESS EXCLUSIVE MODE; "
                "SELECT pg_backend_pid(), clock_timestamp() AT TIME ZONE 'UTC'; "
                "SELECT pg_sleep(30); SELECT clock_timestamp() AT TIME ZONE 'UTC'; COMMIT;\n")

    def lock(self):
        # The server releases this lock after thirty seconds even if the client
        # transport disappears. Cleanup never relies on killing an arbitrary DB session.
        w = self.workload
        if w.mysql:
            script = 'IFS= read -r MYSQL_PWD; export MYSQL_PWD; exec mysql --unbuffered --protocol=TCP -h 127.0.0.1 -u "$1" --batch --skip-column-names "$2"'
        else:
            script = 'IFS= read -r PGPASSWORD; export PGPASSWORD; exec psql -X -qAt -F "\t" -h 127.0.0.1 -U "$1" -d "$2" -v ON_ERROR_STOP=1'
        argv = ['kubectl', '--kubeconfig', w.bootstrap.env['E2E_KUBECONFIG'], '-n', w.fixture, 'exec', '-i', 'deployment/' + w.deployment,
                '--', 'sh', '-c', script, 'retention-lock', self.migration['username'], self.migration['database']]
        with (self.root / 'lock.stderr.private').open('xb') as error:
            process = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=error)
            try:
                process.stdin.write((w.credential(self.migration) + '\n' + self.lock_sql()).encode())
                process.stdin.close()
                first = process.stdout.readline().decode().strip().split('\t')
                if len(first) != 2 or not first[0].isdigit():
                    raise ValueError('database did not confirm an acquired lock')
                start = datetime.datetime.fromisoformat(first[1])
                retain(self.root / 'lock-ready.json', {'session': int(first[0]), 'database': self.migration['database'],
                                                      'username': self.migration['username'], 'startedAtUTC': start.isoformat(), 'seconds': 30})
                output = process.stdout.read().decode().strip().splitlines()
                code = process.wait(timeout=10)
                if code or len(output) != 1:
                    raise ValueError('database lock did not finish cleanly')
                end = datetime.datetime.fromisoformat(output[0])
                seconds = (end-start).total_seconds()
                if not 30 <= seconds < 60:
                    raise ValueError('database did not hold the declared thirty-second delay')
                retain(self.root / 'lock-finished.json', {'session': int(first[0]), 'finishedAtUTC': end.isoformat(), 'seconds': seconds})
            finally:
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=5)

    def waiting(self):
        lock = json.loads((self.root / 'lock-ready.json').read_text())
        if lock['database'] != self.migration['database'] or lock['username'] != self.migration['username']:
            raise ValueError('lock belongs to another target')
        if self.workload.mysql:
            query = ("SELECT JSON_OBJECT('session',ID,'query',INFO,'state',STATE) FROM information_schema.PROCESSLIST "
                     f"WHERE USER='{self.migration['username']}' AND DB=DATABASE() AND ID <> CONNECTION_ID() AND ID <> {int(lock['session'])};")
        else:
            query = ("SELECT json_build_object('session',pid,'query',query,'blockers',pg_blocking_pids(pid)) FROM pg_stat_activity "
                     "WHERE datname=current_database() AND usename=current_user AND pid<>pg_backend_pid() "
                     f"AND {int(lock['session'])}=ANY(pg_blocking_pids(pid));")
        raw = self.workload.sql(self.migration, query.encode())
        matches = []
        for line in raw.decode().splitlines():
            row = json.loads(line)
            if statement(row.get('query') or '') != statement(self.up.read_text()):
                continue
            if self.workload.mysql and 'lock' not in row.get('state', '').lower():
                continue
            if type(row.get('session')) is not int or row['session'] <= 0 or row['session'] == lock['session']:
                continue
            if not self.workload.mysql and lock['session'] not in row.get('blockers', []):
                continue
            matches.append(row)
        if len(matches) > 1:
            raise ValueError('multiple native Apply sessions are waiting')
        if not matches:
            return False
        retain(self.root / 'apply-waiting.json', {'lock': lock, 'apply': matches[0], 'upSHA256': sha(self.up.read_bytes()),
                                               'observedAt': datetime.datetime.now(datetime.timezone.utc).isoformat()})
        return True


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('prepare', 'lock', 'waiting'))
    parser.add_argument('--state', required=True)
    parser.add_argument('--inputs', required=True)
    parser.add_argument('--evidence', required=True)
    parser.add_argument('--round', type=int, required=True)
    parser.add_argument('--identities', required=True)
    args = parser.parse_args()
    os.umask(0o077)
    fault = RetentionFault(Workload(args.state, args.inputs), args.evidence, args.round,
                           json.loads(Path(args.identities).read_text()))
    if args.action == 'waiting':
        return 0 if fault.waiting() else 3
    getattr(fault, args.action)()
    return 0


if __name__ == '__main__':
    sys.exit(main())
