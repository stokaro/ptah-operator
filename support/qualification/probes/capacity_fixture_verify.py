#!/usr/bin/env python3
"""Execute capacity fixture SQL with pinned Ptah on disposable databases.

This validates inputs for the workload driver. It does not execute an operator,
publish OCI artifacts, or establish capacity, soak, or architecture acceptance.
"""

import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import time

from capacity_bootstrap import database_account
from capacity_fixtures import (BANDS, HISTORIES, payload_table, schema_sql, seed_sql,
                               verify_row_inventory, write_migrations)
from database_restore import IMAGES


def digest(value):
    return hashlib.sha256(value).hexdigest()


class Verification:
    def __init__(self, args):
        self.args = args
        self.root = Path(args.output).resolve()
        self.root.mkdir(mode=0o700)
        self.docker = ['docker', '--context', args.docker_context]
        self.owner = 'ptah-capacity-fixtures-' + secrets.token_hex(6)
        self.container = self.owner + '-' + args.engine
        self.report = {'scope': __doc__.strip(), 'engine': args.engine,
                       'dockerContext': args.docker_context, 'runID': self.owner, 'suite': args.suite,
                       'image': IMAGES[args.engine], 'ptahSHA256': args.ptah_sha256,
                       'startedAt': dt.datetime.now(dt.timezone.utc).isoformat(),
                       'status': 'RUNNING', 'steps': [], 'schemas': [], 'migrations': []}
        source = Path(__file__).resolve().parents[3]
        self.report['operatorSource'] = {
            'planCheckSourceSHA256': digest((source / 'hack/capacityplancheck/main.go').read_bytes()),
            'commit': subprocess.check_output(['git', '-C', str(source), 'rev-parse', 'HEAD'], text=True).strip(),
            'status': subprocess.check_output(['git', '-C', str(source), 'status', '--porcelain'], text=True),
            'procedures': {name: digest(Path(__file__).with_name(name).read_bytes()) for name in
                           ('capacity_fixture_verify.py', 'capacity_fixtures.py', 'capacity_bootstrap.py', 'database_restore.py')},
        }
        self.started = False
        self.credentials = []
        self.database = None
        self.save()

    def save(self):
        (self.root / 'result.json').write_text(json.dumps(self.report, indent=2) + '\n')

    def call(self, action, args, data=None, required=True):
        index = len(self.report['steps'])
        result = subprocess.run(args, input=data, capture_output=True, timeout=240)
        # SQL and CLI errors may quote connection settings. This directory and
        # its files are private; the summary never copies command arguments.
        prefix = f'{index:04d}'
        (self.root / (prefix + '.stdout.private')).write_bytes(result.stdout)
        (self.root / (prefix + '.stderr.private')).write_bytes(result.stderr)
        self.report['steps'].append({'action': action, 'exitCode': result.returncode,
                                     'stdout': prefix + '.stdout.private', 'stderr': prefix + '.stderr.private'})
        self.save()
        if required and result.returncode:
            raise RuntimeError(action + ' failed; private diagnostics: ' + prefix)
        return result

    def env_file(self, name, values):
        path = self.root / (name + '.env.private')
        with path.open('x') as output:
            output.write(''.join(k + '=' + v + '\n' for k, v in values.items()))
        self.credentials.append(path)
        return path

    def sql(self, query, admin=False, required=True):
        env = self.admin_env if admin else self.owner_env
        args = self.docker + ['exec', '-i', '--env-file', str(env), self.container]
        if self.args.engine == 'postgresql':
            args += ['psql', '-X', '-qAt', '-F', '\t', '-h', '127.0.0.1', '-v', 'ON_ERROR_STOP=1',
                     '-d', 'postgres' if admin else self.database]
        else:
            args += ['mysql', '--protocol=TCP', '-h', '127.0.0.1', '--batch', '--skip-column-names',
                     '-u', 'root' if admin else self.username]
            if not admin:
                args.append(self.database)
        return self.call('administrator SQL' if admin else 'database-owner SQL', args, query.encode(), required)

    def ptah(self, action, args):
        return self.call(action, self.docker + ['exec', '--workdir', '/work', '--env-file', str(self.owner_env),
                                              self.container, '/work/ptah', *args])

    def start(self):
        self.plancheck = self.root / 'capacityplancheck'
        source = Path(__file__).resolve().parents[3]
        self.call('build operator plan safety check', ['go', '-C', str(source), 'build', '-o', str(self.plancheck), './hack/capacityplancheck'])
        self.report['planCheckSHA256'] = digest(self.plancheck.read_bytes())
        self.save()
        binary = Path(self.args.ptah_binary).resolve()
        if digest(binary.read_bytes()) != self.args.ptah_sha256:
            raise ValueError('Ptah binary does not match its declared SHA-256')
        # Require a cached, digest-pinned image. This probe does not leave a
        # newly pulled image behind on the shared Docker host.
        self.call('verify cached database image', self.docker + ['image', 'inspect', IMAGES[self.args.engine]])
        password = secrets.token_hex(24)
        mysql = self.args.engine == 'mysql'
        server = self.env_file('server', {'MYSQL_ROOT_PASSWORD' if mysql else 'POSTGRES_PASSWORD': password})
        self.admin_env = self.env_file('administrator', {'MYSQL_PWD': password} if mysql else {
            'PGUSER': 'postgres', 'PGPASSWORD': password})
        mount = '/var/lib/mysql' if mysql else '/var/lib/postgresql/data'
        # Register before CREATE. Cleanup verifies the run label even when its
        # response is lost, and refuses a same-name container owned by anyone else.
        self.started = True
        self.call('start isolated database', self.docker + [
            'run', '--pull=never', '-d', '--name', self.container, '--label', 'ptah.capacity-fixtures=' + self.owner,
            '--network', 'none', '--cpus', '1', '--memory', '1g', '--tmpfs', mount + ':size=1g',
            '--env-file', str(server), IMAGES[self.args.engine]])
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            ready = self.sql('SELECT 1;', admin=True, required=False)
            if ready.returncode == 0 and ready.stdout.strip() == b'1':
                break
            time.sleep(1)
        else:
            raise RuntimeError('disposable database did not become ready')
        self.call('create private tool directory', self.docker + ['exec', self.container, 'mkdir', '-p', '/work'])
        self.call('copy pinned Ptah', self.docker + ['cp', str(binary), self.container + ':/work/ptah'])
        self.call('make tool executable', self.docker + ['exec', self.container, 'chmod', '755', '/work/ptah'])
        copied = self.call('verify copied Ptah', self.docker + ['exec', self.container, 'sha256sum', '/work/ptah'])
        if copied.stdout.decode().split()[0] != self.args.ptah_sha256:
            raise RuntimeError('container received a different Ptah binary')
        inspected = self.call('record runtime identity', self.docker + ['inspect', self.container])
        identity = json.loads(inspected.stdout)[0]
        self.report['containerID'] = identity['Id']
        self.report['imageID'] = identity['Image']
        self.save()

    def account(self, index):
        password = secrets.token_hex(24)
        engine = 'MySQL' if self.args.engine == 'mysql' else 'PostgreSQL'
        self.database, self.username, sql = database_account(engine, index, password)
        self.sql(sql, admin=True)
        if engine == 'MySQL':
            values = {'MYSQL_PWD': password,
                      'PTAH_DB_URL': f'mysql://{self.username}:{password}@tcp(127.0.0.1:3306)/{self.database}'}
        else:
            values = {'PGUSER': self.username, 'PGPASSWORD': password,
                      'PTAH_DB_URL': f'postgres://{self.username}:{password}@127.0.0.1:5432/{self.database}?sslmode=disable'}
        values.update(PTAH_CONNECT_TIMEOUT='10s', PTAH_LOCK_TIMEOUT='30s', HOME='/work')
        self.owner_env = self.env_file('owner-' + str(index), values)

    def rows(self, slot):
        result = self.sql('SELECT id, payload FROM capacity_rows ORDER BY id;')
        return verify_row_inventory(result.stdout, slot)

    def plan(self, name, content):
        path = self.root / (name + '.sql')
        path.write_bytes(content)
        self.call('stage desired schema', self.docker + ['cp', str(path), self.container + ':/work/schema.sql'])
        remote = '/work/' + name + '.plan.json'
        self.ptah('compute native plan ' + name, ['schema', 'plan', '--schema-file', '/work/schema.sql',
                  '--name', 'capacity-fixture', '--output', remote])
        local = self.root / (name + '.plan.json')
        self.call('retain native plan', self.docker + ['cp', self.container + ':' + remote, str(local)])
        return remote, local.read_bytes()

    def schema(self, band, slot):
        self.account(slot)
        baseline = schema_sql(self.args.engine, band, 0)
        (self.root / (band + '-baseline.sql')).write_bytes(baseline)
        self.sql(baseline.decode())
        self.sql(seed_sql(slot).decode())
        before = self.rows(slot)
        _, first = self.plan(band + '-calibrate-64', schema_sql(self.args.engine, band, 64, 0))
        _, second = self.plan(band + '-calibrate-128', schema_sql(self.args.engine, band, 128, 0))
        slope = (len(second) - len(first)) / 64
        lower, upper, tables = BANDS[band]
        if slope <= 0:
            raise RuntimeError('native plan size does not grow with executable defaults')
        repeated = round(64 + ((lower + upper) // 2 - len(first)) / slope)
        record = {'band': band, 'lowerBytes': lower, 'upperBytes': upper,
                  'tables': tables, 'rowInventoryBefore': before, 'rounds': []}
        self.report['schemas'].append(record)
        self.save()
        previous = []
        for round_number in range(10):
            name = f'{band}-round-{round_number:02d}'
            # Ptah's native SQL includes the old default in its change
            # description. Recalibrate each transition, keeping every attempt;
            # a source byte count alone cannot establish the plan's size.
            for attempt in range(3):
                content = schema_sql(self.args.engine, band, repeated, round_number, previous if self.args.engine == 'mysql' else ())
                remote, raw = self.plan(f'{name}-attempt-{attempt}', content)
                plan = json.loads(raw)
                if not plan.get('statements') or plan.get('destructive') is not False:
                    raise RuntimeError(name + ': expected nonempty, nondestructive SQL')
                if lower <= len(raw) <= upper:
                    break
                repeated = round(repeated + ((lower + upper) // 2 - len(raw)) / slope)
            else:
                raise RuntimeError(f'{name}: actual native plan bytes {len(raw)} are outside [{lower}, {upper}]')
            self.call('check operator plan safety ' + name, [str(self.plancheck), self.args.engine, str(self.root / Path(remote).name)])
            self.ptah('execute native plan ' + name, ['schema', 'apply', '--plan', remote, '--auto-approve', '--json'])
            after = self.rows(slot)
            queries, expected_rows = ['BEGIN;'], []
            for table in range(tables):
                count = repeated // tables + (table < repeated % tables)
                value = '<' * count + f'r{round_number:02d}'
                # Execute the default, then roll back the probe row. The stable
                # workload table still has exactly its original 10,000 rows.
                table_name = payload_table(self.args.engine, table, round_number)
                query = (f'INSERT INTO {table_name} (id) VALUES (0); '
                         f'SELECT {table}, CHAR_LENGTH(payload), MD5(payload) FROM {table_name} WHERE id=0;')
                queries.append(query)
                expected_rows.append(f'{table}\t{len(value)}\t{hashlib.md5(value.encode(), usedforsecurity=False).hexdigest()}\n')
            queries.append('ROLLBACK;')
            if self.sql('\n'.join(queries)).stdout != ''.join(expected_rows).encode():
                raise RuntimeError('native Apply did not install the declared executable defaults')
            previous.append(repeated)
            record['rounds'].append({'round': round_number, 'repeated': repeated, 'sourceSHA256': digest(content),
                                     'planFile': Path(remote).name,
                                     'nativePlanBytes': len(raw), 'nativePlanSHA256': digest(raw),
                                     'rowInventoryAfter': after, 'defaultsVerified': tables})
            self.save()

    def history(self, length, slot):
        self.account(slot)
        rounds = 1 if length == 128 else 10
        record = {'initialLength': length, 'slot': slot, 'rounds': []}
        self.report['migrations'].append(record)
        for round_number in range(rounds):
            count = length + round_number
            name = f'history-{length:03d}-round-{round_number:02d}'
            local = self.root / name
            write_migrations(local, count)
            remote = '/work/' + name
            self.call('stage migration directory', self.docker + ['cp', str(local), self.container + ':' + remote])
            args = ['--migrations-dir', remote, '--dir-format', 'ptah']
            self.ptah('hash migration directory', ['migrations', 'hash', '--dir', remote, '--dir-format', 'ptah'])
            self.call('retain migration sum', self.docker + ['cp', self.container + ':' + remote + '/ptah.sum', str(local / 'ptah.sum')])
            tx = 'none' if self.args.engine == 'mysql' else 'file'
            if round_number == 0:
                self.ptah('apply history prefix', ['migrations', 'up', *args, '--to-version', str(length - 1), '--tx-mode', tx])
                self.sql(seed_sql(slot).decode())
            before = self.rows(slot)
            status = json.loads(self.ptah('read mixed history', ['migrations', 'status', *args, '--json', '--verify-sum']).stdout)
            if status.get('applied_migrations') != list(range(1, count)) or status.get('pending_migrations') != [count]:
                raise RuntimeError('fixture does not contain the declared applied prefix and pending version')
            self.ptah('apply pending migration', ['migrations', 'up', *args, '--tx-mode', tx, '--verify-sum'])
            after = self.rows(slot)
            status = json.loads(self.ptah('verify complete history', ['migrations', 'status', *args, '--json', '--verify-sum']).stdout)
            if status.get('applied_migrations') != list(range(1, count + 1)) or status.get('pending_migrations') not in ([], None):
                raise RuntimeError('native migration history did not reach the declared version')
            wrong = ' OR '.join(f'history_{v:03d} IS NULL OR history_{v:03d} <> {v}' for v in range(2, count + 1))
            if self.sql('SELECT COUNT(*) FROM capacity_rows WHERE ' + wrong + ';').stdout.strip() != b'0':
                raise RuntimeError('migration defaults changed or lost populated rows')
            record['rounds'].append({'round': round_number, 'appliedBefore': count - 1, 'pendingBefore': 1,
                                     'appliedAfter': count, 'rowInventoryBefore': before, 'rowInventoryAfter': after,
                                     'files': [{'path': x.name, 'sha256': digest(x.read_bytes()), 'bytes': x.stat().st_size}
                                               for x in sorted(local.iterdir())]})
            self.save()

    def cleanup(self):
        if self.started:
            found = self.call('find owned container for cleanup', self.docker + ['ps', '-aq', '--filter', 'name=^/' + self.container + '$'])
            if found.stdout.strip():
                identity = json.loads(self.call('verify cleanup ownership', self.docker + ['inspect', self.container]).stdout)[0]
                if identity['Config']['Labels'].get('ptah.capacity-fixtures') != self.owner:
                    raise RuntimeError('refusing to remove a foreign container')
                self.call('remove owned container and anonymous volumes', self.docker + ['rm', '-fv', identity['Id']])
            remaining = self.call('verify container absence', self.docker + ['ps', '-aq', '--filter', 'label=ptah.capacity-fixtures=' + self.owner])
            if remaining.stdout.strip():
                raise RuntimeError('owned containers remain')
        for path in self.credentials:
            path.unlink(missing_ok=True)
        self.report['cleanupComplete'] = True
        self.save()

    def run(self):
        try:
            self.start()
            if self.args.suite in ('all', 'schemas'):
                for slot, band in enumerate(BANDS):
                    self.schema(band, slot)
            if self.args.suite in ('all', 'migrations'):
                for slot, length in enumerate(HISTORIES, 10):
                    self.history(length, slot)
            self.report['status'] = 'PASS'
        except BaseException as error:
            self.report['status'] = 'FAIL'
            self.report['error'] = str(error)
            raise
        finally:
            try:
                self.cleanup()
            except BaseException as error:
                self.report['status'] = 'FAIL'
                self.report['cleanupError'] = str(error)
                raise
            finally:
                self.report['finishedAt'] = dt.datetime.now(dt.timezone.utc).isoformat()
                self.save()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--engine', choices=sorted(IMAGES), required=True)
    parser.add_argument('--docker-context', required=True)
    parser.add_argument('--suite', choices=['all', 'schemas', 'migrations'], default='all',
                        help='select a focused rerun; the report records its scope')
    parser.add_argument('--ptah-binary', required=True, help='pinned Linux binary compatible with the Docker host')
    parser.add_argument('--ptah-sha256', required=True)
    parser.add_argument('--output', required=True, help='new private output directory')
    args = parser.parse_args()
    if not re.fullmatch('[0-9a-f]{64}', args.ptah_sha256):
        parser.error('--ptah-sha256 must be a lowercase SHA-256 digest')
    os.umask(0o077)
    Verification(args).run()


if __name__ == '__main__':
    main()
