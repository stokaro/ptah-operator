import base64,hashlib,json,pathlib,subprocess,sys
sys.path.insert(0,str(pathlib.Path('support/qualification/probes').resolve()))
from result_first_harvest import publication,oversized_refusal
root=pathlib.Path('/private/tmp/ptah-result-136-boundaries-20261003')
e=dict(l.split('=',1) for l in pathlib.Path('/private/tmp/ptah-result-136.env').read_text().splitlines() if '=' in l)
inputs=json.loads((root/'inputs.json').read_text())
k=['kubectl','--kubeconfig',e['E2E_KUBECONFIG'],'--request-timeout=20s']
d=['docker','--config',e['E2E_DOCKER_CONFIG'],'--context',e['E2E_DOCKER_CONTEXT']]
def run(args,data=None):return subprocess.run(args,input=data,capture_output=True,text=True,check=True).stdout
def get(kind,name=None,ns=None):return json.loads(run(k+(['-n',ns] if ns else [])+['get',kind]+([name] if name else [])+['-o','json']))
def sql(engine,database,query):
 if engine=='mysql':
  args=k+['-n',e['E2E_TEST_NAMESPACE'],'exec','-i','deployment/demo-mysql','--','sh','-ec','MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql --protocol=TCP -h 127.0.0.1 -u root --batch --skip-column-names "$1"','probe',database]
 else:
  args=d+['exec','-i',e['E2E_EXTERNAL_POSTGRES_CONTAINER_ID'],'sh','-ec','PGPASSWORD="$POSTGRES_PASSWORD" exec psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$1" -At -v ON_ERROR_STOP=1','probe',database]
 return run(args,query+'\n').strip()
results=[]
for engine in ('postgresql','mysql'):
 for boundary in ('maximum','oversized'):
  name=engine+'-'+boundary;folder=root/name;assert (folder/'exit-code').read_text().strip()=='0'
  case=json.loads((folder/'evidence'/('first-harvest-maximum.json' if boundary=='maximum' else 'oversized-refusal.json')).read_text())
  assert case['status']=='passed' and case['procedureSHA256']==inputs['procedureSHA256']
  ns=case['namespace'];resource=get('ptahschema','first-harvest',ns);assert resource['metadata']['uid']==case['resourceUID']
  records={r['metadata']['name']:r for r in get('ptahresultrecords',ns=ns)['items']}
  intent,receipt,data=publication(records,case['jobUID']);assert receipt['metadata']['uid']==case['receiptUID'] and intent['metadata']['uid']==case['intentUID']
  assert 'sha256:'+hashlib.sha256(data).hexdigest()==case['payloadDigest']
  intents=[json.loads(base64.b64decode(r['spec']['data'])) for r in records.values() if r['spec']['type']=='intent']
  assert not run(k+['-n',ns,'get','pod',case['binding']['podName'],'--ignore-not-found','-o','name']).strip()
  assert len(case['managerUIDsBeforeDelivery'])==2 and len(case['managerUIDsBeforeConsumption'])==2 and not set(case['managerUIDsBeforeDelivery'])&set(case['managerUIDsBeforeConsumption'])
  database='result_136_'+engine+'_'+boundary+'_020';applies=[r for r in intents if r['binding']['operation']=='apply']
  plans=get('ptahschemaplans',ns=ns)['items'];chunks=get('ptahschemaplanchunks',ns=ns)['items'];jobs=get('jobs',ns=ns)['items']
  if boundary=='maximum':
   assert case['converged'] and case['approvedApplyJobs']==1 and len(applies)==1 and applies[0]['binding']['jobUID']==case['applyJobUID']
   assert not resource['status'].get('activeOperation') and any(c['type']=='InSync' and c['status']=='True' and c['observedGeneration']==resource['metadata']['generation'] for c in resource['status']['conditions'])
   assert len(plans)==1 and len(chunks)==16 and plans[0]['metadata']['uid']==case['planUID']
   by_name={c['metadata']['name']:c for c in chunks};parts=[]
   for i,ref in enumerate(plans[0]['spec']['chunks']):
    chunk=by_name[ref['name']];part=base64.b64decode(chunk['spec']['data'],validate=True)
    assert ref['index']==i and ref['size']==len(part)==524288 and 'sha256:'+hashlib.sha256(part).hexdigest()==ref['digest']
    assert chunk['metadata']['ownerReferences'][0]['uid']==case['planUID'];parts.append(part)
   raw=b''.join(parts);assert len(raw)==8388608 and 'sha256:'+hashlib.sha256(raw).hexdigest()==case['planContentDigest']
   document=json.loads(raw);dialect='mysql' if engine=='mysql' else 'postgres';count=100 if engine=='mysql' else 1
   repeated,suffix=case['repeatedCharacters'],case['asciiSuffix'];assert document['dialect']==dialect and len(document['statements'])==count and raw.count(b'\\u003c')==repeated
   for i in range(count):
    table='e2e_plan_size_limit'+('_%03d'%i if engine=='mysql' else '')
    amount=repeated//count+(i<repeated%count);ascii_count=suffix if i==count-1 else 0
    found=[s for s in document['statements'] if s['sql'].startswith('-- '+dialect.upper()+' TABLE: '+table+' --\nCREATE TABLE ')]
    assert len(found)==1 and found[0]['sql'].count('<')==amount and found[0]['sql'].count("DEFAULT '"+'<'*amount+'x'*ascii_count+"'")==1
   if engine=='mysql':
    witness=sql(engine,database,"SELECT CONCAT(COUNT(*), ':', SUM(CHAR_LENGTH(column_default)), ':', SUM(CHAR_LENGTH(column_default)-CHAR_LENGTH(REPLACE(column_default,'<','')))) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name LIKE 'e2e_plan_size_limit_%' AND column_name='payload'")
   else:
    witness=sql(engine,database,"SELECT length(payload)::text || ':' || length(replace(payload,'x',''))::text || ':' || length(replace(payload,'<',''))::text FROM e2e_plan_size_limit WHERE id=1")
   assert witness==case['databaseDefault']
  else:
   assert [r['binding']['jobUID'] for r in intents if r['binding']['operation']=='plan']==[case['jobUID']]
   oversized_refusal(resource,json.loads(data),resource['status']['source']['digest'],case['binding']['operationID'])
   assert not plans and not chunks and not applies
   assert not any(j['spec']['template'].get('metadata',{}).get('labels',{}).get('operator.ptah.run/operation')=='apply' for j in jobs)
   assert sql(engine,database,"SELECT count(*) FROM information_schema.tables WHERE table_schema="+("DATABASE()" if engine=='mysql' else "'public'"))=='0'
   witness='0 tables'
  rb=json.loads((folder/'evidence/manager-rolebinding-before.json').read_text());assert get('rolebinding',rb['metadata']['name'],e['E2E_OPERATOR_NAMESPACE'])['subjects']==rb['subjects']
  for kind in ('validatingadmissionpolicy','validatingadmissionpolicybinding'):
   assert not run(k+['get',kind,ns+'-gate','--ignore-not-found','-o','name']).strip()
  results.append({'engine':engine,'boundary':boundary,'namespace':ns,'resourceUID':case['resourceUID'],'receiptUID':case['receiptUID'],'payloadDigest':case['payloadDigest'],'planBytes':8388608 if boundary=='maximum' else 8388609,'planChunks':len(chunks),'applyPublications':len(applies),'databaseWitness':witness,'originalPodAbsent':True,'managersReplacedBeforeHarvest':True,'leaderBindingRestored':True,'admissionGateAbsent':True})
