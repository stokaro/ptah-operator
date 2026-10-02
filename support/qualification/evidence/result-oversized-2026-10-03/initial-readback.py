import base64,hashlib,json,subprocess,sys
from pathlib import Path
sys.path.insert(0,str(Path('support/qualification/probes').resolve()))
from result_first_harvest import publication,oversized_refusal
root=Path('/private/tmp/ptah-result-oversized-20261003')
e=dict(l.split('=',1) for l in Path('/private/tmp/ptah-result-restart.env').read_text().splitlines() if '=' in l)
k=['kubectl','--kubeconfig',e['E2E_KUBECONFIG'],'--request-timeout=20s']
def run(args,data=None):return subprocess.run(args,input=data,capture_output=True,text=True,check=True).stdout
def get(kind,name=None,ns=None):return json.loads(run(k+(['-n',ns] if ns else [])+['get',kind]+([name] if name else [])+['-o','json']))
results=[]
for engine in ('postgresql','mysql'):
 folder=root/engine
 if engine=='postgresql':
  assert (folder/'exit-code').read_text().strip()=='1'
  log=(folder/'run.private.log').read_text()
  assert log.rstrip().endswith('RuntimeError: bounded wait failed: refused')
  assert 'Producing Pod and logs removed and both manager processes restarted before first harvest' in log
  observed=json.loads((folder/'observed-resource.json').read_text());manifest=json.loads((folder/'observed-manifest.json').read_text())
  receipt=json.loads((folder/'observed-receipt.json').read_text());binding=manifest['binding']
  case={'status':'passed-after-verifier-correction','nativeRunExitCode':1,'nativePlanBytes':8388609,
        'namespace':binding['namespace'],'resourceUID':binding['uid'],'jobUID':binding['jobUID'],'binding':binding,
        'intentUID':json.loads(base64.b64decode(receipt['spec']['data']))['manifestUID'],
        'receiptUID':receipt['metadata']['uid'],'payloadDigest':manifest['digest']}
  ns=case['namespace']
 else:
  assert (folder/'exit-code').read_text().strip()=='0'
  case=json.loads((folder/'evidence/oversized-refusal.json').read_text());ns=case['namespace']
  assert case['status']=='passed' and case['nativePlanBytes']==8388609
  assert hashlib.sha256((root/'first-harvest-corrected.py.executed').read_bytes()).hexdigest()==case['procedureSHA256']
 resource=get('ptahschema','first-harvest',ns);assert resource['metadata']['uid']==case['resourceUID']
 records={r['metadata']['name']:r for r in get('ptahresultrecords',ns=ns)['items']}
 intent,receipt,data=publication(records,case['jobUID']);assert intent['metadata']['uid']==case['intentUID'] and receipt['metadata']['uid']==case['receiptUID']
 assert 'sha256:'+hashlib.sha256(data).hexdigest()==case['payloadDigest']
 payload=json.loads(data);saved=json.loads((folder/('observed-result.json' if engine=='postgresql' else 'evidence/refused-result.json')).read_text());assert payload==saved
 original=json.loads((folder/('observed-resource.json' if engine=='postgresql' else 'evidence/refused-resource.json')).read_text())
 oversized_refusal(resource,payload,original['status']['source']['digest'],case['binding']['operationID'])
 assert not get('ptahschemaplans',ns=ns)['items'] and not get('ptahschemaplanchunks',ns=ns)['items']
 jobs=get('jobs',ns=ns)['items'];assert len([j for j in jobs if j['metadata']['name'].startswith('ptah-plan-')])==1
 assert not any(j['spec']['template'].get('metadata',{}).get('labels',{}).get('operator.ptah.run/operation')=='apply' for j in jobs)
 assert not run(k+['-n',ns,'get','pod',case['binding']['podName'],'--ignore-not-found','-o','name']).strip()
 rb=json.loads((folder/'evidence/manager-rolebinding-before.json').read_text())
 assert get('rolebinding',rb['metadata']['name'],e['E2E_OPERATOR_NAMESPACE'])['subjects']==rb['subjects']
 for kind in ('validatingadmissionpolicy','validatingadmissionpolicybinding'):
  assert not run(k+['get',kind,ns+'-gate','--ignore-not-found','-o','name']).strip()
 database='result_oversized_'+engine+'_020'
 if engine=='mysql':
  args=k+['-n',e['E2E_TEST_NAMESPACE'],'exec','-i','deployment/demo-mysql','--','sh','-ec','MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql --protocol=TCP -h 127.0.0.1 -u root --batch --skip-column-names "$1"','probe',database]
  query='SELECT count(*) FROM information_schema.tables WHERE table_schema=DATABASE(); SELECT VERSION();'
 else:
  args=['docker','--config',e['E2E_DOCKER_CONFIG'],'--context',e['E2E_DOCKER_CONTEXT'],'exec','-i',e['E2E_EXTERNAL_POSTGRES_CONTAINER_ID'],'sh','-ec','PGPASSWORD="$POSTGRES_PASSWORD" exec psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$1" -At -v ON_ERROR_STOP=1','probe',database]
  query="SELECT count(*) FROM information_schema.tables WHERE table_schema='public'; SHOW server_version;"
 db=run(args,query+'\n').strip().splitlines();assert db[0]=='0'
 results.append({'engine':engine,'nativeRunExitCode':int((folder/'exit-code').read_text()),'verifierCorrection':engine=='postgresql','databaseVersion':db[1],'namespace':ns,'namespaceUID':get('namespace',ns)['metadata']['uid'],'resourceUID':case['resourceUID'],'receiptUID':case['receiptUID'],'payloadDigest':case['payloadDigest'],'nativePlanBytes':8388609,'plans':0,'planChunks':0,'applyJobs':0,'databaseTables':0,'originalPodAbsent':True,'leaderBindingRestored':True,'admissionGateAbsent':True})
nodes=get('nodes')['items'];assert len(nodes)==4;kubelets=[]
for node in nodes:
 name=node['metadata']['name'];config=json.loads(run(k+['get','--raw','/api/v1/nodes/'+name+'/proxy/configz']))['kubeletconfig'];assert config['containerLogMaxSize']=='10Mi'
 kubelets.append({'node':name,'kubeletVersion':node['status']['nodeInfo']['kubeletVersion'],'architecture':node['status']['nodeInfo']['architecture'],'containerLogMaxSize':config['containerLogMaxSize']})
runtime=[p for p in get('pods',ns=e['E2E_OPERATOR_NAMESPACE'])['items'] if not p['metadata'].get('deletionTimestamp')]
assert len(runtime)==3 and all(any(c['type']=='Ready' and c['status']=='True' for c in p['status']['conditions']) for p in runtime)
result={'status':'passed','cases':results,'kubelets':kubelets,'runtime':[{'name':p['metadata']['name'],'uid':p['metadata']['uid'],'images':[{'image':c['image'],'imageID':c['imageID']} for c in p['status']['containerStatuses']]} for p in runtime]}
(root/'readback.json').write_text(json.dumps(result,indent=2)+'\n');print(json.dumps({'status':'passed','cases':results}))
