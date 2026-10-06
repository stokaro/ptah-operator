"""Prepare isolated capacity lab fixtures; this is not full qualification."""

import argparse
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import sys
import time

import capacity_unrelated


def database_account(engine, index, credential):
    """Create one database owner without global privileges or grant options."""
    if type(index) is not int or index < 0 or not re.fullmatch(r'[0-9a-f]{48}', credential):
        raise ValueError('database account needs a nonnegative slot and a generated credential')
    username = f'capacity_user_{index:03d}'
    if engine == 'PostgreSQL':
        database = f'capacity_{index:03d}'
        sql = (f"CREATE ROLE {username} LOGIN PASSWORD '{credential}' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;\n"
               f'CREATE DATABASE {database} OWNER {username};\n'
               f'REVOKE ALL ON DATABASE {database} FROM PUBLIC;\n'
               f'GRANT CONNECT ON DATABASE {database} TO {username};\n')
    elif engine == 'MySQL':
        # Database-level MySQL grants treat underscores as wildcards. Keep
        # their names alphanumeric so one grant names exactly one database.
        database = f'capacity{index:03d}'
        sql = (f'CREATE DATABASE `{database}`;\n'
               f"CREATE USER '{username}'@'%' IDENTIFIED BY '{credential}';\n"
               f"GRANT ALL PRIVILEGES ON `{database}`.* TO '{username}'@'%';\n")
    else:
        raise ValueError('unsupported workload engine: ' + str(engine))
    return database, username, sql


