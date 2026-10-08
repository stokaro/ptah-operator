#!/usr/bin/env python3
"""Native operator/database-loss pilot; other recovery cells remain required."""
import argparse
import base64
import copy
import importlib.util
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import time
from urllib.parse import urlencode
from result_first_harvest import publication
from restore_during_apply import ApplyLoss, terminal_apply_pods

REPO = Path(__file__).resolve().parents[3]
OPERATOR_CRDS = frozenset(name + '.operator.ptah.run' for name in (
    'ptahschemas', 'ptahschemaplans', 'ptahschemaapprovals', 'ptahschemaplanchunks',
    'ptahmigrations', 'ptahmigrationplans', 'ptahmigrationapprovals',
    'ptahmigrationrunacknowledgments', 'ptahrealms', 'ptahresultrecords'))
spec = importlib.util.spec_from_file_location('database_restore', REPO / 'support/qualification/probes/database_restore.py')
db = importlib.util.module_from_spec(spec)
spec.loader.exec_module(db)


class OperatorProbe(db.Probe):
    cold_cluster_recovery = False

    def __init__(self, engine, family, environment, root, loss="database", timing="idle"):
        if timing not in ('idle', 'operator-lag', 'during-apply'):
            raise ValueError('Unknown recovery timing')
        if timing == 'during-apply' and loss != 'database' and not self.cold_cluster_recovery:
            raise ValueError('This procedure supports database loss only during Apply; use cluster_restore.py for operator or combined loss')
        if timing == 'operator-lag' and loss == 'database' and not self.cold_cluster_recovery:
            raise ValueError('The operator-lag procedure must restore the older operator backup; use cluster_restore.py for an isolated recovery copy')
        super().__init__(engine, root)
        self.family = family
        self.loss = loss
        self.timing = timing
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
        self.report.update(scope='Native recovery pilot; operator loss means a namespaced-state rebuild with new UIDs. CRDs and the installed release survive. No final-artifact or full-matrix acceptance', family=family, lossType=loss, timing=timing,
            operatorRevision=self.envs['E2E_CONTROLLER_REVISION'], operatorImage=self.envs['E2E_CONTROLLER_IMAGE'], executorImage=self.envs['E2E_EXECUTOR_IMAGE'],
            procedureSHA256=db.digest(Path(__file__).read_bytes()), databaseProcedureSHA256=db.digest((REPO / 'support/qualification/probes/database_restore.py').read_bytes()))
        self.report['applyRecoveryProcedureSHA256'] = db.digest((REPO / 'support/qualification/probes/restore_during_apply.py').read_bytes())
        self.watches = {}

    @property
    def rebuilds_operator(self):
        return self.loss != 'database'

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

    @staticmethod
    def artifact_files(family, revision):
        if family not in ('schema', 'migration') or revision not in (1, 2, 3):
            raise ValueError('The recovery fixture requires a declared family and revision')
        table = 'CREATE TABLE recovery_canary (id INTEGER PRIMARY KEY, value TEXT NOT NULL'
        if family == 'schema':
            columns = ''.join(', ' + column + ' INTEGER NOT NULL DEFAULT 1'
                              for number, column in ((2, 'recovered'), (3, 'resumed')) if revision >= number)
            return {'schema.sql': table + columns + ');\n'}
        files = {'0000000001_create_canary.up.sql': table + "); INSERT INTO recovery_canary VALUES (1,'before-backup'),(2,'preserve-this-row');\n", '0000000001_create_canary.down.sql': 'DROP TABLE recovery_canary;\n'}
        for number, name, column in ((2, 'recover', 'recovered'), (3, 'resume', 'resumed')):
            if revision >= number:
                prefix = f'{number:010d}_{name}'
                files[prefix + '.up.sql'] = 'ALTER TABLE recovery_canary ADD COLUMN ' + column + ' INTEGER NOT NULL DEFAULT 1;\n'
                files[prefix + '.down.sql'] = 'ALTER TABLE recovery_canary DROP COLUMN ' + column + ';\n'
        return files

    def publish(self, revision):
        files = self.artifact_files(self.family, revision)
        if self.family == 'schema':
            path = '/input/schema.sql'
            args = ['schema', 'push', '--schema-file', path, '--dialect', 'postgres' if self.engine == 'postgresql' else 'mysql']
        else:
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

    def stopped_apply_pods(self, job_uids, require_success=True):
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            selected = []
            for pod in self.read('pods', False)['items']:
                if any(o.get('uid') in job_uids for o in pod['metadata'].get('ownerReferences', [])):
                    selected.append(pod)
            if terminal_apply_pods(selected, job_uids, require_success):
                return selected
            time.sleep(1)
        raise RuntimeError('the exact Apply Pods did not all reach the required terminal state')

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
            owners = [o for o in meta.get('ownerReferences', []) if o.get('uid') in getattr(self, 'resource_uids', {self.uid})]
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

    @staticmethod
    def rebuild_object(original):
        # A rebuild uses new API identities. Never rewrite the immutable backup
        # or restore a stale owner reference, finalizer, status or generation.
        value = copy.deepcopy(original)
        value.pop('status', None)
        value['metadata'] = {k: v for k, v in value['metadata'].items()
                             if k in ('name', 'namespace', 'labels', 'annotations')}
        if value['kind'] == 'Service':
            for key in ('clusterIP', 'clusterIPs', 'healthCheckNodePort'):
                value['spec'].pop(key, None)
        if value['kind'] in ('PtahSchema', 'PtahMigration'):
            value['spec']['suspend'] = True
            value['spec'].setdefault('policy', {})['apply'] = 'OnApproval'
        return value

    def preserved_contract(self):
        crds = self.read('customresourcedefinitions', False)['items']
        selected = [r for r in crds if r['spec']['group'] == 'operator.ptah.run']
        self.check('every installed operator CRD is inventoried, including durable results',
                   len(selected) == len(OPERATOR_CRDS) and
                   {r['metadata']['name'] for r in selected} == OPERATOR_CRDS)
        result = [{'kind': 'CustomResourceDefinition', 'name': r['metadata']['name'],
                   'uid': r['metadata']['uid'], 'spec': r['spec']} for r in selected]
        for kind in ('mutatingwebhookconfigurations', 'validatingwebhookconfigurations'):
            r = self.read(kind, 'ptah-operator-admission')
            result.append({'kind': r['kind'], 'name': r['metadata']['name'],
                           'uid': r['metadata']['uid'], 'webhooks': r['webhooks']})
        return sorted(result, key=lambda r: (r['kind'], r['name']))

    def namespace_backup(self):
        dependencies = []
        for kind, names in (
                ('secrets', ('restore-target', 'restore-registry', 'restore-pull')),
                ('configmaps', ('restore-policy',)),
                ('services', ('restore-registry', 'restore-database')),
                ('endpointslices', ('restore-registry-endpoint', 'restore-database-endpoint'))):
            dependencies += [self.read(kind, name) for name in names]
        plans = self.read(self.kind.lower() + 'plans', False)['items']
        approvals = self.read(self.kind.lower() + 'approvals', False)['items']
        chunks = self.read('ptahschemaplanchunks', False)['items'] if self.family == 'schema' else []
        self.check('namespace backup contains plans and consumed approvals', bool(plans) and bool(approvals))
        if self.family == 'schema':
            published = {(ref['name'], ref['uid']) for plan in plans
                         for ref in plan.get('status', {}).get('publishedChunks', [])}
            actual = {(r['metadata']['name'], r['metadata']['uid']) for r in chunks}
            self.check('backup retains every exact published plan chunk', bool(published) and published <= actual)
        return {'namespace': self.read('namespaces', self.namespace), 'dependencies': dependencies,
                'plans': plans, 'approvals': approvals, 'chunks': chunks,
                'jobs': self.read('jobs', False)['items'], 'pods': self.read('pods', False)['items'],
                'preservedContract': self.preserved_contract(), 'durableResults': self.result_backup()}

    def result_backup(self):
        """Return confidential backup material; callers must encrypt this object."""
        namespace = self.envs['E2E_OPERATOR_NAMESPACE']
        def installed(kind, name=None):
            return json.loads(self.command('inventory result recovery ' + kind,
                ['kubectl', '--kubeconfig', self.envs['E2E_KUBECONFIG'], '--request-timeout=30s',
                 '-n', namespace, 'get', kind] + ([name] if name else []) + ['-o', 'json']).stdout)
        deployments = installed('deployments')['items']
        rotators = [d for d in deployments if any(a.startswith('--result-journal-secret-name=')
                    for c in d['spec']['template']['spec']['containers'] for a in c.get('args', []))]
        if not rotators:
            self.check('disabled result delivery has no omitted durable records',
                       not self.read('ptahresultrecords', False)['items'])
            return {'enabled': False}
        self.check('one installed rotator owns result trust', len(rotators) == 1)
        args = [a for c in rotators[0]['spec']['template']['spec']['containers'] for a in c.get('args', [])]
        def argument(flag):
            values = [a.split('=', 1)[1] for a in args if a.startswith(flag + '=')]
            self.check('one concrete ' + flag + ' is inventoried', len(values) == 1 and bool(values[0]))
            return values[0]
        trust = installed('secret', argument('--result-secret-name'))
        journal = installed('secret', argument('--result-journal-secret-name'))
        policy = installed('configmap', argument('--result-enrollment-policy'))
        records = self.read('ptahresultrecords', False)['items']
        credential_uids = {r['metadata']['uid'] for r in records if r['spec']['type'] == 'credential'}
        projections = [s for s in self.read('secrets', False)['items']
                       if any(o['uid'] in credential_uids for o in s['metadata'].get('ownerReferences', []))]
        complete = []
        for record in records:
            if record['spec']['type'] != 'intent':
                continue
            cohort = {r['metadata']['name']: r for r in records
                      if r['metadata'].get('namespace') == record['metadata'].get('namespace')}
            manifest = json.loads(base64.b64decode(record['spec']['data'], validate=True))
            if 'inline' in manifest or record['metadata']['name'] + '-complete' in cohort:
                complete.append(publication(cohort, manifest['binding']['jobUID']))
        self.check('durable backup includes completed results and credentials',
                   bool(complete) and bool(credential_uids))
        self.validate_result_material(trust, journal, policy)
        return {'enabled': True, 'records': records, 'credentialProjections': projections,
                'trust': trust, 'journal': journal, 'enrollmentPolicy': policy,
                'restoreRule': 'Preserve original UIDs and owner bindings for a control-plane restore. In a rebuild, archive these objects as evidence; never rewrite their bindings or replay them as new authorization.'}

    @staticmethod
    def validate_result_material(trust, journal, policy):
        fields = {'tls.crt', 'tls.key', 'ca.crt', 'client-ca.crt', 'client-ca.key', 'client-trust.crt'}
        if set(trust.get('data', {})) != fields or not all(trust['data'].values()):
            raise RuntimeError('Result backup is missing receiver trust or signing keys')
        if set(journal.get('data', {})) != {'rotation.json'} or not policy.get('data'):
            raise RuntimeError('Result backup is missing the rotation journal or enrollment policy')
        state = json.loads(base64.b64decode(journal['data']['rotation.json'], validate=True))
        if (not trust['metadata'].get('uid') or not policy['metadata'].get('uid')
                or state.get('projectionUID') != trust['metadata']['uid']
                or state.get('policyUID') != policy['metadata']['uid']):
            raise RuntimeError('Result journal binds different trust or enrollment identities')

    def lose_namespace(self):
        self.kubectl('destroy the original operator-state namespace',
                     ['delete', 'namespace', self.namespace, '--wait=true', '--timeout=90s'])
        self.created_namespace = False
        gone = self.kubectl('verify original namespace was deleted',
                            ['get', 'namespace', self.namespace], required=False)
        self.check('original namespace is absent before restoration',
                   gone.returncode != 0 and b'NotFound' in gone.stderr and self.namespace.encode() in gone.stderr)

    def verify_restored_contract(self, original):
        self.check('installed CRDs and admission identity survived namespace loss',
                   original == self.preserved_contract())

    def restore_namespace(self, checkpoint_path, key):
        data = self.command('decrypt retained operator-state backup',
                            ['age', '-d', '-i', str(key), str(checkpoint_path)]).stdout
        self.check('restored operator backup matches its recorded checksum',
                   db.digest(data) == self.report['backups']['operator-checkpoint']['plaintextSHA256'])
        saved = json.loads(data)
        del data
        archive = saved['namespaceState']
        self.kubectl('recreate the lost namespace', ['create', 'namespace', self.namespace])
        self.created_namespace = True
        mappings = []
        for original in archive['dependencies']:
            restored = self.create(self.rebuild_object(original))
            mappings.append({'kind': original['kind'], 'name': original['metadata']['name'],
                             'oldUID': original['metadata']['uid'], 'newUID': restored['metadata']['uid']})
        restored = self.create(self.rebuild_object(saved['resource']))
        self.uid = restored['metadata']['uid']
        self.resource_uids.add(self.uid)
        mappings.append({'kind': self.kind, 'name': self.name,
                         'oldUID': saved['resource']['metadata']['uid'], 'newUID': self.uid})
        ns = self.read('namespaces', self.namespace)
        mappings.append({'kind': 'Namespace', 'name': self.namespace,
                         'oldUID': archive['namespace']['metadata']['uid'], 'newUID': ns['metadata']['uid']})
        self.check('all rebuilt namespace and dependency identities are new',
                   len(mappings) == 10 and all(r['oldUID'] != r['newUID'] for r in mappings))
        paused = self.settled('Suspended')
        self.check('the rebuilt resource starts suspended with explicit approval',
                   paused['spec']['suspend'] and paused['spec']['policy']['apply'] == 'OnApproval')
        self.verify_restored_contract(archive['preservedContract'])
        self.report['rebuild'] = {'identityMappings': mappings,
                                 'preservedContractSHA256': db.digest(json.dumps(archive['preservedContract'], sort_keys=True).encode()),
                                 'archiveOnly': 'Old plans, chunks, approvals and execution records retain their original identities in the encrypted backup. They are not re-created as authorization for new UIDs.',
                                 'controlledSpecEdits': {'suspend': True, 'policy.apply': 'OnApproval'}}
        self.persist()

    def inventories(self, container):
        result = self.inspect(container)
        if self.family == 'migration':
            result['history'] = self.sql(container, 'SELECT * FROM schema_migrations ORDER BY version;').stdout
            self.check('real migration history is populated', bool(result['history'].strip()))
        return result

    def verify_recovered_results(self, checkpoint, fresh_apply_uids):
        original = checkpoint.get('namespaceState', checkpoint).get('durableResults', {})
        if not original.get('enabled'):
            return
        restored = self.result_backup()
        self.check('durable delivery remains enabled after recovery', restored['enabled'])
        records = {r['metadata']['name']: r for r in restored['records']}
        receipts = []
        for job_uid in fresh_apply_uids:
            intent, receipt, payload = publication(records, job_uid)
            binding = json.loads(base64.b64decode(intent['spec']['data']))['binding']
            self.check('restored controller consumed successful fresh Apply bytes',
                       binding['uid'] == self.uid and binding['namespace'] == self.namespace
                       and binding['operation'] == ('apply' if self.family == 'schema' else 'migration-apply')
                       and json.loads(payload)['childExitCode'] == 0)
            receipts.append({'jobUID': job_uid, 'receiptUID': receipt['metadata']['uid'],
                             'payloadSHA256': db.digest(payload)})
        self.check('one fresh Apply has a complete durable receipt', len(receipts) == 1)
        identities = {name: {'originalUID': original[name]['metadata']['uid'],
                             'recoveredUID': restored[name]['metadata']['uid']}
                      for name in ('trust', 'journal', 'enrollmentPolicy')}
        replacement = bool(self.report.get('targetCluster'))
        self.check('result trust follows the declared retained-or-rebuilt installation',
                   all((r['originalUID'] != r['recoveredUID']) == replacement for r in identities.values()))
        self.report['recoveredDelivery'] = {'receipts': receipts, 'installationIdentities': identities,
            'originalRecordsArchived': len(original['records']),
            'mode': 'new installation and authority; original bindings remain archived' if replacement else 'retained installation and authority'}

    @staticmethod
    def wait_for_backup_lag(completed, monotonic=time.monotonic, sleep=time.sleep):
        # Date the gap from completed backup bytes, not from the poll that
        # noticed a later Apply. A wall-clock jump cannot shorten this wait.
        while (remaining := completed + 300 - monotonic()) > 0:
            sleep(min(remaining, 30))

    def advance_lagged_database(self, source, source_field, original_jobs, original_pods, recipient, key, wrong):
        reference = self.publish(2)
        self.patch({'spec': {source_field: self.source_spec(reference), 'suspend': False}})
        ready = self.settled('AwaitingApproval')
        plan = self.read(self.kind.lower() + 'plans', ready['status']['plan']['name'])
        self.check('the lagged backup has not authorized the intervening change',
                   self.watched_apply_jobs(self.barrier('lag-unapproved-boundary')) == set(original_jobs))
        self.wait_for_backup_lag(self.operator_backup_completed)
        approval = json.loads(self.approve(ready, plan, 'restore-intervening').stdout)
        applied = self.settled('InSync')
        self.patch({'spec': {'suspend': True}})
        suspended = self.settled('Suspended')
        live_jobs = self.apply_jobs()
        added = set(live_jobs) - set(original_jobs)
        self.check('one approved Apply committed after the older operator backup',
                   len(added) == 1 and all(j.get('status', {}).get('succeeded') == 1 for j in live_jobs.values()))
        # The controller's completed-Job TTL is five minutes. Keep the already
        # verified terminal evidence from the immutable recovery point rather
        # than requiring those objects to survive the deliberate backup lag.
        jobs = {**original_jobs, **live_jobs}
        pods = original_pods + self.stopped_apply_pods(added)
        self.check('the intervening Apply added its actual database column',
                   self.sql(source, 'SELECT count(*) FROM recovery_canary WHERE recovered=1;').stdout.strip() == b'2')
        if self.family == 'migration':
            self.check('the intervening migration is in actual database history',
                       self.sql(source, 'SELECT version FROM schema_migrations ORDER BY version;').stdout.strip() == b'1\n2')
        self.check('the complete watch accounts for both pre-loss Applies',
                   self.watched_apply_jobs(self.barrier('lag-committed-boundary')) == set(jobs))
        evidence = {'sourceField': source_field, 'reference': reference, 'plan': plan,
                    'approval': self.read(self.kind.lower() + 'approvals', 'restore-intervening'),
                    'applied': applied, 'suspended': suspended, 'jobs': list(jobs.values()), 'pods': pods}
        if self.family == 'schema':
            expected = {(ref['name'], ref['uid']) for ref in plan.get('status', {}).get('publishedChunks', [])}
            chunks = self.read('ptahschemaplanchunks', False)['items']
            evidence['chunks'] = [chunk for chunk in chunks
                                  if (chunk['metadata']['name'], chunk['metadata']['uid']) in expected]
            self.check('intervening execution evidence includes every exact schema SQL chunk',
                       bool(expected) and len(evidence['chunks']) == len(expected) and
                       {(chunk['metadata']['name'], chunk['metadata']['uid']) for chunk in evidence['chunks']} == expected)
        self.encrypted('intervening-execution', json.dumps(evidence).encode(), recipient, key, wrong)
        self.report['interveningExecution'] = {'artifactDigest': self.report['artifacts']['2']['digest'],
            'planUID': plan['metadata']['uid'], 'approvalUID': approval['metadata']['uid'],
            'jobUIDs': sorted(added), 'completedAt': db.now()}
        self.report['consistencyAssumptions'] = ('The immutable operator checkpoint predates a separately approved, '
            'completed change by at least five minutes. Database backup starts only after that executor has '
            'stopped and the resource is suspended again. Recovery retains the old operator source while '
            'suspended, diagnoses the newer database, then explicitly selects the already-committed artifact '
            'from separate encrypted execution evidence. No active DDL is included in the backup.')
        self.persist()
        return self.inventories(source), jobs, pods

    def select_diagnosed_source(self, checkpoint_path, key, source_field):
        # Do not silently reconstruct the newer desired state from memory.
        # Read both immutable archives and record the controlled recovery edit.
        archives = {}
        for name, path in (('operator-checkpoint', checkpoint_path),
                           ('intervening-execution', self.root / 'intervening-execution.age'),
                           ('operator-update', self.root / 'operator-update.age')):
            payload = self.command('read the original ' + name + ' during lag diagnosis',
                                   ['age', '-d', '-i', str(key), str(path)]).stdout
            self.check(name + ' still matches its recorded recovery bytes',
                       db.digest(payload) == self.report['backups'][name]['plaintextSHA256'])
            archives[name] = json.loads(payload)
        old, later = archives['operator-checkpoint'], archives['intervening-execution']
        update = archives['operator-update']
        self.validate_lagged_update(old, later, update, self.report['backups'])
        live = self.settled('Suspended')
        self.check('the restored operator really retains the older source while suspended',
                   live['spec']['suspend'] is True and live['spec']['policy']['apply'] == 'OnApproval' and
                   live['spec'][source_field] == old['resource']['spec'][source_field] and
                   later['sourceField'] == source_field and
                   old['resource']['spec'][source_field]['ociRef'] != later['reference'])
        self.check('diagnosis has not replayed an old Apply',
                   self.watched_apply_jobs(self.barrier('lag-diagnosis-boundary')) ==
                   {job['metadata']['uid'] for job in later['jobs']})
        self.patch({'spec': {source_field: self.source_spec(later['reference'])}})
        selected = self.settled('Suspended')
        self.check('the diagnosed source is selected without enabling execution',
                   selected['spec']['suspend'] is True and selected['spec']['policy']['apply'] == 'OnApproval' and
                   selected['spec'][source_field] == later['suspended']['spec'][source_field])
        self.report['lagDiagnosis'] = {'completedAt': db.now(),
            'operatorCheckpointSHA256': self.report['backups']['operator-checkpoint']['plaintextSHA256'],
            'interveningEvidenceSHA256': self.report['backups']['intervening-execution']['plaintextSHA256'],
            'operatorUpdateSHA256': self.report['backups']['operator-update']['plaintextSHA256'],
            'recoveryPointStartedAt': update['startedAt'],
            'controlledSourceChange': {'from': old['resource']['spec'][source_field]['ociRef'], 'to': later['reference']},
            'decision': 'The restored database matches the newer completed execution. Select that immutable artifact while suspended; retain OnApproval and do not replay the older source.'}
        self.lag_execution = later
        self.persist()

    @staticmethod
    def validate_lagged_update(old, later, update, backups):
        """The later recovery point must cover the whole declared rebuild.

        This procedure changes only the desired artifact between checkpoints.
        Refuse a changed dependency or installation instead of silently
        restoring its older bytes. Execution and result identities stay in
        the encrypted archives; they never become replacement authorization.
        """
        def require(condition, reason):
            if not condition:
                raise RuntimeError('Incomplete lagged recovery kit: ' + reason)
        require(update.get('baseSHA256') == backups['operator-checkpoint']['plaintextSHA256'] and
                update.get('executionSHA256') == backups['intervening-execution']['plaintextSHA256'],
                'checkpoint or execution binding differs')
        started = db.dt.datetime.fromisoformat(update['startedAt'])
        require(started.tzinfo is not None, 'recovery point has no time zone')
        previous, current = old['resource'], update['resource']
        field = later['sourceField']
        require(previous['metadata'].get('uid') and previous['metadata']['uid'] == current['metadata'].get('uid') ==
                later['suspended']['metadata'].get('uid'), 'resource identity differs')
        expected = copy.deepcopy(previous['spec']); expected[field] = later['suspended']['spec'][field]
        require(current['spec'] == expected == later['suspended']['spec'] and
                current['spec']['suspend'] is True and current['spec']['policy']['apply'] == 'OnApproval' and
                current['spec'][field]['ociRef'] == later['reference'] and
                not current.get('status', {}).get('activeOperation') and
                not current.get('status', {}).get('unresolvedRun') and
                not current['metadata'].get('annotations', {}).get('operator.ptah.run/unresolved-run'),
                'resource is not the declared quiescent update')
        base, fresh = old['namespaceState'], update['namespaceState']
        required = {'namespace', 'dependencies', 'plans', 'approvals', 'chunks', 'jobs', 'pods',
                    'preservedContract', 'durableResults'}
        require(required <= set(base) and required <= set(fresh), 'namespace inventory is incomplete')
        require(base['namespace']['metadata']['uid'] == fresh['namespace']['metadata']['uid'], 'namespace identity differs')
        def dependencies(objects):
            values = {}
            for obj in objects:
                identity = (obj['kind'], obj['metadata']['name'], obj['metadata']['uid'])
                require(identity not in values, 'duplicate dependency')
                values[identity] = {k: v for k, v in obj.items() if k != 'metadata'}
                values[identity]['metadata'] = {k: v for k, v in obj['metadata'].items()
                                               if k not in ('resourceVersion', 'managedFields')}
            require(len(values) == 8, 'the rebuild requires all eight dependencies')
            return values
        require(dependencies(base['dependencies']) == dependencies(fresh['dependencies']), 'dependency changed after the base backup')
        for name in ('preservedContract', 'release', 'sourceCluster'):
            require(base.get(name) == fresh.get(name), name + ' changed after the base backup')
        require(bool(base['preservedContract']), 'installation contract is empty')
        for collection, item in (('plans', later['plan']), ('approvals', later['approval'])):
            matches = [r for r in fresh[collection] if r['metadata']['uid'] == item['metadata']['uid']]
            require(len(matches) == 1 and matches[0]['spec'] == item['spec'], 'intervening ' + collection + ' missing or changed')
        require(any(c.get('type') == 'Consumed' and c.get('status') == 'True'
                    for c in later['approval'].get('status', {}).get('conditions', [])), 'intervening approval was not consumed')
        jobs = {job['metadata']['uid'] for job in later['jobs']}
        require(len(jobs) == len(later['jobs']) == 2 and terminal_apply_pods(later['pods'], jobs),
                'both exact pre-loss executions must have stopped')
        original_jobs = {j['metadata']['uid'] for j in old['jobs']}
        new_jobs = jobs - original_jobs
        require(len(original_jobs) == len(new_jobs) == 1 and original_jobs < jobs,
                'the intervening execution is not distinct')
        for uid in new_jobs:
            require(any(j['metadata']['uid'] == uid for j in fresh['jobs']) and
                    any(any(o.get('uid') == uid for o in p['metadata'].get('ownerReferences', []))
                        for p in fresh['pods']), 'the latest inventory omitted the intervening executor')
        for chunk in later.get('chunks', []):
            matches = [r for r in fresh['chunks'] if r['metadata']['uid'] == chunk['metadata']['uid']]
            require(len(matches) == 1 and matches[0]['spec'] == chunk['spec'], 'intervening plan chunk missing or changed')
        results = fresh['durableResults']
        require(results.get('enabled') is base['durableResults'].get('enabled') and
                type(results.get('enabled')) is bool, 'result delivery mode differs')
        if results['enabled']:
            OperatorProbe.validate_result_material(results['trust'], results['journal'], results['enrollmentPolicy'])
            records = {r['metadata']['name']: r for r in results['records']}
            for uid in new_jobs:
                intent, _, payload = publication(records, uid)
                binding = json.loads(base64.b64decode(intent['spec']['data']))['binding']
                require(binding['uid'] == current['metadata']['uid'] and
                        binding['namespace'] == current['metadata']['namespace'] and
                        binding['operation'] == ('migration-apply' if current['kind'] == 'PtahMigration' else 'apply') and
                        json.loads(payload)['childExitCode'] == 0, 'intervening durable result is not the resource success')

    def backup_lagged_update(self, checkpoint, recipient, key, wrong):
        started = db.now()
        payload = self.command('read encrypted execution evidence before the recovery update',
                               ['age', '-d', '-i', str(key), str(self.root / 'intervening-execution.age')]).stdout
        self.check('the recovery update binds the recorded intervening bytes',
                   db.digest(payload) == self.report['backups']['intervening-execution']['plaintextSHA256'])
        later = json.loads(payload)
        update = {'startedAt': started, 'baseSHA256': self.report['backups']['operator-checkpoint']['plaintextSHA256'],
                  'executionSHA256': self.report['backups']['intervening-execution']['plaintextSHA256'],
                  'resource': self.settled('Suspended'), 'namespaceState': self.namespace_backup()}
        self.validate_lagged_update(checkpoint, later, update, self.report['backups'])
        self.encrypted('operator-update', json.dumps(update).encode(), recipient, key, wrong)
        self.recovery_update = update
        self.report['operatorUpdateCompletedAt'] = db.now()
        self.persist()

    @staticmethod
    def lagged_recovery_point(report):
        """Date only the complete kit actually read during recovery."""
        diagnosis = report['lagDiagnosis']
        for archive, field in (('operator-checkpoint', 'operatorCheckpointSHA256'),
                               ('intervening-execution', 'interveningEvidenceSHA256'),
                               ('operator-update', 'operatorUpdateSHA256')):
            digest = report['backups'][archive]['plaintextSHA256']
            if not re.fullmatch('[0-9a-f]{64}', digest) or diagnosis[field] != digest:
                raise ValueError('Recovery diagnosis does not bind the complete backup kit')
        point = db.dt.datetime.fromisoformat(diagnosis['recoveryPointStartedAt'])
        completed = db.dt.datetime.fromisoformat(report['operatorUpdateCompletedAt'])
        database = db.dt.datetime.fromisoformat(report['databaseBackupStartedAt'])
        loss = db.dt.datetime.fromisoformat(report['lossInjectedAt'])
        if not all(t.tzinfo is not None for t in (point, completed, database, loss)) or not point <= completed <= database <= loss:
            raise ValueError('Operator update was not completed before database backup and loss')
        age = (loss - point).total_seconds()
        if not 0 <= age <= 300:
            raise ValueError('The complete operator recovery point exceeds five minutes')
        return age

    @staticmethod
    def finalize_result(report):
        # A completed restore is useful diagnostic evidence even if its RPO
        # failed. Keep that distinction in both the retained record and exit
        # status; consumers must not need to discover a hidden failed field.
        if report.get('status') != 'PASS':
            return
        if report.get('cleanupSucceeded') is not True:
            report.update(status='FAIL', failure='Owned recovery resource cleanup did not succeed')
            return
        rpo = report.get('profileRPO', {})
        operator_ok = rpo.get('operatorBase') == 'PASS'
        if 'combinedOperatorRecoveryKit' in rpo:
            operator_ok = False
            if report.get('timing') == 'operator-lag' and rpo['combinedOperatorRecoveryKit'] == 'PASS':
                try:
                    OperatorProbe.lagged_recovery_point(report)
                    operator_ok = True
                except (KeyError, ValueError, TypeError):
                    pass
        if rpo.get('database') != 'PASS' or not operator_ok:
            report.update(status='FAIL', failure='Recovery completed, but the required recovery-point objective is not established')

    def run(self):
        in_flight = None
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
            original_uid = self.uid
            self.resource_uids = {self.uid}
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
            if self.rebuilds_operator:
                checkpoint['namespaceState'] = self.namespace_backup()
            else:
                checkpoint['durableResults'] = self.result_backup()
            checkpoint_path = self.encrypted('operator-checkpoint', json.dumps(checkpoint).encode(), recipient, key, wrong)
            self.report['operatorBackupCompletedAt'] = db.now()
            self.operator_backup_completed = time.monotonic()
            if self.timing == 'operator-lag':
                before, original_jobs, original_pods = self.advance_lagged_database(
                    source, source_field, original_jobs, original_pods, recipient, key, wrong)
                self.backup_lagged_update(checkpoint, recipient, key, wrong)
            self.report['databaseBackupStartedAt'] = db.now()
            if self.timing == 'operator-lag':
                lag = (db.dt.datetime.fromisoformat(self.report['databaseBackupStartedAt']) -
                       db.dt.datetime.fromisoformat(self.report['operatorBackupCompletedAt'])).total_seconds()
                self.check('the operator backup is at least five minutes older than the database backup',
                           lag >= 300 and time.monotonic() - self.operator_backup_completed >= 300)
                self.report['operatorBackupLagSeconds'] = lag
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
            self.sql(source, "INSERT INTO recovery_canary(id,value) VALUES(3,'after-backup-expected-loss');")
            old_ids = set(self.sql(source, 'SELECT id FROM recovery_canary ORDER BY id;').stdout.decode().splitlines())
            self.check('late write committed before loss', old_ids == {'1', '2', '3'})
            if self.loss == 'operator':
                before['rows'] = self.inspect(source)['rows']
            if self.timing == 'during-apply':
                in_flight = ApplyLoss(self, db.DOCKER)
                original_jobs = in_flight.arm(source, source_field, original_jobs, recipient, key, wrong)
                in_flight.verify_loss_boundary(source)
            self.report['lossInjectedAt'] = db.now()
            if in_flight:
                loss_at = db.dt.datetime.fromisoformat(self.report['lossInjectedAt'])
                self.check('both recovery points are fresh before injecting in-flight loss',
                           all(0 <= (loss_at - db.dt.datetime.fromisoformat(self.report[field])).total_seconds() <= 300
                               for field in ('backupStartedAt', 'databaseBackupStartedAt')))
            clock = time.monotonic()
            if self.rebuilds_operator:
                self.lose_namespace()
                if in_flight:
                    self.check('the original cluster is destroyed before fencing surviving SQL',
                               self.cold_cluster_recovery and bool(self.report.get('sourceDestroyedAt')) and
                               self.report['checks'].get('the original control plane and workload nodes were destroyed') is True)
                    in_flight.fence(source, password)
            if self.loss == 'operator':
                restored = source
            else:
                self.command('destroy original database instance', db.DOCKER + ['rm', '-f', source]); self.containers.remove(source)
                self.command('destroy original database storage', db.DOCKER + ['volume', 'rm', volume]); self.volumes.remove(volume)
                if in_flight:
                    in_flight.close()
                    if self.loss == 'database':
                        original_pods += in_flight.stopped(source_field, suspended['spec'][source_field])
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
            if self.rebuilds_operator:
                self.restore_namespace(checkpoint_path, key)
            after = self.inventories(restored)
            self.report['inventories'] = {}
            for name in before:
                self.check('restored ' + name + ' matches the recovery point', bool(before[name]) and before[name] == after[name])
                self.report['inventories'][name] = {'beforeSHA256': db.digest(before[name]), 'afterSHA256': db.digest(after[name])}
            new_ids = set(self.sql(restored, 'SELECT id FROM recovery_canary ORDER BY id;').stdout.decode().splitlines())
            expected_loss = set() if self.loss == 'operator' else {'3'}
            self.check('only the declared data loss occurred', old_ids - new_ids == expected_loss and not (new_ids - old_ids))
            self.check('restored reader authenticates', self.sql(restored, 'SELECT count(*) FROM recovery_canary;', reader).stdout.strip() == str(len(new_ids)).encode())
            denied = self.sql(restored, 'CREATE TABLE forbidden_by_grants(id integer);', reader, required=False)
            self.check('restored reader cannot mutate schema', denied.returncode != 0 and (b'permission denied for schema public' if self.engine == 'postgresql' else b'ERROR 1142') in denied.stderr)
            if self.loss != 'operator':
                self.connect_database(restored, True)
            if in_flight:
                in_flight.diagnose(restored)
                in_flight.allow_recovered_writer(restored, password)
            if self.timing == 'operator-lag':
                self.select_diagnosed_source(checkpoint_path, key, source_field)
            self.patch({'spec': {'suspend': False}})
            recovered = self.settled('InSync')
            if in_flight:
                in_flight.verify_resolution(recovered)
            if not self.rebuilds_operator:
                self.check('operator identity survived database loss', recovered['metadata']['uid'] == original_uid)
                self.check('execution binding survived database loss', recovered['status'].get('executionBinding') == suspended['status'].get('executionBinding') and bool(suspended['status'].get('executionBinding')))
                self.check('target Secret identity survived database loss', self.read('secrets', 'restore-target')['metadata']['uid'] == target_secret['metadata']['uid'])
            else:
                self.check('the rebuilt resource has a fresh execution binding',
                           recovered['metadata']['uid'] != original_uid and bool(recovered['status'].get('executionBinding')) and
                           recovered['status']['executionBinding']['epoch'] != suspended['status']['executionBinding']['epoch'])
                self.check('the target Secret was restored with a new identity',
                           self.read('secrets', 'restore-target')['metadata']['uid'] != target_secret['metadata']['uid'])
            self.check('old approval did not replay after database restoration', self.watched_apply_jobs(self.barrier('restored-watch-boundary')) == set(original_jobs))
            final_revision = 3 if self.timing in ('operator-lag', 'during-apply') else 2
            fresh_ref = self.publish(final_revision)
            self.patch({'spec': {source_field: self.source_spec(fresh_ref)}})
            ready = self.settled('AwaitingApproval')
            fresh_plan = self.read(self.kind.lower() + 'plans', ready['status']['plan']['name'])
            self.check('recovery change has a new exact plan', fresh_plan['metadata']['uid'] != old_plan['metadata']['uid'])
            denied = self.approve(ready, old_plan, 'restore-old-replay', required=False)
            expected = b"referenced plan is no longer current for the schema" if self.family == 'schema' else b"referenced plan is no longer the migration's current plan"
            if not self.rebuilds_operator:
                self.check('admission rejects an old plan approval', denied.returncode != 0 and expected in denied.stderr)
                if in_flight:
                    denied_in_flight = self.approve(ready, in_flight.plan, 'restore-in-flight-replay', required=False)
                    self.check('admission rejects replay of the interrupted Apply plan',
                               denied_in_flight.returncode != 0 and expected in denied_in_flight.stderr)
            else:
                expected = b'read referenced plan:' if self.family == 'schema' else b'read referenced migration plan:'
                self.check('admission refuses the original backup plan approval',
                           denied.returncode != 0 and expected in denied.stderr and b'not found' in denied.stderr and old_plan['metadata']['name'].encode() in denied.stderr)
                denied_uid = self.approve(resource, fresh_plan, 'restore-old-resource', required=False)
                self.check('admission refuses the old resource UID against the fresh plan',
                           denied_uid.returncode != 0 and ('approval ' + self.family + ' reference does not match the plan').encode() in denied_uid.stderr)
                if in_flight:
                    denied_in_flight = self.approve(ready, in_flight.plan, 'restore-in-flight-replay', required=False)
                    self.check('admission refuses the interrupted Apply plan from the lost cluster',
                               denied_in_flight.returncode != 0 and expected in denied_in_flight.stderr and
                               b'not found' in denied_in_flight.stderr and in_flight.plan['metadata']['name'].encode() in denied_in_flight.stderr)
                    absent = self.kubectl('confirm interrupted authorization was not recreated',
                                         ['get', self.kind.lower() + 'approvals', 'restore-in-flight'], required=False)
                    self.check('the replacement contains no recreated in-flight approval',
                               absent.returncode != 0 and b'NotFound' in absent.stderr and b'restore-in-flight' in absent.stderr)
                if self.timing == 'operator-lag':
                    later_plan = self.lag_execution['plan']
                    denied_later = self.approve(ready, later_plan, 'restore-intervening-replay', required=False)
                    self.check('admission also refuses replay of the intervening completed plan',
                               denied_later.returncode != 0 and expected in denied_later.stderr and
                               b'not found' in denied_later.stderr and later_plan['metadata']['name'].encode() in denied_later.stderr)
                    absent = self.kubectl('confirm intervening authorization was not recreated',
                                          ['get', self.kind.lower() + 'approvals', 'restore-intervening'], required=False)
                    self.check('the rebuilt namespace contains no recreated intervening approval',
                               absent.returncode != 0 and b'NotFound' in absent.stderr and b'restore-intervening' in absent.stderr)
            self.check('no Apply before fresh authorization', self.watched_apply_jobs(self.barrier('fresh-approval-watch-boundary')) == set(original_jobs))
            fresh_approval = json.loads(self.approve(ready, fresh_plan, 'restore-fresh').stdout)
            final = self.settled('InSync')
            current_apply_jobs = self.apply_jobs()
            new_apply_uids = set(current_apply_jobs) - set(original_jobs)
            self.check('fresh authorization has one completed Apply Job', len(new_apply_uids) == 1)
            new_pods = self.stopped_apply_pods(new_apply_uids)
            old_finished = max(db.dt.datetime.fromisoformat(s['state']['terminated']['finishedAt'].replace('Z', '+00:00')) for p in original_pods for s in p['status']['containerStatuses'] + p['status'].get('initContainerStatuses', []))
            if in_flight and self.loss != 'database':
                old_finished = max(old_finished, in_flight.execution_ended_at)
            new_created = min(db.dt.datetime.fromisoformat(current_apply_jobs[u]['metadata']['creationTimestamp'].replace('Z', '+00:00')) for u in new_apply_uids)
            self.check('replacement Apply starts after original execution ended', new_created >= old_finished)
            if not self.rebuilds_operator:
                retained = self.read(self.kind.lower() + 'approvals', 'restore-original')
                self.check('original approval identity and stamped spec are unchanged', retained['metadata']['uid'] == stored_old_approval['metadata']['uid'] and retained['spec'] == stored_old_approval['spec'])
                if in_flight:
                    retained = self.read(self.kind.lower() + 'approvals', 'restore-in-flight')
                    self.check('the interrupted approval remains the original consumed decision',
                               retained['metadata']['uid'] == in_flight.evidence['approval']['metadata']['uid'] and
                               retained['spec'] == in_flight.evidence['approval']['spec'] and
                               any(c['type'] == 'Consumed' and c['status'] == 'True'
                                   for c in retained.get('status', {}).get('conditions', [])))
            else:
                absent = self.kubectl('confirm original authorization was not recreated',
                                      ['get', self.kind.lower() + 'approvals', 'restore-original'], required=False)
                self.check('the rebuilt namespace contains no recreated old approval',
                           absent.returncode != 0 and b'NotFound' in absent.stderr and b'restore-original' in absent.stderr)
            final_column = 'resumed' if final_revision == 3 else 'recovered'
            final_columns = 'recovered=1 AND resumed=1' if in_flight else final_column + '=1'
            self.check('approved change reached the restored database', self.sql(restored, 'SELECT count(*) FROM recovery_canary WHERE ' + final_columns + ';').stdout.strip() == str(len(new_ids)).encode())
            final_jobs = self.watched_apply_jobs(self.barrier('final-watch-boundary'))
            self.check('exactly one fresh Apply followed restoration', len(final_jobs - set(original_jobs)) == 1 and set(original_jobs) <= final_jobs)
            if self.family == 'migration':
                self.check('fresh approved migration is recorded in actual history', self.sql(restored, 'SELECT version FROM schema_migrations ORDER BY version;').stdout.strip() == '\n'.join(str(v) for v in range(1, final_revision + 1)).encode())
            self.verify_recovered_results(checkpoint, final_jobs - set(original_jobs))
            recovery_seconds = round(time.monotonic()-clock, 3)
            rpo_seconds = (db.dt.datetime.fromisoformat(self.report['lossInjectedAt']) - db.dt.datetime.fromisoformat(self.report['databaseBackupStartedAt'])).total_seconds()
            self.check('database recovery point is within five minutes of loss', 0 <= rpo_seconds <= 300)
            operator_age = (db.dt.datetime.fromisoformat(self.report['lossInjectedAt']) - db.dt.datetime.fromisoformat(self.report['backupStartedAt'])).total_seconds()
            self.report['operatorBaseBackupAgeUpperBoundSeconds'] = operator_age
            self.report['profileRPO'] = {'database': 'PASS', 'operatorBase': 'PASS' if 0 <= operator_age <= 300 else 'FAIL'}
            if self.timing != 'operator-lag':
                self.check('the operator checkpoint is within five minutes of loss', 0 <= operator_age <= 300)
            else:
                self.report['operatorKitRPOUpperBoundSeconds'] = self.lagged_recovery_point(self.report)
                self.report['profileRPO']['combinedOperatorRecoveryKit'] = 'PASS'
                self.report['profileRPO']['reason'] = ('The base checkpoint remains older than five minutes. '
                    'Recovery read and verified the linked full operator update and intervening execution archive '
                    'before selecting the committed source. The complete kit, not the base alone, supplies the recovery point.')
            rto_bound = 900 if self.loss == 'operator' else 1800
            self.check('verified recovery meets the declared loss-type bound', 0 < recovery_seconds <= rto_bound)
            self.report.update(status='PASS', functionalRestore='PASS', expectedLostRowIDs=sorted(int(v) for v in expected_loss), observedLostRowIDs=sorted(int(v) for v in old_ids - new_ids), serviceRestoredAt=db.now(), recoverySeconds=recovery_seconds,
                databaseRPOUpperBoundSeconds=rpo_seconds,
                identity={'resourceUID': self.uid, 'originalResourceUID': original_uid, 'oldPlanUID': old_plan['metadata']['uid'], 'oldApprovalUID': old_approval['metadata']['uid'], 'freshPlanUID': fresh_plan['metadata']['uid'], 'freshApprovalUID': fresh_approval['metadata']['uid'], 'originalApplyJobUIDs': sorted(original_jobs), 'freshApplyJobUIDs': sorted(final_jobs - set(original_jobs)), 'originalApplyPodUIDs': [p['metadata']['uid'] for p in original_pods], 'freshApplyPodUIDs': [p['metadata']['uid'] for p in new_pods]})
            if in_flight and self.loss != 'database':
                self.report['identity']['originalApplyPodUIDs'].append(in_flight.pod['metadata']['uid'])
            self.persist()
        except BaseException as exc:
            self.report.update(status='FAIL', failure=type(exc).__name__ + ': ' + str(exc)); self.persist(); raise
        finally:
            clean = True
            if in_flight:
                try:
                    in_flight.close()
                except (OSError, subprocess.TimeoutExpired):
                    clean = False
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
                clean &= self.cleanup_namespace()
            for container in reversed(self.containers):
                clean &= self.command('remove owned container', db.DOCKER + ['rm', '-f', container], required=False).returncode == 0
            for volume in reversed(self.volumes):
                clean &= self.command('remove owned volume', db.DOCKER + ['volume', 'rm', volume], required=False).returncode == 0
            if self.network:
                clean &= self.command('remove owned network', db.DOCKER + ['network', 'rm', self.network], required=False).returncode == 0
            for file in self.env_files:
                file.unlink(missing_ok=True)
            (self.root / 'wrong-identity.private').unlink(missing_ok=True)
            self.report.update(cleanupSucceeded=bool(clean), completedAt=db.now())
            self.finalize_result(self.report)
            self.persist()
            if not clean:
                raise RuntimeError('owned database resource cleanup failed')

    def cleanup_namespace(self):
        return self.kubectl('remove owned recovery namespace',
                            ['delete', 'namespace', self.namespace, '--wait=true', '--timeout=90s'],
                            required=False).returncode == 0


if __name__ == '__main__':
    os.umask(0o077)
    parser = argparse.ArgumentParser()
    parser.add_argument('engine', choices=db.IMAGES)
    parser.add_argument('family', choices=['schema', 'migration'])
    parser.add_argument('environment', type=Path)
    parser.add_argument('output', type=Path)
    parser.add_argument('--loss', choices=['database', 'operator', 'combined'], default='database')
    parser.add_argument('--timing', choices=['idle', 'operator-lag', 'during-apply'], default='idle')
    args = parser.parse_args()
    probe = OperatorProbe(args.engine, args.family, args.environment, args.output, args.loss, args.timing)
    probe.run()
    print(json.dumps({k: probe.report.get(k) for k in ('engine', 'family', 'status', 'functionalRestore', 'profileRPO', 'recoverySeconds', 'cleanupSucceeded')}))
    raise SystemExit(0 if probe.report.get('status') == 'PASS' else 2)
