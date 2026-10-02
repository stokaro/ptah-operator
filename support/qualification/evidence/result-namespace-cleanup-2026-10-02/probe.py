#!/usr/bin/env python3
"""Verify foreground retirement of the two retained native migration namespaces."""
import base64
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import subprocess
import time

ROOT = Path('/private/tmp/ptah-namespace-retirement-installed')
ENV = dict(line.split('=', 1) for line in Path('/private/tmp/ptah-result-restart.env').read_text().splitlines() if '=' in line)
NS = ('ptah-result-network-pg-migration', 'ptah-result-network-mysql-migration')
K = ['kubectl', '--kubeconfig', ENV['E2E_KUBECONFIG'], '--request-timeout=20s']
D = ['docker', '--config', ENV['E2E_DOCKER_CONFIG'], '--context', ENV['E2E_DOCKER_CONTEXT']]
REPORT = {'status': 'RUNNING', 'startedAt': dt.datetime.now(dt.timezone.utc).isoformat(), 'cases': {}}


def instant(s):
    return dt.datetime.fromisoformat(s.replace('Z', '+00:00'))


def now():
    return dt.datetime.now(dt.timezone.utc)


def run(args, data=None, required=True, timeout=60):
    p = subprocess.run(args, input=data, capture_output=True, text=True, timeout=timeout)
    if required and p.returncode:
        raise RuntimeError(args[0] + ' failed: ' + p.stderr[-1200:])
    return p


def get(kind, namespace, name=None, missing=False):
    args = K + ['-n', namespace, 'get', kind] + ([name] if name else []) + ['-o', 'json']
    if missing:
        args += ['--ignore-not-found']
    p = run(args)
    return json.loads(p.stdout) if p.stdout.strip() else None


def save(name, obj):
    text = json.dumps(obj, indent=2) + '\n'
    assert 'PRIVATE KEY' not in text
    (ROOT / name).write_text(text)


def persist():
    save('report.json', REPORT)


def public(record):
    m = record['metadata']
    return {'name': m['name'], 'uid': m['uid'], 'type': record['spec']['type'],
            'createdAt': m['creationTimestamp'], 'owners': m.get('ownerReferences', []),
            'dataSHA256': hashlib.sha256(base64.b64decode(record['spec']['data'])).hexdigest()}


def records(namespace):
    return {r['metadata']['name']: r for r in get('ptahresultrecords', namespace)['items']}


def wait(fn, seconds):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        result = fn()
        if result:
            return result
        time.sleep(2)
    raise RuntimeError('Timed out: ' + fn.__name__)


