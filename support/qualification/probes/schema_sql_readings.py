#!/usr/bin/env python3
"""Capture the schema SQL readings the e2e audit contract is witnessed by.

The executor's schema commands are run against disposable PostgreSQL 17 and
MySQL 8.4 servers that journal every statement they receive, one isolated
command per journal, following testdata/e2e/readings/schema-diagnostics.md.
The journals are written in the shape the checked-in readings use, with the
executor's client address rewritten to the address those readings name, so a
reading taken with the pinned executor can be compared byte for byte with the
file it replaces. Run it once with the executor the readings were taken with
(--compare) to show the procedure reproduces them, then with the new one.

It removes only the containers and the network it created.
"""

import argparse
import binascii
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[3]
FIXTURES = ROOT / 'testdata/e2e'
SERVERS = {
    'postgresql': 'postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73',
    'mysql': 'mysql:8.4@sha256:b3b90af2a6552ae30c266fdb7d5dd55f3afb72404bb78d37fe8a23eb857fd3fb',
}
DATABASE = 'ptah_audit_schema'
ACCOUNT = 'ptah_audit'
AUDIT_HOST = '172.19.0.3'
CONTROL_V3 = "INSERT INTO e2e_widgets (id, name, note) VALUES (701, 'identity-control', 'preserved-identity-row')"
CONTROL_V1 = "INSERT INTO e2e_widgets (id, name) VALUES (701, 'identity-control')"
# The table the exclusion scenario keeps outside the managed scope, as the
# lifecycle creates it.
EXCLUDED = ['CREATE TABLE e2e_excluded_policy_keep (id bigint NOT NULL PRIMARY KEY, note varchar(255))',
            "INSERT INTO e2e_excluded_policy_keep VALUES (701, 'outside-managed-scope')"]
OBSERVE = ['schema', 'drift', '--schema-file', '/desired.sql', '--format', 'json']
PLAN = ['schema', 'plan', '--schema-file', '/desired.sql', '--output', '/tmp/plan.json', '--json']
APPLY = ['schema', 'apply', '--plan', '/plan.json', '--auto-approve', '--json']


def run(argv, data=None, check=True):
    result = subprocess.run(argv, input=data, capture_output=True)
    if check and result.returncode:
        raise RuntimeError(f"{' '.join(argv[:4])} exited {result.returncode}: {result.stderr.decode()[-400:]}")
    return result


