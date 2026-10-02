import hashlib,json,os,pathlib,subprocess,time
E=os.environ;out=pathlib.Path('/private/tmp/ptah-result-abandoned-evidence')
def run(args,data=None):
 p=subprocess.run(args,input=data,text=True,capture_output=True,timeout=60)
 if p.returncode:raise RuntimeError(args[0]+' failed: '+p.stderr[-1000:])
 return p.stdout
def k(*args,data=None):return run(['kubectl','--kubeconfig',E['E2E_KUBECONFIG'],'--request-timeout=20s',*args],data)
def d(*args,data=None):return run(['docker','--context',E['E2E_DOCKER_CONTEXT'],*args],data)
def parse(s):return json.loads(k('create','--dry-run=client','--validate=false','-f','-','-o','json',data=s))
assert json.loads((out/'verification.json').read_text())['status']=='passed'
report=[]
for suffix in ('control-plane','control-plane2','control-plane3'):
 name=E['E2E_KIND_CLUSTER_NAME']+'-'+suffix
 assert d('inspect','--format','{{index .Config.Labels "io.x-k8s.kind.cluster"}}',name).strip()==E['E2E_KIND_CLUSTER_NAME']
 path='/etc/kubernetes/manifests/kube-apiserver.yaml';directory='/etc/kubernetes/result-acceptance-audit'
 original=(out/(name+'-apiserver-before.yaml')).read_text();current=parse(d('exec',name,'cat',path));expected=parse(original)
 container=current['spec']['containers'][0]
 flags=['--audit-policy-file='+directory+'/policy.json','--audit-log-path='+directory+'/events.jsonl','--audit-log-mode=blocking','--audit-log-maxsize=64','--audit-log-maxbackup=1']
 assert all(container['command'].count(f)==1 for f in flags)
 container['command']=[f for f in container['command'] if f not in flags]
 assert len([v for v in container['volumeMounts'] if v['name']=='result-acceptance-audit'])==1
 assert len([v for v in current['spec']['volumes'] if v['name']=='result-acceptance-audit'])==1
 container['volumeMounts']=[v for v in container['volumeMounts'] if v['name']!='result-acceptance-audit']
 current['spec']['volumes']=[v for v in current['spec']['volumes'] if v['name']!='result-acceptance-audit']
 assert current==expected,'API server changed outside the owned audit configuration'
 before=json.loads(k('-n','kube-system','get','pod','kube-apiserver-'+name,'-o','json'))
 d('exec','-i',name,'sh','-c','cat > '+directory+'/restore.yaml && mv '+directory+'/restore.yaml '+path,data=original)
 deadline=time.monotonic()+180
 while time.monotonic()<deadline:
  after=json.loads(k('-n','kube-system','get','pod','kube-apiserver-'+name,'-o','json'))
  if after['metadata']['uid']!=before['metadata']['uid'] and any(c['type']=='Ready' and c['status']=='True' for c in after.get('status',{}).get('conditions',[])):
   assert not any(a.startswith('--audit-') for a in after['spec']['containers'][0]['command']);break
  time.sleep(3)
 else:raise RuntimeError('Restored API server did not become ready: '+name)
 assert d('exec',name,'cat',path)==original
 report.append({'node':name,'originalManifestSHA256':hashlib.sha256(original.encode()).hexdigest(),'restoredPodUID':after['metadata']['uid'],'ready':True})
 print('Original API server restored:',name,flush=True)
(out/'audit-restored.json').write_text(json.dumps(report,indent=2)+'\n')
