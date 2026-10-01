"""Owned background objects for the frozen capacity profile."""

from concurrent.futures import ThreadPoolExecutor
import datetime
import hashlib
import json
import re

LABEL = 'operator.ptah.run/capacity-unrelated'
COUNT = 1000
PAYLOAD = 'x' * 1024


def objects(namespace, index, run, image):
    meta = {'name': f'background-{index:04d}', 'namespace': namespace, 'labels': {LABEL: run}}
    config = {'apiVersion': 'v1', 'kind': 'ConfigMap', 'metadata': meta,
              'immutable': True, 'data': {'payload': PAYLOAD}}
    job = {'apiVersion': 'batch/v1', 'kind': 'Job', 'metadata': meta, 'spec': {
        'backoffLimit': 0, 'activeDeadlineSeconds': 120, 'completions': 1, 'parallelism': 1,
        'template': {'metadata': {'labels': {LABEL: run}}, 'spec': {
            'restartPolicy': 'Never', 'automountServiceAccountToken': False,
            'imagePullSecrets': [{'name': 'demo-registry-pull'}],
            'securityContext': {'runAsNonRoot': True, 'runAsUser': 65532, 'runAsGroup': 65532,
                                'seccompProfile': {'type': 'RuntimeDefault'}},
            'containers': [{'name': 'complete', 'image': image, 'imagePullPolicy': 'IfNotPresent',
                            'command': ['/bin/sh', '-c', 'exit 0'],
                            'securityContext': {'allowPrivilegeEscalation': False,
                                                'readOnlyRootFilesystem': True,
                                                'capabilities': {'drop': ['ALL']}},
                            'resources': {'requests': {'cpu': '1m', 'memory': '16Mi'},
                                          'limits': {'cpu': '100m', 'memory': '32Mi'}}}]}}}}
    return config, job


def inventory(configs, jobs, pods, expected, image, run):
    """Require the complete original population and actual successful Pods."""
    if len(expected) != COUNT * 2 or len(configs) != COUNT or len(jobs) != COUNT or len(pods) != COUNT:
        raise ValueError('unrelated object population is incomplete')
    identities = {(v['kind'], v['metadata']['namespace'], v['metadata']['name']): v['metadata']['uid']
                  for v in expected}
    if len(identities) != COUNT * 2 or len(set(identities.values())) != COUNT * 2:
        raise ValueError('unrelated object identities are duplicated')
    owned_pods = {}
    for pod in pods:
        owners = [o for o in pod['metadata'].get('ownerReferences', [])
                  if o.get('controller') is True and o.get('kind') == 'Job' and o.get('apiVersion') == 'batch/v1']
        if len(owners) != 1 or owners[0]['uid'] in owned_pods or not pod['metadata'].get('uid'):
            raise ValueError('unrelated Pod lacks a unique Job owner')
        owned_pods[owners[0]['uid']] = pod
    examined = set()
    for obj in configs + jobs:
        meta = obj['metadata']; key = (obj['kind'], meta['namespace'], meta['name'])
        if (key in examined or not meta.get('uid') or identities.get(key) != meta['uid'] or
                meta.get('deletionTimestamp') or meta.get('labels', {}).get(LABEL) != run or
                'operator.ptah.run/capacity' in meta.get('labels', {}) or
                meta.get('labels', {}).get('app.kubernetes.io/managed-by') == 'ptah-operator'):
            raise ValueError('unrelated object changed identity or workload membership')
        examined.add(key)
        if obj['kind'] == 'ConfigMap':
            if obj.get('data') != {'payload': PAYLOAD} or obj.get('binaryData') or obj.get('immutable') is not True:
                raise ValueError('unrelated ConfigMap payload changed')
            continue
        status = obj.get('status', {})
        if (status.get('succeeded') != 1 or status.get('active', 0) or status.get('failed', 0) or
                not status.get('completionTime') or
                not any(c.get('type') == 'Complete' and c.get('status') == 'True' for c in status.get('conditions', []))):
            raise ValueError('unrelated Job did not complete successfully')
        pod = owned_pods.get(meta['uid'], {})
        containers = pod.get('status', {}).get('containerStatuses', [])
        if (pod.get('metadata', {}).get('namespace') != meta['namespace'] or
                pod.get('status', {}).get('phase') != 'Succeeded' or len(containers) != 1 or
                containers[0].get('name') != 'complete' or not containers[0].get('imageID') or
                containers[0].get('restartCount') != 0 or
                pod.get('spec', {}).get('containers', [{}])[0].get('image') != image):
            raise ValueError('unrelated Job has no matching successful Pod')
        terminated = containers[0].get('state', {}).get('terminated', {})
        if terminated.get('exitCode') != 0 or not terminated.get('startedAt') or not terminated.get('finishedAt'):
            raise ValueError('unrelated Pod has no successful execution evidence')
    return {'configMaps': len(configs), 'payloadBytesPerConfigMap': 1024,
            'completedJobs': len(jobs), 'successfulPods': len(pods)}