def main():
    if not __debug__:
        raise RuntimeError('This proof requires assertions')
    image = json.loads((ROOT / 'image.json').read_text())
    REPORT['runtime'] = image
    REPORT['procedureSHA256'] = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
    before = {}
    for ns in NS:
        namespace = get('namespace', ns, ns)
        assert namespace['metadata']['labels']['operator.ptah.run/acceptance-owner'] == ENV['E2E_KIND_CLUSTER_NAME']
        assert namespace['metadata'].get('deletionTimestamp')
        rows = records(ns)
        assert len(rows) == 4 and {r['spec']['type'] for r in rows.values()} == {'intent', 'chunk', 'complete', 'credential'}
        assert not get('ptahmigrations', ns)['items'] and not get('jobs', ns)['items']
        assert all(not r['metadata'].get('deletionTimestamp') for r in rows.values())
        before[ns] = {n: public(r) for n, r in rows.items()}
        REPORT['cases'][ns] = {'namespaceUID': namespace['metadata']['uid'], 'namespaceDeletionTimestamp': namespace['metadata']['deletionTimestamp'], 'before': list(before[ns].values())}
    pin_ns = 'ptah-result-partial-mysql'
    migration = get('ptahmigration', pin_ns, 'lost-ack')
    assert migration['spec']['suspend']
    pin_uid = migration['status']['lastRun']['jobUID']
    all_pins = records(pin_ns)
    intents = [r for r in all_pins.values() if r['spec']['type'] == 'intent' and json.loads(base64.b64decode(r['spec']['data']))['binding']['jobUID'] == pin_uid]
    assert len(intents) == 1
    intent_uid = intents[0]['metadata']['uid']
    pins = {n: public(r) for n, r in all_pins.items() if r['metadata']['uid'] == intent_uid or any(o['uid'] == intent_uid for o in r['metadata'].get('ownerReferences', [])) or (r['spec']['type'] == 'credential' and r['metadata']['annotations']['operator.ptah.run/result-job-uid'] == pin_uid)}
    assert len(pins) == 4 and {r['type'] for r in pins.values()} == {'intent', 'chunk', 'complete', 'credential'}
    REPORT['pinnedControl'] = {'namespace': pin_ns, 'resourceUID': migration['metadata']['uid'], 'jobUID': pin_uid, 'records': list(pins.values())}
    opns = ENV['E2E_OPERATOR_NAMESPACE']
    manager = ENV['E2E_CONTROLLER_NAME']
    deployment = get('deployment', opns, manager)
    containers = deployment['spec']['template']['spec']['containers']
    assert len(containers) == 1 and deployment['spec']['replicas'] == 2
    REPORT['previousManagerImage'] = containers[0]['image']
    persist()
    run(K + ['-n', opns, 'set', 'image', 'deployment/' + manager, containers[0]['name'] + '=' + image['image']])
    run(K + ['-n', opns, 'rollout', 'status', 'deployment/' + manager, '--timeout=240s'], timeout=260)
    deployment = get('deployment', opns, manager)
    assert deployment['status']['readyReplicas'] == 2
    selector = deployment['spec']['selector']['matchLabels']
    pods = [p for p in get('pods', opns)['items'] if all(p['metadata']['labels'].get(k) == v for k, v in selector.items()) and not p['metadata'].get('deletionTimestamp')]
    assert len(pods) == 2 and all(p['spec']['containers'][0]['image'] == image['image'] for p in pods)
    REPORT['managerPods'] = [{'name': p['metadata']['name'], 'uid': p['metadata']['uid'], 'imageID': p['status']['containerStatuses'][0]['imageID']} for p in pods]
    deadlines = {}
    for ns in NS:
        def retired():
            rows = records(ns)
            roots = [r for r in rows.values() if r['spec']['type'] in ('intent', 'credential')]
            return rows if len(roots) == 2 and all(r['metadata'].get('deletionTimestamp') and r['metadata'].get('finalizers') == ['foregroundDeletion'] for r in roots) else None
        rows = wait(retired, 240)
        assert {n: public(r) for n, r in rows.items()} == before[ns]
        assert not any(r['spec']['type'] == 'retired' for r in rows.values())
        intent = next(r for r in rows.values() if r['spec']['type'] == 'intent')
        stamps = {}
        for name, record in rows.items():
            anchor = record if record['spec']['type'] == 'credential' else intent
            stamp = anchor['metadata']['deletionTimestamp']
            deadline = max(instant(stamp), instant(record['metadata']['creationTimestamp'])) + dt.timedelta(hours=1)
            deadlines[ns, name] = deadline
            stamps[name] = {'retiredAt': stamp, 'deadline': deadline.isoformat(), 'anchorUID': anchor['metadata']['uid']}
            denied = run(K + ['-n', ns, 'delete', 'ptahresultrecord', name, '--dry-run=server', '--cascade=background', '--wait=false'], required=False)
            assert denied.returncode and 'admission webhook' in denied.stderr
        REPORT['cases'][ns]['retirement'] = stamps
        print(ns, 'retained until', max(deadlines[n, r] for n, r in deadlines if n == ns).isoformat(), flush=True)
    persist()
    stop = time.monotonic() + 4200
    last_print = 0
    while time.monotonic() < stop:
        current_pins = records(pin_ns)
        assert all(name in current_pins and public(current_pins[name]) == value for name, value in pins.items())
        gone = []
        for ns in NS:
            current = records(ns)
            for name, expected in before[ns].items():
                if name in current:
                    assert public(current[name]) == expected
                else:
                    assert now() >= deadlines[ns, name], 'record disappeared before its retention deadline'
            namespace = get('namespace', ns, ns, missing=True)
            if namespace:
                assert namespace['metadata']['uid'] == REPORT['cases'][ns]['namespaceUID']
            else:
                assert not current
                gone.append(ns)
        REPORT['lastObservedAt'] = now().isoformat()
        REPORT['removedNamespaces'] = gone
        persist()
        if len(gone) == len(NS):
            break
        if time.monotonic() - last_print > 55:
            print('retention hold; namespaces remaining:', len(NS) - len(gone), flush=True)
            last_print = time.monotonic()
        time.sleep(5)
    else:
        raise RuntimeError('Namespaces did not finish collection after their full retention window')
    audits = []
    for suffix in ('control-plane', 'control-plane2', 'control-plane3'):
        node = ENV['E2E_KIND_CLUSTER_NAME'] + '-' + suffix
        raw = run(D + ['exec', node, 'cat', '/etc/kubernetes/result-acceptance-audit/events.jsonl']).stdout
        for line in raw.splitlines():
            event = json.loads(line)
            ref = event.get('objectRef', {})
            if ref.get('namespace') in NS and ref.get('resource') == 'ptahresultrecords' and event.get('stage') == 'ResponseComplete':
                audits.append(event)
    assert audits
    save('record-audit.json', audits)
    deletion_events = {}
    for ns in NS:
        for name, value in before[ns].items():
            successful = [a for a in audits if a.get('objectRef', {}).get('namespace') == ns and a['objectRef'].get('name') == name and 200 <= a.get('responseStatus', {}).get('code', 0) < 300 and a.get('verb') in ('delete', 'patch', 'update')]
            assert successful, 'missing API mutation evidence for ' + name
            after = []
            for event in successful:
                stamp = instant(event['requestReceivedTimestamp'])
                if stamp < deadlines[ns, name]:
                    assert value['type'] in ('intent', 'credential') and event['verb'] == 'delete' and event.get('requestObject', {}).get('propagationPolicy') == 'Foreground'
                    assert event['requestObject'].get('preconditions', {}).get('uid') == value['uid']
                else:
                    after.append(event['auditID'])
            assert after, 'no eligible deletion or finalizer completion for ' + name
            deletion_events[ns + '/' + name] = after
    REPORT.update(status='PASS', completedAt=now().isoformat(), pinnedRecordsPreserved=len(pins), mutationEvidence=deletion_events)
    persist()
    save('verification.json', {'status': 'passed', 'namespacesRemoved': list(NS), 'retainedUntilAPIDeadlines': True, 'pinnedRecordsPreserved': len(pins)})
    print('PASS: both namespaces removed, full API-timed retention preserved, control publication unchanged', flush=True)


try:
    main()
except BaseException as error:
    REPORT.update(status='FAIL', failure=type(error).__name__ + ': ' + str(error))
    persist()
    raise
