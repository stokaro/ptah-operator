#!/usr/bin/env python3
"""Prepare populated, varied inputs for the operator capacity driver.

This supplies inputs and database checks, not full profile acceptance. The
soak, churn, approval, retention and overload scenarios are separate obligations.
"""

import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import time

from capacity_artifacts import ArtifactReader
from capacity_bootstrap import Bootstrap
from capacity_fixtures import BANDS, payload_table, schema_sql, seed_sql, verify_row_inventory, write_migrations

ROOT = Path(__file__).resolve().parents[3]


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def calibration(engine):
    value = json.loads((ROOT / 'support/capacity/input-calibration.json').read_text())
    pin = json.loads((ROOT / 'support/ptah.json').read_text())['releases'][0]['verified'][0]['ptahCommit']
    if value['schemaVersion'] != 1 or value['ptahCommit'] != pin or value['generatorSHA256'] != sha(Path(__file__).with_name('capacity_fixtures.py').read_bytes()):
        raise ValueError('capacity input calibration does not match the pinned Ptah and generator')
    bands = value['engines'][engine]['bands']
    if set(bands) != set(BANDS) or any(len(v) != 10 or any(type(n) is not int or n < 0 for n in v) for v in bands.values()):
        raise ValueError('capacity calibration must cover every band and all ten versions')
    return value['ptahCommit'], bands


def generate(directory, engine):
    pin, bands = calibration(engine)
    dialect = {'PostgreSQL': 'postgresql', 'MySQL': 'mysql'}[engine]
    root = Path(directory)
    root.mkdir(mode=0o700)
    artifacts = {}
    for band in BANDS:
        (root / (band + '-baseline.sql')).write_bytes(schema_sql(dialect, band, 0))
        for version, repeated in enumerate(bands[band]):
            name = f'schema-{band}-{version}'
            content = schema_sql(dialect, band, repeated, version, bands[band][:version] if dialect == 'mysql' else ())
            (root / (name + '.sql')).write_bytes(content)
            artifacts[name] = {'kind': 'schema', 'path': name + '.sql', 'sourceSHA256': sha(content)}
    for initial in (2, 32, 128):
        for version in range(1 if initial == 128 else 10):
            count = initial + version
            name = f'migration-{initial}-{version}'
            write_migrations(root / name, count)
            artifacts[name] = {'kind': 'migration', 'path': name, 'versions': count}
    schemas, migrations = [], []
    for slot in range(10):
        band = list(BANDS)[slot % 3]
        schemas.append({'slot': slot, 'band': band,
                        'artifacts': [f'schema-{band}-{r if slot < 5 else 0}' for r in range(10)]})
        initial = 2 if slot < 4 else 32 if slot < 7 else 128
        migrations.append({'slot': slot, 'historyLength': initial,
                           'artifacts': [f'migration-{initial}-{r if slot < 5 else 0}' for r in range(10)]})
    return {'schemaVersion': 1, 'engine': engine, 'ptahCommit': pin,
            'schemas': schemas, 'migrations': migrations, 'artifacts': artifacts}


