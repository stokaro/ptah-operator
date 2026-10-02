import json,subprocess
from pathlib import Path
root=Path('/private/tmp/ptah-result-mysql-maximum-20261003')
e=dict(l.split('=',1) for l in Path('/private/tmp/ptah-result-restart.env').read_text().splitlines() if '=' in l)
case=json.loads((root/'evidence/first-harvest-maximum.json').read_text())
assert case['converged'] and json.loads((root/'readback.json').read_text())['status']=='passed'
ns=case['namespace']; assert ns=='ptah-result-mysql-maximum'
k=['kubectl','--kubeconfig',e['E2E_KUBECONFIG'],'--request-timeout=20s']
def run(args,data=None):return subprocess.run(args,input=data,capture_output=True,text=True,check=True).stdout
namespace=json.loads(run(k+['get','namespace',ns,'-o','json']))
assert namespace['metadata']['labels']['operator.ptah.run/acceptance-owner']==e['E2E_KIND_CLUSTER_NAME']
resource=json.loads(run(k+['-n',ns,'get','ptahschema','first-harvest','-o','json']))
assert resource['metadata']['uid']==case['resourceUID'] and not resource['status'].get('activeOperation')
run(k+['-n',ns,'patch','ptahschema','first-harvest','--type=merge','-p',json.dumps({'spec':{'suspend':True}})])
run(k+['delete','namespace',ns,'--wait=false'])
mysql=k+['-n',e['E2E_TEST_NAMESPACE'],'exec','-i','deployment/demo-mysql','--','sh','-ec','MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql --protocol=TCP -h 127.0.0.1 -u root --batch --skip-column-names']
run(mysql,'DROP DATABASE `result_mysql_maximum_020`;\n')
assert run(mysql,"SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name='result_mysql_maximum_020';\n").strip()=='0'
current=run(k+['get','namespace',ns,'--ignore-not-found','-o','json'])
result={'databaseAbsent':True,'namespace':ns,'namespaceUID':namespace['metadata']['uid'],'namespaceRemovalRequested':True,'namespaceAbsent':not bool(current.strip()),'retentionBypassed':False,'sharedLabRetained':True}
if current.strip():
 metadata=json.loads(current)['metadata'];assert metadata['uid']==namespace['metadata']['uid'] and metadata['deletionTimestamp']
 result['namespaceDeletionTimestamp']=metadata['deletionTimestamp']
(root/'cleanup.json').write_text(json.dumps(result,indent=2)+'\n')
print(json.dumps(result))
