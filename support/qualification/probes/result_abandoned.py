#!/usr/bin/env python3
"""Collect an abandoned quota-interrupted Plan using the installed collector."""
import base64
import datetime as dt
import hashlib
import json
import os
import pathlib
import re
import subprocess
import time
from result_retention_evidence import verify_abandoned, instant
from result_first_harvest import pod_binding_record


def main():
    if not __debug__:
        raise RuntimeError('Acceptance assertions require Python without optimization')
    e = os.environ
    ns = e.get('RESULT_PROBE_NAMESPACE', 'ptah-result-abandoned')
    source_namespace = e['RESULT_PROBE_SOURCE_NAMESPACE']
    source_name = e['RESULT_PROBE_SOURCE_NAME']
    pin_ns = e['RESULT_PROBE_PIN_NAMESPACE']
    artifact_digest = e['RESULT_PROBE_ARTIFACT_DIGEST']
    assert re.fullmatch('sha256:[0-9a-f]{64}', artifact_digest)
    out = pathlib.Path(e['RESULT_PROBE_EVIDENCE_DIR'])
    out.mkdir(exist_ok=True)

    def run(args, data=None):
        p = subprocess.run(args, input=data, text=True, capture_output=True, timeout=60)
        if p.returncode:
            raise RuntimeError(args[0] + ' failed: ' + p.stderr[-1000:])
        return p.stdout

    def k(*args, namespace=ns, data=None):
        return run(['kubectl', '--kubeconfig', e['E2E_KUBECONFIG'], '--request-timeout=30s', '-n', namespace, *args], data)

    def get(kind, name=None, namespace=ns):
        return json.loads(k('get', kind, *([name] if name else []), '-o', 'json', namespace=namespace))

    def create(o):
        return json.loads(k('create', '-f', '-', '-o', 'json', data=json.dumps(o)))

    def wait(fn, seconds=180):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            value = fn()
            if value:
                return value
            time.sleep(.5)
        raise RuntimeError('Timed out waiting for ' + fn.__name__)

    def save(name, obj):
        text = json.dumps(obj, indent=2) + '\n'
        assert 'PRIVATE KEY' not in text
        (out / name).write_text(text)

    def decode(r):
        return json.loads(base64.b64decode(r['spec']['data'], validate=True))

    def public(r):
        m = r['metadata']
        v = {'name': m['name'], 'uid': m['uid'], 'createdAt': m['creationTimestamp'],
             'owners': m.get('ownerReferences', []), 'type': r['spec']['type'],
             'dataDigest': 'sha256:' + hashlib.sha256(base64.b64decode(r['spec']['data'])).hexdigest()}
        if r['spec']['type'] in ('intent', 'retired', 'credential'):
            v['binding'] = decode(r)['binding']
        return v

    def records(namespace=ns):
        return {r['metadata']['name']: r for r in get('ptahresultrecords', namespace=namespace)['items']}

    nodes = get('nodes')['items']
    for node in nodes:
        name = node['metadata']['name']
        owner = run(['docker', '--context', e['E2E_DOCKER_CONTEXT'], 'inspect', '--format', '{{index .Config.Labels "io.x-k8s.kind.cluster"}}', name]).strip()
        assert owner == e['E2E_KIND_CLUSTER_NAME']
        config = json.loads(k('get', '--raw', '/api/v1/nodes/' + name + '/proxy/configz'))
        assert config['kubeletconfig']['containerLogMaxSize'] == '10Mi'
    assert len(nodes) == 4
    create({'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': ns, 'labels': {'operator.ptah.run/acceptance-owner': e['E2E_KIND_CLUSTER_NAME']}}})
    for kind, name in [('secret', 'demo-registry'), ('secret', 'demo-registry-pull'), ('secret', 'demo-database'), ('configmap', 'demo-verification-policy')]:
        obj = get(kind, name, e['E2E_TEST_NAMESPACE'])
        obj['metadata'] = {'name': name, 'namespace': ns}
        create(obj)
    k('patch', 'serviceaccount', 'default', '--type=merge', '-p', json.dumps({'imagePullSecrets': [{'name': 'demo-registry-pull'}]}))
    source = get('ptahschema', source_name, source_namespace)
    assert get('namespace', source_namespace)['metadata']['labels']['operator.ptah.run/acceptance-owner'] == e['E2E_KIND_CLUSTER_NAME']
    source = {'apiVersion': source['apiVersion'], 'kind': source['kind'], 'metadata': {'name': 'abandoned', 'namespace': ns}, 'spec': source['spec']}
    source['spec']['policy']['apply'] = 'Never'
    source['spec']['target']['coordinationKey'] = 'qualification/abandoned/plan'
    source['spec']['target']['urlFrom'] = {'name': 'demo-database', 'key': 'url'}
    assert '@sha256:' in source['spec']['desired']['ociRef']
    source['spec']['desired']['ociRef'] = source['spec']['desired']['ociRef'].rsplit('@', 1)[0] + '@' + artifact_digest
    source['spec']['suspend'] = False
    gate = ns + '-gate'
    message = 'Acceptance probe holds abandoned Plan Pods'
    create({'apiVersion': 'admissionregistration.k8s.io/v1', 'kind': 'ValidatingAdmissionPolicy',
            'metadata': {'name': gate}, 'spec': {'failurePolicy': 'Fail', 'matchConstraints': {
                'resourceRules': [{'apiGroups': [''], 'apiVersions': ['v1'], 'operations': ['CREATE'], 'resources': ['pods']}]},
                'validations': [{'expression': "!has(object.metadata.labels) || !('operator.ptah.run/operation' in object.metadata.labels) || object.metadata.labels['operator.ptah.run/operation'] != 'plan'", 'message': message}]}})
    try:
        create({'apiVersion': 'admissionregistration.k8s.io/v1', 'kind': 'ValidatingAdmissionPolicyBinding',
                'metadata': {'name': gate}, 'spec': {'policyName': gate, 'validationActions': ['Deny'],
                'matchResources': {'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': ns}}}}})
        def gate_ready():
            probe = {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': 'gate-probe', 'namespace': ns,
                     'labels': {'operator.ptah.run/operation': 'plan'}},
                     'spec': {'automountServiceAccountToken': False, 'restartPolicy': 'Never',
                              'containers': [{'name': 'probe', 'image': e['RESULT_PROBE_FIXTURE_IMAGE']}]}}
            result = subprocess.run(['kubectl', '--kubeconfig', e['E2E_KUBECONFIG'], 'create', '--dry-run=server', '-f', '-'],
                                    input=json.dumps(probe), text=True, capture_output=True, timeout=30)
            return result.returncode != 0 and message in result.stderr
        wait(gate_ready, 30)
        resource = create(source)
        def held_plan():
            op = get('ptahschema', 'abandoned').get('status', {}).get('activeOperation') or {}
            return op if op.get('type') in ('Plan', 'plan') and op.get('jobUID') else None
        operation = wait(held_plan, 300)
        assert not any(any(o['uid'] == operation['jobUID'] for o in p['metadata'].get('ownerReferences', [])) for p in get('pods')['items'])
        preceding = records()
        key = 'count/ptahresultrecords.operator.ptah.run'
        limit = str(len(preceding) + 2)
        create({'apiVersion': 'v1', 'kind': 'ResourceQuota', 'metadata': {'name': 'hold-partial', 'namespace': ns}, 'spec': {'hard': {key: limit}}})
        wait(lambda: get('resourcequota', 'hold-partial').get('status', {}).get('hard', {}).get(key) == limit)
    finally:
        k('delete', 'validatingadmissionpolicybinding', gate, '--ignore-not-found')
        k('delete', 'validatingadmissionpolicy', gate, '--ignore-not-found')

    def cohort(rows, job_uid):
        intents = {r['metadata']['uid'] for r in rows.values() if r['spec']['type'] == 'intent' and decode(r)['binding']['jobUID'] == job_uid}
        return {name: r for name, r in rows.items() if (
            r['spec']['type'] in ('intent', 'retired', 'credential') and decode(r)['binding']['jobUID'] == job_uid)
            or any(o['uid'] in intents for o in r['metadata'].get('ownerReferences', []))}

    def partial():
        all_rows = records()
        assert all(n in all_rows and public(all_rows[n]) == public(r) for n, r in preceding.items()), 'Earlier operation changed during the quota fault'
        rows = cohort(all_rows, operation['jobUID'])
        intents = [r for r in rows.values() if r['spec']['type'] == 'intent']
        if not intents:
            return None
        assert len(rows) == 2 and len(intents) == 1
        assert {r['spec']['type'] for r in rows.values()} == {'intent', 'credential'}
        return rows

    interrupted = wait(partial)
    intent = next(r for r in interrupted.values() if r['spec']['type'] == 'intent')
    assert 'inline' not in decode(intent), 'Atomic result is complete, not an interrupted chunk publication'
    binding = decode(intent)['binding']
    assert decode(intent)['size'] > 262144 and decode(intent)['chunks']
    assert binding['operation'] == 'plan' and binding['uid'] == resource['metadata']['uid']
    current = get('ptahschema', 'abandoned')
    assert current['status']['activeOperation']['id'] == binding['operationID']
    job = get('job', binding['jobName'])
    pod = get('pod', binding['podName'])
    assert job['metadata']['uid'] == binding['jobUID'] and pod['metadata']['uid'] == binding['podUID']
    credential = next(r for r in interrupted.values() if r['spec']['type'] == 'credential')
    assert credential['spec'] == pod_binding_record(job, pod)['spec']

    def audit_events():
        events = []
        for node in nodes:
            if 'node-role.kubernetes.io/control-plane' not in node['metadata'].get('labels', {}):
                continue
            raw = run(['docker', '--context', e['E2E_DOCKER_CONTEXT'], 'exec', node['metadata']['name'], 'cat', '/etc/kubernetes/result-acceptance-audit/events.jsonl'])
            events.extend(a for a in map(json.loads, raw.splitlines()) if a.get('objectRef', {}).get('namespace') == ns)
        return events

    def chunk_refusals():
        return [a for a in audit_events() if a.get('verb') == 'create'
                and a.get('stage') == 'ResponseComplete' and a.get('responseStatus', {}).get('code') == 403
                and 'exceeded quota: hold-partial' in a['responseStatus'].get('message', '')
                and a.get('objectRef', {}).get('resource') == 'ptahresultrecords'
                and a['objectRef'].get('name') == intent['metadata']['name'] + '-000']
    refused = wait(chunk_refusals, 60)
    current = get('ptahschema', 'abandoned')
    assert current['status']['activeOperation']['id'] == binding['operationID']
    interrupted_reading = {'resource': current, 'job': job, 'pod': pod,
                          'records': [public(r) for r in interrupted.values()], 'intent': intent,
                          'precedingRecords': [public(r) for r in preceding.values()],
                          'quota': get('resourcequota', 'hold-partial')}
    save('interrupted.json', interrupted_reading)
    save('quota-refusal-audit.json', refused)
    k('patch', 'ptahschema', 'abandoned', '--type=json', '-p', json.dumps([
        {'op': 'test', 'path': '/metadata/uid', 'value': resource['metadata']['uid']},
        {'op': 'test', 'path': '/metadata/resourceVersion', 'value': current['metadata']['resourceVersion']},
        {'op': 'replace', 'path': '/spec/suspend', 'value': True}]))
    delete_options = {'apiVersion': 'v1', 'kind': 'DeleteOptions',
                      'preconditions': {'uid': binding['jobUID']}, 'propagationPolicy': 'Background'}
    save('job-delete-options.json', delete_options)
    if k('get', 'job', binding['jobName'], '--ignore-not-found', '-o', 'name').strip():
        k('delete', '--raw', '/apis/batch/v1/namespaces/' + ns + '/jobs/' + binding['jobName'],
          '-f', '-', data=json.dumps(delete_options))
    wait(lambda: not any(any(o['uid'] == binding['jobUID'] for o in p['metadata'].get('ownerReferences', [])) for p in get('pods')['items']))
    k('delete', 'resourcequota', 'hold-partial')
    wait(lambda: not get('ptahschema', 'abandoned').get('status', {}).get('activeOperation'))

    def retired():
        rows = cohort(records(), binding['jobUID'])
        markers = [r for r in rows.values() if r['spec']['type'] == 'retired']
        if not markers:
            return None
        assert len(rows) == 3 and len(markers) == 1
        assert decode(markers[0])['binding'] == binding
        return rows, markers[0]

    baseline, marker = wait(retired)
    # Reuse a completed native migration's live recovery pin as the control.
    assert get('namespace', pin_ns)['metadata']['labels']['operator.ptah.run/acceptance-owner'] == e['E2E_KIND_CLUSTER_NAME']
    pinned_resource = get('ptahmigration', 'lost-ack', pin_ns)
    assert pinned_resource['spec']['suspend']
    pinned_job = pinned_resource['status']['lastRun']['jobUID']
    pin_all = records(pin_ns)
    pin_intents = [r for r in pin_all.values() if r['spec']['type'] == 'intent' and decode(r)['binding']['jobUID'] == pinned_job]
    assert len(pin_intents) == 1
    pinned_intent = pin_intents[0]
    pinned = {n: r for n, r in pin_all.items() if n == pinned_intent['metadata']['name'] or
              any(o['uid'] == pinned_intent['metadata']['uid'] for o in r['metadata'].get('ownerReferences', [])) or
              (r['spec']['type'] == 'credential' and r['metadata']['annotations']['operator.ptah.run/result-job-uid'] == pinned_job)}
    expected_roles = {'intent', 'credential'} if 'inline' in decode(pinned_intent) else {'intent', 'chunk', 'complete', 'credential'}
    assert {r['spec']['type'] for r in pinned.values()} == expected_roles
    eligible = set(baseline)
    all_rows = baseline | pinned
    marker_value = decode(marker)
    assert marker_value['retentionSeconds'] >= 3600
    deadlines = {n: max(instant(marker['metadata']['creationTimestamp']), instant(r['metadata']['creationTimestamp'])) + dt.timedelta(seconds=marker_value['retentionSeconds']) for n, r in baseline.items()}
    secret_owners = {}
    credential_names = sorted(n for n, r in all_rows.items() if r['spec']['type'] == 'credential')
    assert credential_names
    def secret_census():
        present = sorted(r['metadata']['name'] for namespace in (ns, pin_ns)
                         for r in get('secrets', namespace=namespace)['items']
                         if r['metadata']['name'] in credential_names)
        assert not present, 'Pod-token credential has a Secret projection'
        return {'expectedNames': credential_names, 'presentNames': present}

    def plans():
        return {r['metadata']['uid']: hashlib.sha256(json.dumps(r['spec'], sort_keys=True).encode()).hexdigest()
                for kind in ('ptahschemaplans', 'ptahschemaplanchunks') for r in get(kind, namespace=source_namespace)['items']}

    plan_hashes = plans()
    assert plan_hashes
    manager = get('deployment', e['E2E_CONTROLLER_NAME'], e['E2E_OPERATOR_NAMESPACE'])
    collector = 'system:serviceaccount:' + e['E2E_OPERATOR_NAMESPACE'] + ':' + manager['spec']['template']['spec']['serviceAccountName']
    before = {'records': [public(r) for r in all_rows.values()], 'eligibleNames': sorted(eligible), 'pinnedNames': sorted(pinned),
              'authentication': 'pod-token', 'secretOwners': secret_owners,
              'collectorIdentity': collector,
              'credentialSecretCensus': secret_census(), 'planDigests': plan_hashes, 'deadlines': {n: d.isoformat() for n, d in deadlines.items()},
              'runtimeRevision': e['E2E_CONTROLLER_REVISION'], 'pinnedResource': pinned_resource,
              'procedureSHA256': hashlib.sha256(pathlib.Path(__file__).read_bytes()).hexdigest()}
    save('retention-before.json', before)
    save('retention-markers.json', [{'metadata': marker['metadata'], 'spec': {'value': marker_value}}])
    print('Frozen abandoned publication:', len(eligible), 'records; deadline', max(deadlines.values()).isoformat(), flush=True)
    end = time.monotonic() + 4200
    last = 0
    while time.monotonic() < end:
        now = records()
        remaining_pins = records(pin_ns)
        assert all(n in remaining_pins and public(remaining_pins[n]) == public(r) for n, r in pinned.items())
        secret_census()
        assert not any(j['metadata']['uid'] == binding['jobUID'] for j in get('jobs')['items'])
        assert not get('ptahschema', 'abandoned')['status'].get('activeOperation')
        row = {'observedAt': dt.datetime.now(dt.timezone.utc).isoformat(), 'remainingRecords': len(eligible & now.keys()), 'remainingEligibleSecrets': 0, 'pinnedRecords': len(pinned)}
        with (out / 'retention-observations.jsonl').open('a') as f:
            f.write(json.dumps(row) + '\n')
        if time.monotonic() - last >= 60:
            print(json.dumps(row), flush=True)
            last = time.monotonic()
        if not (eligible & now.keys()):
            break
        time.sleep(5)
    else:
        raise RuntimeError('Abandoned publication or its projection was not collected')
    assert not (eligible & now.keys()) and plans() == plan_hashes
    audits = audit_events()
    after = {'authentication': 'pod-token', 'credentialSecretCensus': secret_census(), 'remainingPinned': [public(remaining_pins[n]) for n in sorted(pinned)], 'planDigests': plans(),
             'eligibleRecordsCollected': len(eligible), 'eligibleSecretProjectionsCollected': len(eligible & secret_owners.keys())}
    markers = json.loads((out / 'retention-markers.json').read_text())
    verdict = verify_abandoned(interrupted_reading, before, after, markers, audits, refused)
    save('retention-after.json', after)
    save('retention-delete-audit.json', [a for a in audits if a.get('verb') == 'delete'])
    save('quota-refusal-audit.json', refused)
    save('verification.json', verdict)
    print('PASS: abandoned publication collected after its API-timed window; pinned evidence unchanged', flush=True)


if __name__ == '__main__':
    main()
