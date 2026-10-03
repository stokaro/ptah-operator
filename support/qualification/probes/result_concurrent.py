"""Replay a committed result through both installed receivers concurrently.

The privileged lab harness borrows the original Pod's credential; no runner
permission changes. Pod tokens stay in memory; historical certificate mode
uses temporary key files. Requests overlap at a body barrier, after TLS and headers.
This proves concurrent redelivery, not a race to publish an absent intent.
"""
import base64
import concurrent.futures
import contextlib
import copy
import datetime as dt
import hashlib
import http.client
import json
import os
import pathlib
import re
import socket
import ssl
import subprocess
import tempfile
import threading
import time


def digest(data):
    return 'sha256:' + hashlib.sha256(data).hexdigest()


def attempt_name(identity):
    b = identity['binding']
    key = [b['namespace'], b['uid'], b['operationID'], b['jobName']]
    return 'ptah-result-' + hashlib.sha256(json.dumps(key, separators=(',', ':')).encode()).hexdigest()


def authority_cases(identity):
    """The changed-authentication boundaries already required by #586."""
    b = identity['binding']
    changes = [('foreign-pod-token', {}), ('namespace', {'namespace': 'foreign-namespace'}),
               ('resource', {'name': 'foreign-resource', 'uid': 'foreign-resource-uid'}),
               ('generation', {'generation': b['generation'] + 1}),
               ('epoch', {'executionBindingID': 'v1-' + '0' * 32}),
               ('operation-attempt', {'operationID': 'sha256:' + '0' * 64}),
               ('job', {'jobUID': 'replacement-job-uid'}), ('pod', {'podUID': 'replacement-pod-uid'})]
    cases = []
    for label, change in changes:
        other = copy.deepcopy(identity)
        other['binding'].update(change)
        if label != 'foreign-pod-token' and other == identity:
            raise ValueError('Authority mutation did not change its claim')
        cases.append((label, other))
    return cases


def pod_token_headers(token, identity):
    """Use the original projected token in memory, never in saved evidence."""
    if not isinstance(token, str) or not re.fullmatch(r'[A-Za-z0-9_.-]{1,8192}', token):
        raise ValueError('Invalid original Pod token')
    if not isinstance(identity, bytes) or len(identity) > 3072:
        raise ValueError('Invalid public Pod identity')
    value = json.loads(identity)
    if not all(value.get('binding', {}).get(key) for key in ('namespace', 'jobUID', 'podUID')):
        raise ValueError('Missing public Pod binding')
    encoded = base64.urlsafe_b64encode(identity).rstrip(b'=').decode('ascii')
    return '\r\nAuthorization: Bearer ' + token + '\r\nX-Ptah-Result-Identity: ' + encoded


def changed_payload(payload):
    before = b'"description":"Record Delivery"'
    if payload.count(before) != 1:
        raise ValueError('Expected exactly one native migration description')
    # Preserve Go JSON field order and canonical encoding. A 409 from the
    # receiver additionally proves this passed Decode, which refuses with 422.
    return payload.replace(before, b'"description":"Changed Delivery"')


def verify_evidence(value):
    def require(condition, message):
        if not condition:
            raise ValueError(message)
    version = value['evidenceVersion']
    require(version in (1, 2), 'Unknown evidence version')
    if version == 2:
        require(value.get('authentication') == 'pod-token', 'Missing Pod-token authentication')
        require(re.fullmatch(r'sha256:[0-9a-f]{64}', value.get('identityDigest', '')),
                'Missing public identity digest')
        require(all(value.get('binding', {}).get(key) for key in ('namespace', 'jobUID', 'podUID')),
                'Missing original Pod binding')
    receipt = value['receipt']
    require(bool(receipt['UID']) and receipt['Size'] > 0, 'Missing original receipt')
    require(value['originalDigest'] == receipt['Digest']
            and value['changedDigest'] != value['originalDigest'], 'No changed payload')
    receivers = value['receivers']
    require(len(receivers) == 2 and len(set(receivers)) == 2 and all(receivers),
            'Expected two distinct receivers')
    rounds = value['rounds']
    require([r['name'] for r in rounds] == ['identical', 'conflicting', 'mixed'], 'Missing delivery round')
    for round_, expected in zip(rounds, [[200, 200], [409, 409], [200, 409]]):
        rows = round_['requests']
        require(len(rows) == 2 and [r['receiverUID'] for r in rows] == receivers,
                'Missing receiver response')
        require(max(r['headersSentNs'] for r in rows) < min(r['bodySentNs'] for r in rows)
                and max(r['bodySentNs'] for r in rows) < min(r['responseReadNs'] for r in rows),
                'Requests did not overlap')
        for row, status in zip(rows, expected):
            require(row['status'] == status, f"{round_['name']} {row['receiverUID']}: expected {status}, received {row['status']}")
            require(row['digest'] == (value['originalDigest'] if status == 200 else value['changedDigest']),
                    'Wrong request bytes')
            if status == 200:
                require(row['receipt'] == receipt, 'Changed durable receipt')
    require(value['publicationUnchanged'] is True, 'Publication changed')
    return {'requests': 6, 'identicalReceipts': 3, 'conflicts': 3}


