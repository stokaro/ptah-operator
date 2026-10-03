#!/usr/bin/env python3
"""Recover an acknowledged 8 MiB Plan before its first controller harvest.

Run only in an owned disposable e2e bootstrap cluster. See result-delivery.md
for required environment, fault restoration, evidence, and scope limits.
Credential bytes stay in process memory and kubectl stdin/stdout pipes.
"""
import base64
import copy
import datetime as dt
import hashlib
import json
import os
import pathlib
import re
import subprocess
import time
import urllib.parse


def publication(records, job_uid):
    """Independently rebuild one acknowledged publication, including its UIDs."""
    def decode(record):
        return base64.b64decode(record['spec']['data'], validate=True)

    def digest(data):
        return 'sha256:' + hashlib.sha256(data).hexdigest()

    def require(value, message):
        if not value:
            raise ValueError(message)

    intents = [r for r in records.values() if r['spec']['type'] == 'intent'
               and json.loads(decode(r))['binding']['jobUID'] == job_uid]
    require(len(intents) == 1, 'Expected one publication for the original Job')
    intent = intents[0]
    name, uid = intent['metadata']['name'], intent['metadata']['uid']
    manifest = json.loads(decode(intent))
    completion = records[name + '-complete']
    receipt = json.loads(decode(completion))
    require(bool(uid) and bool(completion['metadata']['uid']), 'Missing publication UID')
    require(completion['spec']['type'] == 'complete', 'Wrong completion role')
    require(completion['metadata']['ownerReferences'][0]['uid'] == uid,
            'Completion belongs to another intent')
    require(receipt['manifestUID'] == uid and receipt['manifestDigest'] == digest(decode(intent)),
            'Completion does not commit these manifest bytes')
    require(0 < len(manifest['chunks']) <= 97
            and len(receipt['chunkUIDs']) == len(manifest['chunks']), 'Invalid chunk census')
    parts = []
    for i, part in enumerate(manifest['chunks']):
        chunk = records[name + '-' + str(i).zfill(3)]
        body = decode(chunk)
        require(chunk['spec']['type'] == 'chunk'
                and chunk['metadata']['ownerReferences'][0]['uid'] == uid,
                'Chunk belongs to another publication')
        require(bool(chunk['metadata']['uid'])
                and chunk['metadata']['uid'] == receipt['chunkUIDs'][i], 'Chunk UID changed')
        require(0 < len(body) <= 524288 and len(body) == part['size']
                and digest(body) == part['digest'], 'Chunk bytes changed')
        parts.append(body)
    data = b''.join(parts)
    require(0 < len(data) <= 50397184 and len(data) == manifest['size']
            and digest(data) == manifest['digest'], 'Publication bytes changed')
    return intent, completion, data


def size_fixture(engine, plan_bytes=8388608):
    """Reuse the captured native serializer for the exact supported boundaries."""
    if engine not in ('postgresql', 'mysql'):
        raise ValueError('Expected postgresql or mysql')
    if plan_bytes not in (8388608, 8388609):
        raise ValueError('Expected the exact maximum or maximum-plus-one boundary')
    dialect = 'postgres' if engine == 'postgresql' else 'mysql'
    path = pathlib.Path(__file__).resolve().parents[3] / ('testdata/e2e/readings/plan-size-small-' + dialect + '.json')
    raw = path.read_bytes()
    document = json.loads(raw)
    statements = document['statements']
    if (document['dialect'] != dialect or len(statements) != (1 if engine == 'postgresql' else 100)
            or sum(statement['sql'].count('<') for statement in statements) != 32
            or raw.count(b'\\u003c') != 32):
        raise ValueError('Captured native calibration changed')
    repeated, suffix = divmod(plan_bytes - (len(raw) - 32 * 6), 6)
    return dialect, repeated, suffix, hashlib.sha256(raw).hexdigest()


