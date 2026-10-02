"""Read back native collection, pinned records, and restored API readiness."""
import base64
import datetime as dt
import hashlib
import json
from pathlib import Path
import subprocess

root = Path('/private/tmp/ptah-namespace-retirement-installed')
env = dict(line.split('=', 1) for line in Path('/private/tmp/ptah-result-restart.env').read_text().splitlines() if '=' in line)
report = json.loads((root / 'report.json').read_text())
assert json.loads((root / 'verification.json').read_text())['status'] == 'passed'
assert len(json.loads((root / 'audit-restored.json').read_text())) == 3
assert hashlib.sha256((root / 'probe.py').read_bytes()).hexdigest() == report['procedureSHA256']

def get(kind, namespace=None, name=None):
    args = ['kubectl', '--kubeconfig', env['E2E_KUBECONFIG'], '--request-timeout=20s']
    if namespace:
        args += ['-n', namespace]
    args += ['get', kind] + ([name] if name else []) + ['-o', 'json', '--ignore-not-found']
    p = subprocess.run(args, capture_output=True, text=True, check=True, timeout=30)
    return json.loads(p.stdout) if p.stdout.strip() else None

for namespace in report['cases']:
    assert get('namespace', name=namespace) is None
control = report['pinnedControl']
resource = get('ptahmigration', control['namespace'], 'lost-ack')
assert resource['metadata']['uid'] == control['resourceUID']
assert resource['status']['lastRun']['jobUID'] == control['jobUID']
current = {r['metadata']['name']: r for r in get('ptahresultrecords', control['namespace'])['items']}
for expected in control['records']:
    r = current[expected['name']]
    m = r['metadata']
    observed = {'name': m['name'], 'uid': m['uid'], 'type': r['spec']['type'], 'createdAt': m['creationTimestamp'], 'owners': m.get('ownerReferences', []), 'dataSHA256': hashlib.sha256(base64.b64decode(r['spec']['data'])).hexdigest()}
    assert observed == expected
assert len(control['records']) == 4
pods = get('pods', 'kube-system')['items']
apis = [p for p in pods if p['metadata']['name'].startswith('kube-apiserver-')]
assert len(apis) == 3
for p in apis:
    assert any(c['type'] == 'Ready' and c['status'] == 'True' for c in p['status']['conditions'])
    assert not any(a.startswith('--audit-') for a in p['spec']['containers'][0]['command'])
deployment = get('deployment', env['E2E_OPERATOR_NAMESPACE'], env['E2E_CONTROLLER_NAME'])
assert deployment['spec']['replicas'] == deployment['status']['readyReplicas'] == 2
assert deployment['spec']['template']['spec']['containers'][0]['image'] == report['runtime']['image']
result = {'status': 'passed', 'observedAt': dt.datetime.now(dt.timezone.utc).isoformat(), 'namespacesAbsent': list(report['cases']), 'pinnedRecordsUnchanged': 4, 'readyAPIServers': 3, 'temporaryAuditRemoved': True, 'readyManagers': 2, 'managerImage': report['runtime']['image']}
(root / 'final-readback.json').write_text(json.dumps(result, indent=2) + '\n')
print(json.dumps(result))
