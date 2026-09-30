#!/usr/bin/env python3
"""Native operator/database-loss pilot; other recovery cells remain required."""
import argparse
import base64
import importlib.util
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import time
from urllib.parse import urlencode

REPO = Path(__file__).resolve().parents[3]
spec = importlib.util.spec_from_file_location('database_restore', REPO / 'support/qualification/probes/database_restore.py')
db = importlib.util.module_from_spec(spec)
spec.loader.exec_module(db)


class OperatorProbe(db.Probe):
    def __init__(self, engine, family, environment, root):
        super().__init__(engine, root)
        self.family = family
        self.envs = dict(line.split('=', 1) for line in environment.read_text().splitlines() if '=' in line)
        endpoint = self.command('verify the explicit database Docker endpoint', db.DOCKER + ['context', 'inspect', db.DOCKER[-1], '--format', '{{.Endpoints.docker.Host}}']).stdout.decode().strip()
        if self.envs.get('E2E_DOCKER_ENDPOINT') != endpoint:
            self.report.update(status='FAIL', failure='The cluster and database procedures address different Docker daemons')
            self.persist()
            raise RuntimeError(self.report['failure'])
        self.namespace = self.prefix
        self.kind = 'PtahSchema' if family == 'schema' else 'PtahMigration'
        self.resource = 'ptahschemas' if family == 'schema' else 'ptahmigrations'
        self.name = 'restore-case'
        self.api = 'operator.ptah.run/v1alpha1'
        self.created_namespace = False
        self.report.update(scope='Native database-only idle recovery pilot with a real operator; no operator-state loss, in-flight or five-minute lag proof, final-artifact or full-matrix acceptance', family=family,
            operatorRevision=self.envs['E2E_CONTROLLER_REVISION'], operatorImage=self.envs['E2E_CONTROLLER_IMAGE'], executorImage=self.envs['E2E_EXECUTOR_IMAGE'],
            procedureSHA256=db.digest(Path(__file__).read_bytes()), databaseProcedureSHA256=db.digest((REPO / 'support/qualification/probes/database_restore.py').read_bytes()))
        self.watches = {}

    def kubectl(self, action, args, data=None, required=True):
        return self.command(action, ['kubectl', '--kubeconfig', self.envs['E2E_KUBECONFIG'], '--request-timeout=30s', '-n', self.namespace] + args, data, required)

    def obj(self, kind, name, spec=None, api='v1', **fields):
        value = {'apiVersion': api, 'kind': kind, 'metadata': {'name': name, 'namespace': self.namespace}, **fields}
        if spec is not None:
            value['spec'] = spec
        return value

    def create(self, value):
        return json.loads(self.kubectl('create ' + value['kind'], ['create', '-f', '-', '-o', 'json'], json.dumps(value).encode()).stdout)

    def read(self, resource=None, name=None):
        args = ['get', resource or self.resource]
        if name is not False:
            args.append(name or self.name)
        args += ['-o', 'json']
        return json.loads(self.kubectl('read recovery state', args).stdout)

    def patch(self, value):
        return json.loads(self.kubectl('patch recovery resource', ['patch', self.resource, self.name, '--type=merge', '--patch-file=/dev/stdin', '-o', 'json'], json.dumps(value).encode()).stdout)

    def wait(self, action, predicate, seconds=300):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            value = self.read()
            if predicate(value):
                return value
            time.sleep(2)
        (self.root / 'last-resource.private.json').write_text(json.dumps(value))
        raise RuntimeError(action + ' timed out; private resource snapshot retained')

    @staticmethod
    def is_settled(resource, phase):
        status = resource.get('status', {})
        generation = resource.get('metadata', {}).get('generation')
        if not generation or status.get('observedGeneration') != generation:
            return False
        if status.get('phase') != phase or status.get('activeOperation'):
            return False
        if phase == 'InSync':
            return any(c.get('type') == 'Ready' and c.get('status') == 'True' and
                       c.get('observedGeneration') == generation
                       for c in status.get('conditions', []))
        return True

    def settled(self, phase):
        return self.wait('reach current-generation ' + phase, lambda r: self.is_settled(r, phase))

    def secret(self, name, values, typ='Opaque'):
        return self.create(self.obj('Secret', name, type=typ, data={k: base64.b64encode(v.encode()).decode() for k, v in values.items()}))

    def route(self, name, address, port, replace=False):
        if not replace:
            self.create(self.obj('Service', name, {'ports': [{'name': 'tcp', 'port': port, 'targetPort': port}]}))
        endpoint = self.obj('EndpointSlice', name + '-endpoint', api='discovery.k8s.io/v1', addressType='IPv4', endpoints=[{'addresses': [address]}], ports=[{'name': 'tcp', 'port': port, 'protocol': 'TCP'}])
        endpoint['metadata']['labels'] = {'kubernetes.io/service-name': name}
        if replace:
            old = self.read('endpointslices', name + '-endpoint')
            endpoint['metadata']['resourceVersion'] = old['metadata']['resourceVersion']
            self.kubectl('route database service to restored instance', ['replace', '-f', '-'], json.dumps(endpoint).encode())
        else:
            self.create(endpoint)

    def connect_database(self, container, replace=False):
        self.command('connect owned database to task cluster network', db.DOCKER + ['network', 'connect', 'kind', container])
        address = self.command('read database task-network address', db.DOCKER + ['inspect', '--format', '{{(index .NetworkSettings.Networks "kind").IPAddress}}', container]).stdout.decode().strip()
        self.check('database network address is concrete', bool(re.fullmatch(r'\d+\.\d+\.\d+\.\d+', address)))
        self.route('restore-database', address, 5432 if self.engine == 'postgresql' else 3306, replace)

    def prepare_namespace(self):
        self.kubectl('create isolated recovery namespace', ['create', 'namespace', self.namespace])
        self.created_namespace = True
        self.route('restore-registry', self.envs['E2E_REGISTRY_IP'], 5000)
        self.registry = 'restore-registry.' + self.namespace + '.svc.cluster.local:5000'
        credentials = json.loads(Path(self.envs['E2E_REGISTRY_CREDENTIALS_FILE']).read_text())
        self.secret('restore-registry', {'username': credentials['username'], 'password': credentials['password'], 'registry': self.registry, 'allowPlainHTTP': 'true'})
        auth = base64.b64encode((credentials['username'] + ':' + credentials['password']).encode()).decode()
        image_registry = self.envs['E2E_EXECUTOR_IMAGE'].split('/')[0]
        self.secret('restore-pull', {'.dockerconfigjson': json.dumps({'auths': {image_registry: {'auth': auth}}})}, 'kubernetes.io/dockerconfigjson')
        policy_type = 'schema' if self.family == 'schema' else 'migrations'
        self.create(self.obj('ConfigMap', 'restore-policy', immutable=True, data={'policy.yaml': 'version: 1\nartifact_types:\n  - application/vnd.stokaro.ptah.' + policy_type + '.v1\n'}))

    def publish(self, revision):
        table = 'CREATE TABLE recovery_canary (id INTEGER PRIMARY KEY, value TEXT NOT NULL'
        if self.family == 'schema':
            sql = table + (', recovered INTEGER NOT NULL DEFAULT 1' if revision == 2 else '') + ');\n'
            files = {'schema.sql': sql}
            path = '/input/schema.sql'
            args = ['schema', 'push', '--schema-file', path, '--dialect', 'postgres' if self.engine == 'postgresql' else 'mysql']
        else:
            files = {'0000000001_create_canary.up.sql': table + "); INSERT INTO recovery_canary VALUES (1,'before-backup'),(2,'preserve-this-row');\n", '0000000001_create_canary.down.sql': 'DROP TABLE recovery_canary;\n'}
            if revision == 2:
                files.update({'0000000002_recover.up.sql': 'ALTER TABLE recovery_canary ADD COLUMN recovered INTEGER NOT NULL DEFAULT 1;\n', '0000000002_recover.down.sql': 'ALTER TABLE recovery_canary DROP COLUMN recovered;\n'})
            args = ['migrations', 'push', '--migrations-dir', '/input', '--dir-format', 'ptah']
        name = 'restore-publish-' + str(revision)
        self.create(self.obj('ConfigMap', name, data=files))
        reference = 'oci://' + self.registry + '/' + self.prefix + '/' + self.family + ':v' + str(revision)
        args += [reference, '--version', str(revision), '--plain-http']
        mounts = [{'name': 'input', 'mountPath': '/input/' + f, 'subPath': f, 'readOnly': True} for f in files]
        mounts.append({'name': 'work', 'mountPath': '/work'})
        env = [{'name': 'HOME', 'value': '/work'}, {'name': 'TMPDIR', 'value': '/work'}]
        for variable, key in [('PTAH_OCI_USERNAME', 'username'), ('PTAH_OCI_PASSWORD', 'password'), ('PTAH_OCI_REGISTRY', 'registry')]:
            env.append({'name': variable, 'valueFrom': {'secretKeyRef': {'name': 'restore-registry', 'key': key}}})
        job = self.create(self.obj('Job', name, {'backoffLimit': 0, 'activeDeadlineSeconds': 300, 'template': {'spec': {
            'restartPolicy': 'Never', 'automountServiceAccountToken': False, 'imagePullSecrets': [{'name': 'restore-pull'}],
            'securityContext': {'runAsNonRoot': True, 'runAsUser': 65532, 'runAsGroup': 65532, 'fsGroup': 65532, 'seccompProfile': {'type': 'RuntimeDefault'}},
            'containers': [{'name': 'publisher', 'image': self.envs['E2E_EXECUTOR_IMAGE'], 'command': ['/usr/local/bin/ptah'], 'args': args, 'env': env,
                'securityContext': {'allowPrivilegeEscalation': False, 'readOnlyRootFilesystem': True, 'capabilities': {'drop': ['ALL']}}, 'volumeMounts': mounts}],
            'volumes': [{'name': 'input', 'configMap': {'name': name}}, {'name': 'work', 'emptyDir': {'sizeLimit': '64Mi'}}]}}}, api='batch/v1'))
        end = time.monotonic() + 300
        while time.monotonic() < end:
            live = self.read('jobs', name)
            if live.get('status', {}).get('failed'):
                raise RuntimeError('artifact publisher failed')
            if live.get('status', {}).get('succeeded'):
                break
            time.sleep(2)
        else:
            raise RuntimeError('artifact publisher timed out')
        logs = self.kubectl('read publisher digest', ['logs', 'job/' + name]).stdout
        digests = re.findall(rb'sha256:[0-9a-f]{64}', logs)
        self.check('publisher returned immutable artifact digest ' + str(revision), bool(digests))
        digest = digests[-1].decode()
        self.report.setdefault('artifacts', {})[str(revision)] = {'reference': reference, 'digest': digest, 'publisherJobUID': job['metadata']['uid']}
        return reference.split(':v')[0] + '@' + digest

    def source_spec(self, reference):
        return {'ociRef': reference, 'registryAuthFrom': {'name': 'restore-registry', 'mode': 'Environment', 'usernameKey': 'username', 'passwordKey': 'password'}, 'verificationPolicyFrom': {'name': 'restore-policy', 'key': 'policy.yaml'}, 'transport': {'plainHTTP': True}}

    def approve(self, resource, plan, name, required=True):
        value = self.obj(self.kind + 'Approval', name, {self.family + 'Ref': {'name': self.name, 'uid': resource['metadata']['uid']}, 'planRef': {'name': plan['metadata']['name'], 'uid': plan['metadata']['uid']}, 'planFingerprint': plan['spec']['fingerprint']}, api=self.api)
        return self.kubectl('submit exact approval', ['create', '-f', '-', '-o', 'json'] + ([] if required else ['--dry-run=server']), json.dumps(value).encode(), required)

    def apply_jobs(self):
        jobs = self.read('jobs', False)
        return {j['metadata']['uid']: j for j in jobs['items'] if j['metadata'].get('labels', {}).get('operator.ptah.run/operation') == 'apply' and any(o.get('uid') == self.uid for o in j['metadata'].get('ownerReferences', []))}

    def stopped_apply_pods(self, job_uids):
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            selected = []
            for pod in self.read('pods', False)['items']:
                if any(o.get('uid') in job_uids for o in pod['metadata'].get('ownerReferences', [])):
                    selected.append(pod)
            if len(selected) == len(job_uids) and all(p.get('status', {}).get('phase') == 'Succeeded' and p['status'].get('containerStatuses') and all(s.get('state', {}).get('terminated', {}).get('finishedAt') for s in p['status'].get('containerStatuses', []) + p['status'].get('initContainerStatuses', [])) for p in selected):
                return selected
            time.sleep(1)
        raise RuntimeError('the exact Apply Pods did not all stop successfully')

    def begin_watch(self):
        for resource in ('jobs', 'pods'):
            baseline = self.read(resource, False)
            path = self.root / (resource + '-watch.private.json')
            output = open(path, 'wb')
            err = open(self.root / (resource + '-watch.stderr.private'), 'wb')
            api = '/apis/batch/v1' if resource == 'jobs' else '/api/v1'
            url = api + '/namespaces/' + self.namespace + '/' + resource + '?' + urlencode({'watch': 'true', 'resourceVersion': baseline['metadata']['resourceVersion']})
            process = subprocess.Popen(['kubectl', '--kubeconfig', self.envs['E2E_KUBECONFIG'], '--request-timeout=0', 'get', '--raw=' + url], stdout=output, stderr=err)
            err.close()
            self.watches[resource] = (process, output, path, baseline['items'])

    def watched_objects(self, resource, barrier):
        process, output, path, baseline = self.watches[resource]
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            self.check(resource + ' watch remains live', process.poll() is None)
            data = path.read_text()
            decoder, pos, events = json.JSONDecoder(), 0, []
            while pos < len(data):
                while pos < len(data) and data[pos].isspace():
                    pos += 1
                if pos == len(data):
                    break
                try:
                    event, pos = decoder.raw_decode(data, pos)
                except json.JSONDecodeError:
                    break
                if event.get('type') == 'ERROR':
                    raise RuntimeError(resource + ' watch returned an API error')
                events.append(event['object'])
                if event['object'].get('metadata', {}).get('uid') == barrier:
                    return baseline + events
            time.sleep(1)
        raise RuntimeError(resource + ' watch did not reach the exact sentinel')

    def checked_operation(self, metadata, pod_spec):
        labels = metadata.get('labels', {})
        operation = labels.get('operator.ptah.run/operation')
        allowed = {'resolve': 'resolve', 'verify': 'verify'}
        if self.family == 'schema':
            allowed.update({'observe': 'observe', 'plan': 'plan', 'apply': 'apply'})
        else:
            allowed.update({'history': 'migration-history', 'apply': 'migration-apply'})
        if labels.get('operator.ptah.run/' + self.family) != self.name or operation not in allowed:
            raise RuntimeError('owned workload lost its declared resource or operation')
        containers = pod_spec.get('containers', [])
        if len(containers) != 1 or containers[0].get('command') != ['/runner/ptah-runner']:
            raise RuntimeError('owned workload has an unexpected execution command')
        args = containers[0].get('args', [])
        positions = [i for i, v in enumerate(args) if v == '--operation']
        if len(positions) != 1 or positions[0] + 1 >= len(args) or args[positions[0] + 1] != allowed[operation] or any(v.startswith('--operation=') for v in args):
            raise RuntimeError('owned workload operation label disagrees with its runner command')
        return operation

    def watched_apply_jobs(self, barriers):
        jobs = self.watched_objects('jobs', barriers['jobs'])
        pods = self.watched_objects('pods', barriers['pods'])
        owned, templates = {}, {}
        for job in jobs:
            meta = job['metadata']
            owners = [o for o in meta.get('ownerReferences', []) if o.get('uid') == self.uid]
            if not owners:
                if meta.get('labels', {}).get('operator.ptah.run/' + self.family) == self.name:
                    raise RuntimeError('a Job names the resource without its exact owner identity')
                continue
            if len(owners) != 1 or owners[0].get('name') != self.name or owners[0].get('kind') != self.kind or not owners[0].get('controller') or meta.get('namespace') != self.namespace:
                raise RuntimeError('owned Job has an ambiguous resource identity')
            operation = self.checked_operation(meta, job['spec']['template']['spec'])
            uid = meta['uid']
            if uid in templates and templates[uid] != job['spec']['template']:
                raise RuntimeError('an owned Job template changed during recovery')
            templates[uid] = job['spec']['template']
            owned[uid] = (operation, meta['name'])
        applied = {uid for uid, (operation, _) in owned.items() if operation == 'apply'}
        seen, specs = {uid: set() for uid in applied}, {}
        for pod in pods:
            meta = pod['metadata']
            owners = [o for o in meta.get('ownerReferences', []) if o.get('uid') in owned]
            if not owners:
                if meta.get('labels', {}).get('operator.ptah.run/' + self.family) == self.name:
                    raise RuntimeError('a Pod names the resource without an observed owned Job')
                continue
            if len(owners) != 1 or owners[0].get('kind') != 'Job' or not owners[0].get('controller') or meta.get('namespace') != self.namespace:
                raise RuntimeError('owned Pod has an ambiguous Job identity')
            owner = owners[0]
            expected_operation, job_name = owned[owner['uid']]
            if owner['name'] != job_name or self.checked_operation(meta, pod['spec']) != expected_operation:
                raise RuntimeError('owned Pod disagrees with its exact Job')
            uid = meta['uid']
            containers = [pod['spec'].get('containers'), pod['spec'].get('initContainers'), pod['spec'].get('ephemeralContainers')]
            if uid in specs and specs[uid] != containers:
                raise RuntimeError('an owned Pod execution container changed during recovery')
            specs[uid] = containers
            if expected_operation == 'apply':
                seen[owner['uid']].add(uid)
        if not applied or any(len(values) != 1 for values in seen.values()):
            raise RuntimeError('Apply history has a missing or replacement Pod')
        return applied

    def barrier(self, name):
        pod_spec = {'restartPolicy': 'Never', 'automountServiceAccountToken': False, 'nodeSelector': {'restore-watch-boundary': 'unscheduled'}, 'containers': [{'name': 'sentinel', 'image': self.envs['E2E_RUNNER_IMAGE']}]}
        job = self.create(self.obj('Job', name, {'suspend': True, 'template': {'spec': pod_spec}}, api='batch/v1'))
        pod = self.create(self.obj('Pod', name, pod_spec))
        return {'jobs': job['metadata']['uid'], 'pods': pod['metadata']['uid']}

    def inventories(self, container):
        result = self.inspect(container)
        if self.family == 'migration':
            result['history'] = self.sql(container, 'SELECT * FROM schema_migrations ORDER BY version;').stdout
            self.check('real migration history is populated', bool(result['history'].strip()))
        return result

    def run(self):
        try:
            self.prepare_namespace()
            self.command('create isolated database network', db.DOCKER + ['network', 'create', '--internal', '--label', 'ptah.restore-run=' + self.prefix, self.prefix])
            self.network = self.prefix
            password, reader_password = secrets.token_hex(24), secrets.token_hex(24)
            if self.engine == 'mysql':
                self.mysql_client = self.env('administrator-client', {'MYSQL_PWD': password})
            reader = self.env('reader', {'PGUSER': 'drill_reader', 'PGPASSWORD': reader_password} if self.engine == 'postgresql' else {'MYSQL_PWD': reader_password})
            source, volume = self.start('source', password, True)
            if self.engine == 'postgresql':
                self.sql(source, "CREATE ROLE operator_writer LOGIN PASSWORD '" + password + "'; ALTER DATABASE drill OWNER TO operator_writer;")
                url = 'postgres://operator_writer:' + password + '@restore-database.' + self.namespace + '.svc.cluster.local:5432/drill?sslmode=disable'
            else:
                self.sql(source, "CREATE DATABASE drill; CREATE USER 'operator_writer'@'%' IDENTIFIED BY '" + password + "'; GRANT ALL PRIVILEGES ON drill.* TO 'operator_writer'@'%';", database='')
                url = 'mysql://operator_writer:' + password + '@tcp(restore-database.' + self.namespace + '.svc.cluster.local:3306)/drill'
            self.connect_database(source)
            target_secret = self.secret('restore-target', {'url': url})
            ref = self.publish(1)
            source_field = 'desired' if self.family == 'schema' else 'artifact'
            resource = self.create(self.obj(self.kind, self.name, {'target': {'engine': 'PostgreSQL' if self.engine == 'postgresql' else 'MySQL', 'urlFrom': {'name': 'restore-target', 'key': 'url'}, 'coordinationKey': self.prefix}, source_field: self.source_spec(ref), 'policy': {'apply': 'OnApproval', 'transactionMode': 'file' if self.engine == 'postgresql' else 'none'}, 'interval': '1h', 'execution': {'activeDeadlineSeconds': 300, 'connectTimeout': '10s', 'failureRetryInterval': '5s', 'imagePullSecrets': [{'name': 'restore-pull'}]}}, api=self.api))
            self.uid = resource['metadata']['uid']
            ready = self.settled('AwaitingApproval')
            old_plan = self.read(self.kind.lower() + 'plans', ready['status']['plan']['name'])
            old_approval = json.loads(self.approve(ready, old_plan, 'restore-original').stdout)
            applied = self.settled('InSync')
            if self.family == 'schema':
                self.sql(source, "INSERT INTO recovery_canary VALUES(1,'before-backup'),(2,'preserve-this-row');")
            if self.engine == 'postgresql':
                self.sql(source, "CREATE ROLE drill_reader LOGIN PASSWORD '" + reader_password + "'; GRANT CONNECT ON DATABASE drill TO drill_reader; GRANT USAGE ON SCHEMA public TO drill_reader; GRANT SELECT ON recovery_canary TO drill_reader;")
            else:
                self.sql(source, "CREATE USER 'drill_reader'@'%' IDENTIFIED BY '" + reader_password + "'; GRANT SELECT ON drill.* TO 'drill_reader'@'%';")
            self.patch({'spec': {'suspend': True}})
            suspended = self.wait('quiesce the resource for an idle backup', lambda r: r.get('spec', {}).get('suspend') is True and self.is_settled(r, 'Suspended') and any(c['type'] == 'Ready' and c['status'] == 'False' and c['reason'] == 'Suspended' and c.get('observedGeneration') == r['metadata']['generation'] for c in r.get('status', {}).get('conditions', [])))
            original_jobs = self.apply_jobs()
            self.check('initial approval dispatched exactly one successful Apply', len(original_jobs) == 1 and all(j.get('status', {}).get('succeeded') == 1 for j in original_jobs.values()))
            original_pods = self.stopped_apply_pods(set(original_jobs))
            self.check('no original executor is running at the idle recovery point', len(original_pods) == 1)
            stored_old_approval = self.read(self.kind.lower() + 'approvals', 'restore-original')
            self.check('original approval is durably consumed', any(c['type'] == 'Consumed' and c['status'] == 'True' and c['reason'] == 'DispatchCommitted' for c in stored_old_approval.get('status', {}).get('conditions', [])))
            before = self.inventories(source)
            self.check('recovery point contains exactly the fixture rows', self.sql(source, 'SELECT id FROM recovery_canary ORDER BY id;').stdout.strip() == b'1\n2')
            self.begin_watch()
            self.check('both workload watches reach the starting boundary', self.watched_apply_jobs(self.barrier('start-watch-boundary')) == set(original_jobs))
            key, wrong = self.root / 'restore-identity.private', self.root / 'wrong-identity.private'
            self.command('create backup identity', ['age-keygen', '-o', str(key)])
            self.command('create wrong backup identity', ['age-keygen', '-o', str(wrong)])
            recipient = self.command('read backup recipient', ['age-keygen', '-y', str(key)]).stdout.decode().strip()
            self.report['backupStartedAt'] = db.now()
            checkpoint = {'resource': suspended, 'initialApplied': applied, 'plan': old_plan, 'approval': stored_old_approval, 'jobs': list(original_jobs.values()), 'pods': original_pods, 'targetSecret': target_secret}
            self.encrypted('operator-checkpoint', json.dumps(checkpoint).encode(), recipient, key, wrong)
            if self.engine == 'postgresql':
                roles = self.command('back up roles', db.DOCKER + ['exec', source, 'pg_dumpall', '-h', '127.0.0.1', '--roles-only']).stdout
                archive = self.command('back up database including actual history', db.DOCKER + ['exec', source, 'pg_dump', '-h', '127.0.0.1', '-Fc', '-d', 'drill']).stdout
                paths = [('roles', self.encrypted('roles', roles, recipient, key, wrong)), ('database', self.encrypted('database', archive, recipient, key, wrong))]
                del roles, archive
            else:
                archive = self.command('back up databases and grants including actual history', db.DOCKER + ['exec', '--env-file', str(self.mysql_client), source, 'mysqldump', '--protocol=TCP', '--host=127.0.0.1', '--user=root', '--all-databases', '--single-transaction', '--routines', '--events', '--triggers', '--flush-privileges', '--set-gtid-purged=OFF']).stdout
                paths = [('database', self.encrypted('database', archive, recipient, key, wrong))]
                del archive
            self.report['backupCompletedAt'] = db.now()
            self.sql(source, "INSERT INTO recovery_canary VALUES(3,'after-backup-expected-loss');")
            old_ids = set(self.sql(source, 'SELECT id FROM recovery_canary ORDER BY id;').stdout.decode().splitlines())
            self.check('late write committed before loss', old_ids == {'1', '2', '3'})
            self.report['lossInjectedAt'] = db.now()
            clock = time.monotonic()
            self.command('destroy original database instance', db.DOCKER + ['rm', '-f', source]); self.containers.remove(source)
            self.command('destroy original database storage', db.DOCKER + ['volume', 'rm', volume]); self.volumes.remove(volume)
            restored, _ = self.start('restored', password, False)
            if self.engine == 'postgresql':
                empty = self.sql(restored, "SELECT count(*) FROM pg_database WHERE datname='drill'; SELECT count(*) FROM pg_roles WHERE rolname='operator_writer';", database='postgres').stdout.strip()
            else:
                empty = self.sql(restored, "SELECT count(*) FROM information_schema.schemata WHERE schema_name='drill'; SELECT count(*) FROM mysql.user WHERE user='operator_writer';", database='').stdout.strip()
            self.check('replacement database starts without data or writer identity', empty == b'0\n0')
            for name, path in paths:
                data = self.command('decrypt retained backup ' + name, ['age', '-d', '-i', str(key), str(path)]).stdout
                if self.engine == 'postgresql' and name == 'database':
                    self.command('restore database, owners and history', db.DOCKER + ['exec', '-i', restored, 'pg_restore', '-h', '127.0.0.1', '--exit-on-error', '--create', '-d', 'postgres'], data)
                else:
                    self.sql(restored, data.decode(), database='postgres' if self.engine == 'postgresql' else '')
                del data
            after = self.inventories(restored)
            self.report['inventories'] = {}
            for name in before:
                self.check('restored ' + name + ' matches the recovery point', bool(before[name]) and before[name] == after[name])
                self.report['inventories'][name] = {'beforeSHA256': db.digest(before[name]), 'afterSHA256': db.digest(after[name])}
            new_ids = set(self.sql(restored, 'SELECT id FROM recovery_canary ORDER BY id;').stdout.decode().splitlines())
            self.check('only declared post-backup row was lost', old_ids - new_ids == {'3'} and not (new_ids - old_ids))
            self.check('restored reader authenticates', self.sql(restored, 'SELECT count(*) FROM recovery_canary;', reader).stdout.strip() == b'2')
            denied = self.sql(restored, 'CREATE TABLE forbidden_by_grants(id integer);', reader, required=False)
            self.check('restored reader cannot mutate schema', denied.returncode != 0 and (b'permission denied for schema public' if self.engine == 'postgresql' else b'ERROR 1142') in denied.stderr)
            self.connect_database(restored, True)
            self.patch({'spec': {'suspend': False}})
            recovered = self.settled('InSync')
            self.check('operator identity survived database loss', recovered['metadata']['uid'] == self.uid)
            self.check('execution binding survived database loss', recovered['status'].get('executionBinding') == suspended['status'].get('executionBinding') and bool(suspended['status'].get('executionBinding')))
            self.check('target Secret identity survived database loss', self.read('secrets', 'restore-target')['metadata']['uid'] == target_secret['metadata']['uid'])
            self.check('old approval did not replay after database restoration', self.watched_apply_jobs(self.barrier('restored-watch-boundary')) == set(original_jobs))
            fresh_ref = self.publish(2)
            self.patch({'spec': {source_field: self.source_spec(fresh_ref)}})
            ready = self.settled('AwaitingApproval')
            fresh_plan = self.read(self.kind.lower() + 'plans', ready['status']['plan']['name'])
            self.check('recovery change has a new exact plan', fresh_plan['metadata']['uid'] != old_plan['metadata']['uid'])
            denied = self.approve(ready, old_plan, 'restore-old-replay', required=False)
            expected = b"referenced plan is no longer current for the schema" if self.family == 'schema' else b"referenced plan is no longer the migration's current plan"
            self.check('admission rejects an old plan approval', denied.returncode != 0 and expected in denied.stderr)
            self.check('no Apply before fresh authorization', self.watched_apply_jobs(self.barrier('fresh-approval-watch-boundary')) == set(original_jobs))
            fresh_approval = json.loads(self.approve(ready, fresh_plan, 'restore-fresh').stdout)
            final = self.settled('InSync')
            current_apply_jobs = self.apply_jobs()
            new_apply_uids = set(current_apply_jobs) - set(original_jobs)
            self.check('fresh authorization has one completed Apply Job', len(new_apply_uids) == 1)
            new_pods = self.stopped_apply_pods(new_apply_uids)
            old_finished = max(db.dt.datetime.fromisoformat(s['state']['terminated']['finishedAt'].replace('Z', '+00:00')) for p in original_pods for s in p['status']['containerStatuses'] + p['status'].get('initContainerStatuses', []))
            new_created = min(db.dt.datetime.fromisoformat(current_apply_jobs[u]['metadata']['creationTimestamp'].replace('Z', '+00:00')) for u in new_apply_uids)
            self.check('replacement Apply starts after original execution ended', new_created >= old_finished)
            retained = self.read(self.kind.lower() + 'approvals', 'restore-original')
            self.check('original approval identity and stamped spec are unchanged', retained['metadata']['uid'] == stored_old_approval['metadata']['uid'] and retained['spec'] == stored_old_approval['spec'])
            self.check('approved change reached the restored database', self.sql(restored, 'SELECT count(*) FROM recovery_canary WHERE recovered=1;').stdout.strip() == b'2')
            final_jobs = self.watched_apply_jobs(self.barrier('final-watch-boundary'))
            self.check('exactly one fresh Apply followed restoration', len(final_jobs - set(original_jobs)) == 1 and set(original_jobs) <= final_jobs)
            if self.family == 'migration':
                self.check('fresh approved migration is recorded in actual history', self.sql(restored, 'SELECT version FROM schema_migrations ORDER BY version;').stdout.strip() == b'1\n2')
            recovery_seconds = round(time.monotonic()-clock, 3)
            rpo_seconds = (db.dt.datetime.fromisoformat(self.report['lossInjectedAt']) - db.dt.datetime.fromisoformat(self.report['backupStartedAt'])).total_seconds()
            self.check('database recovery point is within five minutes of loss', 0 <= rpo_seconds <= 300)
            self.check('verified database-only recovery meets the thirty-minute bound', 0 < recovery_seconds <= 1800)
            self.report.update(status='PASS', expectedLostRowIDs=[3], observedLostRowIDs=sorted(int(v) for v in old_ids - new_ids), serviceRestoredAt=db.now(), recoverySeconds=recovery_seconds,
                databaseRPOUpperBoundSeconds=rpo_seconds,
                identity={'resourceUID': self.uid, 'oldPlanUID': old_plan['metadata']['uid'], 'oldApprovalUID': old_approval['metadata']['uid'], 'freshPlanUID': fresh_plan['metadata']['uid'], 'freshApprovalUID': fresh_approval['metadata']['uid'], 'originalApplyJobUIDs': sorted(original_jobs), 'freshApplyJobUIDs': sorted(final_jobs - set(original_jobs)), 'originalApplyPodUIDs': [p['metadata']['uid'] for p in original_pods], 'freshApplyPodUIDs': [p['metadata']['uid'] for p in new_pods]})
            self.persist()
        except BaseException as exc:
            self.report.update(status='FAIL', failure=type(exc).__name__ + ': ' + str(exc)); self.persist(); raise
        finally:
            clean = True
            for process, output, _, _ in self.watches.values():
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=10)
                    clean = False
                output.close()
            if self.created_namespace:
                clean &= self.kubectl('remove owned recovery namespace', ['delete', 'namespace', self.namespace, '--wait=true', '--timeout=90s'], required=False).returncode == 0
            for container in reversed(self.containers):
                clean &= self.command('remove owned container', db.DOCKER + ['rm', '-f', container], required=False).returncode == 0
            for volume in reversed(self.volumes):
                clean &= self.command('remove owned volume', db.DOCKER + ['volume', 'rm', volume], required=False).returncode == 0
            if self.network:
                clean &= self.command('remove owned network', db.DOCKER + ['network', 'rm', self.network], required=False).returncode == 0
            for file in self.env_files:
                file.unlink(missing_ok=True)
            (self.root / 'wrong-identity.private').unlink(missing_ok=True)
            self.report.update(cleanupSucceeded=bool(clean), completedAt=db.now()); self.persist()
            if not clean:
                raise RuntimeError('owned database resource cleanup failed')


if __name__ == '__main__':
    os.umask(0o077)
    parser = argparse.ArgumentParser()
    parser.add_argument('engine', choices=db.IMAGES)
    parser.add_argument('family', choices=['schema', 'migration'])
    parser.add_argument('environment', type=Path)
    parser.add_argument('output', type=Path)
    args = parser.parse_args()
    probe = OperatorProbe(args.engine, args.family, args.environment, args.output)
    probe.run()
    print(json.dumps({k: probe.report.get(k) for k in ('engine', 'family', 'status', 'recoverySeconds', 'cleanupSucceeded')}))