def verify_authority_evidence(value):
    def require(condition, message):
        if not condition:
            raise ValueError(message)
    require(value['evidenceVersion'] == 1 and value['authentication'] == 'pod-token', 'Missing token authority evidence')
    identity, receivers, receipt = value['identity'], value['receivers'], value['receipt']
    foreign = value['foreignPod']
    require(foreign['uid'] and foreign['uid'] != identity['binding']['podUID']
            and foreign['namespace'] == identity['binding']['namespace']
            and foreign['tokenReviewAuthenticated'] is True
            and foreign['audiences'] == ['operator.ptah.run/results'], 'Missing authenticated foreign-Pod control')
    require(len(receivers) == 2 and len(set(receivers)) == 2 and all(receivers), 'Missing distinct receivers')
    require(bool(receipt['UID']) and receipt['Name'] == attempt_name(identity) + '-complete', 'Missing original receipt')
    expected = authority_cases(identity)
    require([c['name'] for c in value['cases']] == [label for label, _ in expected], 'Missing authority refusal case')
    for case, (label, claim) in zip(value['cases'], expected):
        require(case['identity'] == claim, 'Wrong authority mutation')
        require([(r['receiverUID'], r['method']) for r in case['requests']]
                == [(uid, method) for uid in receivers for method in ('HEAD', 'PUT')], 'Missing receiver or submission method')
        for row in case['requests']:
            require(row['status'] == 403 and row['path'] == '/v1/results/' + attempt_name(claim), 'Expected authority refusal on the claimed route')
            require(row['identityDigest'] == digest(json.dumps(claim, separators=(',', ':')).encode()), 'Wrong transmitted claim')
            require(row['credential'] == ('foreign-pod-token' if label == 'foreign-pod-token' else 'original-pod-token'), 'Wrong credential control')
            require(row['bodyWithheld'] == (row['method'] == 'PUT'), 'Authority must be refused before body upload')
    for stage in ('before', 'after'):
        rows = value[stage]
        require([(r['receiverUID'], r['method']) for r in rows]
                == [(uid, method) for uid in receivers for method in ('HEAD', 'PUT')], 'Missing positive control')
        for row in rows:
            require(row['status'] == (204 if row['method'] == 'HEAD' else 200), 'Original authority stopped working')
            require(row['path'] == '/v1/results/' + attempt_name(identity)
                    and row['identityDigest'] == digest(json.dumps(identity, separators=(',', ':')).encode())
                    and row['credential'] == 'original-pod-token', 'Wrong positive-control authority')
            if row['method'] == 'PUT':
                require(row['receipt'] == receipt, 'Positive control changed the receipt')
    require(value['publicationUnchanged'] is True, 'Refusal changed the publication')
    return {'refusedRequests': len(expected) * 4, 'positiveControls': 8}


