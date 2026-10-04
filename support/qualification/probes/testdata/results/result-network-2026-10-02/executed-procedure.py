#!/usr/bin/env python3
"""Exercise the shipped receiver ingress and runner egress with native workflows."""
import base64
import copy
import hashlib
import json
import os
import pathlib
import subprocess
import time
import urllib.parse
from result_first_harvest import publication
from result_upload_budget import schema_witness, EXPECTED_SCHEMA

OPERATIONS = {'PtahSchema': {'resolve', 'verify', 'observe', 'plan', 'apply'},
              'PtahMigration': {'resolve', 'verify', 'migration-history', 'migration-apply'}}


def objects_from_output(output):
    objects = []
    decoder = json.JSONDecoder()
    while output.strip():
        obj, end = decoder.raw_decode(output.lstrip())
        objects.extend(obj['items'] if obj.get('kind') == 'List' else [obj])
        output = output.lstrip()[end:]
    if not objects:
        raise ValueError('No rendered Kubernetes objects')
    return objects


def selected(labels, selector):
    if not all(labels.get(k) == v for k, v in selector.get('matchLabels', {}).items()):
        return False
    for rule in selector.get('matchExpressions', []):
        if rule['operator'] != 'In':
            raise ValueError('Unexpected selector operator')
        if labels.get(rule['key']) not in rule['values']:
            return False
    return True


def verify(e):
    def require(ok, message):
        if not ok:
            raise ValueError(message)
    require(len(e['nodes']) == 4 and all(n['containerLogMaxSize'] == '10Mi' for n in e['nodes']), 'Missing default logging')
    require(len(e['receiverUIDs']) == 2 and len(set(e['receiverUIDs'])) == 2, 'Missing HA receivers')
    require(e['beforeIngress'] == 'open open open' and e['afterIngress'] == 'closed closed closed', 'Missing actual ingress refusal and allowed control')
    require(e['afterRemoval'] == 'open open open', 'Ingress was not restored')
    expected = {(f, engine) for f in OPERATIONS for engine in ('PostgreSQL', 'MySQL')}
    require({(r['family'], r['engine']) for r in e['workflows']} == expected and len(e['workflows']) == 4, 'Incomplete workflow denominator')
    for row in e['workflows']:
        require(set(row['publications']) == OPERATIONS[row['family']], 'Missing native operation under policy')
        require(row['database'] == (EXPECTED_SCHEMA if row['family'] == 'PtahSchema' else 1), 'No native database convergence')
        require(row['resultPolicySelectedEveryJob'] and row['ingressSelectedReceivers'], 'Policy did not select its workload')
        reason = 'InSync' if row['family'] == 'PtahSchema' else 'HistoryMatched'
        require(any(c['type'] == 'Ready' and c['status'] == 'True' and c['reason'] == reason and c['observedGeneration'] == row['generation'] for c in row['conditions']) and row['applyJobs'] == 1, 'Workflow did not converge with one Apply')
    require(e['policiesRemoved'] and e['probesRemoved'], 'Temporary network controls remain')
    return {'nativeWorkflows': 4, 'familyOperationEnginePairs': 18, 'deniedReceiverPaths': 3}


