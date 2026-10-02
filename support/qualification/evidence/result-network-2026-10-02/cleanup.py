import json,os,pathlib,subprocess,urllib.parse
E=os.environ;out=pathlib.Path('/private/tmp/ptah-result-network-evidence');k=['kubectl','--kubeconfig',E['E2E_KUBECONFIG']]
def run(args,data=None):
 p=subprocess.run(args,input=data,text=True,capture_output=True,timeout=120)
 if p.returncode: raise RuntimeError(p.stderr[-1000:])
 return p.stdout
report={'namespaces':[],'databases':[]}
e=json.loads((out/'network.json').read_text());assert e['verification']['nativeWorkflows']==4
for row in e['workflows']:
 ns=row['namespace'];obj=json.loads(run(k+['get','namespace',ns,'-o','json']))
 assert obj['metadata']['labels']['operator.ptah.run/acceptance-owner']==E['E2E_KIND_CLUSTER_NAME']
 assert not json.loads(run(k+['-n',ns,'get','networkpolicies','-o','json']))['items']
 raw=run(k+['-n',ns,'get',row['family'].lower(),'network-proof','--ignore-not-found','-o','json'])
 if raw.strip():
  resource=json.loads(raw)
  assert resource['metadata']['uid']==row['resourceUID'] and resource['spec']['suspend']
 run(k+['delete','namespace',ns,'--wait=false'])
 report['namespaces'].append({'name':ns,'uid':obj['metadata']['uid'],'deletionRequested':True,'pendingRetention':True})
pg=json.loads(pathlib.Path(E['E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE']).read_text())
for suffix in ('schema','migration'):
 db='result_network_pg_'+suffix
 run(['docker','--context',E['E2E_DOCKER_CONTEXT'],'exec','-i',E['E2E_EXTERNAL_POSTGRES_CONTAINER_ID'],'sh','-ec','PGPASSWORD="$POSTGRES_PASSWORD" exec psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$1" -At -v ON_ERROR_STOP=1','cleanup',urllib.parse.urlsplit(pg['url']).path[1:]],'DROP DATABASE '+db+';\n')
 report['databases'].append({'name':db,'dropSucceeded':True})
 db='result_network_mysql_'+suffix
 run(k+['-n',E['E2E_TEST_NAMESPACE'],'exec','-i','deployment/demo-mysql','--','sh','-ec','MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql --protocol=TCP -h 127.0.0.1 -u root mysql'],'DROP DATABASE `'+db+'`; REVOKE ALL ON `'+db+'`.* FROM \'demo\'@\'%\';\n')
 report['databases'].append({'name':db,'dropSucceeded':True})
assert not json.loads(run(k+['-n',E['E2E_OPERATOR_NAMESPACE'],'get','networkpolicies','-o','json']))['items']
report['operatorPoliciesAbsent']=True
report['sharedLabRetainedForRemainingQualification']=True
report['namespaceRetentionNote']='Result retention admission delays namespace collection until eligible; no guard, finalizer or retention window was bypassed.'
(out/'cleanup.json').write_text(json.dumps(report,indent=2)+'\n')
print(json.dumps(report))