def oversized_refusal(schema, payload, artifact_digest, operation_id):
    """Require the actual native saved-file size and exact source/target binding."""
    status = schema['status']
    if (not schema['metadata'].get('uid')
            or status.get('plan') or status.get('phase') != 'Failed'
            or status['source']['digest'] != artifact_digest or not status['source']['verified']
            or not any(c['type'] == 'ReconciliationFailed' and c['status'] == 'True'
                       and c['reason'] == 'OperationFailed'
                       and c['observedGeneration'] == schema['metadata']['generation']
                       for c in status['conditions'])):
        raise ValueError('The original verified resource did not retain its exact Plan refusal')
    for key in ('coordinationDigest', 'targetIdentityDigest'):
        expected = status['target']['coordinationDigest' if key == 'coordinationDigest' else 'identityDigest']
        if not re.fullmatch('sha256:[0-9a-f]{64}', expected) or payload.get(key) != expected:
            raise ValueError('The refusal belongs to another target')
    if (payload.get('operation') != 'plan' or not operation_id or payload.get('operationId') != operation_id
            or payload.get('childExitCode') != 0 or payload.get('truncation') is not None
            or any(payload.get(key) for key in ('stdout', 'planContentDigest', 'planOutcome',
                                               'mutationStarted', 'uncertain'))):
        raise ValueError('The oversized Plan dispatched incorrectly or exposed executable bytes')
    error = payload.get('error') or {}
    if (error.get('code') != 'invalid_plan_output'
            or error.get('message') != 'plan output exceeds the configured plan limit: saved file has 8388609 bytes; limit is 8388608'):
        raise ValueError('The refusal does not measure an actual native plan exactly one byte over the limit')


def pod_binding_record(job, pod):
    """Enroll the original Pod while reconciliation is paused for first harvest.

    The manager's actual admission handler validates this public record. The
    receiver still authenticates the runner's own projected token independently.
    No token or private key is read by the probe.
    """
    jm, pm, spec = job['metadata'], pod['metadata'], pod['spec']
    owner = jm['ownerReferences'][0]
    if (len(jm['ownerReferences']) != 1 or not owner.get('controller')
            or owner['apiVersion'] != 'operator.ptah.run/v1alpha1'
            or pm['namespace'] != jm['namespace'] or not jm.get('uid') or not pm.get('uid')
            or len(pm['ownerReferences']) != 1
            or pm['ownerReferences'][0].get('uid') != jm['uid']
            or spec.get('automountServiceAccountToken') is not False
            or len(spec['containers']) != 1):
        raise ValueError('Expected the exact original Job and isolated Pod')
    projections = [(v['name'], s['serviceAccountToken']) for v in spec.get('volumes', [])
                   for s in v.get('projected', {}).get('sources', []) if 'serviceAccountToken' in s]
    expected = [('result-credentials', {'audience': 'operator.ptah.run/results',
                                      'expirationSeconds': 3600, 'path': 'token'})]
    if projections != expected:
        raise ValueError('Expected only the receiver-audience Pod token')
    templates = [e['value'] for e in spec['containers'][0]['env']
                 if e['name'] == 'PTAH_RESULT_IDENTITY_TEMPLATE' and 'value' in e]
    if len(templates) != 1:
        raise ValueError('Missing the admitted public identity template')
    identity = json.loads(templates[0])
    b = identity['binding']
    if (any(b[k] for k in ('jobUID', 'podName', 'podUID')) or b['namespace'] != jm['namespace']
            or b['jobName'] != jm['name'] or b['uid'] != owner['uid']
            or b['name'] != owner['name'] or b['kind'] != owner['kind']):
        raise ValueError('The admitted template belongs to another operation')
    b.update(jobUID=jm['uid'], podName=pm['name'], podUID=pm['uid'])
    name = 'ptah-result-key-' + hashlib.sha256(
        '\0'.join((b['uid'], b['operationID'], b['jobName'])).encode()).hexdigest()[:32]
    raw = json.dumps(identity, separators=(',', ':')).encode()
    return {'apiVersion': 'operator.ptah.run/v1alpha1', 'kind': 'PtahResultRecord',
            'metadata': {'name': name, 'namespace': b['namespace'],
                         'labels': {'app.kubernetes.io/managed-by': 'ptah-operator',
                                    'app.kubernetes.io/component': 'result-credential'},
                         'annotations': {'operator.ptah.run/result-pod-uid': b['podUID'],
                                         'operator.ptah.run/result-pod-name': b['podName'],
                                         'operator.ptah.run/result-job-uid': b['jobUID'],
                                         'operator.ptah.run/result-operation-id': b['operationID']},
                         'ownerReferences': [{'apiVersion': owner['apiVersion'], 'kind': b['kind'],
                                              'name': b['name'], 'uid': b['uid'],
                                              'controller': True, 'blockOwnerDeletion': True}]},
            'spec': {'type': 'credential', 'data': base64.b64encode(raw).decode()}}


