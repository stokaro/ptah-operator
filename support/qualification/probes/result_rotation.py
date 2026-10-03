"""Interrupt installed result leaf renewal before projection, then resume it.

Called only by the owned-cluster first-harvest probe while leadership is held.
Private journal/projection values stay in memory; evidence contains hashes only.
"""
import base64
import copy
import hashlib
import json
import pathlib
import time


def digest(value):
    return hashlib.sha256(base64.b64decode(value, validate=True)).hexdigest()


def verify_evidence(value):
    def require(ok, message):
        if not ok:
            raise ValueError(message)
    require(value['phaseBefore'] == 'stable' and value['phaseInterrupted'] == 'leaf'
            and value['phaseAfter'] == 'stable', 'Missing actual leaf transition')
    require(value['gateRefused'] and value['projectionHeld'], 'Projection was not held')
    require(value['rotatorUIDBefore'] != value['rotatorUIDAfter']
            and value['rotatorUIDBefore'] and value['rotatorUIDAfter'], 'Rotator did not restart')
    require(value['candidateBeforeRestart'] == value['candidateAfterRestart']
            == value['leafAfter'], 'Restart replaced the persisted candidate')
    require(value['leafBefore'] != value['leafAfter'], 'Serving certificate did not rotate')
    require(value['keyBefore'] != value['keyAfter'], 'Serving key did not rotate')
    require(value['serverCABefore'] == value['serverCAAfter']
            and value['clientCABefore'] == value['clientCAAfter'], 'Leaf renewal changed CA')
    require(len(value['managerUIDsBefore']) == len(set(value['managerUIDsBefore'])) == 2
            and value['managerUIDsBefore'] == value['managerUIDsAfter'], 'Manager identity changed during projection')
    require(value['projectionUIDBefore'] == value['projectionUIDAfter']
            and value['journalUIDBefore'] == value['journalUIDAfter'], 'Trust object was replaced')
    require(value['policyBefore'] == value['policyAfter'], 'Leaf renewal changed enrollment')
    require(value['rotatorReadyAfter'] and value['restoredArguments'], 'Rotation did not recover')
    return {'persistedCandidateResumed': True, 'servingReplicas': 2}