class Client:
    """Bounded TLS connections through owned per-Pod port forwards."""
    def __init__(self, environment, managers, host, credential, *, pod_token=None, identity=None):
        self.environment, self.managers, self.host = environment, managers, host
        self.stack = contextlib.ExitStack()
        self.ports = []
        self.token_headers = ''
        self.identity = None
        try:
            directory = pathlib.Path(self.stack.enter_context(tempfile.TemporaryDirectory(prefix='ptah-result-client-')))
            if pod_token is not None:
                self.token_headers = pod_token_headers(pod_token, identity)
                self.identity = identity
            elif identity is not None:
                raise ValueError('Public identity requires the original Pod token')
            for key in (('ca.crt',) if self.identity is not None else ('tls.crt', 'tls.key', 'ca.crt')):
                path = directory / key
                with open(path, 'xb', opener=lambda p, flags: os.open(p, flags, 0o600)) as f:
                    f.write(base64.b64decode(credential[key], validate=True))
            self.tls = ssl.create_default_context(cafile=str(directory / 'ca.crt'))
            self.tls.minimum_version = ssl.TLSVersion.TLSv1_3
            if self.identity is None:
                self.tls.load_cert_chain(str(directory / 'tls.crt'), str(directory / 'tls.key'))
            for manager in managers:
                with socket.socket() as sock:
                    sock.bind(('127.0.0.1', 0))
                    port = sock.getsockname()[1]
                process = subprocess.Popen(['kubectl', '--kubeconfig', environment['E2E_KUBECONFIG'],
                    '-n', environment['E2E_OPERATOR_NAMESPACE'], 'port-forward',
                    'pod/' + manager['metadata']['name'], str(port) + ':9444'],
                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                self.stack.callback(self.stop, process)
                end = time.monotonic() + 15
                while True:
                    try:
                        with socket.create_connection(('127.0.0.1', port), timeout=1):
                            break
                    except OSError:
                        if process.poll() is not None or time.monotonic() >= end:
                            raise RuntimeError('Receiver port forward did not start')
                        time.sleep(0.1)
                self.ports.append(port)
        except BaseException:
            self.close()
            raise

    @staticmethod
    def stop(process):
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()

    def close(self):
        self.stack.close()
        self.token_headers = ''

    def request(self, index, payload, name, barrier=None, *, identity=None, token_override=None, withhold_body=False, method='PUT'):
        auth = self.token_headers
        if identity is not None:
            token = (token_override if token_override is not None else
                     self.token_headers.split('\r\n')[1].removeprefix('Authorization: Bearer '))
            auth = pod_token_headers(token, identity)
        body = b'' if method == 'HEAD' else payload
        connection = socket.create_connection(('127.0.0.1', self.ports[index]), timeout=10)
        with connection, self.tls.wrap_socket(connection, server_hostname=self.host) as tls:
            headers = (method + ' /v1/results/' + name + ' HTTP/1.1\r\nHost: ' + self.host
                + '\r\nContent-Type: application/vnd.ptah.result.v1+json\r\nContent-Length: '
                + str(len(body)) + '\r\nX-Ptah-Result-Digest: ' + digest(payload)
                + auth
                + '\r\nConnection: close\r\n\r\n')
            tls.sendall(headers.encode('ascii'))
            row = {'receiverUID': self.managers[index]['metadata']['uid'],
                   'digest': digest(payload), 'headersSentNs': time.monotonic_ns()}
            if barrier is not None:
                barrier.wait(timeout=10)
            if not withhold_body:
                tls.sendall(body)
            row['bodySentNs'] = None if withhold_body else time.monotonic_ns()
            response = http.client.HTTPResponse(tls, method=method)
            response.begin()
            body = response.read(4097)
            if len(body) > 4096:
                raise ValueError('Oversized receiver response')
            row.update(responseReadNs=time.monotonic_ns(), status=response.status)
            if response.status == 200:
                row['receipt'] = json.loads(body)
            if identity is not None:
                row.update(method=method, path='/v1/results/' + name, identityDigest=digest(identity),
                           credential='foreign-pod-token' if token_override is not None else 'original-pod-token',
                           bodyWithheld=withhold_body)
            tls.unwrap().close()
            return row

    def run_authority(self, payload, name, receipt, foreign_token, foreign_pod):
        identity = json.loads(self.identity)
        if attempt_name(identity) != name:
            raise ValueError('Claimed-route calculation differs from the real publication')

        def requests(claim, label=None):
            raw = json.dumps(claim, separators=(',', ':')).encode()
            rows = []
            for index in range(2):
                for method in ('HEAD', 'PUT'):
                    row = self.request(index, payload, attempt_name(claim), identity=raw,
                                       token_override=foreign_token if label == 'foreign-pod-token' else None,
                                       withhold_body=label is not None and method == 'PUT', method=method)
                    rows.append(row)
                    print('Authority request:', label or 'original-control', index, method, row['status'], flush=True)
            return rows

        before = requests(identity)
        cases = [{'name': label, 'identity': claim, 'requests': requests(claim, label)}
                 for label, claim in authority_cases(identity)]
        return {'evidenceVersion': 1, 'authentication': 'pod-token', 'identity': identity,
                'receivers': [m['metadata']['uid'] for m in self.managers], 'receipt': receipt, 'foreignPod': foreign_pod,
                'before': before, 'cases': cases, 'after': requests(identity),
                'observedAt': dt.datetime.now(dt.timezone.utc).isoformat(),
                'procedureSHA256': hashlib.sha256(pathlib.Path(__file__).read_bytes()).hexdigest()}

    def run(self, payload, name, receipt):
        changed = changed_payload(payload)
        rounds = []
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as executor:
            for label, bodies in [('identical', [payload, payload]),
                                  ('conflicting', [changed, changed]),
                                  ('mixed', [payload, changed])]:
                barrier = threading.Barrier(2)
                futures = [executor.submit(self.request, i, body, name, barrier)
                           for i, body in enumerate(bodies)]
                rounds.append({'name': label, 'requests': [f.result(timeout=25) for f in futures]})
                if label == 'identical' and receipt is None:
                    receipt = rounds[-1]['requests'][0].get('receipt', {})
        result = {'evidenceVersion': 1, 'receipt': receipt, 'originalDigest': digest(payload),
                'changedDigest': digest(changed), 'receivers': [m['metadata']['uid'] for m in self.managers],
                'rounds': rounds, 'observedAt': dt.datetime.now(dt.timezone.utc).isoformat(),
                'procedureSHA256': hashlib.sha256(pathlib.Path(__file__).read_bytes()).hexdigest()}
        if self.identity is not None:
            result.update(evidenceVersion=2, authentication='pod-token', identityDigest=digest(self.identity),
                          binding=json.loads(self.identity)['binding'])
        return result
