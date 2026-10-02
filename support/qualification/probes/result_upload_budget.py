"""Bound stalled installed uploads while another native migration progresses.

The held Apply Pod has not started SQL. Its credential authorizes the fault
requests, while its real controller must keep renewing the database Lease.
Only the receiver on the current leader is occupied during the peer migration;
both replicas are then occupied to measure refusal and independent recovery.
"""
import base64
import concurrent.futures
import copy
import datetime as dt
import hashlib
import http.client
import json
import pathlib
import socket
import subprocess
import threading
import time
import urllib.parse

from result_concurrent import Client
from result_first_harvest import publication

SECOND = 1_000_000_000
# Case budgets, fixed before execution. These are not general capacity limits.
UPLOAD_SECONDS = 120
RESPONSE_SECONDS = 5
RENEWAL_SECONDS = 15


def verify_evidence(value):
    def require(condition, message):
        if not condition:
            raise ValueError(message)
    require(value['evidenceVersion'] == 1 and value['engine'] in ('PostgreSQL', 'MySQL'), 'Unknown case')
    require(value['budgets'] == {'uploadSeconds': 120, 'responseSeconds': 5, 'renewalSeconds': 15}, 'Changed budgets')
    slow = value['slowUploads']
    require(len(slow) == 2 and len({r['receiverUID'] for r in slow}) == 2, 'Missing receiver')
    require(slow[0]['receiverUID'] == value['leaderUID'], 'Leader receiver was not occupied')
    for row in slow:
        elapsed = (row['responseNs'] - row['sentNs']) / SECOND
        require(row['status'] == 408 and 119 <= elapsed <= 125, 'Upload did not reach its two-minute bound')
        require(row['declaredBytes'] == 4096 and row['sentBytes'] == 1, 'No incomplete body')
    require(slow[0]['sentNs'] < value['peerCreatedNs'] < value['peerConvergedNs'] < slow[1]['sentNs'] < slow[0]['responseNs'] < slow[1]['responseNs'], 'No peer progress or overlapping saturation')
    checks = value['checks']
    expected = [('initial', 0, 204), ('initial', 1, 204), ('leader-busy', 0, 503),
                ('follower-free', 1, 204), ('both-busy', 0, 503), ('both-busy', 1, 503),
                ('leader-recovered', 0, 204), ('follower-still-busy', 1, 503),
                ('recovered', 0, 204), ('recovered', 1, 204)]
    require([(r['phase'], r['index'], r['status']) for r in checks] == expected, 'Missing admission or recovery check')
    for row in checks:
        require(row['receiverUID'] == slow[row['index']]['receiverUID'] and
                0 < row['responseNs'] - row['sentNs'] <= RESPONSE_SECONDS * SECOND, 'Unbounded or wrong receiver response')
        if row['status'] == 503:
            require(row['retryAfter'] == '1', 'No bounded retry advice')
        if row['phase'] in ('leader-busy', 'both-busy', 'follower-still-busy'):
            active = slow[row['index']]
            require(active['sentNs'] < row['sentNs'] < row['responseNs'] < active['responseNs'], 'Refusal outside occupied slot')
        if row['phase'] in ('leader-recovered', 'recovered'):
            require(row['sentNs'] > slow[row['index']]['responseNs'], 'Recovery preceded timeout')
    rows = value['leaseSamples']
    require(len(rows) >= 20 and rows[0]['observedNs'] < slow[0]['sentNs'] and rows[-1]['observedNs'] > slow[1]['responseNs'], 'Lease observation does not cover fault')
    key = ('uid', 'holder', 'epoch', 'duration')
    require(all(rows[0][k] for k in key), 'Empty Lease identity')
    require(all(all(r[k] == rows[0][k] for k in key) for r in rows), 'Lease identity changed')
    require(rows[0]['epoch'] == value['leaseEpoch'], 'Lease belongs to another operation')
    renewals = []
    for row in rows:
        instant = dt.datetime.fromisoformat(row['renewTime'].replace('Z', '+00:00'))
        if not renewals or instant != renewals[-1]:
            renewals.append(instant)
    require(len(renewals) >= 15, 'No continuous Lease renewal')
    require(all(0 < (b - a).total_seconds() <= RENEWAL_SECONDS for a, b in zip(renewals, renewals[1:])), 'Lease renewal exceeded case budget')
    # Observation cadence also bounds a stalled tail after the final renewal.
    require(all(0 < b['observedNs'] - a['observedNs'] <= 3 * SECOND for a, b in zip(rows, rows[1:])), 'Lease observation gap')
    last_change = rows[0]['observedNs']
    for previous, row in zip(rows, rows[1:]):
        if previous['renewTime'] != row['renewTime']:
            last_change = row['observedNs']
        require(row['observedNs'] - last_change <= RENEWAL_SECONDS * SECOND, 'Lease stopped renewing')
    require(value['leaderUnchanged'] is True and value['publicationsBeforeRelease'] == 0, 'Leader changed or partial result published')
    require(value['serviceRestored'] is True, 'Service route not restored')
    require(value['calibration']['afterRollback'] == '0:1:true' and value['calibration']['afterReset'] == '0:1:false', 'Ineffective original SQL witness')
    require(value['peerCalibration']['afterRollback'] == '0:1:true' and value['peerCalibration']['afterReset'] == '0:1:false', 'Ineffective peer SQL witness')
    executions = value['executions']
    require(set(executions) == {'original', 'peer'}, 'Missing independent execution')
    require(executions['original']['resourceUID'] != executions['peer']['resourceUID'], 'Peer is original resource')
    for row in executions.values():
        require(row['databaseWitness'] == '1:1:true' and row['applyJobs'] == row['executionPods'] == 1
                and row['podRestarts'] == 0 and row['podPhase'] == 'Succeeded' and row['runnerAPICredentials'] is False,
                'SQL replay, replacement execution, or runner API credentials')
        require(row['jobUID'] and row['podUID'] and row['receiptUID'], 'Missing execution identity')
        require(any(c['type'] == 'Ready' and c['status'] == 'True' and c.get('reason') == 'HistoryMatched'
                    and c.get('observedGeneration') == row['generation'] for c in row['conditions']), 'No current convergence')
    return {'timedOutUploads': 2, 'renewals': len(renewals), 'independentSQLExecutions': 2}