def main():
    if not __debug__:
        raise RuntimeError('Run without Python optimization')
    E = os.environ
    source, opns = E['E2E_TEST_NAMESPACE'], E['E2E_OPERATOR_NAMESPACE']
    out = pathlib.Path(E['RESULT_PROBE_EVIDENCE_DIR']); out.mkdir(exist_ok=True)
    namespaces, resources, created_policies, probes = [], [], [], []
    evidence = {'sourceRevision': E['E2E_CONTROLLER_REVISION'], 'workflows': [], 'nodes': [],
                'procedureSHA256': hashlib.sha256(pathlib.Path(__file__).read_bytes()).hexdigest()}

    def run(args, data=None):
        p = subprocess.run(args, input=data, capture_output=True, text=True, timeout=120)
        if p.returncode:
            raise RuntimeError(args[0] + ' failed: ' + p.stderr[-900:])
        return p.stdout

    def k(*args, ns=source, data=None):
        return run(['kubectl', '--kubeconfig', E['E2E_KUBECONFIG'], '--request-timeout=30s', '-n', ns, *args], data)

    def get(kind, name=None, ns=source):
        return json.loads(k('get', kind, *([name] if name else []), '-o', 'json', ns=ns))

    def create(obj):
        return json.loads(k('create', '-f', '-', '-o', 'json', ns=obj['metadata'].get('namespace', source), data=json.dumps(obj)))

    def save(name, obj):
        text = json.dumps(obj, indent=2) + '\n'; assert 'PRIVATE KEY' not in text
        (out / name).write_text(text)

    def wait(fn, timeout=240):
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            value = fn()
            if value:
                return value
            time.sleep(1)
        raise RuntimeError('Timed out: ' + fn.__name__)

    def policy(obj):
        stored = create(obj)
        created_policies.append((stored['metadata']['namespace'], stored['metadata']['name'], stored['metadata']['uid']))
        return stored

    def new_namespace(name, source_ns, family):
        create({'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': name, 'labels': {'operator.ptah.run/acceptance-owner': E['E2E_KIND_CLUSTER_NAME']}}})
        namespaces.append(name)
        for kind, objname in [('secret', 'demo-registry'), ('secret', 'demo-registry-pull'), ('configmap', 'demo-verification-policy' if family == 'PtahSchema' else 'demo-migration-verification-policy')]:
            obj = get(kind, objname, source_ns); obj['metadata'] = {'name': objname, 'namespace': name}; create(obj)
        k('patch', 'serviceaccount', 'default', '--type=merge', '-p', json.dumps({'imagePullSecrets': [{'name': 'demo-registry-pull'}]}), ns=name)

    nodes = get('nodes')['items']
    for node in nodes:
        name = node['metadata']['name']
        owner = run(['docker', '--context', E['E2E_DOCKER_CONTEXT'], 'inspect', '--format', '{{index .Config.Labels "io.x-k8s.kind.cluster"}}', name]).strip()
        assert owner == E['E2E_KIND_CLUSTER_NAME']
        cfg = json.loads(k('get', '--raw', '/api/v1/nodes/' + name + '/proxy/configz'))
        assert cfg['kubeletconfig']['containerLogMaxSize'] == '10Mi'
        evidence['nodes'].append({'name': name, 'version': node['status']['nodeInfo']['kubeletVersion'], 'architecture': node['status']['nodeInfo']['architecture'], 'containerLogMaxSize': '10Mi'})
    assert len(nodes) == 4 and not get('networkpolicies', ns=opns)['items']
    manager = get('deployment', E['E2E_CONTROLLER_NAME'], opns)
    selector = manager['spec']['selector']['matchLabels']
    receivers = [p for p in get('pods', ns=opns)['items'] if selected(p['metadata']['labels'], {'matchLabels': selector})]
    assert len(receivers) == 2 and all(any(c['type'] == 'Ready' and c['status'] == 'True' for c in p['status']['conditions']) for p in receivers)
    evidence['receiverUIDs'] = [p['metadata']['uid'] for p in receivers]
    evidence['operatorImage'] = manager['spec']['template']['spec']['containers'][0]['image']
    evidence['kindnet'] = get('daemonset', 'kindnet', 'kube-system')
    services = [s for s in get('services', ns=opns)['items'] if any(p.get('targetPort') == 'results' for p in s['spec']['ports'])]
    assert len(services) == 1
    service = services[0]
    endpoints = [(p['status']['podIP'], 9444) for p in receivers] + [(service['spec']['clusterIP'], 443)]
    worker = [n['metadata']['name'] for n in nodes if 'node-role.kubernetes.io/control-plane' not in n['metadata'].get('labels', {})]
    assert len(worker) == 1
    first_ns = 'ptah-result-network-pg-schema'
    new_namespace(first_ns, 'ptah-result-schema-budget-pg', 'PtahSchema')

    def probe(name):
        script = 'sleep 5\n' + '\n'.join('if timeout 8 nc -z -w 4 ' + ip + ' ' + str(port) + '; then echo open; else echo closed; fi' for ip, port in endpoints)
        obj = {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': name, 'namespace': first_ns}, 'spec': {
            'nodeName': worker[0], 'restartPolicy': 'Never', 'automountServiceAccountToken': False,
            'securityContext': {'runAsNonRoot': True, 'runAsUser': 65532, 'runAsGroup': 65532, 'seccompProfile': {'type': 'RuntimeDefault'}},
            'containers': [{'name': 'probe', 'image': E['E2E_POSTGRES_IMAGE'], 'command': ['/bin/sh', '-ec', script],
                'resources': {'requests': {'cpu': '10m', 'memory': '16Mi'}, 'limits': {'cpu': '100m', 'memory': '32Mi'}},
                'securityContext': {'allowPrivilegeEscalation': False, 'readOnlyRootFilesystem': True, 'capabilities': {'drop': ['ALL']}}}]}}
        stored = create(obj); probes.append((first_ns, name, stored['metadata']['uid']))
        def ended():
            current = get('pod', name, first_ns)
            assert current['status']['phase'] != 'Failed', 'Network probe failed'
            return current if current['status']['phase'] == 'Succeeded' else None
        result = wait(ended, 90)
        lines = k('logs', name, ns=first_ns).splitlines()
        assert len(lines) == 3 and set(lines) <= {'open', 'closed'}
        save(name + '.json', {'pod': result, 'reading': lines, 'targets': endpoints})
        return ' '.join(lines)

    try:
        evidence['beforeIngress'] = probe('network-before-ingress')
        assert evidence['beforeIngress'] == 'open open open'
        peers = [{'ipBlock': {'cidr': a['address'] + '/32'}} for n in nodes for a in n['status']['addresses'] if a['type'] == 'InternalIP']
        values = out / 'network-values.json'
        save(values.name, {'resultDelivery': {'enabled': True, 'networkPolicy': {'enabled': True, 'infrastructurePeers': peers}}})
        rendered = run(['helm', 'template', E['E2E_HELM_RELEASE'], E['E2E_CHART_PACKAGE'], '--namespace', opns,
                        '--values', E['E2E_CANDIDATE_VALUES_FILE'], '--values', str(values), '--show-only', 'templates/result-delivery.yaml'])
        objects = objects_from_output(k('create', '--dry-run=client', '--validate=false', '-f', '-', '-o', 'json', ns=opns, data=rendered))
        ingress = [o for o in objects if o['kind'] == 'NetworkPolicy']; assert len(ingress) == 1
        receiver_policy = policy(ingress[0]); save('receiver-policy.json', receiver_policy)
        assert all(selected(p['metadata']['labels'], receiver_policy['spec']['podSelector']) for p in receivers)
        enforcement_deadline = time.monotonic() + 180
        attempt = 0
        while True:
            attempt += 1
            evidence['afterIngress'] = probe('network-after-ingress-' + str(attempt))
            if evidence['afterIngress'] == 'closed closed closed':
                break
            assert time.monotonic() < enforcement_deadline, 'Receiver ingress was not enforced'

        print('Installed receiver policy blocks both Pod endpoints and the Service from an unselected Pod', flush=True)

        raw = pathlib.Path('examples/networkpolicy-egress.yaml').read_text()
        example = objects_from_output(k('create', '--dry-run=client', '--validate=false', '-f', '-', '-o', 'json', ns='application', data=raw))
        assert len(example) == 8
        registry_endpoints = get('endpointslices')['items']
        registry_ips = {a for s in registry_endpoints if s['metadata'].get('labels', {}).get('kubernetes.io/service-name') == 'e2e-registry' for endpoint in s['endpoints'] for a in endpoint['addresses']}
        assert len(registry_ips) == 1
        registry_ip = registry_ips.pop()
        pg = json.loads(pathlib.Path(E['E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE']).read_text())
        mysql = {k: base64.b64decode(v).decode() for k, v in get('secret', 'demo-mysql-database')['data'].items()}
        for engine, suffix, creds in [('PostgreSQL', 'pg', pg), ('MySQL', 'mysql', mysql)]:
            for family, short in [('PtahSchema', 'schema'), ('PtahMigration', 'migration')]:
                ns = 'ptah-result-network-' + suffix + '-' + short
                db = 'result_network_' + suffix + '_' + short
                template_ns = 'ptah-result-schema-budget-' + suffix if family == 'PtahSchema' else 'ptah-result-partial-' + suffix
                if ns != first_ns:
                    new_namespace(ns, template_ns, family)
                def sql(query, database=None):
                    if engine == 'MySQL':
                        return k('exec', '-i', 'deployment/demo-mysql', '--', 'sh', '-ec', 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql --protocol=TCP -h 127.0.0.1 -u root --batch --skip-column-names "$1"', 'probe', database or db, data=query + '\n').strip()
                    return run(['docker', '--context', E['E2E_DOCKER_CONTEXT'], 'exec', '-i', E['E2E_EXTERNAL_POSTGRES_CONTAINER_ID'], 'sh', '-ec', 'PGPASSWORD="$POSTGRES_PASSWORD" exec psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$1" -At -v ON_ERROR_STOP=1', 'probe', database or db], query + '\n').strip()
                if engine == 'PostgreSQL':
                    owner = '"' + creds['username'].replace('"', '""') + '"'
                    sql('CREATE DATABASE ' + db + ' OWNER ' + owner + ';', urllib.parse.urlsplit(creds['url']).path[1:])
                    url = urllib.parse.urlunsplit(urllib.parse.urlsplit(creds['url'])._replace(path='/' + db))
                    host = urllib.parse.urlsplit(url).hostname
                    db_port = urllib.parse.urlsplit(url).port or 5432
                    if family == 'PtahMigration':
                        sql('SET ROLE ' + owner + '; CREATE SEQUENCE delivery_probe_sequence; CREATE TABLE delivery_probe_calls(n bigint NOT NULL);')
                    db_addresses = {a for endpoint_slice in registry_endpoints if endpoint_slice['metadata'].get('labels', {}).get('kubernetes.io/service-name') == host.split('.')[0] for endpoint in endpoint_slice['endpoints'] for a in endpoint['addresses']}
                    assert len(db_addresses) == 1
                    database_peer = {'ipBlock': {'cidr': db_addresses.pop() + '/32'}}
                else:
                    sql('CREATE DATABASE `' + db + '`; GRANT ALL ON `' + db + '`.* TO \'demo\'@\'%\';', 'mysql')
                    url = 'mysql://demo:' + urllib.parse.quote(creds['password'], safe='') + '@demo-mysql.' + source + '.svc.cluster.local:3306/' + db
                    db_port = 3306
                    database_peer = {'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': source}}, 'podSelector': {'matchLabels': {'app.kubernetes.io/name': 'demo-mysql'}}}
                    if family == 'PtahMigration':
                        sql('CREATE TABLE delivery_probe_calls(n BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY) ENGINE=InnoDB;')
                create({'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'network-database', 'namespace': ns}, 'stringData': {'url': url}})
                policies = []
                for original in example:
                    obj = copy.deepcopy(original); obj['metadata'] = {'name': original['metadata']['name'], 'namespace': ns}
                    if obj['metadata']['name'].endswith('-registry'):
                        obj['spec']['egress'][0]['to'] = [{'ipBlock': {'cidr': registry_ip + '/32'}}]
                    if obj['metadata']['name'].endswith('-database'):
                        obj['spec']['egress'][0] = {'to': [database_peer], 'ports': [{'protocol': 'TCP', 'port': db_port}]}
                    if obj['metadata']['name'].endswith('-results'):
                        obj['spec']['egress'][0]['to'] = [{'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': opns}}, 'podSelector': {'matchLabels': selector}}]
                    assert obj['spec']['podSelector'] == original['spec']['podSelector']
                    policies.append(policy(obj))
                save(ns + '-policies.json', policies)
                original = get(family.lower(), 'lost-ack', template_ns)
                obj = {'apiVersion': original['apiVersion'], 'kind': family, 'metadata': {'name': 'network-proof', 'namespace': ns}, 'spec': copy.deepcopy(original['spec'])}
                obj['spec']['suspend'] = False; obj['spec']['interval'] = '2h'; obj['spec']['policy']['apply'] = 'Always'
                obj['spec']['target']['urlFrom']['name'] = 'network-database'; obj['spec']['target']['coordinationKey'] = 'qualification/' + ns
                resource = create(obj); resources.append((family.lower(), ns, resource['metadata']['uid']))
                jobs = {}
                def converged():
                    for job in get('jobs', ns=ns)['items']:
                        jobs[job['metadata']['uid']] = job
                    current = get(family.lower(), 'network-proof', ns)
                    reason = 'InSync' if family == 'PtahSchema' else 'HistoryMatched'
                    return current if not current.get('status', {}).get('activeOperation') and any(c['type'] == 'Ready' and c['status'] == 'True' and c['reason'] == reason and c['observedGeneration'] == current['metadata']['generation'] for c in current.get('status', {}).get('conditions', [])) else None
                final = wait(converged, 300)
                records = {r['metadata']['name']: r for r in get('ptahresultrecords', ns=ns)['items'] if r['spec']['type'] in ('intent', 'chunk', 'complete')}
                publications = {}
                for record in records.values():
                    if record['spec']['type'] != 'intent':
                        continue
                    manifest = json.loads(base64.b64decode(record['spec']['data']))
                    binding = manifest['binding']; assert binding['uid'] == resource['metadata']['uid']
                    _, receipt, payload = publication(records, binding['jobUID'])
                    assert json.loads(payload)['childExitCode'] == 0
                    publications[binding['operation']] = {'jobUID': binding['jobUID'], 'receiptUID': receipt['metadata']['uid'], 'digest': manifest['digest']}
                result_policy = next(p for p in policies if p['metadata']['name'].endswith('-results'))
                assert jobs and all(selected(j['spec']['template']['metadata']['labels'], result_policy['spec']['podSelector']) for j in jobs.values())
                apply_jobs = [j for j in jobs.values() if j['spec']['template']['metadata']['labels'].get('operator.ptah.run/operation') == 'apply']
                native = schema_witness(sql, engine, db) if family == 'PtahSchema' else int(sql('SELECT count(*) FROM delivery_probe_calls;'))
                row = {'family': family, 'engine': engine, 'resourceUID': resource['metadata']['uid'], 'namespace': ns, 'publications': publications,
                       'database': native, 'applyJobs': len(apply_jobs), 'conditions': final['status']['conditions'], 'generation': final['metadata']['generation'], 'resultPolicySelectedEveryJob': True, 'ingressSelectedReceivers': True}
                evidence['workflows'].append(row)
                save(ns + '-workflow.json', {'resource': final, 'jobs': list(jobs.values()), 'publications': records, 'verification': row})
                k('patch', family.lower(), 'network-proof', '--type=merge', '-p', '{"spec":{"suspend":true}}', ns=ns)
                print('PASS under shipped network policies:', engine, family, sorted(publications), flush=True)
        for namespace, name, uid in list(created_policies):
            assert get('networkpolicy', name, namespace)['metadata']['uid'] == uid
            k('delete', 'networkpolicy', name, ns=namespace)
        created_policies.clear(); evidence['policiesRemoved'] = True
        evidence['afterRemoval'] = probe('network-after-removal')
    finally:
        for kind, namespace, uid in resources:
            current = get(kind, 'network-proof', namespace)
            assert current['metadata']['uid'] == uid
            k('patch', kind, 'network-proof', '--type=merge', '-p', '{"spec":{"suspend":true}}', ns=namespace)
        for namespace, name, uid in created_policies:
            assert get('networkpolicy', name, namespace)['metadata']['uid'] == uid
            k('delete', 'networkpolicy', name, ns=namespace)
        for namespace, name, uid in probes:
            assert get('pod', name, namespace)['metadata']['uid'] == uid
            k('delete', 'pod', name, '--wait=true', '--timeout=60s', ns=namespace)
        evidence['probesRemoved'] = True
        save('network.json', evidence)
    evidence['verification'] = verify(evidence); save('network.json', evidence)
    print('PASS: native durable delivery through enforced receiver ingress and task egress', flush=True)


if __name__ == '__main__':
    main()
