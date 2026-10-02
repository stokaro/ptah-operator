import hashlib,json,os,pathlib,subprocess
root=pathlib.Path('/private/tmp/ptah-result-135-boundaries-20261003')
e=dict(l.split('=',1) for l in pathlib.Path('/private/tmp/ptah-result-135.env').read_text().splitlines() if '=' in l)
assert e['E2E_KUBERNETES_VERSION']=='1.35.8' and e['E2E_CONTROLLER_REVISION']=='942f5704ad1777bb81990062e51e5c6f7a43dc2c'
e['DOCKER_CONFIG']=e['E2E_DOCKER_CONFIG']
d=['docker','--config',e['E2E_DOCKER_CONFIG'],'--context',e['E2E_DOCKER_CONTEXT']]
refs=[r for r in e['E2E_CREATED_IMAGE_REFS'].split(',') if '/e2e-fixture:' in r and r.startswith('127.0.0.1:')]
assert len(refs)==1
info=json.loads(subprocess.check_output(d+['image','inspect',refs[0]],text=True))[0]
assert info['Config']['Labels']['ptah.run/e2e-role']=='fixture' and info['Config']['Labels']['ptah.run/e2e-operator-revision']==e['E2E_CONTROLLER_REVISION']
digests=[r.split('@',1)[1] for r in info['RepoDigests'] if r.startswith(refs[0].split('/e2e-fixture:')[0]+'/e2e-fixture@')];assert len(set(digests))==1
e['RESULT_PROBE_FIXTURE_IMAGE']=e['E2E_REGISTRY_HOST']+'/e2e-fixture@'+digests[0]
source=pathlib.Path('support/qualification/probes/result_first_harvest.py');(root/'executed-probe.py').write_bytes(source.read_bytes())
inputs={'controllerRevision':e['E2E_CONTROLLER_REVISION'],'controllerImage':e['E2E_CONTROLLER_IMAGE'],'runnerImage':e['E2E_RUNNER_IMAGE'],'executorImage':e['E2E_EXECUTOR_IMAGE'],'fixtureImage':e['RESULT_PROBE_FIXTURE_IMAGE'],'ptahCommit':e['E2E_PTAH_REVISION'],'kubernetes':e['E2E_KUBERNETES_VERSION'],'chartSHA256':hashlib.sha256(pathlib.Path(e['E2E_CHART_PACKAGE']).read_bytes()).hexdigest(),'procedureSHA256':hashlib.sha256(source.read_bytes()).hexdigest()}
(root/'inputs.json').write_text(json.dumps(inputs,indent=2)+'\n')
for engine in ('postgresql','mysql'):
 for boundary,plan_bytes in (('maximum',8388608),('oversized',8388609)):
  name=engine+'-'+boundary;case=root/name;case.mkdir(mode=0o700)
  e.update({'RESULT_PROBE_ENGINE':engine,'RESULT_PROBE_PLAN_BYTES':str(plan_bytes),'RESULT_PROBE_NAMESPACE':'ptah-result-135-'+name,'RESULT_PROBE_DATABASE':'result_135_'+engine+'_'+boundary+'_020','RESULT_PROBE_EVIDENCE_DIR':str(case/'evidence')})
  env={**os.environ,**e}
  for key in ('RESULT_PROBE_RESTORE_CONTROL_PLANE','RESULT_PROBE_ROTATE_CA','RESULT_PROBE_ROTATE_LEAF'):env.pop(key,None)
  print('START',name,flush=True)
  with (case/'run.private.log').open('w') as log:
   result=subprocess.run(['python3','-B','-u',str(source)],env=env,stdout=log,stderr=subprocess.STDOUT)
  (case/'exit-code').write_text(str(result.returncode)+'\n');print('END',name,'exit',result.returncode,flush=True)
  if result.returncode:raise SystemExit(result.returncode)