def run(*, k, get, create, wait, save, witness, records, open_gate, resource, pod,
        operation, engine, environment, calibration, managers, host, credential,
        service, restore_service, endpoints_are, gate, sql, creds, database, migration_sql):
    ns, opns = resource['metadata']['namespace'], environment['E2E_OPERATOR_NAMESPACE']
    leader = get('lease', 'ptah-operator.operator.ptah.run', opns)
    holder = leader['spec']['holderIdentity']
    managers = sorted(managers, key=lambda p: not holder.startswith(p['metadata']['name'] + '_'))
    assert holder.startswith(managers[0]['metadata']['name'] + '_')
    target = [r for r in get('leases', namespace=opns)['items']
              if r['metadata'].get('annotations', {}).get('operator.ptah.run/lease-epoch') == operation['leaseEpoch']]
    assert len(target) == 1
    lease_name = target[0]['metadata']['name']
    attempt = 'ptah-result-' + hashlib.sha256(json.dumps([ns, resource['metadata']['uid'], operation['id'], operation['jobName']], separators=(',', ':')).encode()).hexdigest()
    peer_database = database + '_peer'
    if engine == 'MySQL':
        sql('CREATE DATABASE `' + peer_database + '`; GRANT ALL ON `' + peer_database + '`.* TO \'demo\'@\'%\';', 'mysql')
        sql('CREATE TABLE delivery_probe_calls(n BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY) ENGINE=InnoDB;', peer_database)
    else:
        owner = '"' + creds['username'].replace('"', '""') + '"'
        sql('CREATE DATABASE ' + peer_database + ' OWNER ' + owner + ';', urllib.parse.urlsplit(creds['url']).path[1:])
        sql('SET ROLE ' + owner + '; CREATE SEQUENCE delivery_probe_sequence; CREATE TABLE delivery_probe_calls(n bigint NOT NULL);', peer_database)
    sql('BEGIN; ' + migration_sql + ' ROLLBACK;', peer_database)
    peer_calibration = {'afterRollback': witness(peer_database)}
    sql('TRUNCATE delivery_probe_calls;' if engine == 'MySQL' else 'ALTER SEQUENCE delivery_probe_sequence RESTART WITH 1;', peer_database)
    peer_calibration['afterReset'] = witness(peer_database)
    assert peer_calibration == {'afterRollback': '0:1:true', 'afterReset': '0:1:false'}
    url = urllib.parse.urlunsplit(urllib.parse.urlsplit(creds['url'])._replace(path='/' + peer_database))
    create({'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'peer-database', 'namespace': ns}, 'stringData': {'url': url}})
    peer_manifest = {'apiVersion': resource['apiVersion'], 'kind': resource['kind'],
                     'metadata': {'name': 'peer', 'namespace': ns}, 'spec': copy.deepcopy(resource['spec'])}
    peer_manifest['spec']['target']['urlFrom']['name'] = 'peer-database'
    peer_manifest['spec']['target']['coordinationKey'] += '/peer'
    # Keep only the original Pod gated. Confirm both sides at the real API.
    expression = "!has(object.metadata.annotations) || !('operator.ptah.run/result-pod-name' in object.metadata.annotations) || object.metadata.annotations['operator.ptah.run/result-pod-name'] != " + json.dumps(pod['metadata']['name'])
    k('patch', 'validatingadmissionpolicy', gate, '--type=json', '-p', json.dumps([{'op': 'replace', 'path': '/spec/validations/0/expression', 'value': expression}]))
    def gate_ready():
        results = []
        for name in [pod['metadata']['name'], 'ptah-m-apply-peer-probe']:
            probe = {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'gate-check', 'namespace': ns, 'annotations': {'operator.ptah.run/result-pod-name': name}}}
            r = subprocess.run(['kubectl', '--kubeconfig', environment['E2E_KUBECONFIG'], 'create', '--dry-run=server', '-f', '-'], input=json.dumps(probe), text=True, capture_output=True, timeout=30)
            results.append((r.returncode, r.stderr))
        return results[0][0] != 0 and 'Acceptance probe holds' in results[0][1] and results[1][0] == 0
    wait(gate_ready, 30)
    samples, errors, checks = [], [], []
    stop = threading.Event()
    def sample():
        try:
            while not stop.is_set():
                lease = get('lease', lease_name, opns)
                samples.append({'observedNs': time.monotonic_ns(), 'uid': lease['metadata']['uid'],
                    'resourceVersion': lease['metadata']['resourceVersion'], 'holder': lease['spec']['holderIdentity'],
                    'epoch': lease['metadata']['annotations']['operator.ptah.run/lease-epoch'],
                    'renewTime': lease['spec']['renewTime'], 'duration': lease['spec']['leaseDurationSeconds']})
                stop.wait(0.5)
        except BaseException as e:
            errors.append(str(e))
    thread = threading.Thread(target=sample)
    client = Client(environment, managers, host, credential)
    sockets = []
    label = 'qualification.ptah.run/receiver'
    follower = managers[1]
    assert label not in follower['metadata'].get('labels', {})
    routed = labeled = False
    pool = concurrent.futures.ThreadPoolExecutor(max_workers=2)
    slow = []
    def head(phase, index):
        start = time.monotonic_ns()
        connection = socket.create_connection(('127.0.0.1', client.ports[index]), timeout=5)
        with connection, client.tls.wrap_socket(connection, server_hostname=host) as tls:
            tls.sendall(('HEAD /v1/results/' + attempt + ' HTTP/1.1\r\nHost: ' + host + '\r\nConnection: close\r\n\r\n').encode())
            response = http.client.HTTPResponse(tls, method='HEAD')
            response.begin()
            row = {'phase': phase, 'index': index, 'receiverUID': managers[index]['metadata']['uid'],
                   'sentNs': start, 'responseNs': time.monotonic_ns(), 'status': response.status,
                   'retryAfter': response.getheader('Retry-After')}
            checks.append(row)
            return row['status']
    def begin_slow(index):
        raw = socket.create_connection(('127.0.0.1', client.ports[index]), timeout=10)
        try:
            tls = client.tls.wrap_socket(raw, server_hostname=host)
        except BaseException:
            raw.close()
            raise
        sockets.append(tls)
        tls.settimeout(130)
        headers = ('PUT /v1/results/' + attempt + ' HTTP/1.1\r\nHost: ' + host +
                   '\r\nContent-Type: application/vnd.ptah.result.v1+json\r\nContent-Length: 4096\r\nX-Ptah-Result-Digest: sha256:' + '0' * 64 + '\r\nConnection: close\r\n\r\n{')
        tls.sendall(headers.encode())
        row = {'receiverUID': managers[index]['metadata']['uid'], 'sentNs': time.monotonic_ns(), 'declaredBytes': 4096, 'sentBytes': 1}
        slow.append(row)
        def finish():
            response = http.client.HTTPResponse(tls)
            response.begin()
            assert len(response.read(4097)) <= 4096
            row.update(responseNs=time.monotonic_ns(), status=response.status)
            tls.close()
            return row
        return pool.submit(finish)
    def converged(name):
        r = get('ptahmigration', name)
        return r if not r.get('status', {}).get('activeOperation') and any(
            c['type'] == 'Ready' and c['status'] == 'True' and c.get('reason') == 'HistoryMatched'
            and c.get('observedGeneration') == r['metadata']['generation'] for c in r.get('status', {}).get('conditions', [])) else None
    try:
        assert head('initial', 0) == head('initial', 1) == 204
        k('patch', 'pod', follower['metadata']['name'], '--type=json', '-p', json.dumps([
            {'op': 'test', 'path': '/metadata/uid', 'value': follower['metadata']['uid']},
            {'op': 'add', 'path': '/metadata/labels/qualification.ptah.run~1receiver', 'value': ns}]), namespace=opns)
        labeled = True
        routed = True
        k('patch', 'service', service['metadata']['name'], '--type=json', '-p', json.dumps([
            {'op': 'test', 'path': '/metadata/uid', 'value': service['metadata']['uid']},
            {'op': 'replace', 'path': '/spec/selector', 'value': {label: ns}}]), namespace=opns)
        wait(lambda: endpoints_are({follower['metadata']['uid']}), 30)
        thread.start()
        wait(lambda: bool(samples), 10)
        first = begin_slow(0)
        time.sleep(0.5)
        assert head('leader-busy', 0) == 503 and head('follower-free', 1) == 204
        peer_created = time.monotonic_ns()
        peer = create(peer_manifest)
        wait(lambda: converged('peer'), 100)
        peer_converged = time.monotonic_ns()
        assert witness(peer_database) == '1:1:true'
        print('Independent native migration converged while the leader receiver was stalled', flush=True)
        second = begin_slow(1)
        time.sleep(0.5)
        assert head('both-busy', 0) == head('both-busy', 1) == 503
        first.result(timeout=125)
        assert head('leader-recovered', 0) == 204 and head('follower-still-busy', 1) == 503
        second.result(timeout=125)
        assert head('recovered', 0) == head('recovered', 1) == 204
        time.sleep(1)
        stop.set()
        thread.join(timeout=35)
        assert not thread.is_alive() and not errors
        current_leader = get('lease', 'ptah-operator.operator.ptah.run', opns)
        assert current_leader['metadata']['uid'] == leader['metadata']['uid'] and current_leader['spec']['holderIdentity'] == holder
        assert not any(r['metadata']['name'] == attempt for r in records())
        assert witness() == '0:1:false'
        restore_service()
        routed = False
        wait(lambda: endpoints_are({m['metadata']['uid'] for m in managers}), 30)
        open_gate()
        wait(lambda: converged(resource['metadata']['name']), 300)
        executions = {}
        for key, obj, db in [('original', resource, database), ('peer', peer, peer_database)]:
            final = converged(obj['metadata']['name'])
            assert final and final['metadata']['uid'] == obj['metadata']['uid']
            jobs = [j for j in get('jobs')['items'] if j['metadata']['name'].startswith('ptah-m-apply-')
                    and any(o['uid'] == obj['metadata']['uid'] for o in j['metadata'].get('ownerReferences', []))]
            assert len(jobs) == 1
            job = jobs[0]
            pods = [p for p in get('pods')['items'] if any(o['uid'] == job['metadata']['uid'] for o in p['metadata'].get('ownerReferences', []))]
            assert len(pods) == 1
            p = pods[0]
            if key == 'original':
                assert job['metadata']['uid'] == operation['jobUID'] and p['metadata']['uid'] == pod['metadata']['uid']
            rs = {r['metadata']['name']: r for r in records() if r['spec']['type'] in ('intent', 'chunk', 'complete')}
            intent, complete, payload = publication(rs, job['metadata']['uid'])
            selected = {name: r for name, r in rs.items() if name == intent['metadata']['name'] or any(o['uid'] == intent['metadata']['uid'] for o in r['metadata'].get('ownerReferences', []))}
            save(key + '-publication.json', selected)
            executions[key] = {'resourceUID': obj['metadata']['uid'], 'generation': final['metadata']['generation'],
                'jobUID': job['metadata']['uid'], 'podUID': p['metadata']['uid'], 'receiptUID': complete['metadata']['uid'],
                'databaseWitness': witness(db), 'applyJobs': len(jobs), 'executionPods': len(pods),
                'podPhase': p['status']['phase'], 'podRestarts': sum(c['restartCount'] for c in p['status']['containerStatuses']),
                'runnerAPICredentials': p['spec'].get('automountServiceAccountToken', True) or any('serviceAccountToken' in s for v in p['spec'].get('volumes', []) for s in v.get('projected', {}).get('sources', [])),
                'conditions': final['status']['conditions']}
        result = {'evidenceVersion': 1, 'engine': engine, 'commit': environment['E2E_CONTROLLER_REVISION'],
            'namespace': ns, 'leaderUID': managers[0]['metadata']['uid'], 'leaderUnchanged': True,
            'budgets': {'uploadSeconds': UPLOAD_SECONDS, 'responseSeconds': RESPONSE_SECONDS, 'renewalSeconds': RENEWAL_SECONDS},
            'slowUploads': slow, 'checks': checks, 'leaseSamples': samples, 'leaseEpoch': operation['leaseEpoch'],
            'peerCreatedNs': peer_created, 'peerConvergedNs': peer_converged, 'publicationsBeforeRelease': 0,
            'serviceRestored': True, 'calibration': calibration, 'peerCalibration': peer_calibration, 'executions': executions,
            'procedureSHA256': {name: hashlib.sha256(pathlib.Path(__file__).with_name(name).read_bytes()).hexdigest()
                                for name in ['result_lost_ack.py', 'result_upload_budget.py', 'result_concurrent.py']},
            'completedAt': dt.datetime.now(dt.timezone.utc).isoformat()}
        save('upload-budget.json', result)
        verify_evidence(result)
        print('PASS: two bounded stalled uploads, continuous Apply Lease renewal, independent native progress', flush=True)
    finally:
        stop.set()
        if thread.ident:
            thread.join(timeout=35)
        for tls in sockets:
            try:
                tls.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            tls.close()
        pool.shutdown(wait=True, cancel_futures=True)
        client.close()
        if routed:
            restore_service()
        if labeled:
            k('patch', 'pod', follower['metadata']['name'], '--type=json', '-p', json.dumps([
                {'op': 'test', 'path': '/metadata/uid', 'value': follower['metadata']['uid']},
                {'op': 'remove', 'path': '/metadata/labels/qualification.ptah.run~1receiver'}]), namespace=opns)
