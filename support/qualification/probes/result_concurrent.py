"""Replay a committed result through both installed receivers concurrently.

The privileged lab harness borrows the original Pod's credential; no runner
permission changes. Requests overlap at a body barrier, after TLS and headers.
This proves concurrent redelivery, not a race to publish an absent intent.
"""
import base64
import concurrent.futures
import contextlib
import datetime as dt
import hashlib
import http.client
import json
import os
import pathlib
import socket
import ssl
import subprocess
import tempfile
import threading
import time


def digest(data):
    return 'sha256:' + hashlib.sha256(data).hexdigest()


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
    require(value['evidenceVersion'] == 1, 'Unknown evidence version')
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


class Client:
    """Bounded TLS connections through owned per-Pod port forwards."""
    def __init__(self, environment, managers, host, credential):
        self.environment, self.managers, self.host = environment, managers, host
        self.stack = contextlib.ExitStack()
        self.ports = []
        try:
            directory = pathlib.Path(self.stack.enter_context(tempfile.TemporaryDirectory(prefix='ptah-result-client-')))
            for key in ('tls.crt', 'tls.key', 'ca.crt'):
                path = directory / key
                with open(path, 'xb', opener=lambda p, flags: os.open(p, flags, 0o600)) as f:
                    f.write(base64.b64decode(credential[key], validate=True))
            self.tls = ssl.create_default_context(cafile=str(directory / 'ca.crt'))
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

    def request(self, index, payload, name, barrier):
        connection = socket.create_connection(('127.0.0.1', self.ports[index]), timeout=10)
        with connection, self.tls.wrap_socket(connection, server_hostname=self.host) as tls:
            headers = ('PUT /v1/results/' + name + ' HTTP/1.1\r\nHost: ' + self.host
                + '\r\nContent-Type: application/vnd.ptah.result.v1+json\r\nContent-Length: '
                + str(len(payload)) + '\r\nX-Ptah-Result-Digest: ' + digest(payload)
                + '\r\nConnection: close\r\n\r\n')
            tls.sendall(headers.encode('ascii'))
            row = {'receiverUID': self.managers[index]['metadata']['uid'],
                   'digest': digest(payload), 'headersSentNs': time.monotonic_ns()}
            barrier.wait(timeout=10)
            tls.sendall(payload)
            row['bodySentNs'] = time.monotonic_ns()
            response = http.client.HTTPResponse(tls)
            response.begin()
            body = response.read(4097)
            if len(body) > 4096:
                raise ValueError('Oversized receiver response')
            row.update(responseReadNs=time.monotonic_ns(), status=response.status)
            if response.status == 200:
                row['receipt'] = json.loads(body)
            return row

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
        return {'evidenceVersion': 1, 'receipt': receipt, 'originalDigest': digest(payload),
                'changedDigest': digest(changed), 'receivers': [m['metadata']['uid'] for m in self.managers],
                'rounds': rounds, 'observedAt': dt.datetime.now(dt.timezone.utc).isoformat(),
                'procedureSHA256': hashlib.sha256(pathlib.Path(__file__).read_bytes()).hexdigest()}
