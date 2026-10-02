"""Restore an owned kind control plane before its first durable-result harvest.

Called only after the first-harvest probe removes leader permission and the
producer Pod. All members restore the same encrypted snapshot. The databases
survive; this does not replace the declared database-loss recovery matrix.
"""
import hashlib
import json
import os
from pathlib import Path
import secrets
import subprocess
import time

from operator_restore import OperatorProbe


def digest(value):
    return hashlib.sha256(value).hexdigest()


def retained_objects(inventory):
    values = inventory['records'] + inventory['credentialProjections']
    values += [inventory['trust'], inventory['journal'], inventory['enrollmentPolicy']]
    result = {value['kind'] + '/' + value['metadata']['namespace'] + '/' + value['metadata']['name']: {
        'uid': value['metadata']['uid'],
        'contentSHA256': digest(json.dumps({'payload': value.get('spec', value.get('data')),
            'owners': value['metadata'].get('ownerReferences', []),
            'annotations': value['metadata'].get('annotations', {}),
            'labels': value['metadata'].get('labels', {})}, sort_keys=True).encode()),
    } for value in values}
    if not result or len(result) != len(values) or not all(row['uid'] for row in result.values()):
        raise ValueError('Missing or duplicate retained object identities')
    return result


def ready_replacements(before, pods):
    current = {p['metadata']['uid']: p for p in pods}
    if len(before) != 3:
        raise ValueError('Expected two receivers and one rotator')
    for prior in before:
        pod = current.get(prior['uid'])
        if pod is None or pod['metadata'].get('deletionTimestamp'):
            return False
        if not any(c['type'] == 'Ready' and c['status'] == 'True' for c in pod.get('status', {}).get('conditions', [])):
            return False
        statuses = pod.get('status', {}).get('containerStatuses', [])
        if (len(statuses) != 1 or not statuses[0]['ready'] or not statuses[0].get('containerID')
                or statuses[0]['containerID'] == prior['containerID']
                or statuses[0]['imageID'] != prior['imageID']):
            return False
        if not statuses[0].get('state', {}).get('running', {}).get('startedAt'):
            return False
    return True


