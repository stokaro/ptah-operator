import base64, hashlib, json, subprocess
from pathlib import Path
root=Path('/private/tmp/ptah-result-mysql-maximum-20261003')
e=dict(l.split('=',1) for l in Path('/private/tmp/ptah-result-restart.env').read_text().splitlines() if '=' in l)
assert (root/'exit-code').read_text().strip()=='0'
case=json.loads((root/'evidence/first-harvest-maximum.json').read_text())
assert case['status']=='passed' and case['converged'] and case['approvedApplyJobs']==1
assert case['planBytes']==8388608 and case['planChunks']==16 and case['databaseDefault']=='100:1393516:1393515'
assert hashlib.sha256((root/'first-harvest.py.executed').read_bytes()).hexdigest()==case['procedureSHA256']
k=['kubectl','--kubeconfig',e['E2E_KUBECONFIG'],'--request-timeout=20s']
def run(args): return subprocess.run(args,capture_output=True,text=True,check=True).stdout
def get(kind,name=None,ns=None):return json.loads(run(k+(['-n',ns] if ns else [])+['get',kind]+([name] if name else [])+['-o','json']))
ns=case['namespace']
resource=get('ptahschema','first-harvest',ns)
assert resource['metadata']['uid']==case['resourceUID'] and not resource['status'].get('activeOperation')
assert any(c['type']=='InSync' and c['status']=='True' and c['observedGeneration']==resource['metadata']['generation'] for c in resource['status']['conditions'])
plans=get('ptahschemaplans',ns=ns)['items']; assert len(plans)==1
plan=plans[0]; assert plan['metadata']['uid']==case['planUID']
raw=b''.join(base64.b64decode(get('ptahschemaplanchunk',ref['name'],ns)['spec']['data']) for ref in plan['spec']['chunks'])
assert len(raw)==8388608 and 'sha256:'+hashlib.sha256(raw).hexdigest()==case['planContentDigest']
document=json.loads(raw); assert document['dialect']=='mysql' and len(document['statements'])==100
repeated,suffix=case['repeatedCharacters'],case['asciiSuffix']
assert raw.count(b'\\u003c')==repeated
for i in range(100):
 count=repeated//100+(i<repeated%100); ascii_count=suffix if i==99 else 0
 prefix='-- MYSQL TABLE: e2e_plan_size_limit_%03d --\nCREATE TABLE '%i
 found=[s for s in document['statements'] if s['sql'].startswith(prefix)]
 assert len(found)==1 and found[0]['sql'].count('<')==count
 assert found[0]['sql'].count("DEFAULT '"+'<'*count+'x'*ascii_count+"'")==1
jobs=get('jobs',ns=ns)['items']; applies=[j for j in jobs if j['spec']['template'].get('metadata',{}).get('labels',{}).get('operator.ptah.run/operation')=='apply']
assert len(applies)==1 and applies[0]['metadata']['uid']==case['applyJobUID']
assert any(c['type']=='Complete' and c['status']=='True' for c in applies[0]['status']['conditions'])
assert not run(k+['-n',ns,'get','pod',case['binding']['podName'],'--ignore-not-found','-o','name']).strip()
saved_rb=json.loads((root/'evidence/manager-rolebinding-before.json').read_text())
assert get('rolebinding',saved_rb['metadata']['name'],e['E2E_OPERATOR_NAMESPACE'])['subjects']==saved_rb['subjects']
for kind in ('validatingadmissionpolicy','validatingadmissionpolicybinding'):
 assert not run(k+['get',kind,ns+'-gate','--ignore-not-found','-o','name']).strip()
runtime=get('pods',ns=e['E2E_OPERATOR_NAMESPACE'])['items']; runtime=[p for p in runtime if not p['metadata'].get('deletionTimestamp')]
assert len(runtime)==3 and all(any(c['type']=='Ready' and c['status']=='True' for c in p['status']['conditions']) for p in runtime)
nodes=get('nodes')['items']; assert len(nodes)==4
kubelets=[]
for node in nodes:
 name=node['metadata']['name']; config=json.loads(run(k+['get','--raw','/api/v1/nodes/'+name+'/proxy/configz']))['kubeletconfig']
 assert config['containerLogMaxSize']=='10Mi'
 kubelets.append({'node':name,'uid':node['metadata']['uid'],'kubeletVersion':node['status']['nodeInfo']['kubeletVersion'],'architecture':node['status']['nodeInfo']['architecture'],'containerLogMaxSize':config['containerLogMaxSize']})
query="SELECT CONCAT(COUNT(*), ':', SUM(CHAR_LENGTH(column_default)), ':', SUM(CHAR_LENGTH(column_default)-CHAR_LENGTH(REPLACE(column_default,'<','')))) FROM information_schema.columns WHERE table_schema='result_mysql_maximum_020' AND table_name LIKE 'e2e_plan_size_limit_%' AND column_name='payload'; SELECT VERSION();"
actual=subprocess.run(k+['-n',e['E2E_TEST_NAMESPACE'],'exec','-i','deployment/demo-mysql','--','sh','-ec','MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql --protocol=TCP -h 127.0.0.1 -u root --batch --skip-column-names'],input=query,text=True,capture_output=True,check=True).stdout.strip().splitlines()
assert actual[0]==case['databaseDefault']
result={'status':'passed','originalPodAbsent':True,'planBytes':len(raw),'nativeStatements':100,'escapedCharacters':repeated,'nativeDatabaseDefault':actual[0],'databaseVersion':actual[1],'approvedApplyJobs':1,'leaderBindingRestored':True,'admissionGateAbsent':True,'kubelets':kubelets,'runtime':[{'name':p['metadata']['name'],'uid':p['metadata']['uid'],'images':[{'image':c['image'],'imageID':c['imageID']} for c in p['status']['containerStatuses']]} for p in runtime]}
(root/'readback.json').write_text(json.dumps(result,indent=2)+'\n')
print(json.dumps({key:value for key,value in result.items() if key not in ('runtime','kubelets')}))
