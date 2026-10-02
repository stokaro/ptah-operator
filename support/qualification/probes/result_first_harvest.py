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
    credentials = json.loads(pathlib.Path(E['E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE']).read_text())
    owner = '"' + credentials['username'].replace('"', '""') + '"'
    run(['docker', '--context', E['E2E_DOCKER_CONTEXT'], 'exec', '-i', E['E2E_EXTERNAL_POSTGRES_CONTAINER_ID'], 'sh', '-ec', 'PGPASSWORD="$POSTGRES_PASSWORD" exec psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1'], 'CREATE DATABASE ' + database + ' OWNER ' + owner + ';\n')
    url = urllib.parse.urlunsplit(urllib.parse.urlsplit(credentials['url'])._replace(path='/' + database))
    create({'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'first-harvest-database', 'namespace': ns}, 'stringData': {'url': url}})
    publisher = get('job', 'result-schema-publish', source)
    template = copy.deepcopy(publisher['spec']['template'])
    template.pop('metadata', None)
    podspec = template['spec']
    container = podspec['containers'][0]
    container['args'] = ['schema', 'push', 'oci://' + E['E2E_REGISTRY_HOST'] + '/schemas/demo:' + ns, '--schema-file', '/schema/schema.sql', '--dialect', 'postgres', '--plain-http']
    podspec['volumes'] = [{'name': 'schema', 'emptyDir': {'sizeLimit': '8Mi'}}, {'name': 'work', 'emptyDir': {'sizeLimit': '64Mi'}}]
    podspec['initContainers'] = [{'name': 'generate', 'image': fixture, 'command': ['/e2e-handcraft-oci'], 'args': ['plan-size-schema', 'postgres', '1398002', '4', '/schema/schema.sql'], 'volumeMounts': [{'name': 'schema', 'mountPath': '/schema'}], 'securityContext': copy.deepcopy(container['securityContext'])}]
    create({'apiVersion': 'batch/v1', 'kind': 'Job', 'metadata': {'name': 'first-harvest-publish', 'namespace': ns}, 'spec': {'backoffLimit': 0, 'activeDeadlineSeconds': 300, 'template': template}})
    k('wait', '--for=condition=complete', 'job/first-harvest-publish', '--timeout=300s')
    artifact_digest = re.search('^Digest: (sha256:[0-9a-f]{64})$', k('logs', 'job/first-harvest-publish'), re.M).group(1)
    policy = {'apiVersion': 'admissionregistration.k8s.io/v1', 'kind': 'ValidatingAdmissionPolicy', 'metadata': {'name': gate}, 'spec': {'failurePolicy': 'Fail', 'matchConstraints': {'resourceRules': [{'apiGroups': [''], 'apiVersions': ['v1'], 'operations': ['CREATE'], 'resources': ['secrets']}]}, 'validations': [{'expression': "!has(object.metadata.annotations) || !('operator.ptah.run/result-pod-name' in object.metadata.annotations) || !object.metadata.annotations['operator.ptah.run/result-pod-name'].startsWith('ptah-plan-')", 'message': 'Acceptance probe holds Plan credentials before execution'}]}}
    try:
        create(policy)
        gated = True
        create({'apiVersion': 'admissionregistration.k8s.io/v1', 'kind': 'ValidatingAdmissionPolicyBinding', 'metadata': {'name': gate}, 'spec': {'policyName': gate, 'validationActions': ['Deny'], 'matchResources': {'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': ns}}}}})

        def gate_ready():
            obj = {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'probe', 'namespace': ns, 'annotations': {'operator.ptah.run/result-pod-name': 'ptah-plan-proof'}}}
            p = subprocess.run(['kubectl', '--kubeconfig', E['E2E_KUBECONFIG'], 'create', '--dry-run=server', '-f', '-'], input=json.dumps(obj), text=True, capture_output=True)
            return p.returncode and 'Acceptance probe holds Plan credentials' in p.stderr
        wait(gate_ready)
        schema = get('ptahschema', 'storefront', source)
        spec = copy.deepcopy(schema['spec'])
        spec['target']['coordinationKey'] = 'acceptance/' + ns
        spec['target']['urlFrom']['name'] = 'first-harvest-database'
        spec['policy']['apply'] = 'OnApproval'
        spec['interval'] = '2h'
        spec['desired']['ociRef'] = 'oci://' + E['E2E_REGISTRY_HOST'] + '/schemas/demo@' + artifact_digest
        spec['execution']['activeDeadlineSeconds'] = 900
        spec['execution']['resources'] = {'requests': {'cpu': '100m', 'memory': '64Mi'}, 'limits': {'cpu': '1', 'memory': '512Mi'}}
        resource = create({'apiVersion': schema['apiVersion'], 'kind': schema['kind'], 'metadata': {'name': 'first-harvest', 'namespace': ns}, 'spec': spec})
        uid = resource['metadata']['uid']

        def held_credential():
            candidates = [r for r in records() if r['spec']['type'] == 'credential' and r['metadata'].get('annotations', {}).get('operator.ptah.run/result-pod-name', '').startswith('ptah-plan-')]
            return candidates[0] if len(candidates) == 1 else None
        credential = wait(held_credential, 240)
        before = claim()
        assert before['type'] == 'Plan' or before['type'] == 'plan'
        assert not plans(), 'Plan was published before the gate'
        secret_name = credential['metadata']['name']
        assert secret_name not in {x['metadata']['name'] for x in get('secrets')['items']}
        job_name = before['jobName']
        job_uid = before['jobUID']
        pod_name = credential['metadata']['annotations']['operator.ptah.run/result-pod-name']
        pod = get('pod', pod_name)
        assert pod['status']['phase'] == 'Pending'
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
        cm = credential['metadata']
        projection = {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {key: cm[key] for key in ('name', 'namespace', 'labels', 'annotations') if key in cm}, 'type': 'kubernetes.io/tls', 'immutable': True, 'data': dec(credential)}
        projection['metadata']['ownerReferences'] = [{'apiVersion': 'operator.ptah.run/v1alpha1', 'kind': 'PtahResultRecord', 'name': cm['name'], 'uid': cm['uid'], 'controller': True, 'blockOwnerDeletion': False}]

        def project_after_gate_removed():
            try:
                return create(projection, '--as=' + user)
            except RuntimeError as e:
                if 'Acceptance probe holds Plan credentials' not in str(e):
                    raise
                return False
        wait(project_after_gate_removed, 30)
        k('wait', '--for=condition=complete', 'job/' + job_name, '--timeout=180s')
        rs = {r['metadata']['name']: r for r in records()}
        intent, completion, data = publication(rs, job_uid)
        manifest = dec(intent)
        assert claim() == before and (not plans()), 'a controller harvested before the fault'
        print('Plan completed and receipt verified before any controller could harvest', flush=True)
        k('delete', 'pod', pod_name, '--wait=true', '--timeout=60s')
        assert not k('get', 'pod', pod_name, '--ignore-not-found', '-o', 'name').strip()
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
    sql = "INSERT INTO e2e_plan_size_limit (id) VALUES (1); SELECT length(payload)::text || ':' || length(replace(payload,'x',''))::text || ':' || length(replace(payload,'<',''))::text FROM e2e_plan_size_limit WHERE id=1;"
    actual = run(['docker', '--context', E['E2E_DOCKER_CONTEXT'], 'exec', '-i', E['E2E_EXTERNAL_POSTGRES_CONTAINER_ID'], 'sh', '-ec', 'PGPASSWORD="$POSTGRES_PASSWORD" exec psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$1" -At -v ON_ERROR_STOP=1', 'probe', database], sql + '\n').strip().splitlines()[-1]
    assert actual == '1398006:1398002:4'
    evidence.update(converged=True, applyJobUID=apply_intents[0]['binding']['jobUID'], approvedApplyJobs=1, databaseDefault=actual, completedAt=dt.datetime.now(dt.timezone.utc).isoformat())
    save('first-harvest-maximum.json', evidence)
    print('PASS: recovered exact 8 MiB plan approved, one Apply, native database default verified', flush=True)
if __name__ == '__main__':
    main()