def verify(bootstrap, checkpoint):
    proof = bootstrap.state.get('unrelated')
    if proof is None:
        raise ValueError('unrelated population has not been prepared')
    readback = {kind: [] for kind in ('configmaps', 'jobs', 'pods')}
    for ns in proof['namespaces']:
        current = json.loads(bootstrap.command(['get', 'namespace', ns['name'], '-o', 'json']))
        if current['metadata']['uid'] != ns['uid'] or ns['name'] in bootstrap.state['workloadNamespaces']:
            raise ValueError('unrelated namespace changed identity or entered the workload')
        for kind in readback:
            raw = bootstrap.command(['-n', ns['name'], 'get', kind, '-l', LABEL + '=' + bootstrap.state['runID'], '-o', 'json'])
            readback[kind].extend(json.loads(raw)['items'])
    counts = inventory(readback['configmaps'], readback['jobs'], readback['pods'], proof['objects'], proof['image'], bootstrap.state['runID'])
    record = {'observedAt': datetime.datetime.now(datetime.timezone.utc).isoformat(),
              'namespaces': proof['namespaces'], 'counts': counts, 'objects': readback}
    path = bootstrap.path.parent / f'unrelated-{checkpoint}.json'
    with path.open('x') as output:
        json.dump(record, output, indent=2); output.write('\n')
    return {'path': path.name, 'sha256': hashlib.sha256(path.read_bytes()).hexdigest(), **counts}


def prepare(bootstrap, minor):
    image = bootstrap.env['E2E_POSTGRES_IMAGE']
    if not re.search(r'@sha256:[0-9a-f]{64}$', image):
        raise ValueError('unrelated jobs require a digest-pinned image')
    run = bootstrap.state['runID']
    names = [f'ptah-capacity-{run}-unrelated-{i}' for i in range(2)]
    proof = {'image': image, 'namespaces': [], 'objects': []}
    bootstrap.state['unrelated'] = proof
    for name in names:
        bootstrap.namespace(name, minor, True)
        proof['namespaces'].append(dict(bootstrap.state['namespaces'][-1]))
        bootstrap.copy(bootstrap.read('secret', 'demo-registry-pull', bootstrap.state['fixtureNamespace']), name)
        bootstrap.command(['-n', name, 'wait', '--for=create', 'serviceaccount/default', '--timeout=30s'])
        bootstrap.create(bootstrap.object(name, 'NetworkPolicy', 'no-network', spec={
            'podSelector': {}, 'policyTypes': ['Ingress', 'Egress'], 'ingress': [], 'egress': []}))
        bootstrap.create(bootstrap.object(name, 'ResourceQuota', 'background', spec={'hard': {
            'pods': '520', 'count/jobs.batch': '500', 'configmaps': '502'}}))
    specs = [objects(names[i % 2], i, run, image) for i in range(COUNT)]
    def create(spec):
        obj = bootstrap.create(spec)
        if not obj['metadata'].get('uid'):
            raise ValueError('unrelated CREATE returned no UID')
        return {'kind': obj['kind'], 'metadata': {k: obj['metadata'][k] for k in ('name', 'namespace', 'uid')}}
    # Preparation runs before measurement. Limit live work to twenty Jobs;
    # completed Jobs and Pods stay present throughout the measured workload.
    with ThreadPoolExecutor(max_workers=8) as pool:
        proof['objects'].extend(pool.map(create, [c for c, _ in specs]))
        for start in range(0, COUNT, 20):
            created = list(pool.map(create, [j for _, j in specs[start:start + 20]]))
            proof['objects'].extend(created)
            bootstrap.save()
            for name in names:
                jobs = ['job/' + j['metadata']['name'] for j in created if j['metadata']['namespace'] == name]
                bootstrap.command(['-n', name, 'wait', '--for=condition=complete', '--timeout=180s', *jobs], timeout=210)
    proof['prepared'] = verify(bootstrap, 'prepared')
    bootstrap.save()