def run(environment):
    e = dict(environment)
    root = Path(e['RESULT_RESTORE_PRIVATE_DIR'])
    root.mkdir(mode=0o700, parents=False, exist_ok=False)
    os.chmod(root, 0o700)
    token = 'ptah-control-restore-' + secrets.token_hex(6)
    scratch = '/tmp/' + token
    docker = ['docker', '--context', e['E2E_DOCKER_CONTEXT']]
    kubectl = ['kubectl', '--kubeconfig', e['E2E_KUBECONFIG'], '--request-timeout=20s']
    report = {'status': 'RUNNING', 'procedureSHA256': digest(Path(__file__).read_bytes()),
              'startedAt': time.time(), 'steps': [], 'stoppedKubelets': [], 'installedStores': []}
    nodes = []
    stopped = False
    replacement_started = False
    restored = False

    def save():
        (root / 'report.json').write_text(json.dumps(report, indent=2) + '\n')

    def command(args, data=None, required=True, timeout=180):
        result = subprocess.run(args, input=data, capture_output=True, timeout=timeout)
        report['steps'].append({'program': args[0], 'exitCode': result.returncode, 'at': time.time()})
        if result.returncode and required:
            (root / ('failure-' + str(len(report['steps'])) + '.private.log')).write_bytes(result.stderr)
            raise RuntimeError('Command failed; private diagnostics retained for ' + args[0])
        return result

    def get(kind, name=None, namespace=None):
        args = kubectl + (['-n', namespace] if namespace else []) + ['get', kind]
        args += [name] if name else ([] if namespace else ['-A'])
        return json.loads(command(args + ['-o', 'json']).stdout)

    def node_command(node, *args, data=None, required=True):
        return command(docker + ['exec'] + (['-i'] if data is not None else []) + [node, *args], data, required)

    def etcd(member, *args, required=True):
        return node_command(member['node'], 'crictl', 'exec', member['containerID'], 'etcdctl',
            '--endpoints=https://127.0.0.1:2379', '--cacert=/etc/kubernetes/pki/etcd/ca.crt',
            '--cert=/etc/kubernetes/pki/etcd/healthcheck-client.crt',
            '--key=/etc/kubernetes/pki/etcd/healthcheck-client.key', *args, required=required)

    def inventory():
        probe = object.__new__(OperatorProbe)
        probe.envs = e; probe.namespace = e['RESULT_PROBE_NAMESPACE']; probe.root = root
        probe.report = {'checks': {}, 'steps': []}
        return retained_objects(probe.result_backup())

    def wait(fn, seconds):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            try:
                value = fn()
                if value:
                    return value
            except (RuntimeError, json.JSONDecodeError):
                pass
            time.sleep(2)
        raise RuntimeError('Timed out: ' + fn.__name__)

    try:
        save()
        cluster = e['E2E_KIND_CLUSTER_NAME']
        assert cluster.startswith('ptah-e2e-')
        nodes = [n['metadata']['name'] for n in get('nodes')['items']]
        assert len(nodes) == 4
        for node in nodes:
            owner = command(docker + ['inspect', '--format', '{{index .Config.Labels "io.x-k8s.kind.cluster"}}', node]).stdout.decode().strip()
            assert owner == cluster
        namespace = get('namespace', e['RESULT_PROBE_NAMESPACE'])
        assert namespace['metadata']['labels']['operator.ptah.run/acceptance-owner'] == cluster
        report['namespaceUID'] = namespace['metadata']['uid']
        deployment = get('deployment', e['E2E_CONTROLLER_NAME'], e['E2E_OPERATOR_NAMESPACE'])
        sa = deployment['spec']['template']['spec']['serviceAccountName']
        rolebinding = get('rolebinding', sa, e['E2E_OPERATOR_NAMESPACE'])
        assert not rolebinding.get('subjects'), 'First-harvest leader fence is not active'
        for job in get('jobs')['items']:
            operation = job['spec']['template'].get('metadata', {}).get('labels', {}).get('operator.ptah.run/operation')
            if operation in ('apply', 'migration-apply'):
                assert any(c['type'] in ('Complete', 'Failed') and c['status'] == 'True' for c in job.get('status', {}).get('conditions', [])), 'A mutating Job is still live'
        pods = get('pods')['items']
        for pod in pods:
            if pod['metadata'].get('labels', {}).get('operator.ptah.run/operation') in ('apply', 'migration-apply'):
                assert pod.get('status', {}).get('phase') in ('Succeeded', 'Failed'), 'A mutating Pod can still execute'
        runtime = [p for p in pods if p['metadata']['namespace'] == e['E2E_OPERATOR_NAMESPACE'] and not p['metadata'].get('deletionTimestamp')]
        assert len(runtime) == 3
        assert sorted(p['spec']['containers'][0]['name'] for p in runtime) == ['certificate-rotator', 'manager', 'manager']
        before = []
        for pod in runtime:
            statuses = pod['status']['containerStatuses']
            assert len(statuses) == 1 and statuses[0]['ready']
            before.append({'name': pod['metadata']['name'], 'uid': pod['metadata']['uid'], 'node': pod['spec']['nodeName'], 'containerID': statuses[0]['containerID'], 'imageID': statuses[0]['imageID']})
        report['runtimeBefore'] = before
        members = []
        control_pods = []
        for pod in pods:
            if pod['metadata']['namespace'] != 'kube-system':
                continue
            name = pod['metadata']['name']
            if not name.startswith(('etcd-', 'kube-apiserver-', 'kube-controller-manager-', 'kube-scheduler-')):
                continue
            container_id = pod['status']['containerStatuses'][0]['containerID'].removeprefix('containerd://')
            control_pods.append({'node': pod['spec']['nodeName'], 'name': name, 'containerID': container_id})
            if name.startswith('etcd-'):
                container = pod['spec']['containers'][0]
                flags = dict(arg[2:].split('=', 1) for arg in container['command'][1:] if arg.startswith('--') and '=' in arg)
                assert flags['data-dir'] == '/var/lib/etcd' and flags['name'] == pod['spec']['nodeName']
                members.append({'node': pod['spec']['nodeName'], 'name': flags['name'], 'peerURL': flags['initial-advertise-peer-urls'], 'containerID': container_id, 'image': container['image']})
        assert len(members) == 3 and len(control_pods) == 12
        report['membersBefore'] = members
        report['sourceEtcd'] = [json.loads(etcd(m, 'endpoint', 'status', '--write-out=json').stdout)[0]['Status'] for m in members]
        # Freeze workload processes before inventory and backup. The API and
        # etcd remain live until the encrypted archive has been checked.
        stopped = True
        for node in nodes:
            report['stoppedKubelets'].append(node)
            node_command(node, 'systemctl', 'stop', 'kubelet')
        for pod in before:
            node_command(pod['node'], 'crictl', 'stop', '--timeout', '10', pod['containerID'].removeprefix('containerd://'))
        for pod in control_pods:
            if pod['name'].startswith(('kube-controller-manager-', 'kube-scheduler-')):
                node_command(pod['node'], 'crictl', 'stop', '--timeout', '10', pod['containerID'])
        original = inventory()
        report['retainedBefore'] = original
        save()
        print('Runtime stopped; capturing the original result and key identities', flush=True)
        source = members[0]
        snapshot = '/var/lib/etcd/' + token + '.db'
        report['backupStartedAt'] = time.time()
        etcd(source, 'snapshot', 'save', snapshot)
        raw = node_command(source['node'], 'cat', snapshot).stdout
        report['snapshotSHA256'] = digest(raw)
        key = root / 'identity.private'
        command(['age-keygen', '-o', str(key)])
        recipient = command(['age-keygen', '-y', str(key)]).stdout.decode().strip()
        archive = root / 'snapshot.db.age'
        archive.write_bytes(command(['age', '-r', recipient], raw).stdout)
        os.chmod(archive, 0o600)
        report['archiveSHA256'] = digest(archive.read_bytes())
        recovered = command(['age', '-d', '-i', str(key), str(archive)]).stdout
        assert digest(recovered) == report['snapshotSHA256']
        del raw
        node_command(source['node'], 'rm', snapshot)
        report['backupCompletedAt'] = time.time()
        save()
        print('Encrypted control-plane snapshot decrypted and checksum verified', flush=True)
        initial = ','.join(m['name'] + '=' + m['peerURL'] for m in members)
        # Prepare all replacement stores from the decrypted archive before
        # removing original stores. No replacement process runs alongside one.
        for member in members:
            node = member['node']
            node_command(node, 'mkdir', '-m', '700', scratch)
            node_command(node, 'sh', '-ec', 'umask 077; cat > "$1"', 'snapshot', scratch + '/snapshot.db', data=recovered)
            mount = 'type=bind,src=' + scratch + ',dst=/work,options=rbind:rw'
            node_command(node, 'ctr', '-n', 'k8s.io', 'run', '--rm', '--mount', mount,
                member['image'], token, '/usr/local/bin/etcdutl', 'snapshot', 'restore', '/work/snapshot.db',
                '--name', member['name'], '--data-dir', '/work/data', '--initial-cluster', initial,
                '--initial-advertise-peer-urls', member['peerURL'], '--initial-cluster-token', token,
                '--bump-revision', '1000000000', '--mark-compacted')
        del recovered
        report['lossStartedAt'] = time.time(); save()
        for prefix in ('kube-apiserver-', 'etcd-'):
            for pod in control_pods:
                if pod['name'].startswith(prefix):
                    node_command(pod['node'], 'crictl', 'stop', '--timeout', '10', pod['containerID'])
                    state = json.loads(node_command(pod['node'], 'crictl', 'inspect', pod['containerID']).stdout)
                    assert state['status']['state'] == 'CONTAINER_EXITED'
        # Every member is stopped; replace only the validated owned nodes' data.
        replacement_started = True
        for member in members:
            node = member['node']
            node_command(node, 'rm', '-rf', '/var/lib/etcd')
            node_command(node, 'mv', scratch + '/data', '/var/lib/etcd')
            report['installedStores'].append(node); save()
        restored = True
        print('All original etcd stores replaced from the same encrypted snapshot', flush=True)
        for node in nodes:
            node_command(node, 'systemctl', 'start', 'kubelet')
        stopped = False
        wait(lambda: command(kubectl + ['get', '--raw=/readyz'], required=False).returncode == 0, 180)
        wait(lambda: ready_replacements(before, get('pods', namespace=e['E2E_OPERATOR_NAMESPACE'])['items']), 240)
        current = get('pods', namespace=e['E2E_OPERATOR_NAMESPACE'])['items']
        report['runtimeAfter'] = [{'name': p['metadata']['name'], 'uid': p['metadata']['uid'], 'containerID': p['status']['containerStatuses'][0]['containerID'], 'imageID': p['status']['containerStatuses'][0]['imageID']} for p in current if not p['metadata'].get('deletionTimestamp')]
        assert get('namespace', e['RESULT_PROBE_NAMESPACE'])['metadata']['uid'] == report['namespaceUID']
        after = inventory()
        assert original == after, 'Restored result/key identities or bytes changed'
        report['retainedAfter'] = after
        def recovered_members():
            current_members = get('pods', namespace='kube-system')['items']
            states = []
            for member in members:
                pod = next(p for p in current_members if p['metadata']['name'] == 'etcd-' + member['node'])
                current_id = pod['status']['containerStatuses'][0]['containerID'].removeprefix('containerd://')
                if current_id == member['containerID']:
                    return False
                current_member = dict(member, containerID=current_id)
                states.append(json.loads(etcd(current_member, 'endpoint', 'status', '--write-out=json').stdout)[0]['Status'])
                old_revision = report['sourceEtcd'][0]['header']['revision']
                compacted = etcd(current_member, 'get', '/ptah-restoration-compaction-check', '--rev=' + str(old_revision), required=False)
                assert compacted.returncode and b'required revision has been compacted' in compacted.stderr
            return states
        states = wait(recovered_members, 180)
        assert len({s['header']['cluster_id'] for s in states}) == 1
        assert states[0]['header']['cluster_id'] != report['sourceEtcd'][0]['header']['cluster_id']
        assert {s['header']['member_id'] for s in states}.isdisjoint(s['header']['member_id'] for s in report['sourceEtcd'])
        assert min(s['header']['revision'] for s in states) >= max(s['header']['revision'] for s in report['sourceEtcd']) + 1000000000
        report.update(restoredEtcd=states, stateRestoredAt=time.time(), status='PASS', revisionBump=1000000000, markedCompacted=True)
        print('Restored control plane: original result/key UIDs and bytes, new etcd members and runtime processes', flush=True)
    except BaseException as error:
        report.update(status='FAIL', failure=type(error).__name__ + ': ' + str(error))
        raise
    finally:
        # Before replacement, restarting kubelets restores the original store.
        # A partial replacement must stay fenced for recovery from the archive.
        if stopped and (not replacement_started or restored):
            for node in report['stoppedKubelets']:
                node_command(node, 'systemctl', 'start', 'kubelet', required=False)
        if restored or not replacement_started:
            for member in report.get('membersBefore', []):
                node_command(member['node'], 'rm', '-rf', scratch, required=False)
        report['completedAt'] = time.time(); save()
