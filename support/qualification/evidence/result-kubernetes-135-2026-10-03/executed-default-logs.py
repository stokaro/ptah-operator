import json,os,subprocess,time
E=os.environ
def run(args):return subprocess.check_output(args,text=True).strip()
def k(*args):return run(['kubectl','--kubeconfig',E['E2E_KUBECONFIG'],'--request-timeout=20s',*args])
def d(*args):return run(['docker','--context',E['E2E_DOCKER_CONTEXT'],*args])
nodes=json.loads(k('get','nodes','-o','json'))['items'];assert nodes
for node in nodes:
 name=node['metadata']['name'];assert d('inspect','--format','{{index .Config.Labels "io.x-k8s.kind.cluster"}}',name)==E['E2E_KIND_CLUSTER_NAME']
 d('exec',name,'sed','-i','/^containerLogMaxSize:/d','/var/lib/kubelet/config.yaml')
 d('exec',name,'systemctl','restart','kubelet')
 for _ in range(60):
  try:
   config=json.loads(k('get','--raw','/api/v1/nodes/'+name+'/proxy/configz'))
   if config['kubeletconfig']['containerLogMaxSize']=='10Mi':break
  except (subprocess.CalledProcessError,ValueError):pass
  time.sleep(1)
 else:raise RuntimeError('default kubelet logging did not become effective')
 print(name,'containerLogMaxSize=10Mi',flush=True)
