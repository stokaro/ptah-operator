import json,re,subprocess
from pathlib import Path
root=Path('/private/tmp/ptah-result-restored-state-20261003')
e=dict(l.split('=',1) for l in Path('/private/tmp/ptah-result-restart.env').read_text().splitlines() if '=' in l)
case=json.loads((root/'evidence/first-harvest-maximum.json').read_text())
assert case['converged'] and json.loads((root/'readback.json').read_text())['status']=='passed'
ns=case['namespace']; database=json.loads((root/'inputs.json').read_text())['RESULT_PROBE_DATABASE']
assert ns=='ptah-result-restored-state' and re.fullmatch('[a-z][a-z0-9_]{1,49}',database)
k=['kubectl','--kubeconfig',e['E2E_KUBECONFIG'],'--request-timeout=20s']
d=['docker','--config',e['E2E_DOCKER_CONFIG'],'--context',e['E2E_DOCKER_CONTEXT']]
def run(args,data=None):return subprocess.run(args,input=data,capture_output=True,text=True,check=True).stdout
namespace=json.loads(run(k+['get','namespace',ns,'-o','json']))
assert namespace['metadata']['labels']['operator.ptah.run/acceptance-owner']==e['E2E_KIND_CLUSTER_NAME']
resource=json.loads(run(k+['-n',ns,'get','ptahschema','first-harvest','-o','json']))
assert resource['metadata']['uid']==case['resourceUID'] and not resource['status'].get('activeOperation')
run(k+['-n',ns,'patch','ptahschema','first-harvest','--type=merge','-p',json.dumps({'spec':{'suspend':True}})])
configs=[]
for node in json.loads(run(k+['get','nodes','-o','json']))['items']:
 name=node['metadata']['name']; config=json.loads(run(k+['get','--raw','/api/v1/nodes/'+name+'/proxy/configz']))['kubeletconfig']
 assert config['containerLogMaxSize']=='10Mi'
 configs.append({'node':name,'containerLogMaxSize':config['containerLogMaxSize']})
(root/'kubelets.json').write_text(json.dumps(configs,indent=2)+'\n')
run(k+['delete','namespace',ns,'--wait=false'])
run(d+['exec','-i',e['E2E_EXTERNAL_POSTGRES_CONTAINER_ID'],'sh','-ec','PGPASSWORD="$POSTGRES_PASSWORD" exec psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -At -v ON_ERROR_STOP=1'], 'DROP DATABASE '+database+' WITH (FORCE);\n')
remaining=run(d+['exec','-i',e['E2E_EXTERNAL_POSTGRES_CONTAINER_ID'],'sh','-ec','PGPASSWORD="$POSTGRES_PASSWORD" exec psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -At -v ON_ERROR_STOP=1'], "SELECT count(*) FROM pg_database WHERE datname='"+database+"';\n").strip()
assert remaining=='0'
current=run(k+['get','namespace',ns,'--ignore-not-found','-o','json'])
result={'databaseAbsent':True,'namespace':ns,'namespaceUID':namespace['metadata']['uid'],'namespaceRemovalRequested':True,'namespaceAbsent':not bool(current.strip()),'retentionBypassed':False,'sharedLabRetained':True}
if current.strip():
 metadata=json.loads(current)['metadata'];assert metadata['uid']==namespace['metadata']['uid'] and metadata['deletionTimestamp']
 result['namespaceDeletionTimestamp']=metadata['deletionTimestamp']
(root/'cleanup.json').write_text(json.dumps(result,indent=2)+'\n')
print(json.dumps(result))
