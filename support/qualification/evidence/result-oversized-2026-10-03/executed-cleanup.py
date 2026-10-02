import json,subprocess
from pathlib import Path
root=Path('/private/tmp/ptah-result-oversized-20261003')
e=dict(l.split('=',1) for l in Path('/private/tmp/ptah-result-restart.env').read_text().splitlines() if '=' in l)
report=json.loads((root/'readback.json').read_text());assert report['status']=='passed' and len(report['cases'])==2
k=['kubectl','--kubeconfig',e['E2E_KUBECONFIG'],'--request-timeout=20s']
def run(args,data=None):return subprocess.run(args,input=data,capture_output=True,text=True,check=True).stdout
results=[]
for case in report['cases']:
 engine=case['engine'];ns=case['namespace'];database='result_oversized_'+engine+'_020'
 namespace=json.loads(run(k+['get','namespace',ns,'-o','json']))
 assert namespace['metadata']['uid']==case['namespaceUID'] and namespace['metadata']['labels']['operator.ptah.run/acceptance-owner']==e['E2E_KIND_CLUSTER_NAME']
 resource=json.loads(run(k+['-n',ns,'get','ptahschema','first-harvest','-o','json']))
 assert resource['metadata']['uid']==case['resourceUID'] and not resource['status'].get('activeOperation',{}).get('jobUID')
 run(k+['-n',ns,'patch','ptahschema','first-harvest','--type=merge','-p',json.dumps({'spec':{'suspend':True}})])
 run(k+['delete','namespace',ns,'--wait=false'])
 if engine=='mysql':
  args=k+['-n',e['E2E_TEST_NAMESPACE'],'exec','-i','deployment/demo-mysql','--','sh','-ec','MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql --protocol=TCP -h 127.0.0.1 -u root --batch --skip-column-names']
  run(args,'DROP DATABASE `'+database+'`;\n')
  remaining=run(args,"SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name='"+database+"';\n").strip()
 else:
  args=['docker','--config',e['E2E_DOCKER_CONFIG'],'--context',e['E2E_DOCKER_CONTEXT'],'exec','-i',e['E2E_EXTERNAL_POSTGRES_CONTAINER_ID'],'sh','-ec','PGPASSWORD="$POSTGRES_PASSWORD" exec psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -At -v ON_ERROR_STOP=1']
  run(args,'DROP DATABASE '+database+' WITH (FORCE);\n')
  remaining=run(args,"SELECT count(*) FROM pg_database WHERE datname='"+database+"';\n").strip()
 assert remaining=='0'
 current=run(k+['get','namespace',ns,'--ignore-not-found','-o','json'])
 result={'engine':engine,'databaseAbsent':True,'namespace':ns,'namespaceUID':case['namespaceUID'],'namespaceRemovalRequested':True,'namespaceAbsent':not bool(current.strip()),'retentionBypassed':False}
 if current.strip():
  metadata=json.loads(current)['metadata'];assert metadata['uid']==case['namespaceUID'] and metadata['deletionTimestamp'];result['namespaceDeletionTimestamp']=metadata['deletionTimestamp']
 results.append(result)
report={'cases':results,'sharedLabRetained':True};(root/'cleanup.json').write_text(json.dumps(report,indent=2)+'\n');print(json.dumps(report))