class Lab:
    """One disposable server, its network, and the executor runs against it."""

    def __init__(self, context, engine, image):
        self.engine, self.image = engine, image
        self.docker = ['docker', '--context', context]
        self.name = f'ptah-sql-readings-{engine}-{secrets.token_hex(4)}'
        self.password = secrets.token_hex(16)
        self.created = []

    def __enter__(self):
        run(self.docker + ['network', 'create', self.name])
        self.created.append(('network', self.name))
        server = self.name + '-server'
        if self.engine == 'postgresql':
            run(self.docker + ['run', '-d', '--name', server, '--network', self.name,
                               '-e', f'POSTGRES_USER={ACCOUNT}', '-e', f'POSTGRES_PASSWORD={self.password}', '-e', f'POSTGRES_DB={DATABASE}',
                               SERVERS['postgresql'], '-c', 'log_statement=all', '-c', 'logging_collector=on',
                               '-c', 'log_destination=jsonlog', '-c', 'log_directory=/tmp/pglog', '-c', 'log_filename=pg'])
        else:
            run(self.docker + ['run', '-d', '--name', server, '--network', self.name,
                               '-e', f'MYSQL_ROOT_PASSWORD={self.password}', '-e', f'MYSQL_DATABASE={DATABASE}',
                               '-e', f'MYSQL_USER={ACCOUNT}', '-e', f'MYSQL_PASSWORD={self.password}', SERVERS['mysql'],
                               '--general-log=1', '--log-output=TABLE'])
        self.created.append(('container', server))
        self.server = server
        deadline = time.time() + 180
        while time.time() < deadline:
            probe = ['pg_isready', '-U', ACCOUNT, '-d', DATABASE] if self.engine == 'postgresql' else \
                ['mysql', '-uroot', f'-p{self.password}', '-e', 'SELECT 1']
            if run(self.docker + ['exec', server] + probe, check=False).returncode == 0:
                break
            time.sleep(2)
        else:
            raise RuntimeError(f'{self.engine} did not start')
        time.sleep(3)
        if self.engine == 'mysql':
            # The image's own initialization connected through the socket; the
            # journal begins empty for the readings.
            self.sql('TRUNCATE mysql.general_log', root=True)
        return self

    def __exit__(self, *_):
        for kind, name in reversed(self.created):
            if kind == 'container':
                run(self.docker + ['rm', '-f', '-v', name], check=False)
            else:
                run(self.docker + ['network', 'rm', name], check=False)

    def sql(self, text, root=False):
        if self.engine == 'postgresql':
            return run(self.docker + ['exec', '-i', self.server, 'psql', '-q', '-v', 'ON_ERROR_STOP=1', '-U', ACCOUNT, '-d', DATABASE],
                       text.encode()).stdout
        user = ['-uroot', f'-p{self.password}'] if root else [f'-u{ACCOUNT}', f'-p{self.password}', DATABASE]
        return run(self.docker + ['exec', '-i', self.server, 'mysql', '-N', '-B'] + user, text.encode()).stdout

    def seed(self, *files, statements=()):
        for name in files:
            self.sql((FIXTURES / name).read_text())
        for statement in statements:
            self.sql(statement + ';')

    def mark(self):
        if self.engine == 'postgresql':
            return int(run(self.docker + ['exec', self.server, 'sh', '-c', 'wc -l < /tmp/pglog/pg.json']).stdout.decode().strip() or 0)
        return self.sql("SELECT COALESCE(MAX(event_time), '1970-01-01') FROM mysql.general_log", root=True).decode().strip()

    def journal(self, since):
        """The executor's records after a mark, rewritten to the readings' client address."""
        if self.engine == 'postgresql':
            raw = run(self.docker + ['exec', self.server, 'sh', '-c', f'tail -n +{since + 1} /tmp/pglog/pg.json']).stdout.decode()
            rows = []
            for line in raw.splitlines():
                record = json.loads(line)
                if record.get('remote_host') in (None, '[local]') or record.get('message') is None:
                    continue
                row = {'dbname': record.get('dbname'), 'remote_host': AUDIT_HOST, 'message': record['message']}
                for key in ('statement', 'detail'):
                    if record.get(key) is not None:
                        row[key] = record[key]
                rows.append(row)
            return rows
        query = ("SELECT DATE_FORMAT(event_time, '%Y-%m-%dT%H:%i:%s.%f'), command_type, user_host, thread_id, HEX(argument) "
                 f"FROM mysql.general_log WHERE event_time > '{since}' AND user_host NOT LIKE '%localhost%' "
                 "AND user_host LIKE '%@  [%' ORDER BY event_time, thread_id")
        rows = []
        for line in self.sql(query, root=True).decode().splitlines():
            stamp, command, client, thread, argument = line.split('\t')
            host = client[client.rindex('[') + 1:client.rindex(']')]
            client = client.replace(f'[{host}]', f'[{AUDIT_HOST}]')
            argument = binascii.unhexlify(argument).replace(host.encode(), AUDIT_HOST.encode()).hex().upper()
            rows.append({'time': stamp, 'type': command, 'client': client, 'thread': int(thread), 'argumentHex': argument})
        return rows

    def ptah(self, args, desired=None, plan=None, environment=None):
        """Run one executor command; return its exit code, journal and saved plan."""
        container = self.name + '-ptah-' + secrets.token_hex(3)
        url = (f'postgres://{ACCOUNT}:{self.password}@{self.server}:5432/{DATABASE}?sslmode=disable'
               if self.engine == 'postgresql' else f'mysql://{ACCOUNT}:{self.password}@{self.server}:3306/{DATABASE}')
        env = ['-e', f'PTAH_DB_URL={url}']
        for key, value in (environment or {}).items():
            env += ['-e', f'{key}={value}']
        run(self.docker + ['create', '--name', container, '--network', self.name] + env + [self.image] + args)
        try:
            if desired:
                run(self.docker + ['cp', str(FIXTURES / desired), container + ':/desired.sql'])
            if plan:
                run(self.docker + ['cp', str(plan), container + ':/plan.json'])
            mark = self.mark()
            time.sleep(1.1)
            code = run(self.docker + ['start', '-a', container], check=False).returncode
            time.sleep(1.1)
            journal = self.journal(mark)
            saved = None
            if '--output' in args:
                target = Path(os.environ.get('TMPDIR', '/tmp')) / (container + '.json')
                if run(self.docker + ['cp', container + ':/tmp/plan.json', str(target)], check=False).returncode == 0:
                    saved = target
            return code, journal, saved
        finally:
            run(self.docker + ['rm', '-f', '-v', container], check=False)


