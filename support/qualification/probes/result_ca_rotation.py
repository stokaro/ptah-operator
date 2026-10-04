"""Recover an installed expired pending CA transition before first harvest.

The signed expired material is an injected outage fixture. Production clocks,
credential lifetimes and retirement waits remain unchanged. Keys stay in pipes.
"""
import base64
import hashlib
import json
import pathlib
import ssl
import subprocess
import time

from result_rotation import digest


def verify_evidence(e):
    def require(ok, message):
        if not ok:
            raise ValueError(message)
    require(e['phaseBefore'] == 'stable' and e['seededPhase'] == 'prepare'
            and e['heldPhase'] == 'prepare' and e['phaseAfter'] == 'stable', 'Missing CA recovery transition')
    require(len(e['expiredNotAfter']) == 6 and all(t + 300 < e['seededAt'] for t in e['expiredNotAfter'].values()), 'Fixture authorities did not expire past skew')
    require(e['gateRefused'] and e['projectionHeld'] and e['managersUnavailableWithExpiredTrust'], 'Missing installed fault')
    require(e['candidateBeforeRestart'] == e['candidateAfterRestart'] == e['currentAfter'], 'Recovery changed the persisted candidate')
    require(all(e['candidateBeforeRestart'][key] != e['expiredCandidate'][key] and e['currentAfter'][key] != e['currentBefore'][key]
                for key in ('ServerCA', 'ServerKey', 'ClientCA', 'ClientKey')), 'Expired candidate or original CA was reused')
    require(e['fenceBeforeRestart'] and e['fenceBeforeRestart'] == e['fenceAfterRestart'], 'Restart changed the retirement fence')
    require(e['rotatorUIDBefore'] and e['rotatorUIDBefore'] != e['rotatorUIDAfter'], 'Rotator was not replaced')
    require(len(set(e['managerUIDsBefore'])) == len(set(e['managerUIDsAfter'])) == 2
            and not set(e['managerUIDsBefore']) & set(e['managerUIDsAfter']), 'Receiver processes survived the fault')
    require(e['projectionUIDBefore'] == e['projectionUIDAfter'] and e['journalUIDBefore'] == e['journalUIDAfter'], 'Trust objects were replaced')
    require(e['receiversReady'] and e['rotatorReady'] and e['temporaryGateRemoved']
            and e['replicasRestored'] and e['argumentsUnchanged'], 'Incomplete recovery or cleanup')
    return {'expiredCandidateDiscarded': True, 'newAuthoritiesInstalled': True, 'persistedCandidateResumed': True, 'servingReplicas': 2}


