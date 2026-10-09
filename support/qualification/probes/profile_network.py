#!/usr/bin/env python3
"""Apply the documented egress policies and prove what they allow and deny.

A NetworkPolicy that nothing enforces proves the YAML parses. This installs
examples/networkpolicy-egress.yaml in each capacity workload namespace, bound to
the lab's real registry, database and manager, adds egress policies for the
release and database namespaces, and then runs short-lived probe Pods. Each
probe carries an operation's labels and reports which of DNS, the database,
the registry, the API server and the result receiver it can reach. Every
expectation is written down before the probe runs, the allowed controls run
before the policies exist, and a single difference fails the run.

The probes are deleted with UID preconditions; the policies stay, because the
measurement that follows runs under them.
"""

import argparse
import concurrent.futures
import copy
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(Path(__file__).resolve().parent))
from profile_lab import Lab, database_service  # noqa: E402

TARGETS = ('dns', 'database', 'registry', 'api', 'receiver')

SCRIPT = '''sleep 5
reach() {
 if timeout 8 nc -z -w 4 "$1" "$2" >/dev/null 2>&1; then echo "$3=open"; else echo "$3=closed"; fi
}
if timeout 8 nslookup kubernetes.default.svc.cluster.local >/dev/null 2>&1; then echo dns=open; else echo dns=closed; fi
reach "$DATABASE" "$DATABASE_PORT" database
reach "$REGISTRY" 5443 registry
reach "$API" 6443 api
reach "$RECEIVER" 9444 receiver
'''


def task_expectations(family, operation):
    """What an operation Pod may reach under the documented policies."""
    expected = {target: 'open' for target in TARGETS}
    expected['api'] = 'closed'
    expected['database'] = 'open' if operation in ('observe', 'plan', 'history', 'apply') else 'closed'
    expected['registry'] = 'closed' if family == 'schema' and operation == 'apply' else 'open'
    return expected


def bind_policy(policy, source, registry_selector, database_namespace, database_selector, database_port, operator_namespace, manager_selector):
    """One documented policy, pointed at this lab's real peers."""
    bound = copy.deepcopy(policy)
    name = bound['metadata']['name']
    rule = bound['spec']['egress'][0] if bound['spec'].get('egress') else None
    if name.endswith('-registry'):
        rule.update(to=[{'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': source}},
                         'podSelector': {'matchLabels': registry_selector}}], ports=[{'protocol': 'TCP', 'port': 5443}])
    elif name.endswith('-database'):
        rule.update(to=[{'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': database_namespace}},
                         'podSelector': {'matchLabels': database_selector}}], ports=[{'protocol': 'TCP', 'port': database_port}])
    elif name.endswith('-results'):
        rule['to'] = [{'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': operator_namespace}},
                       'podSelector': {'matchLabels': manager_selector}}]
    if bound['spec']['podSelector'] != policy['spec']['podSelector']:
        raise ValueError('binding a policy changed which Pods it selects')
    return bound


