import json, os, pathlib, subprocess, time
E=os.environ
out=pathlib.Path('/private/tmp/ptah-result-abandoned-evidence');out.mkdir(exist_ok=True)
def run(argv, text=None):
 p=subprocess.run(argv,input=text,text=True,capture_output=True)
 if p.returncode: raise RuntimeError(f'{argv[0]} failed: {p.stderr[-1000:]}')
 return p.stdout
def k(*args):return run(['kubectl','--kubeconfig',E['E2E_KUBECONFIG'],'--request-timeout=20s',*args])
def d(*args,text=None):return run(['docker','--context',E['E2E_DOCKER_CONTEXT'],*args],text)
policy={'apiVersion':'audit.k8s.io/v1','kind':'Policy','omitStages':['RequestReceived'],'rules':[
 {'level':'Request','verbs':['delete'],'resources':[{'group':'operator.ptah.run','resources':['ptahresultrecords']},{'group':'','resources':['secrets']}]},
 {'level':'Metadata','resources':[{'group':'operator.ptah.run','resources':['ptahresultrecords']}]},
 {'level':'None'}]}
# DELETE bodies contain only DeleteOptions. No create/update body, Secret value,
# credential record, result payload, or response body enters these audit files.
nodes=json.loads(k('get','nodes','-o','json'))['items']
record=[]
for node in nodes:
 name=node['metadata']['name']
 owner=d('inspect','--format','{{index .Config.Labels "io.x-k8s.kind.cluster"}}',name).strip()
 assert owner==E['E2E_KIND_CLUSTER_NAME'], 'refuse an unrelated node'
 for attempt in range(40):
  try:
   config=json.loads(k('get','--raw','/api/v1/nodes/'+name+'/proxy/configz'))
   if config['kubeletconfig']['containerLogMaxSize']=='10Mi':break
  except RuntimeError:pass
  time.sleep(2)
 else:raise RuntimeError('default logging did not become effective: '+name)
 record.append({'node':name,'containerLogMaxSize':config['kubeletconfig']['containerLogMaxSize']})
 if 'node-role.kubernetes.io/control-plane' not in node['metadata'].get('labels',{}):continue
 podname='kube-apiserver-'+name
 old=json.loads(k('-n','kube-system','get','pod',podname,'-o','json'))
 path='/etc/kubernetes/manifests/kube-apiserver.yaml'
 manifest=d('exec',name,'cat',path)
 (out/(name+'-apiserver-before.yaml')).write_text(manifest)
 obj=json.loads(run(['kubectl','--kubeconfig',E['E2E_KUBECONFIG'],'create','--dry-run=client','--validate=false','-f','-','-o','json'],manifest))
 container=obj['spec']['containers'][0]
 assert not any(x.startswith('--audit-') for x in container['command']), 'audit already configured'
 directory='/etc/kubernetes/result-acceptance-audit'
 container['command'] += ['--audit-policy-file='+directory+'/policy.json','--audit-log-path='+directory+'/events.jsonl','--audit-log-mode=blocking','--audit-log-maxsize=64','--audit-log-maxbackup=1']
 container.setdefault('volumeMounts',[]).append({'name':'result-acceptance-audit','mountPath':directory})
 obj['spec'].setdefault('volumes',[]).append({'name':'result-acceptance-audit','hostPath':{'path':directory,'type':'Directory'}})
 d('exec',name,'mkdir','-p',directory)
 d('exec','-i',name,'sh','-c','cat > '+directory+'/policy.json',text=json.dumps(policy))
 d('exec','-i',name,'sh','-c','cat > '+directory+'/next-apiserver.json && mv '+directory+'/next-apiserver.json '+path,text=json.dumps(obj))
 deadline=time.monotonic()+180
 while time.monotonic()<deadline:
  try:
   new=json.loads(k('-n','kube-system','get','pod',podname,'-o','json'))
   ready=any(c['type']=='Ready' and c['status']=='True' for c in new.get('status',{}).get('conditions',[]))
   if new['metadata']['uid']!=old['metadata']['uid'] and ready and '--audit-log-mode=blocking' in new['spec']['containers'][0]['command']:break
  except RuntimeError:pass
  time.sleep(3)
 else:raise RuntimeError('audited API server did not become ready: '+name)
 print('audited API server ready:',name,flush=True)
(out/'kubelets.json').write_text(json.dumps(record,indent=2)+'\n')
(out/'audit-policy.json').write_text(json.dumps(policy,indent=2)+'\n')
print('all kubelets use 10Mi; API DELETE audit enabled',flush=True)