def main():
    if not __debug__:
        raise RuntimeError('Run without Python optimization; acceptance assertions are required')
    E = os.environ
    source = E['E2E_TEST_NAMESPACE']
    ns = E['RESULT_PROBE_NAMESPACE']
    opns = E['E2E_OPERATOR_NAMESPACE']
    deploy = E['E2E_CONTROLLER_NAME']
    gate = ns + '-gate'
    out = pathlib.Path(E['RESULT_PROBE_EVIDENCE_DIR'])
    out.mkdir(parents=True, exist_ok=True)
    database = E['RESULT_PROBE_DATABASE']
    fixture = E['RESULT_PROBE_FIXTURE_IMAGE']
    engine = E.get('RESULT_PROBE_ENGINE', 'postgresql')
    plan_bytes = int(E.get('RESULT_PROBE_PLAN_BYTES', '8388608'))
    dialect, repeated, suffix, calibration_digest = size_fixture(engine, plan_bytes)

    def run(argv, data=None):
        p = subprocess.run(argv, input=data, text=True, capture_output=True, timeout=360)
        if p.returncode:
            raise RuntimeError(f'{argv[0]} failed: {p.stderr[-1600:]}')
        return p.stdout

    def k(*args, data=None, namespace=ns):
        return run(['kubectl', '--kubeconfig', E['E2E_KUBECONFIG'], '--request-timeout=30s', '-n', namespace, *args], data)

    def get(kind, name=None, namespace=ns):
        return json.loads(k('get', kind, *([name] if name else []), '-o', 'json', namespace=namespace))

    def create(o, *extra):
        return json.loads(k(*extra, 'create', '-f', '-', '-o', 'json', data=json.dumps(o)))

    def wait(fn, seconds=180):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            value = fn()
            if value:
                return value
            time.sleep(0.5)
        raise RuntimeError('bounded wait failed: ' + fn.__name__)

    def save(name, x):
        (out / name).write_text(json.dumps(x, indent=2) + '\n')

    def dec(r):
        return json.loads(base64.b64decode(r['spec']['data'], validate=True))

    def records():
        return get('ptahresultrecords')['items']

    def restart():
        k('rollout', 'restart', 'deployment/' + deploy, namespace=opns)
        k('rollout', 'status', 'deployment/' + deploy, '--timeout=120s', namespace=opns)

    def claim():
        return get('ptahschema', 'first-harvest')['status']['activeOperation']

    def plans():
        return get('ptahschemaplans')['items']

    if not re.fullmatch('[a-z][a-z0-9-]{1,49}', ns) or not re.fullmatch('[a-z][a-z0-9_]{1,49}', database):
        raise ValueError('Use a fresh bounded namespace and SQL identifier')
    if not re.fullmatch(re.escape(E['E2E_REGISTRY_HOST']) + '/e2e-fixture@sha256:[0-9a-f]{64}', fixture):
        raise ValueError('The fixture must use the task registry and an immutable digest')
    if not re.fullmatch('[0-9a-f]{40}', E['E2E_CONTROLLER_REVISION']):
        raise ValueError('The operator revision must be a full commit')
    endpoint = run(['docker', '--context', E['E2E_DOCKER_CONTEXT'], 'context', 'inspect', E['E2E_DOCKER_CONTEXT'], '--format', '{{.Endpoints.docker.Host}}']).strip()
    if endpoint != E['E2E_DOCKER_ENDPOINT']:
        raise ValueError('The selected Docker context does not own the cluster')
    nodes = get('nodes')['items']
    if not nodes:
        raise ValueError('No cluster nodes were examined')
    for node in nodes:
        name = node['metadata']['name']
        owner = run(['docker', '--context', E['E2E_DOCKER_CONTEXT'], 'inspect', '--format', '{{index .Config.Labels "io.x-k8s.kind.cluster"}}', name]).strip()
        if owner != E['E2E_KIND_CLUSTER_NAME']:
            raise ValueError('Refuse an unrelated cluster node')
        config = json.loads(k('get', '--raw', '/api/v1/nodes/' + name + '/proxy/configz'))
        if config['kubeletconfig']['containerLogMaxSize'] != '10Mi':
            raise ValueError('This proof requires effective default kubelet log size')
    manager = get('deployment', deploy, opns)
    sa = manager['spec']['template']['spec']['serviceAccountName']
    user = 'system:serviceaccount:' + opns + ':' + sa
    selector = ','.join((key + '=' + value for key, value in manager['spec']['selector']['matchLabels'].items()))
    rb = get('rolebinding', sa, opns)
    original_subjects = rb['subjects']
    paused = False
    gated = False
    create({'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': ns, 'labels': {'operator.ptah.run/acceptance-owner': E['E2E_KIND_CLUSTER_NAME']}}})
    for kind, name in [('secret', 'demo-registry'), ('secret', 'demo-registry-pull'), ('configmap', 'demo-verification-policy')]:
        obj = get(kind, name, source)
        obj['metadata'] = {'name': name, 'namespace': ns}
        create(obj)
    k('patch', 'serviceaccount', 'default', '--type=merge', '-p', json.dumps({'imagePullSecrets': [{'name': 'demo-registry-pull'}]}))
    def sql(query, target=database):
        if engine == 'mysql':
            return k('exec', '-i', 'deployment/demo-mysql', '--', 'sh', '-ec',
                     'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql --protocol=TCP -h 127.0.0.1 -u root --batch --skip-column-names "$1"',
                     'probe', target, data=query + '\n', namespace=source).strip()
        return run(['docker', '--context', E['E2E_DOCKER_CONTEXT'], 'exec', '-i', E['E2E_EXTERNAL_POSTGRES_CONTAINER_ID'],
                    'sh', '-ec', 'PGPASSWORD="$POSTGRES_PASSWORD" exec psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$1" -At -v ON_ERROR_STOP=1',
                    'probe', target], query + '\n').strip()

    if engine == 'mysql':
        credentials = {key: base64.b64decode(value).decode() for key, value in get('secret', 'demo-mysql-database', source)['data'].items()}
        sql("CREATE DATABASE `" + database + "`; GRANT ALL ON `" + database + "`.* TO 'demo'@'%';", 'mysql')
        url = 'mysql://demo:' + urllib.parse.quote(credentials['password'], safe='') + '@demo-mysql.' + source + '.svc.cluster.local:3306/' + database
    else:
        credentials = json.loads(pathlib.Path(E['E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE']).read_text())
        owner = '"' + credentials['username'].replace('"', '""') + '"'
        sql('CREATE DATABASE ' + database + ' OWNER ' + owner + ';', urllib.parse.urlsplit(credentials['url']).path[1:])
        url = urllib.parse.urlunsplit(urllib.parse.urlsplit(credentials['url'])._replace(path='/' + database))
    create({'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'first-harvest-database', 'namespace': ns}, 'stringData': {'url': url}})
    security = {'allowPrivilegeEscalation': False, 'readOnlyRootFilesystem': True,
                'capabilities': {'drop': ['ALL']}}
    container = {'name': 'publisher', 'image': E['E2E_EXECUTOR_IMAGE'],
                 'command': ['/usr/local/bin/ptah'], 'securityContext': security,
                 'resources': {'requests': {'cpu': '100m', 'memory': '32Mi'},
                               'limits': {'cpu': '1', 'memory': '256Mi'}},
                 'env': [{'name': 'HOME', 'value': '/work'}, {'name': 'TMPDIR', 'value': '/work'}] + [
                     {'name': 'PTAH_OCI_' + name.upper(), 'valueFrom': {'secretKeyRef': {
                     'name': 'demo-registry', 'key': name}}} for name in ('registry', 'username', 'password')],
                 'volumeMounts': [{'name': 'schema', 'mountPath': '/schema', 'readOnly': True},
                                  {'name': 'work', 'mountPath': '/work'}]}
    podspec = {'restartPolicy': 'Never', 'automountServiceAccountToken': False,
               'imagePullSecrets': [{'name': 'demo-registry-pull'}],
               'securityContext': {'runAsNonRoot': True, 'runAsUser': 65532, 'runAsGroup': 65532,
                                   'fsGroup': 65532, 'seccompProfile': {'type': 'RuntimeDefault'}},
               'containers': [container]}
    template = {'spec': podspec}
    container['args'] = ['schema', 'push', 'oci://' + E['E2E_REGISTRY_HOST'] + '/schemas/demo:' + ns, '--schema-file', '/schema/schema.sql', '--dialect', dialect, '--plain-http']
    podspec['volumes'] = [{'name': 'schema', 'emptyDir': {'sizeLimit': '8Mi'}}, {'name': 'work', 'emptyDir': {'sizeLimit': '64Mi'}}]
    podspec['initContainers'] = [{'name': 'generate', 'image': fixture, 'command': ['/e2e-handcraft-oci'], 'args': ['plan-size-schema', dialect, str(repeated), str(suffix), '/schema/schema.sql'], 'volumeMounts': [{'name': 'schema', 'mountPath': '/schema'}], 'securityContext': copy.deepcopy(container['securityContext'])}]
    create({'apiVersion': 'batch/v1', 'kind': 'Job', 'metadata': {'name': 'first-harvest-publish', 'namespace': ns}, 'spec': {'backoffLimit': 0, 'activeDeadlineSeconds': 300, 'template': template}})
    k('wait', '--for=condition=complete', 'job/first-harvest-publish', '--timeout=300s')
    artifact_digest = re.search('^Digest: (sha256:[0-9a-f]{64})$', k('logs', 'job/first-harvest-publish'), re.M).group(1)
    message = 'Acceptance probe holds Plan Pods before execution'
    expression = ("!has(object.metadata.labels) || !("
                  "('operator.ptah.run/operation' in object.metadata.labels && "
                  "object.metadata.labels['operator.ptah.run/operation'] == 'plan') || "
                  "('operator.ptah.run/acceptance-gate-probe' in object.metadata.labels))")
    policy = {'apiVersion': 'admissionregistration.k8s.io/v1', 'kind': 'ValidatingAdmissionPolicy',
              'metadata': {'name': gate}, 'spec': {'failurePolicy': 'Fail',
                  'matchConstraints': {'resourceRules': [{'apiGroups': [''], 'apiVersions': ['v1'],
                      'operations': ['CREATE'], 'resources': ['pods']}]},
                  'validations': [{'expression': expression, 'message': message}]}}
    try:
        create(policy)
        gated = True
        create({'apiVersion': 'admissionregistration.k8s.io/v1', 'kind': 'ValidatingAdmissionPolicyBinding', 'metadata': {'name': gate}, 'spec': {'policyName': gate, 'validationActions': ['Deny'], 'matchResources': {'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': ns}}}}})

        def gate_ready():
            obj = {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': 'probe', 'namespace': ns,
                   'labels': {'operator.ptah.run/acceptance-gate-probe': 'true'}},
                   'spec': {'containers': [{'name': 'probe', 'image': fixture}],
                            'automountServiceAccountToken': False, 'restartPolicy': 'Never'}}
            p = subprocess.run(['kubectl', '--kubeconfig', E['E2E_KUBECONFIG'], 'create',
                                '--dry-run=server', '-f', '-'], input=json.dumps(obj), text=True,
                               capture_output=True, timeout=30)
            return p.returncode and message in p.stderr
        wait(gate_ready)
        spec = {'target': {'engine': 'MySQL' if engine == 'mysql' else 'PostgreSQL',
                          'coordinationKey': 'acceptance/' + ns,
                          'urlFrom': {'name': 'first-harvest-database', 'key': 'url'}},
                'desired': {'ociRef': 'oci://' + E['E2E_REGISTRY_HOST'] + '/schemas/demo@' + artifact_digest,
                            'verificationPolicyFrom': {'name': 'demo-verification-policy', 'key': 'policy.yaml'},
                            'registryAuthFrom': {'name': 'demo-registry', 'mode': 'Environment',
                                                 'usernameKey': 'username', 'passwordKey': 'password'},
                            'transport': {'plainHTTP': True}},
                'policy': {'apply': 'OnApproval', 'allowDestructive': False, 'driftSeverity': 'all'},
                'interval': '2h', 'execution': {'activeDeadlineSeconds': 900,
                    'failureRetryInterval': '1h' if plan_bytes == 8388609 else '30s',
                    'resources': {'requests': {'cpu': '100m', 'memory': '64Mi'},
                                  'limits': {'cpu': '1', 'memory': '512Mi'}}}}
        if engine == 'mysql':
            spec['policy']['transactionMode'] = 'none'
        resource = create({'apiVersion': 'operator.ptah.run/v1alpha1', 'kind': 'PtahSchema',
                           'metadata': {'name': 'first-harvest', 'namespace': ns}, 'spec': spec})
        uid = resource['metadata']['uid']

        def held_job():
            operation = get('ptahschema', 'first-harvest').get('status', {}).get('activeOperation') or {}
            if operation.get('type') not in ('Plan', 'plan') or not operation.get('jobUID'):
                return None
            candidates = [j for j in get('jobs')['items'] if j['metadata']['uid'] == operation['jobUID']]
            return candidates[0] if len(candidates) == 1 else None
        job = wait(held_job, 240)
        before = claim()
        job_name, job_uid = job['metadata']['name'], job['metadata']['uid']
        assert not plans(), 'Plan was published before the gate'
        def job_pods():
            return [p for p in get('pods')['items'] if any(
                o.get('uid') == job_uid for o in p['metadata'].get('ownerReferences', []))]
        assert not job_pods(), 'Plan Pod escaped the admission gate'
        assert not any(r['spec']['type'] == 'intent' and dec(r)['binding']['jobUID'] == job_uid
                       for r in records()), 'Plan result exists before execution'
        print('Plan held before execution:', job_name, flush=True)
        save('manager-rolebinding-before.json', rb)
        old_pods = {p['metadata']['uid'] for p in json.loads(k('get', 'pods', '-l', selector, '-o', 'json', namespace=opns))['items']}
        k('patch', 'rolebinding', sa, '--type=merge', '-p', '{"subjects":[]}', namespace=opns)
        paused = True
        restart()

        def leaders_stopped():
            current = json.loads(k('get', 'pods', '-l', selector, '-o', 'json', namespace=opns))['items']
            return len(current) == 2 and (not old_pods & {p['metadata']['uid'] for p in current}) and all((any((c['type'] == 'Ready' and c['status'] == 'True' for c in p['status'].get('conditions', []))) for p in current))
        wait(leaders_stopped, 120)
        access = subprocess.run(['kubectl', '--kubeconfig', E['E2E_KUBECONFIG'], '-n', opns, 'auth', 'can-i', 'get', 'leases.coordination.k8s.io', '--as=' + user], text=True, capture_output=True)
        assert access.returncode == 1 and access.stdout.strip() == 'no'
        assert claim() == before and (not plans())
        stopped_at = dt.datetime.now(dt.timezone.utc).isoformat()
        first_managers = {p['metadata']['uid'] for p in json.loads(k('get', 'pods', '-l', selector, '-o', 'json', namespace=opns))['items']}
        print('Both fresh managers ready with leader Lease access removed', flush=True)
        k('delete', 'validatingadmissionpolicybinding', gate)
        k('delete', 'validatingadmissionpolicy', gate)
        gated = False
        def original_pod():
            pods = job_pods()
            assert len(pods) <= 1, 'Plan attempt created a replacement Pod'
            return pods[0] if pods else None
        pod = wait(original_pod, 180)
        pod_name = pod['metadata']['name']
        # Reconciliation is intentionally stopped. Create its public enrollment
        # record as the manager through the actual admission handler, without
        # reading or copying the token. Only the original Pod can authenticate.
        credential = create(pod_binding_record(job, pod), '--as=' + user)
        save('pod-binding.json', credential)
        assert credential['metadata']['name'] not in {x['metadata']['name'] for x in get('secrets')['items']}
        if plan_bytes == 8388609:
            def job_terminal():
                job = get('job', job_name)
                return job if any(c['type'] in ('Failed', 'Complete') and c['status'] == 'True'
                                  for c in job.get('status', {}).get('conditions', [])) else None
            wait(job_terminal, 180)
        else:
            k('wait', '--for=condition=complete', 'job/' + job_name, '--timeout=180s')
        rs = {r['metadata']['name']: r for r in records()}
        intent, completion, data = publication(rs, job_uid)
        manifest = dec(intent)
        assert claim() == before and (not plans()), 'a controller harvested before the fault'
        print('Plan completed and receipt verified before any controller could harvest', flush=True)
        k('delete', 'pod', pod_name, '--wait=true', '--timeout=60s')
        assert not k('get', 'pod', pod_name, '--ignore-not-found', '-o', 'name').strip()
        if E.get('RESULT_PROBE_RESTORE_CONTROL_PLANE') == '1':
            if E.get('RESULT_PROBE_ROTATE_CA') == '1' or E.get('RESULT_PROBE_ROTATE_LEAF') == '1':
                raise ValueError('Run storage restoration separately from certificate fault injection')
            import result_control_plane_restore
            result_control_plane_restore.run(E)
            assert claim() == before and not plans(), 'controller harvested during storage restoration'
        if E.get('RESULT_PROBE_ROTATE_CA') == '1':
            if E.get('RESULT_PROBE_ROTATE_LEAF') == '1':
                raise ValueError('Select one rotation fault per first-harvest run')
            import result_ca_rotation
            result_ca_rotation.run(k, get, create, wait, save, E)
            assert claim() == before and not plans(), 'controller harvested during CA recovery'
        if E.get('RESULT_PROBE_ROTATE_LEAF') == '1':
            import result_rotation
            result_rotation.run(k, get, create, wait, save, E)
            assert claim() == before and not plans(), 'controller harvested during rotation'
        restart()
        wait(leaders_stopped, 120)
        second_managers = {p['metadata']['uid'] for p in json.loads(k('get', 'pods', '-l', selector, '-o', 'json', namespace=opns))['items']}
        assert not first_managers & second_managers, 'receiver processes survived the second restart'
        assert claim() == before and (not plans())
        persisted = get('ptahresultrecord', completion['metadata']['name'])
        assert persisted['metadata']['uid'] == completion['metadata']['uid']
        assert persisted['spec'] == completion['spec']
        print('Producing Pod and logs removed and both manager processes restarted before first harvest', flush=True)
        k('patch', 'rolebinding', sa, '--type=merge', '-p', json.dumps({'subjects': original_subjects}), namespace=opns)
        paused = False

        if plan_bytes == 8388609:
            def refused():
                r = get('ptahschema', 'first-harvest')
                return r if any(
                    c['type'] == 'ReconciliationFailed' and c['status'] == 'True'
                    and c['reason'] == 'OperationFailed'
                    and c['observedGeneration'] == r['metadata']['generation']
                    for c in r.get('status', {}).get('conditions', [])) else None
            after = wait(refused, 240)
            payload = json.loads(data)
            oversized_refusal(after, payload, artifact_digest, manifest['binding']['operationID'])
            assert after['metadata']['uid'] == uid
            assert not plans() and not get('ptahschemaplanchunks')['items']
            jobs = get('jobs')['items']
            operations = [j for j in jobs if j['metadata']['name'].startswith('ptah-')]
            assert not any(j['spec']['template'].get('metadata', {}).get('labels', {}).get('operator.ptah.run/operation') == 'apply' for j in operations)
            assert [j['metadata']['uid'] for j in operations if j['metadata']['name'].startswith('ptah-plan-')] == [job_uid]
            table_count = sql("SELECT count(*) FROM information_schema.tables WHERE table_schema="
                              + ("DATABASE()" if engine == 'mysql' else "'public'")
                              + " AND table_name LIKE 'e2e_plan_size_limit%'")
            assert table_count == '0'
            save('oversized-refusal.json', {'status': 'passed', 'commit': E['E2E_CONTROLLER_REVISION'],
                 'engine': engine, 'namespace': ns, 'resourceUID': uid, 'binding': manifest['binding'],
                 'jobUID': job_uid, 'podUID': pod['metadata']['uid'], 'intentUID': intent['metadata']['uid'],
                 'receiptUID': completion['metadata']['uid'], 'payloadDigest': manifest['digest'],
                 'payloadBytes': manifest['size'], 'nativePlanBytes': plan_bytes, 'error': payload['error'],
                 'repeatedCharacters': repeated, 'asciiSuffix': suffix, 'calibrationSHA256': calibration_digest,
                 'managerUIDsBeforeDelivery': sorted(first_managers), 'managerUIDsBeforeConsumption': sorted(second_managers),
                 'controllersStoppedAt': stopped_at, 'plans': 0, 'planChunks': 0, 'applyJobs': 0, 'databaseTables': 0,
                 'procedureSHA256': hashlib.sha256(pathlib.Path(__file__).read_bytes()).hexdigest(),
                 'completedAt': dt.datetime.now(dt.timezone.utc).isoformat()})
            save('refused-resource.json', after)
            save('refused-result.json', payload)
            print('PASS: exact 8 MiB + 1 native Plan refused after Pod/log loss and manager replacement; no plan, chunks, Apply, or database effects', flush=True)
            return

        def harvested():
            r = get('ptahschema', 'first-harvest')
            ps = plans()
            return (r, ps) if not r.get('status', {}).get('activeOperation') and r.get('status', {}).get('plan') and (len(ps) == 1) else None
        after, published = wait(harvested, 240)
        assert any((c['type'] == 'Ready' and c['status'] == 'True' for c in published[0]['status']['conditions']))
        assert after['status']['plan']['name'] == published[0]['metadata']['name']
        payload = json.loads(data)
        expected_digest = payload['planContentDigest']
        assert published[0]['spec']['contentDigest'] == expected_digest
        # Reconstruct planstore independently, rather than trusting its Ready
        # condition or repeating the digest written into the plan metadata.
        plan_bytes = []
        for i, ref in enumerate(published[0]['spec']['chunks']):
            chunk = get('ptahschemaplanchunk', ref['name'])
            body = base64.b64decode(chunk['spec']['data'], validate=True)
            assert ref['index'] == i and len(body) == ref['size']
            assert 'sha256:' + hashlib.sha256(body).hexdigest() == ref['digest']
            assert chunk['metadata']['ownerReferences'][0]['uid'] == published[0]['metadata']['uid']
            plan_bytes.append(body)
        rebuilt = b''.join(plan_bytes)
        assert len(rebuilt) == 8388608
        assert 'sha256:' + hashlib.sha256(rebuilt).hexdigest() == expected_digest
        jobs = get('jobs')['items']
        assert all((j['metadata']['uid'] == job_uid for j in jobs if j['metadata']['name'].startswith('ptah-plan-'))), 'replacement Plan Job executed'
        plan_intents = [dec(r) for r in records() if r['spec']['type'] == 'intent' and dec(r)['binding']['operation'] == 'plan']
        assert len(plan_intents) == 1 and plan_intents[0]['binding']['jobUID'] == job_uid
        evidence = {'commit': E['E2E_CONTROLLER_REVISION'], 'namespace': ns, 'resourceUID': uid, 'binding': manifest['binding'], 'jobUID': job_uid, 'podUID': pod['metadata']['uid'], 'intentUID': intent['metadata']['uid'], 'receiptUID': completion['metadata']['uid'], 'payloadDigest': manifest['digest'], 'payloadBytes': manifest['size'], 'planUID': published[0]['metadata']['uid'], 'planContentDigest': expected_digest, 'conditions': after['status']['conditions'], 'status': 'passed', 'managerUIDsBeforeDelivery': sorted(first_managers), 'managerUIDsBeforeConsumption': sorted(second_managers), 'controllersStoppedAt': stopped_at, 'procedureSHA256': hashlib.sha256(pathlib.Path(__file__).read_bytes()).hexdigest(), 'scope': 'Plan receipt first consumed after producing Pod/log removal (completed Job retained) and manager restart. Leader permission was removed while the receiver remained available. This read-only Plan row does not measure SQL replay.'}
        evidence.update(engine=engine, repeatedCharacters=repeated, asciiSuffix=suffix, calibrationSHA256=calibration_digest)
        evidence['planBytes'] = published[0]['spec']['size']
        assert evidence['planBytes'] == 8388608
        evidence['planChunks'] = len(published[0]['spec']['chunks'])
        assert evidence['planChunks'] == 16
        evidence['resultChunks'] = len(manifest['chunks'])
        save('first-harvest-maximum.json', evidence)
        print('PASS: first harvest published the saved Plan after Pod/log removal and manager restart', flush=True)
    finally:
        if paused:
            k('patch', 'rolebinding', sa, '--type=merge', '-p', json.dumps({'subjects': original_subjects}), namespace=opns)
        if gated:
            k('delete', 'validatingadmissionpolicybinding', gate, '--ignore-not-found')
            k('delete', 'validatingadmissionpolicy', gate, '--ignore-not-found')
    plan = published[0]
    create({'apiVersion': 'operator.ptah.run/v1alpha1', 'kind': 'PtahSchemaApproval', 'metadata': {'name': 'first-harvest-approved', 'namespace': ns}, 'spec': {'schemaRef': {'name': 'first-harvest', 'uid': uid}, 'planRef': {'name': plan['metadata']['name'], 'uid': plan['metadata']['uid']}, 'planFingerprint': plan['spec']['fingerprint']}})

    def converged():
        r = get('ptahschema', 'first-harvest')
        return r if any((c['type'] == 'InSync' and c['status'] == 'True' and (c['observedGeneration'] == r['metadata']['generation']) for c in r.get('status', {}).get('conditions', []))) and (not r.get('status', {}).get('activeOperation')) else None
    wait(converged, 600)
    apply_intents = [dec(r) for r in records() if r['spec']['type'] == 'intent' and dec(r)['binding']['operation'] == 'apply']
    assert len(apply_intents) == 1
    if engine == 'mysql':
        actual = sql("SELECT CONCAT(COUNT(*), ':', SUM(CHAR_LENGTH(column_default)), ':', SUM(CHAR_LENGTH(column_default)-CHAR_LENGTH(REPLACE(column_default,'<','')))) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name LIKE 'e2e_plan_size_limit_%' AND column_name='payload'")
        assert actual == f'100:{repeated+suffix}:{repeated}'
        assert sql("SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name LIKE 'e2e_plan_size_limit%'") == '100'
    else:
        actual = sql("INSERT INTO e2e_plan_size_limit (id) VALUES (1); SELECT length(payload)::text || ':' || length(replace(payload,'x',''))::text || ':' || length(replace(payload,'<',''))::text FROM e2e_plan_size_limit WHERE id=1;").splitlines()[-1]
        assert actual == f'{repeated+suffix}:{repeated}:{suffix}'
    evidence.update(converged=True, applyJobUID=apply_intents[0]['binding']['jobUID'], approvedApplyJobs=1, databaseDefault=actual, completedAt=dt.datetime.now(dt.timezone.utc).isoformat())
    save('first-harvest-maximum.json', evidence)
    print('PASS: recovered exact 8 MiB plan approved, one Apply, native database default verified', flush=True)
if __name__ == '__main__':
    main()