class Network:
    def __init__(self, lab_dir):
        self.lab = Lab(lab_dir)
        self.out = self.lab.dir / 'network'
        self.out.mkdir(mode=0o700)
        self.env = self.lab.env
        self.state = json.loads((self.lab.dir / 'capacity-bootstrap.json').read_text())
        self.kubectl = ['kubectl', '--kubeconfig', self.env['E2E_KUBECONFIG'], '--request-timeout=20s']

    def call(self, name, args, obj=None):
        raw = json.dumps(obj).encode() if obj is not None else None
        result = subprocess.run(self.kubectl + args, input=raw, capture_output=True, timeout=30)
        (self.out / (name + '.stderr')).write_bytes(result.stderr)
        if result.returncode:
            raise RuntimeError(name + ' failed; see private diagnostics')
        return result.stdout

    def get(self, resource, name, namespace):
        return json.loads(self.call('read-' + resource + '-' + name, ['-n', namespace, 'get', resource, name, '-o', 'json']))

    def create(self, obj):
        name = obj['metadata']['namespace'] + '-' + obj['metadata']['name']
        stored = json.loads(self.call(name, ['create', '-f', '-', '-o', 'json'], obj))
        (self.out / (name + '.json')).write_text(json.dumps(stored, indent=2) + '\n')
        return stored

    def delete(self, obj):
        meta = obj['metadata']
        options = {'apiVersion': 'v1', 'kind': 'DeleteOptions', 'preconditions': {'uid': meta['uid']}}
        self.call('delete-' + meta['namespace'] + '-' + meta['name'],
                  ['delete', '--raw', '/api/v1/namespaces/' + meta['namespace'] + '/pods/' + meta['name'], '-f', '-'], options)

    def endpoint(self, service, namespace):
        slices = json.loads(self.call('endpoints-' + service, ['-n', namespace, 'get', 'endpointslices',
                                      '-l', 'kubernetes.io/service-name=' + service, '-o', 'json']))['items']
        addresses = sorted({a for s in slices for e in s['endpoints'] if e.get('conditions', {}).get('ready') is not False
                            for a in e['addresses']})
        if not addresses:
            raise RuntimeError(f'{namespace}/{service} has no ready endpoint')
        return addresses

    def probe(self, namespace, name, expected, component=None, operation=None, database=None, port=None):
        labels = {'operator.ptah.run/qualification-network-probe': self.state['runID']}
        if component:
            labels.update({'app.kubernetes.io/managed-by': 'qualification-probe',
                           'app.kubernetes.io/component': component, 'operator.ptah.run/operation': operation})
        values = {'DATABASE': database or self.database, 'DATABASE_PORT': str(port or self.database_port),
                  'REGISTRY': self.registry, 'API': self.api[0], 'RECEIVER': self.receiver}
        obj = {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': name, 'namespace': namespace, 'labels': labels},
               'spec': {'restartPolicy': 'Never', 'automountServiceAccountToken': False, 'enableServiceLinks': False,
                        'imagePullSecrets': [{'name': 'demo-registry-pull'}],
                        'securityContext': {'runAsNonRoot': True, 'runAsUser': 65532, 'runAsGroup': 65532,
                                            'seccompProfile': {'type': 'RuntimeDefault'}},
                        'containers': [{'name': 'probe', 'image': self.env['E2E_POSTGRES_IMAGE'], 'imagePullPolicy': 'IfNotPresent',
                                        'command': ['/bin/sh', '-c', SCRIPT],
                                        'env': [{'name': k, 'value': v} for k, v in values.items()],
                                        'resources': {'requests': {'cpu': '5m', 'memory': '16Mi'}, 'limits': {'cpu': '100m', 'memory': '32Mi'}},
                                        'securityContext': {'allowPrivilegeEscalation': False, 'readOnlyRootFilesystem': True,
                                                            'capabilities': {'drop': ['ALL']}}}]}}
        if namespace == self.operator_namespace:
            obj['spec']['imagePullSecrets'] = self.manager['spec']['template']['spec']['imagePullSecrets']
        stored = self.create(obj)
        try:
            deadline = time.monotonic() + 120
            while True:
                pod = self.get('pod', name, namespace)
                if pod['metadata']['uid'] != stored['metadata']['uid']:
                    raise RuntimeError(f'{namespace}/{name} was replaced')
                if pod.get('status', {}).get('phase') in ('Succeeded', 'Failed'):
                    break
                if time.monotonic() >= deadline:
                    raise RuntimeError(f'probe {namespace}/{name} did not finish')
                time.sleep(1)
            raw = self.call('logs-' + namespace + '-' + name, ['-n', namespace, 'logs', name, '-c', 'probe'])
            (self.out / (namespace + '-' + name + '.log')).write_bytes(raw)
            readings = dict(line.split('=', 1) for line in raw.decode().splitlines() if '=' in line)
            result = {'namespace': namespace, 'name': name, 'uid': stored['metadata']['uid'], 'expected': expected,
                      'actual': readings, 'podPhase': pod['status']['phase']}
            (self.out / (namespace + '-' + name + '-result.json')).write_text(json.dumps(result, indent=2) + '\n')
            if pod['status']['phase'] != 'Succeeded' or readings != expected:
                raise AssertionError(json.dumps(result))
            return result
        finally:
            self.delete(stored)

    def run(self):
        env, state = self.env, self.state
        self.operator_namespace = env['E2E_OPERATOR_NAMESPACE']
        source = env['E2E_TEST_NAMESPACE']
        database_namespace = state['fixtureNamespace']
        service, self.database_port = database_service(state['engine'])
        registry_service = self.get('service', 'e2e-registry-tls', source)
        self.registry = self.endpoint('e2e-registry-tls', source)[0]
        database_svc = self.get('service', service, database_namespace)
        self.database = self.endpoint(service, database_namespace)[0]
        self.api = self.endpoint('kubernetes', 'default')
        self.manager = self.get('deployment', env['E2E_CONTROLLER_NAME'], self.operator_namespace)
        pods = json.loads(self.call('manager-pods', ['-n', self.operator_namespace, 'get', 'pods', '-l',
                                    'app.kubernetes.io/component=controller', '-o', 'json']))['items']
        if len(pods) != 2 or not all(p.get('status', {}).get('podIP') for p in pods):
            raise RuntimeError('the two managers have no addresses')
        self.receiver = pods[0]['status']['podIP']
        external, external_port = env['E2E_EXTERNAL_POSTGRES_IP'], 5432

        open_all = {target: 'open' for target in TARGETS}
        results = []
        for namespace in state['workloadNamespaces']:
            results.append(self.probe(namespace, 'network-allowed-control', open_all))
        for namespace in (self.operator_namespace, database_namespace):
            results.append(self.probe(namespace, 'network-allowed-control', open_all, database=external, port=external_port))

        example = (ROOT / 'examples/networkpolicy-egress.yaml').read_bytes()
        rendered = subprocess.run(self.kubectl + ['create', '--dry-run=client', '--validate=false', '-f', '-', '-o', 'json'],
                                  input=example, capture_output=True, timeout=30, check=True).stdout.decode()
        decoder, original = json.JSONDecoder(), []
        while rendered.strip():
            rendered = rendered.lstrip()
            item, end = decoder.raw_decode(rendered)
            original.append(item)
            rendered = rendered[end:]
        if len(original) != 8:
            raise RuntimeError(f'the egress example declares {len(original)} policies, not 8')
        stored = []
        for namespace in state['workloadNamespaces']:
            for policy in original:
                bound = bind_policy(policy, source, registry_service['spec']['selector'], database_namespace,
                                    database_svc['spec']['selector'], self.database_port, self.operator_namespace,
                                    self.manager['spec']['selector']['matchLabels'])
                bound['metadata'] = {'name': policy['metadata']['name'], 'namespace': namespace}
                stored.append(self.create(bound))
                clone = copy.deepcopy(bound)
                clone['metadata']['name'] += '-probe'
                clone['spec']['podSelector']['matchLabels']['app.kubernetes.io/managed-by'] = 'qualification-probe'
                stored.append(self.create(clone))
        dns = next(p for p in original if p['metadata']['name'] == 'ptah-operations-dns')['spec']['egress']
        api_rule = {'to': [{'ipBlock': {'cidr': address + '/32'}} for address in self.api], 'ports': [{'protocol': 'TCP', 'port': 6443}]}
        serving = {'to': [{'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': self.operator_namespace}}}],
                   'ports': [{'protocol': 'TCP', 'port': 9443}, {'protocol': 'TCP', 'port': 9444}]}
        for namespace, name, rules in ((self.operator_namespace, 'qualification-operator-egress', dns + [api_rule, serving]),
                                       (database_namespace, 'qualification-database-egress', [])):
            stored.append(self.create({'apiVersion': 'networking.k8s.io/v1', 'kind': 'NetworkPolicy',
                                       'metadata': {'name': name, 'namespace': namespace},
                                       'spec': {'podSelector': {}, 'policyTypes': ['Egress'], 'egress': rules}}))

        tasks = []
        for namespace in state['workloadNamespaces']:
            for family, operations in (('schema', ['resolve', 'verify', 'observe', 'plan', 'apply']),
                                       ('migration', ['resolve', 'verify', 'history', 'apply'])):
                for operation in operations:
                    tasks.append((namespace, f'network-{family}-{operation}', task_expectations(family, operation),
                                  family + '-operation', operation))
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
            results.extend(pool.map(lambda args: self.probe(*args), tasks))
        results.append(self.probe(self.operator_namespace, 'network-operator', dict(open_all, database='closed', registry='closed'),
                                  database=external, port=external_port))
        results.append(self.probe(database_namespace, 'network-database', {target: 'closed' for target in TARGETS},
                                  database=external, port=external_port))
        if len(results) != 24:
            raise RuntimeError(f'{len(results)} network controls ran, not 24')
        summary = {'conclusion': 'pass', 'engine': state['engine'], 'exampleSHA256': hashlib.sha256(example).hexdigest(),
                   'results': results, 'policyIdentities': [{'namespace': p['metadata']['namespace'], 'name': p['metadata']['name'],
                                                             'uid': p['metadata']['uid']} for p in stored]}
        (self.out / 'summary.json').write_text(json.dumps(summary, indent=2) + '\n')
        print(f'PASS: {len(results)} live network controls under the documented egress policies')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--lab', required=True)
    args = parser.parse_args()
    os.umask(0o077)
    Network(args.lab).run()
    return 0


if __name__ == '__main__':
    sys.exit(main())