nodes=get('nodes')['items'];assert len(nodes)==4;kubelets=[]
for node in nodes:
 name=node['metadata']['name'];info=node['status']['nodeInfo'];assert info['kubeletVersion']=='v1.36.4'
 assert run(d+['inspect','--format','{{index .Config.Labels "io.x-k8s.kind.cluster"}}',name]).strip()==e['E2E_KIND_CLUSTER_NAME']
 assert not run(d+['exec',name,'sed','-n','/^containerLogMaxSize:/p','/var/lib/kubelet/config.yaml']).strip()
 config=json.loads(run(k+['get','--raw','/api/v1/nodes/'+name+'/proxy/configz']))['kubeletconfig'];assert config['containerLogMaxSize']=='10Mi'
 kubelets.append({'node':name,'uid':node['metadata']['uid'],'kubeletVersion':info['kubeletVersion'],'architecture':info['architecture'],'containerLogMaxSize':config['containerLogMaxSize'],'explicitLogSizeSettingAbsent':True})
runtime=[p for p in get('pods',ns=e['E2E_OPERATOR_NAMESPACE'])['items'] if not p['metadata'].get('deletionTimestamp')]
assert len(runtime)==3 and all(any(c['type']=='Ready' and c['status']=='True' for c in p['status']['conditions']) for p in runtime)
report={'status':'passed','cases':results,'kubelets':kubelets,'runtime':[{'name':p['metadata']['name'],'uid':p['metadata']['uid'],'images':[{'image':c['image'],'imageID':c['imageID']} for c in p['status']['containerStatuses']]} for p in runtime]}
(root/'readback.json').write_text(json.dumps(report,indent=2)+'\n');print(json.dumps({'status':'passed','cases':results}))