def scenarios(engine):
    """Each reading: its file stem, the setup, and the command sequence that produces it."""
    v1, v2, v3, v4, fault = (f'{engine}-{name}.sql' for name in ('v1', 'v2', 'v3', 'v4', 'fault-v1'))
    populated = dict(files=(v3,), statements=(CONTROL_V3,))
    return [
        ('', populated, [('observe', OBSERVE, fault), ('plan', PLAN, fault)]),
        ('destructive-', populated, [('observe', OBSERVE, v4), ('plan', PLAN, v4)]),
        ('exclusion-wide-', dict(files=(v3,), statements=(CONTROL_V3,) + tuple(EXCLUDED)),
         [('observe', OBSERVE, fault), ('plan', PLAN, fault)]),
        ('exclusion-narrow-', dict(files=(v3,), statements=(CONTROL_V3,) + tuple(EXCLUDED)),
         [('observe', OBSERVE, fault), ('plan', PLAN + ['--exclude=e2e_excluded_policy_keep'], fault)]),
        ('initial-v1-', dict(files=(), statements=()), [('observe', OBSERVE, v1), ('plan', PLAN, v1)]),
        ('tag-v2-', dict(files=(v1,), statements=(CONTROL_V1,)), [('observe', OBSERVE, v2), ('plan', PLAN, v2)]),
        ('tag-v3-', dict(files=(v1,), statements=(CONTROL_V1,)), [('observe', OBSERVE, v3), ('plan', PLAN, v3)]),
    ]


def write(out, name, rows, outcome, results):
    (out / name).write_text(''.join(json.dumps(row) + '\n' for row in rows))
    results[name] = {'exitCode': outcome, 'records': len(rows)}


def capture(context, image, engine, out):
    """Write every reading for one engine into out and return what each command did."""
    results = {}
    for variant, setup, steps in scenarios(engine):
        with Lab(context, engine, image) as lab:
            lab.seed(*setup['files'], statements=setup['statements'])
            for operation, args, desired in steps:
                code, rows, saved = lab.ptah(args, desired=desired)
                write(out, f'{engine}-schema-{variant}{operation}.jsonl', rows, code, results)
                if variant.startswith('exclusion') and operation == 'plan':
                    if saved is None:
                        raise RuntimeError(f'{engine} {variant} saved no plan')
                    (out / f'{engine}-schema-{variant}plan.json').write_bytes(saved.read_bytes())
                    saved.unlink()
    # The stale-plan sequence: save a plan, change the database under it,
    # refuse the old plan, then observe, plan, validate and apply afresh.
    fault, v3 = f'{engine}-fault-v1.sql', f'{engine}-v3.sql'
    with Lab(context, engine, image) as lab:
        lab.seed(v3, statements=(CONTROL_V3,))
        code, rows, first = lab.ptah(PLAN, desired=fault)
        write(out, f'{engine}-schema-drift-plan-before.jsonl', rows, code, results)
        lab.sql('ALTER TABLE e2e_widgets DROP COLUMN enabled;')
        code, rows, _ = lab.ptah(APPLY, plan=first, environment={'PTAH_LOCK_TIMEOUT': '60s'})
        write(out, f'{engine}-schema-drift-stale-apply.jsonl', rows, code, results)
        code, rows, _ = lab.ptah(OBSERVE, desired=fault)
        write(out, f'{engine}-schema-drift-observe.jsonl', rows, code, results)
        code, rows, second = lab.ptah(PLAN, desired=fault)
        write(out, f'{engine}-schema-drift-plan.jsonl', rows, code, results)
        code, rows, _ = lab.ptah(APPLY + ['--dry-run'], plan=second, environment={'PTAH_LOCK_TIMEOUT': '30s'})
        write(out, f'{engine}-schema-drift-validate-plan.jsonl', rows, code, results)
        code, rows, _ = lab.ptah(APPLY, plan=second, environment={'PTAH_LOCK_TIMEOUT': '60s'})
        write(out, f'{engine}-schema-drift-apply-current.jsonl', rows, code, results)
        for plan in (first, second):
            plan.unlink()
    # A runner Plan reads the plan twice and validates the first read with a
    # dry run under the resource's lock timeout.
    if engine == 'mysql':
        for timeout in (45, 60):
            with Lab(context, engine, image) as lab:
                lab.seed(v3, statements=(CONTROL_V3,))
                rows, codes = [], []
                code, part, first = lab.ptah(PLAN, desired=fault)
                rows += part
                codes.append(code)
                code, part, again = lab.ptah(PLAN, desired=fault)
                rows += part
                codes.append(code)
                code, part, _ = lab.ptah(APPLY + ['--dry-run'], plan=first, environment={'PTAH_LOCK_TIMEOUT': f'{timeout}s'})
                rows += part
                codes.append(code)
                write(out, f'mysql-schema-lock-{timeout}-plan.jsonl', rows, max(codes), results)
                first.unlink()
                again.unlink()
    return results


def comparable(path):
    """What a reading says, without the timestamps and thread numbers a rerun changes."""
    rows = [json.loads(line) for line in path.read_text().splitlines()]
    if rows and 'message' in rows[0]:
        return rows
    return [(row['type'], row['client'], row['argumentHex']) for row in rows]


