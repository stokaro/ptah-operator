#!/usr/bin/env python3
"""Restore a real encrypted etcd snapshot and verify durable result/key bytes.

Storage recovery only: no replacement controllers or SQL execution run here.
The source lab survives. Never publish the private output directory.
"""
import base64
import hashlib
import json
import os
from pathlib import Path
import secrets
import subprocess
import time
from operator_restore import OperatorProbe
from result_first_harvest import publication


def digest(data):
    return hashlib.sha256(data).hexdigest()


def main():
    if not __debug__:
        raise RuntimeError('Assertions must be enabled')
    os.umask(0o077)
    E = os.environ
    root = Path(E['RESULT_RESTORE_PRIVATE_DIR']); root.mkdir(mode=0o700)
    token = 'ptah-result-restore-' + secrets.token_hex(6)
    node = E['E2E_KIND_CLUSTER_NAME'] + '-control-plane'
    scratch = '/tmp/' + token
    snapshot = '/var/lib/etcd/' + token + '.db'
    task = token + '-reader'
    docker = ['docker', '--context', E['E2E_DOCKER_CONTEXT']]
    node_exec = docker + ['exec', node]
    ctr = node_exec + ['ctr', '-n', 'k8s.io']
    kubectl = ['kubectl', '--kubeconfig', E['E2E_KUBECONFIG'], '--request-timeout=30s']
    source_ctl = kubectl + ['-n', 'kube-system', 'exec', 'etcd-' + node, '--', 'etcdctl',
                  '--endpoints=https://127.0.0.1:2379', '--cacert=/etc/kubernetes/pki/etcd/ca.crt',
                  '--cert=/etc/kubernetes/pki/etcd/healthcheck-client.crt', '--key=/etc/kubernetes/pki/etcd/healthcheck-client.key']
    report = {'status': 'running', 'procedureSHA256': digest(Path(__file__).read_bytes()),
              'sourceRevision': subprocess.check_output(['git', 'rev-parse', 'HEAD']).decode().strip(),
              'startedAt': time.time(), 'cohorts': [], 'steps': []}
    schemas = []
    task_started = False

    def save():
        (root / 'report.json').write_text(json.dumps(report, indent=2) + '\n')

    def run(args, data=None, required=True):
        p = subprocess.run(args, input=data, capture_output=True, timeout=240)
        report['steps'].append({'program': args[0], 'exitCode': p.returncode, 'at': time.time()})
        if p.returncode:
            (root / ('failure-' + str(len(report['steps'])) + '.private.txt')).write_bytes(p.stderr)
            if required:
                raise RuntimeError('Command failed; private diagnostics retained: ' + args[0])
        return p

    def obj(kind, name, ns):
        return json.loads(run(kubectl + ['-n', ns, 'get', kind] + ([name] if name else []) + ['-o', 'json']).stdout)

    def patch(ns, spec):
        return json.loads(run(kubectl + ['-n', ns, 'patch', 'ptahschema', 'lost-ack', '--type=merge', '-p', json.dumps({'spec': spec}), '-o', 'json']).stdout)

    def wait(fn, seconds=180):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            value = fn()
            if value:
                return value
            time.sleep(1)
        raise RuntimeError('Timed out: ' + fn.__name__)

    def get_values(command, prefix, revision=None):
        args = command + ['get', prefix, '--prefix', '--write-out=json']
        if revision is not None:
            args += ['--rev=' + str(revision)]
        result = json.loads(run(args).stdout)
        return {base64.b64decode(k['key']).decode(): base64.b64decode(k['value']) for k in result.get('kvs', [])}

    def reader(*args):
        return ctr + ['tasks', 'exec', '--exec-id', secrets.token_hex(6), task, '/usr/local/bin/etcdctl', '--endpoints=http://127.0.0.1:2379', *args]

    try:
        owner = run(docker + ['inspect', '--format', '{{index .Config.Labels "io.x-k8s.kind.cluster"}}', node]).stdout.decode().strip()
        assert owner == E['E2E_KIND_CLUSTER_NAME'] and owner.startswith('ptah-e2e-')
        etcd_pod = obj('pod', 'etcd-' + node, 'kube-system')
        image = etcd_pod['spec']['containers'][0]['image']
        report['etcdImage'] = image
        report['etcdImageID'] = etcd_pod['status']['containerStatuses'][0]['imageID']
        report['sourceEtcd'] = json.loads(run(source_ctl + ['endpoint', 'status', '--write-out=json']).stdout)[0]['Status']
        # Previous successful schemas have legitimately collected old results.
        # Refresh only read operations under OnApproval; no new SQL is authorized.
        for engine in ('pg', 'mysql'):
            ns = 'ptah-result-schema-budget-' + engine
            original = obj('ptahschema', 'lost-ack', ns)
            assert original['spec']['suspend'] and not original.get('status', {}).get('activeOperation')
            schemas.append((ns, original['metadata']['uid'], original['spec']['policy']))
            current = patch(ns, {'suspend': False, 'policy': {'apply': 'OnApproval'}})
            generation = current['metadata']['generation']
            def observed():
                r = obj('ptahschema', 'lost-ack', ns)
                assert r['metadata']['uid'] == original['metadata']['uid']
                return r if not r.get('status', {}).get('activeOperation') and any(c['type'] == 'Ready' and c['status'] == 'True' and c.get('observedGeneration') == generation for c in r.get('status', {}).get('conditions', [])) else None
            wait(observed)
            jobs = obj('jobs', None, ns)['items']
            assert not any(j['spec']['template']['metadata']['labels'].get('operator.ptah.run/operation') == 'apply' for j in jobs)
            patch(ns, {'suspend': True})
        # Exercise the updated inventory against real records, not a handcrafted fixture.
        selected_objects = []
        inventories = []
        for family, stem in [('PtahSchema', 'ptah-result-schema-budget-'), ('PtahMigration', 'ptah-result-partial-')]:
            for engine in ('pg', 'mysql'):
                ns = stem + engine
                resource = obj(family.lower(), 'lost-ack', ns)
                assert resource['spec']['suspend'] and not resource.get('status', {}).get('activeOperation')
                probe = object.__new__(OperatorProbe)
                probe.envs = dict(E); probe.namespace = ns; probe.root = root
                probe.report = {'checks': {}, 'steps': []}
                inventory = probe.result_backup()
                assert inventory['enabled']
                inventories.append(inventory)
                selected_objects += [resource] + inventory['records'] + inventory['credentialProjections']
                selected_objects += [inventory['trust'], inventory['journal'], inventory['enrollmentPolicy']]
                report['cohorts'].append({'family': family, 'engine': engine, 'namespace': ns,
                    'resourceUID': resource['metadata']['uid'], 'recordCount': len(inventory['records'])})
        key = root / 'identity.private'; wrong = root / 'wrong-identity.private'
        run(['age-keygen', '-o', str(key)]); run(['age-keygen', '-o', str(wrong)])
        recipient = run(['age-keygen', '-y', str(key)]).stdout.decode().strip()
        report['backupStartedAt'] = time.time()
        run(source_ctl + ['snapshot', 'save', snapshot])
        status = json.loads(run(kubectl + ['-n', 'kube-system', 'exec', 'etcd-' + node, '--', 'etcdutl', 'snapshot', 'status', snapshot, '--write-out=json']).stdout)
        report['snapshot'] = status
        snapshot_bytes = run(node_exec + ['cat', snapshot]).stdout
        report['snapshotSHA256'] = digest(snapshot_bytes)
        encrypted = run(['age', '-r', recipient], snapshot_bytes).stdout
        archive = root / 'snapshot.db.age'; archive.write_bytes(encrypted)
        report['archiveSHA256'] = digest(encrypted)
        report['archiveBytes'] = len(encrypted)
        run(node_exec + ['rm', snapshot])
        assert run(['age', '-d', '-i', str(wrong), str(archive)], required=False).returncode != 0
        restored_bytes = run(['age', '-d', '-i', str(key), str(archive)]).stdout
        assert digest(restored_bytes) == report['snapshotSHA256']
        del encrypted, snapshot_bytes
        report['backupCompletedAt'] = time.time()
        run(node_exec + ['mkdir', '-m', '700', scratch])
        run(docker + ['exec', '-i', node, 'sh', '-ec', 'umask 077; cat > "$1"', 'restore', scratch + '/snapshot.db'], restored_bytes)
        del restored_bytes
        mount = 'type=bind,src=' + scratch + ',dst=/work,options=rbind:rw'
        run(ctr + ['run', '--rm', '--mount', mount, image, token + '-unpack', '/usr/local/bin/etcdutl',
            'snapshot', 'restore', '/work/snapshot.db', '--name', 'restored', '--data-dir', '/work/data',
            '--initial-cluster', 'restored=http://127.0.0.1:2380', '--initial-advertise-peer-urls', 'http://127.0.0.1:2380', '--initial-cluster-token', token])
        # No --net-host or CNI: this member has its own network namespace.
        run(ctr + ['run', '-d', '--null-io', '--cpus', '0.5', '--memory-limit', '536870912', '--mount', mount,
            image, task, '/usr/local/bin/etcd', '--name', 'restored', '--data-dir', '/work/data',
            '--listen-client-urls', 'http://127.0.0.1:2379', '--advertise-client-urls', 'http://127.0.0.1:2379',
            '--listen-peer-urls', 'http://127.0.0.1:2380', '--initial-advertise-peer-urls', 'http://127.0.0.1:2380',
            '--initial-cluster', 'restored=http://127.0.0.1:2380', '--initial-cluster-token', token])
        task_started = True
        def healthy():
            return run(reader('endpoint', 'health'), required=False).returncode == 0
        wait(healthy, 30)
        report['restoredEtcd'] = json.loads(run(reader('endpoint', 'status', '--write-out=json')).stdout)[0]['Status']
        assert report['restoredEtcd']['header']['cluster_id'] != report['sourceEtcd']['header']['cluster_id']
        assert report['restoredEtcd']['header']['member_id'] != report['sourceEtcd']['header']['member_id']
        report['restoredNetworkNamespace'] = json.loads(run(ctr + ['containers', 'info', task]).stdout)['Spec']['linux']['namespaces']
        prefixes = {'/registry/operator.ptah.run/ptahresultrecords/' + r['namespace'] + '/' for r in report['cohorts']}
        plurals = {'PtahSchema': 'ptahschemas', 'PtahMigration': 'ptahmigrations', 'Secret': 'secrets', 'ConfigMap': 'configmaps', 'PtahResultRecord': 'ptahresultrecords'}
        object_keys = {}
        for value in selected_objects:
            kind = value['kind']; meta = value['metadata']
            prefix = '/registry/' + ('operator.ptah.run/' if kind.startswith('Ptah') else '') + plurals[kind] + '/' + meta['namespace'] + '/' + meta['name']
            object_keys[prefix] = value
            prefixes.add(prefix)
        before, after = {}, {}
        for prefix in sorted(prefixes):
            before.update(get_values(source_ctl, prefix, status['revision']))
            after.update(get_values(reader(), prefix))
        assert before and before == after
        rows = []
        restored_records = {}
        for path, value in object_keys.items():
            assert path in after
            raw = after[path]; meta = value['metadata']
            assert meta['uid'].encode() in raw
            if value['kind'].startswith('Ptah'):
                loaded = json.loads(raw)
                assert loaded['metadata']['uid'] == meta['uid'] and loaded['spec'] == value['spec']
                if value['kind'] == 'PtahResultRecord':
                    restored_records.setdefault(meta['namespace'], {})[meta['name']] = loaded
            else:
                for data in value.get('data', {}).values():
                    expected = base64.b64decode(data) if value['kind'] == 'Secret' else data.encode()
                    assert expected in raw
            rows.append({'path': path, 'kind': value['kind'], 'uid': meta['uid'], 'sourceSHA256': digest(before[path]), 'restoredSHA256': digest(raw)})
        for cohort in report['cohorts']:
            records = restored_records[cohort['namespace']]
            receipts = []
            for record in records.values():
                if record['spec']['type'] != 'intent' or record['metadata']['name'] + '-complete' not in records:
                    continue
                binding = json.loads(base64.b64decode(record['spec']['data']))['binding']
                assert binding['uid'] == cohort['resourceUID']
                _, receipt, payload = publication(records, binding['jobUID'])
                receipts.append({'operation': binding['operation'], 'jobUID': binding['jobUID'], 'receiptUID': receipt['metadata']['uid'], 'payloadSHA256': digest(payload)})
            assert receipts
            cohort['restoredReceipts'] = receipts
        report.update(storageRows=rows, wrongKeyRefused=True, restoredFromEncryptedArchive=True,
                      storageRestoredAt=time.time(), status='passed')
    except BaseException as error:
        report.update(status='failed', failure=str(error))
        raise
    finally:
        cleanup = []
        if task_started:
            cleanup.append(run(ctr + ['tasks', 'kill', '--signal', 'SIGTERM', task], required=False).returncode)
            time.sleep(1)
            cleanup.append(run(ctr + ['tasks', 'delete', task], required=False).returncode)
            cleanup.append(run(ctr + ['containers', 'delete', task], required=False).returncode)
        cleanup.append(run(node_exec + ['rm', '-f', snapshot], required=False).returncode)
        cleanup.append(run(node_exec + ['rm', '-rf', scratch], required=False).returncode)
        for ns, uid, policy in schemas:
            assert obj('ptahschema', 'lost-ack', ns)['metadata']['uid'] == uid
            patch(ns, {'suspend': True, 'policy': policy})
        report['cleanupExitCodes'] = cleanup
        if any(cleanup):
            report['status'] = 'failed'
        report['completedAt'] = time.time(); save()
    assert report['status'] == 'passed'
    print(json.dumps({'status': report['status'], 'cohorts': len(report['cohorts']), 'storageRows': len(report['storageRows']), 'privateArchiveRetained': str(root)}))


if __name__ == '__main__':
    main()
