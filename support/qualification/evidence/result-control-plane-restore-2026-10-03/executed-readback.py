import hashlib,json,subprocess
from pathlib import Path
root=Path('/private/tmp/ptah-result-restored-state-20261003')
e=dict(l.split('=',1) for l in Path('/private/tmp/ptah-result-restart.env').read_text().splitlines() if '=' in l)
report=json.loads((root/'private/report.json').read_text());case=json.loads((root/'evidence/first-harvest-maximum.json').read_text())
assert (root/'exit-code').read_text().strip()=='0'
assert report['status']=='PASS' and case['status']=='passed' and case['converged'] and case['approvedApplyJobs']==1
assert case['planBytes']==8388608 and case['databaseDefault']=='1398006:1398002:4'
assert report['retainedBefore']==report['retainedAfter'] and report['retainedBefore']
assert hashlib.sha256((root/'result_control_plane_restore.py.executed').read_bytes()).hexdigest()==report['procedureSHA256']
assert hashlib.sha256((root/'result_first_harvest.py.executed').read_bytes()).hexdigest()==case['procedureSHA256']
assert hashlib.sha256((root/'private/snapshot.db.age').read_bytes()).hexdigest()==report['archiveSHA256']
k=['kubectl','--kubeconfig',e['E2E_KUBECONFIG'],'--request-timeout=20s']
d=['docker','--config',e['E2E_DOCKER_CONFIG'],'--context',e['E2E_DOCKER_CONTEXT']]
def run(args):return subprocess.run(args,capture_output=True,text=True,check=True).stdout
def get(kind,name=None,ns=None):return json.loads(run(k+(['-n',ns] if ns else [])+['get',kind]+([name] if name else [])+['-o','json']))
ns='ptah-result-restored-state'
resource=get('ptahschema','first-harvest',ns)
assert resource['metadata']['uid']==case['resourceUID'] and not resource['status'].get('activeOperation')
assert any(c['type']=='InSync' and c['status']=='True' and c['observedGeneration']==resource['metadata']['generation'] for c in resource['status']['conditions'])
jobs=get('jobs',ns=ns)['items']
assert len([j for j in jobs if j['spec']['template'].get('metadata',{}).get('labels',{}).get('operator.ptah.run/operation')=='apply'])==1
saved_rb=json.loads((root/'evidence/manager-rolebinding-before.json').read_text())
assert get('rolebinding',saved_rb['metadata']['name'],e['E2E_OPERATOR_NAMESPACE'])['subjects']==saved_rb['subjects']
for kind in ('validatingadmissionpolicy','validatingadmissionpolicybinding'):
 assert not run(k+['get',kind,ns+'-gate','--ignore-not-found','-o','name']).strip()
runtime=get('pods',ns=e['E2E_OPERATOR_NAMESPACE'])['items']
runtime=[p for p in runtime if not p['metadata'].get('deletionTimestamp')]
assert len(runtime)==3 and all(any(c['type']=='Ready' and c['status']=='True' for c in p['status']['conditions']) for p in runtime)
assert {p['status']['containerStatuses'][0]['imageID'] for p in runtime}=={p['imageID'] for p in report['runtimeBefore']}
node_rows=[]
for node in report['stoppedKubelets']:
 assert run(d+['inspect','--format','{{index .Config.Labels "io.x-k8s.kind.cluster"}}',node]).strip()==e['E2E_KIND_CLUSTER_NAME']
 assert run(d+['exec',node,'systemctl','is-active','kubelet']).strip()=='active'
 leftovers=run(d+['exec',node,'sh','-c','for p in /tmp/ptah-control-restore-*; do if [ -e "$p" ]; then printf "%s\\n" "$p"; fi; done'])
 assert not leftovers.strip()
 containers=run(d+['exec',node,'ctr','-n','k8s.io','containers','list','-q'])
 assert not any(line.startswith('ptah-control-restore-') for line in containers.splitlines())
 node_rows.append({'node':node,'kubeletActive':True,'restorePlaintextAbsent':True,'restoreContainersAbsent':True})
result={'status':'passed','retainedObjects':len(report['retainedBefore']),'nativeConvergence':True,'approvedApplyJobs':1,'leaderBindingRestored':True,'admissionGateAbsent':True,'readyRuntimePods':3,'nodes':node_rows,'privateArchiveSHA256':report['archiveSHA256']}
(root/'readback.json').write_text(json.dumps(result,indent=2)+'\n')
print(json.dumps(result))