CONTRACT_VARIANTS = ('', 'destructive-', 'exclusion-wide-', 'exclusion-narrow-', 'initial-v1-', 'tag-v2-', 'tag-v3-', 'drift-')
OPERATION_ORDER = {'observe': 0, 'plan': 1, 'stale-apply': 2}


def contract_readings(engine):
    """The readings that witness each operation of the diagnostic contract.

    A runner Plan also validates its saved plan, so the validation and the
    runner Plan journals witness Plan; the refused stale Apply witnesses its
    own permission. The allowed Apply is not a diagnostic and grants nothing.
    """
    readings = []
    for variant in CONTRACT_VARIANTS:
        readings += [(f'{engine}-schema-{variant}observe.jsonl', 'observe'), (f'{engine}-schema-{variant}plan.jsonl', 'plan')]
    readings += [(f'{engine}-schema-drift-validate-plan.jsonl', 'plan'), (f'{engine}-schema-drift-stale-apply.jsonl', 'stale-apply')]
    if engine == 'mysql':
        readings += [('mysql-schema-lock-45-plan.jsonl', 'plan'), ('mysql-schema-lock-60-plan.jsonl', 'plan')]
    return readings


def received(engine, path):
    """Each statement a reading shows the server receiving, as the audit reads it."""
    for line in path.read_text().splitlines():
        row = json.loads(line)
        if engine == 'postgresql':
            message = row['message']
            if message.startswith('statement: '):
                sql = message[len('statement: '):]
            elif message.startswith('execute ') and ': ' in message:
                sql = message.split(': ', 1)[1]
            else:
                sql = row.get('statement', '')
            if sql:
                yield 'query', sql, row.get('detail', '')
        elif row['type'] in ('Query', 'Prepare', 'Execute'):
            sql = binascii.unhexlify(row['argumentHex']).decode()
            yield row['type'], sql.replace(f"'{DATABASE}'", "'{{database}}'"), ''


def contract(readings, ptah_commit, scope):
    """The diagnostic contract the readings witness: every statement, and the operations that sent it."""
    order, operations = [], {}
    for engine in ('postgresql', 'mysql'):
        for name, operation in contract_readings(engine):
            for command, sql, parameters in received(engine, Path(readings) / name):
                key = (engine, command, sql, parameters)
                if key not in operations:
                    operations[key] = set()
                    order.append(key)
                operations[key].add(operation)
    return {'schemaVersion': 1, 'ptahCommit': ptah_commit, 'scope': scope,
            'statements': [{'engine': engine, 'operations': sorted(operations[key], key=OPERATION_ORDER.get),
                            'command': command, 'statement': sql, 'parameters': parameters}
                           for key in order for engine, command, sql, parameters in [key]]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--context')
    parser.add_argument('--executor-image')
    parser.add_argument('--engine', choices=sorted(SERVERS), action='append')
    parser.add_argument('--out', required=True, help='directory the readings are written to')
    parser.add_argument('--compare', action='store_true',
                        help='compare every reading with the checked-in one instead of only writing it')
    parser.add_argument('--contract', help='instead of capturing, write the diagnostic contract the readings in --out witness')
    parser.add_argument('--ptah-commit', help='the Ptah commit the readings were taken with, for --contract')
    args = parser.parse_args()
    if args.contract:
        if not args.ptah_commit:
            parser.error('--contract needs --ptah-commit')
        scope = json.loads((ROOT / 'test/e2e/schema-sql-contract.json').read_text())['scope']
        Path(args.contract).write_text(json.dumps(contract(args.out, args.ptah_commit, scope), indent=2) + '\n')
        return
    if not args.context or not args.executor_image:
        parser.error('a capture needs --context and --executor-image')
    if args.context in ('default', 'orbstack'):
        parser.error('name an explicit remote Docker context')
    out = Path(args.out).resolve()
    out.mkdir(parents=True, exist_ok=True)
    results = {}
    for engine in args.engine or sorted(SERVERS):
        results.update(capture(args.context, args.executor_image, engine, out))
    (out / 'capture.json').write_text(json.dumps({'executorImage': args.executor_image, 'readings': results}, indent=2) + '\n')
    mismatched = 0
    for name, result in sorted(results.items()):
        line = f"{name}: exit {result['exitCode']}, {result['records']} records"
        if args.compare:
            recorded = ROOT / 'testdata/e2e/readings' / name
            same = recorded.is_file() and comparable(recorded) == comparable(out / name)
            mismatched += not same
            line += ', matches the checked-in reading' if same else ', DIFFERS from the checked-in reading'
        print(line)
    sys.exit(1 if mismatched else 0)


if __name__ == '__main__':
    main()