class Workload:
    def __init__(self, state, directory):
        self.root = Path(directory).resolve()
        self.bootstrap = Bootstrap(state)
        self.state = json.loads(Path(state).read_text())
        self.engine = self.state['engine']
        self.mysql = self.engine == 'MySQL'
        self.databases = [r for r in self.state['databases'] if r['family'] in ('schema', 'migration')]
        if self.state['schemas'] != 10 or self.state['migrations'] != 10 or len(self.databases) != 20:
            raise ValueError('varied inputs require exactly ten resources of each family')
        if [r['index'] for r in self.databases] != list(range(20)):
            raise ValueError('database slots are missing, duplicated, or reordered')
        self.deployment = 'capacity-mysql' if self.mysql else 'capacity-postgres'
        self.fixture = self.state['fixtureNamespace']
        self.log_index = 0
        self.port = None

    def command(self, label, args, env=None):
        result = subprocess.run(args, env=env, capture_output=True, timeout=180)
        self.log_index += 1
        prefix = self.root / f'command-{os.getpid()}-{self.log_index:04d}'
        prefix.with_suffix('.stdout.private').write_bytes(result.stdout)
        prefix.with_suffix('.stderr.private').write_bytes(result.stderr)
        if result.returncode:
            raise RuntimeError(label + ' failed; private diagnostics: ' + str(prefix))
        return result.stdout

    def credential(self, row):
        secret = self.bootstrap.read('secret', 'capacity-db-' + str(row['index']), row['namespace'])
        url = base64.b64decode(secret['data']['url'], validate=True).decode()
        match = re.fullmatch(r'(?:mysql|postgres)://(capacity_user_[0-9]{3}):([0-9a-f]{48})@.+/(capacity_?[0-9]{3})(?:\?sslmode=disable)?', url)
        if not match or match[1] != row['username'] or match[3] != row['database']:
            raise ValueError('target Secret does not name the owned database and user')
        password = match[2]
        host = f'{self.deployment}.{self.fixture}.svc.cluster.local'
        expected = (f'mysql://{row["username"]}:{password}@tcp({host}:3306)/{row["database"]}' if self.mysql else
                    f'postgres://{row["username"]}:{password}@{host}:5432/{row["database"]}?sslmode=disable')
        if url != expected:
            raise ValueError('target Secret does not name the owned database endpoint')
        return password

    def sql(self, row, sql):
        # The generated password travels on stdin, never in argv or output.
        # The shell consumes its first line and leaves SQL on the same pipe.
        if self.mysql:
            script = 'IFS= read -r MYSQL_PWD; export MYSQL_PWD; exec mysql --protocol=TCP -h 127.0.0.1 -u "$1" --batch --skip-column-names "$2"'
        else:
            script = 'IFS= read -r PGPASSWORD; export PGPASSWORD; exec psql -X -qAt -F "\t" -h 127.0.0.1 -U "$1" -d "$2" -v ON_ERROR_STOP=1'
        raw = (self.credential(row) + '\n').encode() + sql
        return self.bootstrap.command(['-n', self.fixture, 'exec', '-i', 'deployment/' + self.deployment,
                                       '--', 'sh', '-c', script, 'capacity-sql', row['username'], row['database']], raw, timeout=120)

    def ptah(self, row, args):
        if self.port is None:
            raise RuntimeError('no owned database port-forward is active')
        password = self.credential(row)
        url = (f'mysql://{row["username"]}:{password}@tcp(127.0.0.1:{self.port})/{row["database"]}' if self.mysql else
               f'postgres://{row["username"]}:{password}@127.0.0.1:{self.port}/{row["database"]}?sslmode=disable')
        env = dict(os.environ, PTAH_DB_URL=url, PTAH_CONNECT_TIMEOUT='10s', PTAH_LOCK_TIMEOUT='30s')
        return self.command('native Ptah', ['ptah', *args], env)

    def forwarded(self, action):
        log = self.root / f'database-forward-{os.getpid()}.private.log'
        with log.open('xb') as output:
            process = subprocess.Popen(['kubectl', '--kubeconfig', self.bootstrap.env['E2E_KUBECONFIG'],
                                        '-n', self.fixture, 'port-forward', '--address', '127.0.0.1',
                                        'service/' + self.deployment, '0:' + ('3306' if self.mysql else '5432')],
                                       stdout=output, stderr=subprocess.STDOUT)
            try:
                deadline = time.monotonic() + 30
                while process.poll() is None and time.monotonic() < deadline:
                    match = re.search(r'Forwarding from 127\.0\.0\.1:([0-9]+) ->', log.read_text())
                    if match:
                        self.port = int(match[1])
                        break
                    time.sleep(0.1)
                else:
                    raise RuntimeError('database port-forward did not become ready; see ' + str(log))
                return action()
            finally:
                self.port = None
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=5)

    def verify(self, changed):
        bundle = json.loads((self.root / 'bundle.json').read_text())
        pin, bands = calibration(self.engine)
        if bundle['engine'] != self.engine or bundle['ptahCommit'] != pin or changed not in (0, 5):
            raise ValueError('verification must match the prepared workload and supported change batch')
        records = []

        def inspect():
            for row in self.databases:
                slot = row['index']
                index = slot if row['family'] == 'schema' else slot - 10
                version = int(index < changed)
                raw = self.sql(row, b'SELECT id, payload FROM capacity_rows ORDER BY id;')
                (self.root / f'final-rows-{slot:02d}.tsv').write_bytes(raw)
                record = verify_row_inventory(raw, slot)
                if row['family'] == 'schema':
                    band = bundle['schemas'][index]['band']
                    tables = BANDS[band][2]
                    repeated = bands[band][version]
                    queries, expected = ['BEGIN;'], []
                    for table in range(tables):
                        value = '<' * (repeated // tables + (table < repeated % tables)) + f'r{version:02d}'
                        name = payload_table('mysql' if self.engine == 'MySQL' else 'postgresql', table, version)
                        queries.append(f'INSERT INTO {name} (id) VALUES (0); '
                                       f'SELECT {table}, CHAR_LENGTH(payload), MD5(payload) FROM {name} WHERE id=0;')
                        expected.append(f'{table}\t{len(value)}\t{hashlib.md5(value.encode(), usedforsecurity=False).hexdigest()}\n')
                    queries.append('ROLLBACK;')
                    actual = self.sql(row, '\n'.join(queries).encode())
                    (self.root / f'final-defaults-{slot:02d}.tsv').write_bytes(actual)
                    if actual != ''.join(expected).encode():
                        raise ValueError(f'schema slot {slot} has incorrect executable defaults')
                    record.update(band=band, round=version, defaultsVerified=tables)
                else:
                    spec = bundle['migrations'][index]
                    artifact = bundle['artifacts'][spec['artifacts'][version]]
                    count = artifact['versions']
                    raw = self.ptah(row, ['migrations', 'status', '--migrations-dir', str(self.root / artifact['path']),
                                          '--dir-format', 'ptah', '--verify-sum', '--json'])
                    (self.root / f'final-history-{slot:02d}.json').write_bytes(raw)
                    status = json.loads(raw)
                    if status.get('applied_migrations') != list(range(1, count + 1)) or status.get('pending_migrations') not in (None, []):
                        raise ValueError(f'migration slot {slot} has incomplete history')
                    wrong = ' OR '.join(f'history_{v:03d} IS NULL OR history_{v:03d} <> {v}' for v in range(2, count + 1))
                    actual = self.sql(row, ('SELECT COUNT(*) FROM capacity_rows WHERE ' + wrong + ';').encode())
                    (self.root / f'final-migration-defaults-{slot:02d}.tsv').write_bytes(actual)
                    if actual != b'0\n':
                        raise ValueError(f'migration slot {slot} has incorrect populated column values')
                    record.update(historyLength=count, round=version)
                records.append(record)
            return records
        self.forwarded(inspect)
        result = {'engine': self.engine, 'ptahCommit': pin, 'changeBatch': changed, 'slots': records}
        with (self.root / 'database-verification.json').open('x') as output:
            output.write(json.dumps(result, indent=2) + '\n')
        return result

    def prepare(self):
        bundle = generate(self.root, self.engine)
        dialect = 'mysql' if self.mysql else 'postgres'
        registry = os.environ['PTAH_OCI_REGISTRY']
        internal = os.environ['E2E_REGISTRY_HOST']
        reader = ArtifactReader(registry, os.environ['PTAH_OCI_USERNAME'], os.environ['PTAH_OCI_PASSWORD'], self.root / 'oci')
        for name, artifact in bundle['artifacts'].items():
            source = str(self.root / artifact['path'])
            kind = artifact['kind']
            reference = f'oci://{registry}/{kind}s/capacity-inputs:{self.state["runID"]}-{name}'
            if kind == 'schema':
                args = ['schema', 'push', reference, '--schema-file', source, '--dialect', dialect, '--plain-http']
            else:
                self.command('hash migrations', ['ptah', 'migrations', 'hash', '--dir', source, '--dir-format', 'ptah'])
                args = ['migrations', 'push', reference, '--migrations-dir', source, '--dir-format', 'ptah', '--plain-http']
            raw = self.command('publish workload artifact', ['ptah', *args]).decode()
            digests = re.findall(r'^Digest: (sha256:[0-9a-f]{64})$', raw, re.MULTILINE)
            if len(digests) != 1:
                raise RuntimeError('push did not return exactly one immutable digest')
            artifact['reference'] = f'oci://{internal}/{kind}s/capacity-inputs@{digests[0]}'
            files = [Path(source)] if kind == 'schema' else sorted(Path(source).iterdir())
            artifact['sourceFiles'] = [{'path': p.relative_to(self.root).as_posix(), 'bytes': p.stat().st_size,
                                        'sha256': sha(p.read_bytes())} for p in files]
            artifact['readback'] = reader.read(f'oci://{registry}/{kind}s/capacity-inputs@{digests[0]}', kind, artifact['sourceFiles'])
        (self.root / 'bundle.json').write_text(json.dumps(bundle, indent=2) + '\n')

        def populate():
            inventories = []
            for row in self.databases:
                slot = row['index']
                if row['family'] == 'schema':
                    spec = bundle['schemas'][slot]
                    self.sql(row, (self.root / (spec['band'] + '-baseline.sql')).read_bytes())
                else:
                    spec = bundle['migrations'][slot - 10]
                    path = self.root / bundle['artifacts'][spec['artifacts'][0]]['path']
                    self.ptah(row, ['migrations', 'up', '--migrations-dir', str(path), '--dir-format', 'ptah',
                                    '--to-version', str(spec['historyLength'] - 1), '--verify-sum',
                                    '--tx-mode', 'none' if self.mysql else 'file'])
                self.sql(row, seed_sql(slot))
                raw = self.sql(row, b'SELECT id, payload FROM capacity_rows ORDER BY id;')
                (self.root / f'initial-rows-{slot:02d}.tsv').write_bytes(raw)
                inventories.append(verify_row_inventory(raw, slot))
            return inventories
        inventories = self.forwarded(populate)
        catalog = {k: bundle[k] for k in ('schemaVersion', 'engine', 'ptahCommit')}
        catalog['initialRows'] = inventories
        for family in ('schemas', 'migrations'):
            catalog[family] = [{k: v for k, v in row.items() if k != 'artifacts'} |
                               {'references': [bundle['artifacts'][key]['reference'] for key in row['artifacts']]}
                               for row in bundle[family]]
        (self.root / 'catalog.json').write_text(json.dumps(catalog, indent=2) + '\n')
        return catalog


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--state', required=True)
    parser.add_argument('--directory', required=True, help='new private input and evidence directory')
    parser.add_argument('--verify-changed', type=int, choices=(0, 5), help='verify databases after the driver completes')
    args = parser.parse_args()
    os.umask(0o077)
    workload = Workload(args.state, args.directory)
    if args.verify_changed is None:
        workload.prepare()
    else:
        workload.verify(args.verify_changed)


if __name__ == '__main__':
    main()