def run(k, get, create, wait, save, E):
    opns = E['E2E_OPERATOR_NAMESPACE']
    gate = E['RESULT_PROBE_NAMESPACE'] + '-ca-rotation'
    deployments = get('deployments', namespace=opns)['items']
    choices = [d for d in deployments if any(c['name'] == 'certificate-rotator' for c in d['spec']['template']['spec']['containers'])]
    if len(choices) != 1 or choices[0]['spec']['replicas'] != 1:
        raise ValueError('Expected the owned installation rotator')
    deployment = choices[0]
    rotator = deployment['metadata']['name']
    container = deployment['spec']['template']['spec']['containers'][0]
    original_args = container['args']
    options = dict(a[2:].split('=', 1) for a in original_args if a.startswith('--') and '=' in a)
    projection_name, journal_name, policy_name = (options[n] for n in ('result-secret-name', 'result-journal-secret-name', 'result-enrollment-policy'))
    manager = get('deployment', E['E2E_CONTROLLER_NAME'], namespace=opns)
    selector = lambda d: ','.join(a + '=' + b for a, b in d['spec']['selector']['matchLabels'].items())

    def pods(d):
        return json.loads(k('get', 'pods', '-l', selector(d), '-o', 'json', namespace=opns))['items']

    def ready(p):
        return not p['metadata'].get('deletionTimestamp') and any(c['type'] == 'Ready' and c['status'] == 'True' for c in p.get('status', {}).get('conditions', []))

    def scale(n):
        current = get('deployment', rotator, namespace=opns)
        patch = [{'op': 'test', 'path': '/metadata/uid', 'value': deployment['metadata']['uid']},
                 {'op': 'test', 'path': '/metadata/resourceVersion', 'value': current['metadata']['resourceVersion']},
                 {'op': 'replace', 'path': '/spec/replicas', 'value': n}]
        k('patch', 'deployment', rotator, '--type=json', '-p', json.dumps(patch), namespace=opns)

    def write_data(kind, old, data):
        current = get(kind, old['metadata']['name'], namespace=opns)
        patch = [{'op': 'test', 'path': '/metadata/uid', 'value': old['metadata']['uid']},
                 {'op': 'test', 'path': '/metadata/resourceVersion', 'value': current['metadata']['resourceVersion']},
                 {'op': 'replace', 'path': '/data', 'value': data}]
        k('patch', kind, old['metadata']['name'], '--type=json', '-p', json.dumps(patch), namespace=opns)

    def state():
        obj = get('secret', journal_name, namespace=opns)
        return obj, json.loads(base64.b64decode(obj['data']['rotation.json'], validate=True))

    def roots(keys):
        return {n: digest(keys[n]) for n in ('ServerCA', 'ServerKey', 'ClientCA', 'ClientKey')}

    initial_managers = pods(manager)
    if len(initial_managers) != 2 or not all(map(ready, initial_managers)):
        raise ValueError('Expected two ready receivers before the fault')
    evidence = {'managerUIDsBefore': sorted(p['metadata']['uid'] for p in initial_managers),
                'image': container['image'], 'procedureSHA256': hashlib.sha256(pathlib.Path(__file__).read_bytes()).hexdigest(),
                'scope': 'Installed CA recovery from an injected expired pending journal. The real expiry-plus-skew retirement path runs; ordinary two maximum-credential-lifetime waits are not elapsed by this case.'}
    gate_created = seeded = success = False
    original_journal = original_projection = original_policy = None
    try:
        scale(0)
        wait(lambda: not pods(deployment), 90)
        original_journal, before = state()
        original_projection = get('secret', projection_name, namespace=opns)
        original_policy = get('configmap', policy_name, namespace=opns)
        if before['phase'] != 'stable':
            raise ValueError('Refuse to overwrite an existing transition')
        evidence.update(phaseBefore=before['phase'], currentBefore=roots(before['current']),
                        projectionUIDBefore=original_projection['metadata']['uid'], journalUIDBefore=original_journal['metadata']['uid'])
        binary = E['RESULT_TRUST_FIXTURE_BINARY']
        evidence['fixtureBinarySHA256'] = hashlib.sha256(pathlib.Path(binary).read_bytes()).hexdigest()
        result = subprocess.run([binary, 'result-expired-trust'], input=base64.b64decode(original_journal['data']['rotation.json']), capture_output=True, timeout=30)
        if result.returncode:
            raise RuntimeError('Expired-trust fixture refused the original journal')
        fixture = json.loads(result.stdout)
        seed = json.loads(base64.b64decode(fixture['journal']))
        evidence['expiredPublicCertificates'] = fixture['public']
        evidence['expiredNotAfter'] = {}
        for label, value in fixture['public'].items():
            parsed = subprocess.run(['openssl', 'x509', '-noout', '-enddate'], input=base64.b64decode(value), capture_output=True, check=True, timeout=10)
            evidence['expiredNotAfter'][label] = ssl.cert_time_to_seconds(parsed.stdout.decode().strip().split('=', 1)[1])
        evidence.update(seededPhase=seed['phase'], expiredCandidate=roots(seed['next']), seededAt=time.time())
        seeded = True
        write_data('configmap', original_policy, fixture['policy'])
        write_data('secret', original_journal, {'rotation.json': fixture['journal']})
        write_data('secret', original_projection, fixture['projection'])
        validation = {'apiVersion': 'admissionregistration.k8s.io/v1', 'kind': 'ValidatingAdmissionPolicy',
                      'metadata': {'name': gate}, 'spec': {'failurePolicy': 'Fail', 'matchConstraints': {'resourceRules': [
                          {'apiGroups': [''], 'apiVersions': ['v1'], 'operations': ['UPDATE'], 'resources': ['secrets']}]},
                          'matchConditions': [{'name': 'exact-projection', 'expression': 'object.metadata.namespace == ' + json.dumps(opns) + ' && object.metadata.name == ' + json.dumps(projection_name)}],
                          'validations': [{'expression': 'object.data == oldObject.data', 'message': 'Qualification holds recovered CA projection'}]}}
        create(validation)
        gate_created = True
        create({'apiVersion': validation['apiVersion'], 'kind': 'ValidatingAdmissionPolicyBinding', 'metadata': {'name': gate}, 'spec': {'policyName': gate, 'validationActions': ['Deny']}})

        def refuses():
            current = get('secret', projection_name, namespace=opns)
            current['data']['tls.crt'] = base64.b64encode(b'qualification dry run').decode()
            try:
                k('replace', '--dry-run=server', '-f', '-', data=json.dumps(current), namespace=opns)
            except RuntimeError as err:
                if 'Qualification holds recovered CA projection' in str(err):
                    return True
                raise
            return False
        wait(refuses, 30)
        evidence['gateRefused'] = True
        k('delete', 'pod', *(p['metadata']['name'] for p in initial_managers), '--wait=true', '--timeout=60s', namespace=opns)

        def expired_receivers():
            current = pods(manager)
            return len(current) == 2 and not set(evidence['managerUIDsBefore']) & {p['metadata']['uid'] for p in current} and not any(map(ready, current))
        wait(expired_receivers, 120)
        evidence['managersUnavailableWithExpiredTrust'] = True
        scale(1)

        def held():
            _, st = state()
            return st if st['phase'] == 'prepare' and st.get('fencedAt') and roots(st['next']) != evidence['expiredCandidate'] else None
        pending = wait(held, 180)
        evidence.update(heldPhase=pending['phase'], candidateBeforeRestart=roots(pending['next']), fenceBeforeRestart=pending['fencedAt'],
                        projectionHeld=get('secret', projection_name, namespace=opns)['data'] == fixture['projection'])
        active = [p for p in pods(deployment) if not p['metadata'].get('deletionTimestamp')]
        if len(active) != 1:
            raise ValueError('Expected one interrupted rotator')
        old = active[0]
        evidence['rotatorUIDBefore'] = old['metadata']['uid']
        k('delete', 'pod', old['metadata']['name'], '--wait=true', '--timeout=60s', namespace=opns)

        def replacement():
            active = [p for p in pods(deployment) if not p['metadata'].get('deletionTimestamp')]
            return active[0] if len(active) == 1 and active[0]['metadata']['uid'] != evidence['rotatorUIDBefore'] and active[0].get('status', {}).get('phase') == 'Running' else None
        new = wait(replacement, 120)
        evidence['rotatorUIDAfter'] = new['metadata']['uid']
        _, resumed = state()
        evidence.update(candidateAfterRestart=roots(resumed['next']), fenceAfterRestart=resumed['fencedAt'])
        k('delete', 'validatingadmissionpolicybinding', gate)
        k('delete', 'validatingadmissionpolicy', gate)
        gate_created = False

        def stable():
            obj, st = state()
            return (obj, st) if st['phase'] == 'stable' and roots(st['current']) == evidence['candidateBeforeRestart'] else None
        after_obj, after = wait(stable, 360)
        for name in (manager['metadata']['name'], rotator):
            k('rollout', 'status', 'deployment/' + name, '--timeout=180s', namespace=opns)
        current_managers = pods(manager)
        projection = get('secret', projection_name, namespace=opns)
        if digest(projection['data']['ca.crt']) != digest(after['current']['ServerCA']) or digest(projection['data']['client-ca.crt']) != digest(after['current']['ClientCA']):
            raise ValueError('Stable result trust does not project the new CAs')
        evidence.update(phaseAfter=after['phase'], currentAfter=roots(after['current']), journalUIDAfter=after_obj['metadata']['uid'],
                        projectionUIDAfter=projection['metadata']['uid'], managerUIDsAfter=sorted(p['metadata']['uid'] for p in current_managers),
                        receiversReady=len(current_managers) == 2 and all(map(ready, current_managers)), rotatorReady=any(map(ready, pods(deployment))),
                        newServerCA=after['current']['ServerCA'], newClientCA=after['current']['ClientCA'])
        success = True
        print('Expired pending CA journal recovered through a new persisted candidate and rotator replacement', flush=True)
    finally:
        if gate_created:
            k('delete', 'validatingadmissionpolicybinding', gate, '--ignore-not-found')
            k('delete', 'validatingadmissionpolicy', gate, '--ignore-not-found')
        if seeded and not success:
            scale(0)
            wait(lambda: not pods(deployment), 90)
            write_data('configmap', original_policy, original_policy['data'])
            write_data('secret', original_journal, original_journal['data'])
            write_data('secret', original_projection, original_projection['data'])
        scale(1)
        if not success:
            k('rollout', 'restart', 'deployment/' + manager['metadata']['name'], namespace=opns)
            for name in (manager['metadata']['name'], rotator):
                k('rollout', 'status', 'deployment/' + name, '--timeout=240s', namespace=opns)
        final = get('deployment', rotator, namespace=opns)
        evidence.update(replicasRestored=final['spec']['replicas'] == deployment['spec']['replicas'],
                        argumentsUnchanged=final['spec']['template']['spec']['containers'][0]['args'] == original_args,
                        temporaryGateRemoved=True, completedAt=time.time())
        save('result-ca-rotation.json', evidence)
    evidence['verification'] = verify_evidence(evidence)
    save('result-ca-rotation.json', evidence)
    return evidence
