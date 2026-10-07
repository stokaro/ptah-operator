#!/usr/bin/env python3
"""Collect an abandoned quota-interrupted Resolve using the installed collector."""
import base64
import datetime as dt
import hashlib
import json
import os
import pathlib
import subprocess
import time
from result_retention_evidence import verify, instant


def main():
    if not __debug__:
        raise RuntimeError('Acceptance assertions require Python without optimization')
    e = os.environ
    ns = 'ptah-result-abandoned'
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
        if r['spec']['type'] in ('intent', 'retired'):
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
    key = 'count/ptahresultrecords.operator.ptah.run'
    create({'apiVersion': 'v1', 'kind': 'ResourceQuota', 'metadata': {'name': 'hold-partial', 'namespace': ns}, 'spec': {'hard': {key: '2'}}})
    wait(lambda: get('resourcequota', 'hold-partial').get('status', {}).get('hard', {}).get(key) == '2')
    source = get('ptahschema', 'storefront', e['E2E_TEST_NAMESPACE'])
    source = {'apiVersion': source['apiVersion'], 'kind': source['kind'], 'metadata': {'name': 'abandoned', 'namespace': ns}, 'spec': source['spec']}
    source['spec']['policy']['apply'] = 'Never'
    source['spec']['target']['coordinationKey'] = 'qualification/abandoned/resolve'
    source['spec']['suspend'] = False
    resource = create(source)

    def partial():
        rows = records()
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
    assert binding['operation'] == 'resolve' and binding['uid'] == resource['metadata']['uid']
    current = get('ptahschema', 'abandoned')
    assert current['status']['activeOperation']['id'] == binding['operationID']
    job = get('job', binding['jobName'])
    pod = get('pod', binding['podName'])
    assert job['metadata']['uid'] == binding['jobUID'] and pod['metadata']['uid'] == binding['podUID']
    save('interrupted.json', {'resource': current, 'job': job, 'pod': pod,
                            'records': [public(r) for r in interrupted.values()], 'intent': intent,
                            'quota': get('resourcequota', 'hold-partial')})
    k('patch', 'ptahschema', 'abandoned', '--type=json', '-p', json.dumps([
        {'op': 'test', 'path': '/metadata/uid', 'value': resource['metadata']['uid']},
        {'op': 'test', 'path': '/metadata/resourceVersion', 'value': current['metadata']['resourceVersion']},
        {'op': 'replace', 'path': '/spec/suspend', 'value': True}]))
    wait(lambda: not get('ptahschema', 'abandoned').get('status', {}).get('activeOperation'))
    k('delete', 'job', binding['jobName'], '--ignore-not-found', '--wait=true', '--timeout=60s')
    wait(lambda: not get('pods')['items'])
    k('delete', 'resourcequota', 'hold-partial')

    def retired():
        rows = records()
        markers = [r for r in rows.values() if r['spec']['type'] == 'retired']
        if not markers:
            return None
        assert len(rows) == 3 and len(markers) == 1
        assert decode(markers[0])['binding'] == binding
        return rows, markers[0]

    baseline, marker = wait(retired)
    # Reuse a completed native migration's live recovery pin as the control.
    pin_ns = 'ptah-result-partial-mysql'
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
    for namespace, rows in [(ns, baseline), (pin_ns, pinned)]:
        for name, r in rows.items():
            if r['spec']['type'] == 'credential':
                secret_owners[name] = get('secret', name, namespace)['metadata']

    def plans():
        return {r['metadata']['uid']: hashlib.sha256(json.dumps(r['spec'], sort_keys=True).encode()).hexdigest()
                for kind in ('ptahschemaplans', 'ptahschemaplanchunks') for r in get(kind, namespace=e['E2E_TEST_NAMESPACE'])['items']}

    plan_hashes = plans()
    assert plan_hashes
    before = {'records': [public(r) for r in all_rows.values()], 'eligibleNames': sorted(eligible), 'pinnedNames': sorted(pinned),
              'secretOwners': secret_owners, 'planDigests': plan_hashes, 'deadlines': {n: d.isoformat() for n, d in deadlines.items()},
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
        secrets = {}
        for namespace in (ns, pin_ns):
            for r in get('secrets', namespace=namespace)['items']:
                if r['metadata']['name'] in secret_owners:
                    secrets[r['metadata']['name']] = r['metadata']
        assert all(n in secrets and secrets[n]['uid'] == secret_owners[n]['uid'] for n in set(pinned) & secret_owners.keys())
        assert not get('jobs')['items'] and not get('ptahschema', 'abandoned')['status'].get('activeOperation')
        row = {'observedAt': dt.datetime.now(dt.timezone.utc).isoformat(), 'remainingRecords': len(eligible & now.keys()), 'remainingEligibleSecrets': len(eligible & secrets.keys()), 'pinnedRecords': len(pinned)}
        with (out / 'retention-observations.jsonl').open('a') as f:
            f.write(json.dumps(row) + '\n')
        if time.monotonic() - last >= 60:
            print(json.dumps(row), flush=True)
            last = time.monotonic()
        if not (eligible & now.keys()) and not (eligible & secrets.keys()):
            break
        time.sleep(5)
    else:
        raise RuntimeError('Abandoned publication or its projection was not collected')
    assert not now and plans() == plan_hashes
    audits = []
    for node in nodes:
        if 'node-role.kubernetes.io/control-plane' not in node['metadata'].get('labels', {}):
            continue
        raw = run(['docker', '--context', e['E2E_DOCKER_CONTEXT'], 'exec', node['metadata']['name'], 'cat', '/etc/kubernetes/result-acceptance-audit/events.jsonl'])
        for line in raw.splitlines():
            a = json.loads(line)
            if a.get('objectRef', {}).get('namespace') == ns:
                audits.append(a)
    refused = [a for a in audits if a.get('verb') == 'create' and a.get('responseStatus', {}).get('code') == 403
               and a.get('objectRef', {}).get('resource') == 'ptahresultrecords' and a['objectRef'].get('name') == intent['metadata']['name'] + '-000']
    assert refused, 'No actual API refusal of the first chunk'
    after = {'remainingPinned': [public(remaining_pins[n]) for n in sorted(pinned)], 'planDigests': plans(),
             'eligibleRecordsCollected': len(eligible), 'eligibleSecretProjectionsCollected': len(eligible & secret_owners.keys())}
    markers = json.loads((out / 'retention-markers.json').read_text())
    verdict = verify(before, after, markers, audits)
    save('retention-after.json', after)
    save('retention-delete-audit.json', [a for a in audits if a.get('verb') == 'delete'])
    save('quota-refusal-audit.json', refused)
    save('verification.json', verdict)
    print('PASS: abandoned publication collected after its API-timed window; pinned evidence unchanged', flush=True)


if __name__ == '__main__':
    main()
