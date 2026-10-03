from pathlib import Path
import subprocess,json,time
root=Path('/private/tmp/ptah-585-publisher-proof');root.mkdir(exist_ok=True)
d=['docker','--context','remote-dev-container'];network='ptah-585-publisher-repro-net';registry='ptah-585-publisher-repro-registry';executor='127.0.0.1:26065/ptah-executor:2c0c2ad35d0f706f'
def run(args,**kw):return subprocess.run(d+args,capture_output=True,text=True,**kw)
meta=json.loads(run(['image','inspect',executor],check=True).stdout)[0];assert meta['Config']['Labels']['ptah.run/e2e-ptah-commit']=='f6e562c5b0986cd29a53a5cc01938827336b780a'
(root/'executor.json').write_text(json.dumps({'id':meta['Id'],'repoDigests':meta['RepoDigests'],'labels':meta['Config']['Labels']},indent=2)+'\n')
run(['network','create','--internal',network],check=True)
try:
 run(['run','-d','--name',registry,'--network',network,'--tmpfs','/var/lib/registry:rw,size=64m','1be55279f18a'],check=True)
 for _ in range(30):
  ready=run(['exec',registry,'wget','-q','-O','-','http://127.0.0.1:5000/v2/'])
  if ready.returncode==0:break
  time.sleep(1)
 else:raise RuntimeError('registry not ready')
 outcomes=[]
 for engine,version,label in [('postgresql','alert-unresolved','postgresql-original'),('mysql','alert-unresolved','mysql-original'),('mysql','alert-unresolved-mysql','mysql-fixed')]:
  tag='postgresql' if engine=='postgresql' else 'mysql';dialect='postgres' if engine=='postgresql' else 'mysql'
  args=['run','--rm','-i','--name','ptah-585-'+label,'--network',network,'--user','65532:65532','--tmpfs','/work:rw,mode=1777','--env','HOME=/work','--env','TMPDIR=/work','--entrypoint','/bin/sh',executor,'-ec','cat > /work/schema.sql; exec /usr/local/bin/ptah "$@"','probe','schema','push','oci://'+registry+':5000/e2e-alert-schema:'+tag,'--schema-file','/work/schema.sql','--dialect',dialect,'--version',version,'--plain-http']
  r=run(args,input=Path('testdata/e2e/'+engine+'-fault-v1.sql').read_text());(root/(label+'.txt')).write_text(r.stdout+r.stderr);outcomes.append({'case':label,'exitCode':r.returncode,'version':version});print(label,r.returncode,flush=True)
 (root/'outcomes.json').write_text(json.dumps(outcomes,indent=2)+'\n')
 assert [x['exitCode'] for x in outcomes]==[0,2,0],outcomes
 assert 'already exists' in (root/'mysql-original.txt').read_text() or 'immutable' in (root/'mysql-original.txt').read_text() or 'write-once' in (root/'mysql-original.txt').read_text()
 tags=run(['exec',registry,'wget','-q','-O','-','http://127.0.0.1:5000/v2/e2e-alert-schema/tags/list'],check=True).stdout;(root/'tags.json').write_text(tags);print('native collision reproduced; distinct engine version passed',flush=True)
finally:
 cleanup=run(['rm','-f',registry]);removed=run(['network','rm',network]);(root/'cleanup.json').write_text(json.dumps({'registryExitCode':cleanup.returncode,'networkExitCode':removed.returncode},indent=2)+'\n')