def run(k, get, create, wait, save, environment):
    opns = environment['E2E_OPERATOR_NAMESPACE']
    manager_name = environment['E2E_CONTROLLER_NAME']
    gate = environment['RESULT_PROBE_NAMESPACE'] + '-rotation'
    deployments = get('deployments', namespace=opns)['items']
    candidates = [d for d in deployments if any(c['name'] == 'certificate-rotator'
                  for c in d['spec']['template']['spec']['containers'])]
    if len(candidates) != 1:
        raise ValueError('Expected the owned installation rotator')
    deployment = candidates[0]
    name = deployment['metadata']['name']
    containers = deployment['spec']['template']['spec']['containers']
    if len(containers) != 1 or deployment['spec']['replicas'] != 1:
        raise ValueError('Unexpected rotator topology')
    original_args = containers[0]['args']
    options = dict(a[2:].split('=', 1) for a in original_args if a.startswith('--') and '=' in a)
    projection_name = options['result-secret-name']
    journal_name = options['result-journal-secret-name']
    policy_name = options['result-enrollment-policy']
    selector = ','.join(f'{key}={value}' for key, value in deployment['spec']['selector']['matchLabels'].items())
    manager = get('deployment', manager_name, namespace=opns)
    manager_selector = ','.join(f'{key}={value}' for key, value in manager['spec']['selector']['matchLabels'].items())

    def pods(select):
        return json.loads(k('get', 'pods', '-l', select, '-o', 'json', namespace=opns))['items']

    def ready(p):
        return not p['metadata'].get('deletionTimestamp') and any(c['type'] == 'Ready' and c['status'] == 'True' for c in p.get('status', {}).get('conditions', []))

    def journal():
        obj = get('secret', journal_name, namespace=opns)
        return obj, json.loads(base64.b64decode(obj['data']['rotation.json'], validate=True))

    def set_args(args):
        current = get('deployment', name, namespace=opns)
        patch = [{'op': 'test', 'path': '/metadata/uid', 'value': deployment['metadata']['uid']},
                 {'op': 'test', 'path': '/metadata/resourceVersion', 'value': current['metadata']['resourceVersion']},
                 {'op': 'replace', 'path': '/spec/template/spec/containers/0/args', 'value': args}]
        k('patch', 'deployment', name, '--type=json', '-p', json.dumps(patch), namespace=opns)

    before_obj, before = journal()
    projection = get('secret', projection_name, namespace=opns)
    policy = get('configmap', policy_name, namespace=opns)
    managers = pods(manager_selector)
    if before['phase'] != 'stable' or len(managers) != 2 or not all(map(ready, managers)):
        raise ValueError('Rotation requires stable trust and two ready receivers')
    evidence = {'phaseBefore': before['phase'], 'leafBefore': digest(before['current']['ServerCertificate']),
                'keyBefore': digest(before['current']['ServerCertificateKey']),
                'serverCABefore': digest(before['current']['ServerCA']), 'clientCABefore': digest(before['current']['ClientCA']),
                'managerUIDsBefore': sorted(p['metadata']['uid'] for p in managers),
                'projectionUIDBefore': projection['metadata']['uid'], 'journalUIDBefore': before_obj['metadata']['uid'],
                'policyBefore': policy['data'], 'procedureSHA256': hashlib.sha256(pathlib.Path(__file__).read_bytes()).hexdigest(),
                'scope': 'Installed interrupted serving-certificate/key renewal; CA retirement waits are unchanged and not exercised.',
                'image': containers[0]['image'], 'startedAtUnix': time.time()}
    gate_created = args_changed = False
    validation = {'apiVersion': 'admissionregistration.k8s.io/v1', 'kind': 'ValidatingAdmissionPolicy',
                  'metadata': {'name': gate}, 'spec': {'failurePolicy': 'Fail', 'matchConstraints': {'resourceRules': [
                      {'apiGroups': [''], 'apiVersions': ['v1'], 'operations': ['UPDATE'], 'resources': ['secrets']}]},
                      'matchConditions': [{'name': 'exact-projection', 'expression': 'object.metadata.namespace == ' + json.dumps(opns) + ' && object.metadata.name == ' + json.dumps(projection_name)}],
                      'validations': [{'expression': 'object.data == oldObject.data', 'message': 'Qualification holds result trust projection'}]}}
    try:
        create(validation)
        gate_created = True
        create({'apiVersion': validation['apiVersion'], 'kind': 'ValidatingAdmissionPolicyBinding', 'metadata': {'name': gate},
                'spec': {'policyName': gate, 'validationActions': ['Deny']}})

        def refuses_projection():
            probe = copy.deepcopy(projection)
            probe['data']['tls.crt'] = base64.b64encode(b'qualification dry run').decode()
            try:
                k('replace', '--dry-run=server', '-f', '-', data=json.dumps(probe), namespace=opns)
            except RuntimeError as e:
                if 'Qualification holds result trust projection' in str(e):
                    return True
                raise
            return False
        wait(refuses_projection, 30)
        evidence['gateRefused'] = True
        # All shipped leaves have <=2160h validity. The replacement lasts 2400h,
        # so it is not due immediately again. Neither CA nor any fence is edited.
        args = ['--renewal-threshold=2160h' if a.startswith('--renewal-threshold=') else
                '--serving-certificate-validity=2400h' if a.startswith('--serving-certificate-validity=') else a for a in original_args]
        set_args(args)
        args_changed = True

        def held_candidate():
            _, state = journal()
            return state if state['phase'] == 'leaf' and state.get('next') else None
        held = wait(held_candidate, 180)
        evidence['phaseInterrupted'] = held['phase']
        evidence['candidateBeforeRestart'] = digest(held['next']['ServerCertificate'])
        evidence['projectionHeld'] = get('secret', projection_name, namespace=opns)['data'] == projection['data']

        def one_current_rotator():
            current = [p for p in pods(selector) if not p['metadata'].get('deletionTimestamp')]
            return current[0] if len(current) == 1 and current[0]['spec']['containers'][0]['args'] == args else None
        old_rotator = wait(one_current_rotator, 90)
        evidence['rotatorUIDBefore'] = old_rotator['metadata']['uid']
        k('delete', 'pod', old_rotator['metadata']['name'], '--wait=true', '--timeout=60s', namespace=opns)

        def replacement_rotator():
            current = one_current_rotator()
            return current if current and current['metadata']['uid'] != evidence['rotatorUIDBefore'] and current.get('status', {}).get('phase') == 'Running' else None
        new_rotator = wait(replacement_rotator, 120)
        evidence['rotatorUIDAfter'] = new_rotator['metadata']['uid']
        _, resumed = journal()
        evidence['candidateAfterRestart'] = digest(resumed['next']['ServerCertificate'])
        if not evidence['projectionHeld'] or resumed['phase'] != 'leaf':
            raise ValueError('Rotation escaped the interruption gate')
        k('delete', 'validatingadmissionpolicybinding', gate)
        k('delete', 'validatingadmissionpolicy', gate)
        gate_created = False

        def complete():
            obj, state = journal()
            return (obj, state) if state['phase'] == 'stable' and digest(state['current']['ServerCertificate']) == evidence['candidateBeforeRestart'] else None
        after_obj, after = wait(complete, 360)
        k('rollout', 'status', 'deployment/' + name, '--timeout=120s', namespace=opns)
        after_projection = get('secret', projection_name, namespace=opns)
        if digest(after_projection['data']['tls.crt']) != digest(after['current']['ServerCertificate']):
            raise ValueError('Stable journal does not match projection')
        evidence.update(phaseAfter=after['phase'], leafAfter=digest(after['current']['ServerCertificate']),
                        keyAfter=digest(after['current']['ServerCertificateKey']),
                        serverCAAfter=digest(after['current']['ServerCA']), clientCAAfter=digest(after['current']['ClientCA']),
                        managerUIDsAfter=sorted(p['metadata']['uid'] for p in pods(manager_selector)),
                        projectionUIDAfter=after_projection['metadata']['uid'], journalUIDAfter=after_obj['metadata']['uid'],
                        policyAfter=get('configmap', policy_name, namespace=opns)['data'],
                        rotatorReadyAfter=any(ready(p) and p['metadata']['uid'] == evidence['rotatorUIDAfter'] for p in pods(selector)))
        print('Result leaf renewal resumed its persisted candidate after rotator replacement', flush=True)
    finally:
        if gate_created:
            k('delete', 'validatingadmissionpolicybinding', gate, '--ignore-not-found')
            k('delete', 'validatingadmissionpolicy', gate, '--ignore-not-found')
        if args_changed:
            set_args(original_args)
            k('rollout', 'status', 'deployment/' + name, '--timeout=180s', namespace=opns)
        evidence['restoredArguments'] = get('deployment', name, namespace=opns)['spec']['template']['spec']['containers'][0]['args'] == original_args
        evidence['finishedAtUnix'] = time.time()
        save('result-leaf-rotation.json', evidence)
    evidence['verification'] = verify_evidence(evidence)
    save('result-leaf-rotation.json', evidence)
    return evidence