class Bootstrap:
    def __init__(self, state, environment=None):
        self.path = Path(state)
        self.env = dict(os.environ if environment is None else environment)
        self.state = {'namespaces': [], 'workloadNamespaces': []}

    def save(self):
        temporary = self.path.with_suffix('.new')
        with open(temporary, 'w', opener=lambda p, f: os.open(p, f, 0o600)) as output:
            json.dump(self.state, output, indent=2)
            output.write('\n')
        temporary.replace(self.path)

    def command(self, args, value=None, timeout=45):
        data = json.dumps(value).encode() if isinstance(value, dict) else value
        owned_create = (args == ['create', '-f', '-', '-o', 'json'] and
                        isinstance(value, dict) and value.get('kind') != 'ResourceQuota' and
                        value.get('metadata', {}).get('namespace') in
                        {n['name'] for n in self.state['namespaces']})
        deadline = time.monotonic() + timeout
        diagnostic = self.path.with_suffix('.error.log')
        failure = 'kubectl failed; private diagnostics: ' + str(diagnostic)
        for attempt in range(5):
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise RuntimeError(failure)
            result = subprocess.run(['kubectl', '--kubeconfig', self.env['E2E_KUBECONFIG'],
                                     '--request-timeout=30s', *args], input=data,
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                    timeout=remaining, check=False)
            if not result.returncode:
                return result.stdout
            # API errors can quote Secret fields. Keep diagnostics private.
            with open(diagnostic, 'ab', opener=lambda p, f: os.open(p, f, 0o600)) as output:
                output.write(result.stderr)
            # Quota admission can exhaust its own optimistic-update retries
            # before admitting a CREATE. Only that explicit refusal proves the
            # object was not stored; never replay an ambiguous write or SQL exec.
            quota_conflict = re.fullmatch(
                rb'Error from server \(Conflict\): error when creating "STDIN": '
                rb'Operation cannot be fulfilled on resourcequotas "(?:capacity|background)": '
                rb'the object has been modified; please apply your changes to the latest version and try again\n?',
                result.stderr)
            if (not owned_create or not quota_conflict or result.stdout or attempt == 4 or
                    time.monotonic() + 0.5 >= deadline):
                raise RuntimeError(failure)
            time.sleep(0.5)

    def read(self, resource, name, namespace):
        return json.loads(self.command(['-n', namespace, 'get', resource, name, '-o', 'json']))

    def create(self, value):
        return json.loads(self.command(['create', '-f', '-', '-o', 'json'], value))

    def namespace(self, name, minor, restricted):
        labels = {'operator.ptah.run/capacity-bootstrap': self.state['runID']}
        for mode in ('enforce', 'warn', 'audit'):
            labels['pod-security.kubernetes.io/' + mode] = 'restricted' if restricted else 'baseline'
            labels['pod-security.kubernetes.io/' + mode + '-version'] = minor
        self.state['pendingNamespace'] = name
        self.save()
        created = self.create({'apiVersion': 'v1', 'kind': 'Namespace',
                               'metadata': {'name': name, 'labels': labels}})
        uid = created.get('metadata', {}).get('uid')
        if not uid:
            raise RuntimeError('namespace creation returned no UID; refusing name-only cleanup: ' + name)
        self.state['namespaces'].append({'name': name, 'uid': uid})
        self.state.pop('pendingNamespace')
        self.save()

    def copy(self, source, namespace):
        # Do not copy owner references, server metadata or annotations containing
        # a previous applied Secret. Existing objects are never overwritten.
        value = {key: source[key] for key in ('apiVersion', 'kind', 'type', 'data', 'immutable') if key in source}
        value['metadata'] = {'name': source['metadata']['name'], 'namespace': namespace}
        return self.create(value)

    def object(self, namespace, kind, name, **fields):
        versions = {'Deployment': 'apps/v1', 'NetworkPolicy': 'networking.k8s.io/v1'}
        return {'apiVersion': versions.get(kind, 'v1'), 'kind': kind,
                'metadata': {'name': name, 'namespace': namespace}, **fields}

    def prepare(self, workload):
        if self.path.exists():
            raise RuntimeError('state file already exists; refusing to replace an ownership journal')
        if type(workload.get('unrelatedObjects', False)) is not bool:
            raise ValueError('unrelatedObjects must be boolean')
        engine = workload.get('engine', 'PostgreSQL')
        if engine not in ('PostgreSQL', 'MySQL'):
            raise ValueError('unsupported workload engine: ' + str(engine))
        image_key = 'E2E_MYSQL_IMAGE' if engine == 'MySQL' else 'E2E_POSTGRES_IMAGE'
        if not self.env.get(image_key):
            raise ValueError('missing database image: ' + image_key)
        counts = [workload.get(key) for key in ('schemas', 'migrations')]
        if any(type(n) is not int or n < 0 for n in counts) or sum(counts) == 0:
            raise ValueError('workload must declare nonnegative family counts and at least one resource')
        # Read all shared prerequisites before creating anything.
        source = self.env['E2E_TEST_NAMESPACE']
        dependencies = [self.read('secret', name, source) for name in ('demo-registry', 'demo-registry-pull')]
        for name in ('demo-verification-policy', 'demo-migration-verification-policy'):
            policy = self.read('configmap', name, source)
            if policy.get('immutable') is not True:
                raise ValueError('verification policy is not immutable: ' + name)
            dependencies.append(policy)
        version = json.loads(self.command(['version', '-o', 'json']))['serverVersion']['gitVersion']
        match = re.match(r'^v(1\.[0-9]+)\.', version)
        if not match:
            raise ValueError('cannot pin Pod Security Admission to server version')
        minor = 'v' + match[1]
        self.state.update(runID=secrets.token_hex(5), kubernetes=version, engine=engine)
        prefix = 'ptah-capacity-' + self.state['runID']
        fixture = prefix + '-fixtures'
        namespaces = [prefix + '-a', prefix + '-b']
        self.state.update(fixtureNamespace=fixture, workloadNamespaces=namespaces,
                          schemas=counts[0], migrations=counts[1])
        self.save()
        self.namespace(fixture, minor, False)
        self.copy(dependencies[1], fixture)
        for namespace in namespaces:
            self.namespace(namespace, minor, True)
            for dependency in dependencies:
                self.copy(dependency, namespace)
            # This ServiceAccount exists only inside our freshly created namespace.
            # No RoleBinding grants it API writes. Pods receive no token by default.
            self.command(['-n', namespace, 'wait', '--for=create', 'serviceaccount/default', '--timeout=30s'])
            self.command(['-n', namespace, 'patch', 'serviceaccount', 'default', '--type=merge',
                          '--patch', '{"automountServiceAccountToken":false,"imagePullSecrets":[{"name":"demo-registry-pull"}]}'])
            self.create(self.object(namespace, 'ResourceQuota', 'capacity', spec={'hard': {
                'pods': '24', 'count/jobs.batch': '512', 'configmaps': '512', 'secrets': '128',
                'count/ptahschemaplans.operator.ptah.run': '2048',
                'count/ptahschemaplanchunks.operator.ptah.run': '32768',
                'count/ptahmigrationplans.operator.ptah.run': '2048'}}))
            # Operation Pods serve no inbound traffic. Outage injection owns the
            # egress fault; an additional allow rule would bypass that fault.
            self.create(self.object(namespace, 'NetworkPolicy', 'capacity-no-ingress',
                                    spec={'podSelector': {}, 'policyTypes': ['Ingress'], 'ingress': []}))
        self.database(fixture, namespaces, counts)
        if workload.get('unrelatedObjects', False):
            capacity_unrelated.prepare(self, minor)
        return namespaces

    def database(self, fixture, namespaces, counts):
        password = secrets.token_hex(24)
        engine = self.state['engine']
        mysql = engine == 'MySQL'
        name = 'capacity-mysql' if mysql else 'capacity-postgres'
        port = 3306 if mysql else 5432
        port_name = 'mysql' if mysql else 'postgresql'
        image = self.env['E2E_MYSQL_IMAGE' if mysql else 'E2E_POSTGRES_IMAGE']
        admin = ['sh', '-c', 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -u root --batch'] if mysql else ['psql', '-U', 'postgres', '-v', 'ON_ERROR_STOP=1']
        ready = ['sh', '-c', 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -h 127.0.0.1 -u root --execute="SELECT 1"'] if mysql else ['pg_isready', '-U', 'postgres']
        self.create(self.object(fixture, 'Secret', name + '-admin',
                                stringData={'password': password}))
        labels = {'app.kubernetes.io/name': name}
        self.create(self.object(fixture, 'Deployment', name, spec={
            'replicas': 1, 'selector': {'matchLabels': labels}, 'template': {
                'metadata': {'labels': labels}, 'spec': {
                    'automountServiceAccountToken': False,
                    'imagePullSecrets': [{'name': 'demo-registry-pull'}],
                    'containers': [{
                        'name': port_name, 'image': image, 'imagePullPolicy': 'IfNotPresent',
                        'args': ['--max-connections=500'] if mysql else ['-c', 'max_connections=500'],
                        'env': [{'name': 'MYSQL_ROOT_PASSWORD' if mysql else 'POSTGRES_PASSWORD', 'valueFrom': {
                            'secretKeyRef': {'name': name + '-admin', 'key': 'password'}}}],
                        'resources': {'requests': {'cpu': '100m', 'memory': '256Mi'},
                                      'limits': {'cpu': '2', 'memory': '1Gi'}},
                        'ports': [{'name': port_name, 'containerPort': port}],
                        'readinessProbe': {'exec': {'command': ready}, 'periodSeconds': 3}
                    }]}}}))
        self.create(self.object(fixture, 'Service', name, spec={
            'selector': labels, 'ports': [{'name': port_name, 'port': port, 'targetPort': port_name}]}))
        self.create(self.object(fixture, 'NetworkPolicy', 'capacity-database-ingress', spec={
            'podSelector': {'matchLabels': labels}, 'policyTypes': ['Ingress'], 'ingress': [{
                'from': [{'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': ns}}}
                         for ns in namespaces],
                'ports': [{'protocol': 'TCP', 'port': port}]}]}))
        self.command(['-n', fixture, 'rollout', 'status', 'deployment/' + name, '--timeout=300s'], timeout=330)
        self.state['databases'] = []
        for index in range(sum(counts) + 1):
            if index < counts[0]:
                family, slot = 'schema', index
            elif index < sum(counts):
                family, slot = 'migration', index - counts[0]
            else:
                family, slot = 'approval-fixture', 0
            namespace = namespaces[slot % len(namespaces)]
            credential = secrets.token_hex(24)
            database, username, sql = database_account(engine, index, credential)
            self.command(['-n', fixture, 'exec', '-i', 'deploy/' + name, '--', *admin], sql.encode())
            host = f'{name}.{fixture}.svc.cluster.local:{port}'
            url = (f'mysql://{username}:{credential}@tcp({host})/{database}' if mysql else
                   f'postgres://{username}:{credential}@{host}/{database}?sslmode=disable')
            self.create(self.object(namespace, 'Secret', f'capacity-db-{index}', stringData={'url': url}))
            self.state['databases'].append({'index': index, 'family': family, 'namespace': namespace,
                                           'database': database, 'username': username})
            self.save()

    def cleanup(self):
        if not self.path.exists():
            return
        self.state = json.loads(self.path.read_text())
        # A lost CREATE response is ambiguous. Recover ownership only from the
        # unique run label recorded before the request; never adopt by name.
        if pending := self.state.get('pendingNamespace'):
            raw = self.command(['get', 'namespace', pending, '--ignore-not-found', '-o', 'json'])
            if raw.strip():
                meta = json.loads(raw)['metadata']
                if meta.get('labels', {}).get('operator.ptah.run/capacity-bootstrap') != self.state['runID'] or not meta.get('uid'):
                    raise RuntimeError('pending namespace is not owned by this run: ' + pending)
                self.state['namespaces'].append({'name': pending, 'uid': meta['uid']})
            self.state.pop('pendingNamespace')
            self.save()
        # Start all workload deletions together so their one-hour result
        # retention windows overlap. Keep the database alive until they finish.
        # The manager uses resultretention.MinimumWindow (one hour); allow five
        # more minutes for retirement marking and namespace garbage collection.
        fixture = self.state.get('fixtureNamespace')
        groups = ([owned for owned in reversed(self.state['namespaces']) if owned['name'] != fixture],
                  [owned for owned in self.state['namespaces'] if owned['name'] == fixture])
        for group, timeout in zip(groups, (3900, 180)):
            pending = []
            for owned in group:
                raw = self.command(['get', 'namespace', owned['name'], '--ignore-not-found', '-o', 'json'])
                if not raw.strip():
                    self.state['namespaces'].remove(owned)
                    self.save()
                    continue
                if json.loads(raw)['metadata'].get('uid') != owned['uid']:
                    raise RuntimeError('namespace was replaced; refusing cleanup: ' + owned['name'])
                options = {'apiVersion': 'v1', 'kind': 'DeleteOptions',
                           'preconditions': {'uid': owned['uid']}}
                path = self.path.with_suffix('.delete.json')
                path.write_text(json.dumps(options))
                self.command(['delete', '--raw', '/api/v1/namespaces/' + owned['name'], '-f', str(path)])
                pending.append(owned)
            for owned in pending:
                self.command(['wait', '--for=delete', 'namespace/' + owned['name'],
                              f'--timeout={timeout}s'], timeout=timeout + 30)
                self.state['namespaces'].remove(owned)
                self.save()
        self.state['cleaned'] = True
        self.save()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('prepare', 'cleanup', 'engine', 'verify-unrelated'))
    parser.add_argument('--state', required=True)
    parser.add_argument('--workload')
    parser.add_argument('--checkpoint', choices=('before', 'after'))
    args = parser.parse_args()
    bootstrap = Bootstrap(args.state)
    if args.action == 'prepare':
        if not args.workload:
            parser.error('prepare requires --workload')
        namespaces = bootstrap.prepare(json.loads(Path(args.workload).read_text()))
        print(','.join(namespaces))
    elif args.action == 'verify-unrelated':
        if not args.checkpoint:
            parser.error('verify-unrelated requires --checkpoint')
        bootstrap.state = json.loads(bootstrap.path.read_text())
        proof = capacity_unrelated.verify(bootstrap, args.checkpoint)
        bootstrap.state['unrelated'][args.checkpoint] = proof
        bootstrap.save()
    elif args.action == 'engine':
        engine = json.loads(bootstrap.path.read_text())['engine']
        if engine not in ('PostgreSQL', 'MySQL'):
            raise ValueError('invalid engine in ownership journal')
        print(engine)
    else:
        bootstrap.cleanup()


if __name__ == '__main__':
    try:
        main()
    except (RuntimeError, ValueError, KeyError, subprocess.TimeoutExpired) as error:
        print('capacity bootstrap: ' + str(error), file=sys.stderr)
        sys.exit(1)
