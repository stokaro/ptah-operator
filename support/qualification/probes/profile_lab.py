#!/usr/bin/env python3
"""Stand up one frozen-profile qualification lab and measure a capacity cell.

The acceptance harness's bootstrap gives a cluster with the release installed
for its own phases. The frozen 0.2.0 profile asks for more, and this adds it in
order, each step recorded under the lab directory before the next starts:

  bootstrap  the harness stopped after its bootstrap, installing the prepared
             release's own digests (E2E_RELEASE_MANIFEST)
  install    native installer, author and approver identities, and the chart
             reinstalled in ptah-system with a distinct approver, the
             apply-policy guard exempting only system:masters, and restricted
             Pod Security on the release namespace
  kindnet    the CNI rebuilt from the pinned Pending-Pod correction
  fixtures   the registry TLS fixture and the lab's namespace dependencies
  prepare    the capacity namespaces, databases, signed populated inputs and
             unrelated objects for one workload file
  network    the documented egress policies and live allowed/denied traffic
             controls, including the database the workload uses
  measure    the capacity driver run from a toolbox container inside the
             control-plane node's network, so a multi-hour cell does not rest
             on this machine's tunnel; its evidence is copied back by checksum
  down       the whole lab removed

A step that has completed is refused rather than repeated. Credentials,
kubeconfigs and keys stay in the lab directory, which is private.
"""

import argparse
import base64
import copy
import datetime
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import secrets
import shlex
import shutil
import subprocess
import sys
import tarfile
import time
import uuid

ROOT = Path(__file__).resolve().parents[3]
PROBES = ROOT / 'support/qualification/probes'
STEPS = ('bootstrap', 'install', 'kindnet', 'fixtures', 'prepare', 'network', 'measure')
WORKLOAD_NAMESPACES = ['ptah-qualification-a', 'ptah-qualification-b']
QUOTA = {'pods': '24', 'count/jobs.batch': '512', 'configmaps': '512', 'secrets': '128',
         'count/ptahschemaplans.operator.ptah.run': '2048',
         'count/ptahschemaplanchunks.operator.ptah.run': '32768',
         'count/ptahmigrationplans.operator.ptah.run': '2048'}
COSIGN_MODULE = 'github.com/sigstore/cosign/v3/cmd/cosign@v3.0.6'
FREEZE = 'support/qualification/0.2.0-freeze.json'


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def registry_secrets(namespace, host, credentials):
    """The registry Secrets the capacity driver copies, in the shape the demonstration lab writes them."""
    username, password = credentials['username'], credentials['password']
    auth = base64.b64encode(f'{username}:{password}'.encode()).decode()
    config = {'auths': {host: {'username': username, 'password': password, 'auth': auth}}}
    return [
        {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'demo-registry', 'namespace': namespace}, 'type': 'Opaque',
         'stringData': {'username': username, 'password': password, 'registry': host, 'allowPlainHTTP': 'true'}},
        {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'demo-registry-pull', 'namespace': namespace},
         'type': 'kubernetes.io/dockerconfigjson', 'stringData': {'.dockerconfigjson': json.dumps(config)}},
    ]


def profile_freeze(raw, read):
    """The freeze revision a lab measures against, once every input it pins matches.

    read returns a path's bytes at the harness commit, so the check is against
    what the toolbox is built from rather than this working tree.
    """
    freeze = json.loads(raw)
    entries = freeze['sourceFiles'] + [freeze['functionalMatrix']]
    if not freeze['sourceFiles']:
        raise ValueError('the freeze pins no inputs')
    for entry in entries:
        if sha(read(entry['path'])) != entry['sha256']:
            raise ValueError(f"{entry['path']} differs from freeze revision {freeze['revision']}")
    return {'revision': freeze['revision'], 'sha256': sha(raw), 'inputs': len(entries)}


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def read_environment(path):
    values = {}
    for line in Path(path).read_text().splitlines():
        if not line or line.startswith('#'):
            continue
        key, separator, value = line.partition('=')
        parts = shlex.split(value)
        if not separator or key in values or len(parts) > 1:
            raise ValueError(f'invalid or repeated environment record {key!r}')
        values[key] = parts[0] if parts else ''
    return values


def write_environment(path, values):
    Path(path).write_text(''.join(f'{key}={shlex.quote(value)}\n' for key, value in sorted(values.items())))


def database_service(engine):
    """The server the capacity bootstrap provisions for an engine, and its port."""
    if engine == 'PostgreSQL':
        return 'capacity-postgres', 5432
    if engine == 'MySQL':
        return 'capacity-mysql', 3306
    raise ValueError(f'unsupported engine {engine!r}')


def profile_values(candidate):
    """The installation the profile declares, on the candidate's own images."""
    values = {key: copy.deepcopy(candidate[key]) for key in ('image', 'imagePullSecrets', 'execution', 'replicaCount')}
    if values['replicaCount'] != 2:
        raise ValueError('the profile runs two manager replicas')
    values.update(approvals={'requireDistinctApprover': True},
                  applyPolicyGuard={'enabled': True, 'exemptGroups': ['system:masters']})
    return values


def preflight_failures(host, nodes, configz, namespaces, quotas, managers, kindnet, kindnet_image, minor):
    """Everything the profile requires of the lab before a measurement starts."""
    failures = []
    if host.get('os') != 'linux' or host.get('cpus', 0) < 4 or host.get('memoryBytes', 0) < 16 * 1024**3:
        failures.append('the host is below 4 CPUs and 16 GiB or is not Linux')
    roles = [('node-role.kubernetes.io/control-plane' in n['metadata'].get('labels', {})) for n in nodes]
    if len(nodes) != 4 or sum(roles) != 3:
        failures.append('the cluster is not three control-plane nodes and one worker')
    for name, config in configz.items():
        if config.get('kubeletconfig', {}).get('containerLogMaxSize') != '10Mi':
            failures.append(f'kubelet on {name} does not keep the default 10Mi log size')
    for name, namespace in namespaces.items():
        labels = namespace['metadata'].get('labels', {})
        for mode in ('enforce', 'warn', 'audit'):
            if labels.get('pod-security.kubernetes.io/' + mode) != 'restricted' or \
                    labels.get('pod-security.kubernetes.io/' + mode + '-version') != minor:
                failures.append(f'{name} does not {mode} restricted Pod Security at {minor}')
    for name, quota in quotas.items():
        if quota['spec']['hard'] != QUOTA:
            failures.append(f'{name} does not carry the profile quota')
    if len(managers) != 2 or not all(c['ready'] and c['restartCount'] == 0
                                     for p in managers for c in p['status'].get('containerStatuses', [])):
        failures.append('the two managers are not ready and unrestarted')
    active = [p for p in kindnet if not p['metadata'].get('deletionTimestamp')]
    if len(active) != 4 or not all(p['spec']['containers'][0]['image'] == kindnet_image and
                                   all(c['ready'] for c in p['status'].get('containerStatuses', [])) for p in active):
        failures.append('the corrected kindnet is not ready on all four nodes')
    return failures


def archive(files):
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode='w:gz') as tar:
        for name, raw in sorted(files.items()):
            path = PurePosixPath(name)
            if path.is_absolute() or '..' in path.parts:
                raise ValueError('archive member must be a relative path')
            member = tarfile.TarInfo(name)
            member.size, member.mode = len(raw), 0o600
            tar.addfile(member, io.BytesIO(raw))
    return output.getvalue()


def receive(raw, destination):
    """Accept a copied evidence archive only when its own manifest matches it."""
    with tarfile.open(fileobj=io.BytesIO(raw), mode='r:gz') as tar:
        members = tar.getmembers()
        names = [m.name for m in members]
        if len(names) != len(set(names)) or any(not m.isfile() for m in members):
            raise ValueError('evidence archive holds duplicates or non-files')
        for name in names:
            path = PurePosixPath(name)
            if path.is_absolute() or '..' in path.parts:
                raise ValueError('evidence archive escapes its directory')
        contents = {m.name: tar.extractfile(m).read() for m in members}
    expected = json.loads(contents.pop('transfer-manifest.json'))
    if not expected or set(expected) != set(contents) or any(sha(contents[n]) != d for n, d in expected.items()):
        raise ValueError('evidence archive does not match its manifest')
    destination.mkdir(mode=0o700)
    for name, data in contents.items():
        target = destination / name
        target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        target.write_bytes(data)
    return expected


EXPORT = r'''
import hashlib,io,json,pathlib,sys,tarfile
root=pathlib.Path('/work/evidence')
files={}
for path in sorted(root.rglob('*')):
    if path.is_symlink(): raise ValueError('evidence must not contain symlinks')
    if path.is_file(): files[path.relative_to(root).as_posix()]=path.read_bytes()
manifest={name:hashlib.sha256(raw).hexdigest() for name,raw in files.items()}
files['transfer-manifest.json']=(json.dumps(manifest,indent=2)+'\n').encode()
with tarfile.open(fileobj=sys.stdout.buffer,mode='w|gz') as tar:
    for name,raw in files.items():
        member=tarfile.TarInfo(name);member.mode=0o600;member.size=len(raw)
        tar.addfile(member,io.BytesIO(raw))
'''

INNER = r'''
import json,os,pathlib,subprocess,sys
os.umask(0o077)
root=pathlib.Path('/work/evidence')
configuration=json.loads(pathlib.Path('/access/configuration.json').read_text())
os.environ.update(configuration['environment'])
sys.path.insert(0,'/src/support/qualification/probes')
from capacity_bootstrap import Bootstrap
from capacity_workload import Workload
import capacity_unrelated
bootstrap=Bootstrap(root/'capacity-bootstrap.json')
bootstrap.state=json.loads(bootstrap.path.read_text())
unrelated=configuration['unrelated']
if unrelated:
    bootstrap.state['unrelated']['before']=capacity_unrelated.verify(bootstrap,'before')
    bootstrap.save()
with (root/'driver.log').open('xb') as log:
    result=subprocess.run(configuration['argv'],stdout=log,stderr=subprocess.STDOUT)
(root/'driver.exit').write_text(str(result.returncode)+'\n')
if result.returncode: sys.exit(result.returncode)
verify=configuration.get('verify')
if verify:
    Workload(bootstrap.path,root/'inputs').verify(verify['changed'],verify['round'],'final')
if unrelated:
    bootstrap.state=json.loads(bootstrap.path.read_text())
    bootstrap.state['unrelated']['after']=capacity_unrelated.verify(bootstrap,'after')
    bootstrap.save()
'''


class Lab:
    def __init__(self, directory):
        self.dir = Path(directory).resolve()
        self.dir.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.state_path = self.dir / 'lab.json'
        self.state = json.loads(self.state_path.read_text()) if self.state_path.exists() else {'steps': {}}

    def save(self):
        self.state_path.write_text(json.dumps(self.state, indent=2) + '\n')

    def begin(self, step):
        previous = STEPS[STEPS.index(step) - 1] if STEPS.index(step) else None
        if previous and self.state['steps'].get(previous, {}).get('status') != 'done':
            raise RuntimeError(f'{step} needs {previous} first')
        if self.state['steps'].get(step, {}).get('status') == 'done':
            raise RuntimeError(f'{step} already completed in this lab')
        self.state['steps'][step] = {'status': 'running', 'startedAt': now()}
        self.save()

    def finish(self, step, **record):
        self.state['steps'][step].update(record, status='done', finishedAt=now())
        self.save()

    @property
    def env(self):
        return read_environment(self.dir / 'profile.environment' if (self.dir / 'profile.environment').exists()
                                else self.dir / 'environment')

    def child_env(self, extra=None):
        env = dict(os.environ)
        env.update(self.env)
        env.update(DOCKER_CONFIG=env['E2E_DOCKER_CONFIG'], DOCKER_CONTEXT=env['E2E_DOCKER_CONTEXT'],
                   DOCKER_HOST=env['E2E_DOCKER_ENDPOINT'], KIND_EXPERIMENTAL_PROVIDER='docker',
                   PATH=str(self.dir / 'bin') + os.pathsep + os.environ['PATH'])
        env.update(extra or {})
        return env

    def run(self, name, args, data=None, timeout=120, env=None, cwd=ROOT):
        result = subprocess.run(args, input=data, capture_output=True, timeout=timeout, cwd=cwd, env=env or self.child_env())
        logs = self.dir / 'logs'
        logs.mkdir(mode=0o700, exist_ok=True)
        (logs / (name + '.stdout')).write_bytes(result.stdout)
        (logs / (name + '.stderr')).write_bytes(result.stderr)
        if result.returncode:
            raise RuntimeError(f'{name} exited {result.returncode}; private diagnostics in {logs}')
        return result.stdout

    def kubectl(self, kubeconfig=None):
        return ['kubectl', '--kubeconfig', kubeconfig or self.env['E2E_KUBECONFIG'], '--request-timeout=30s']

    def read(self, name, args):
        return json.loads(self.run(name, self.kubectl() + args))

    # bootstrap ------------------------------------------------------------

    def bootstrap(self, context, kubernetes, release_manifest, development=False):
        """Bring the lab up on the release's digests, or on this checkout's own build for a
        development lab. A development lab is recorded as one and qualifies nothing."""
        self.begin('bootstrap')
        if subprocess.run(['git', 'status', '--porcelain'], cwd=ROOT, capture_output=True, text=True).stdout.strip():
            raise RuntimeError('a qualification lab is built from a clean, committed checkout')
        harness = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip()
        show = lambda path: subprocess.run(['git', 'show', f'{harness}:{path}'], cwd=ROOT, capture_output=True, check=True).stdout
        profile = profile_freeze(show(FREEZE), show)
        manifest = Path(release_manifest).resolve() if release_manifest else None
        if (manifest is None) != development:
            raise RuntimeError('a qualification lab names a release manifest; only a development lab omits it')
        env = dict(os.environ, DOCKER_CONTEXT=context, K8S_VERSION=kubernetes,
                   E2E_RUN_ID='lab-' + secrets.token_hex(4), E2E_STOP_AFTER='bootstrap',
                   E2E_ENVIRONMENT_FILE=str(self.dir / 'environment'), E2E_RELEASE_MANIFEST=str(manifest or ''),
                   E2E_TIMING_LEDGER=str(self.dir / 'bootstrap-timings.jsonl'),
                   E2E_TIMING_CONTEXT=str(self.dir / 'bootstrap-context.json'))
        with (self.dir / 'bootstrap.log').open('wb') as log:
            result = subprocess.run(['make', 'e2e'], cwd=ROOT, env=env, stdout=log, stderr=subprocess.STDOUT)
        if result.returncode or not (self.dir / 'environment').is_file():
            raise RuntimeError(f'bootstrap failed; see {self.dir / "bootstrap.log"}')
        self.finish('bootstrap', harnessCommit=harness, profile=profile, context=context, kubernetes=kubernetes, development=development,
                    releaseManifest=str(manifest) if manifest else None,
                    releaseManifestSHA256=sha(manifest.read_bytes()) if manifest else None)

    # install --------------------------------------------------------------

    def install(self):
        self.begin('install')
        env = self.env
        control = env['E2E_KIND_CLUSTER_NAME'] + '-control-plane'
        docker = ['docker', '--context', env['E2E_DOCKER_CONTEXT']]
        cluster = json.loads(self.run('cluster-config', self.kubectl() + ['config', 'view', '--raw', '-o', 'json']))
        nodes = self.read('nodes', ['get', 'nodes', '-o', 'json'])['items']
        if len(nodes) != 4 or sum('node-role.kubernetes.io/control-plane' in n['metadata']['labels'] for n in nodes) != 3:
            raise RuntimeError('the lab is not three control-plane nodes and one worker')
        actors = {}
        for actor in ('installer', 'author', 'approver'):
            name = 'ptah-qualification-' + actor
            args = docker + ['exec', control, 'kubeadm', 'kubeconfig', 'user', '--config', '/kind/kubeadm.conf',
                             '--client-name', name, '--validity-period', '72h']
            if actor == 'installer':
                args += ['--org', 'system:masters']
            path = self.dir / (actor + '.kubeconfig')
            path.write_bytes(self.run(actor + '-generated', args))
            generated = json.loads(self.run(actor + '-config', ['kubectl', '--kubeconfig', str(path), 'config', 'view', '--raw', '-o', 'json']))
            config = copy.deepcopy(cluster)
            config['users'] = generated['users']
            for context in config['contexts']:
                context['context']['user'] = generated['users'][0]['name']
            path.write_text(json.dumps(config))
            path.chmod(0o600)
            identity = json.loads(self.run(actor + '-identity', ['kubectl', '--kubeconfig', str(path), 'auth', 'whoami', '-o', 'json']))['status']['userInfo']
            if identity['username'] != name or ('system:masters' in identity.get('groups', [])) != (actor == 'installer'):
                raise RuntimeError(f'the {actor} identity is not the one requested')
            actors[actor] = identity
        installer = str(self.dir / 'installer.kubeconfig')
        kubectl = self.kubectl(installer)
        helm = ['helm', '--kubeconfig', installer]
        values = profile_values(json.loads(Path(env['E2E_CANDIDATE_VALUES_FILE']).read_text()))
        values_path = self.dir / 'profile-values.json'
        values_path.write_text(json.dumps(values, indent=2) + '\n')
        minor = 'v' + re.match(r'^(1\.[0-9]+)\.', self.state['steps']['bootstrap']['kubernetes'])[1]
        secret_name = values['imagePullSecrets'][0]['name']
        secret = json.loads(self.run('pull-secret', kubectl + ['-n', env['E2E_OPERATOR_NAMESPACE'], 'get', 'secret', secret_name, '-o', 'json']))
        secret = {key: secret[key] for key in ('apiVersion', 'kind', 'type', 'data', 'immutable') if key in secret}
        secret['metadata'] = {'name': secret_name, 'namespace': 'ptah-system'}
        namespace = {'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': 'ptah-system', 'labels': {}}}
        for mode in ('enforce', 'warn', 'audit'):
            namespace['metadata']['labels']['pod-security.kubernetes.io/' + mode] = 'restricted'
            namespace['metadata']['labels']['pod-security.kubernetes.io/' + mode + '-version'] = minor
        self.run('namespace-create', kubectl + ['create', '-f', '-', '-o', 'json'], json.dumps(namespace).encode())
        self.run('pull-secret-create', kubectl + ['create', '-f', '-', '-o', 'json'], json.dumps(secret).encode())
        self.run('original-uninstall', helm + ['uninstall', env['E2E_HELM_RELEASE'], '-n', env['E2E_OPERATOR_NAMESPACE'],
                                               '--wait', '--timeout', '5m'], timeout=330)
        self.run('profile-install', helm + ['install', 'ptah-operator', env['E2E_CHART_PACKAGE'], '-n', 'ptah-system',
                                            '-f', str(values_path), '--wait', '--timeout', '5m'], timeout=330)
        deployments = json.loads(self.run('deployment', kubectl + ['-n', 'ptah-system', 'get', 'deployments', '-l',
            'app.kubernetes.io/instance=ptah-operator,app.kubernetes.io/component=controller', '-o', 'json']))['items']
        if len(deployments) != 1 or deployments[0]['status'].get('readyReplicas') != 2 or \
                deployments[0]['spec']['template']['spec']['containers'][0]['image'] != env['E2E_CONTROLLER_IMAGE']:
            raise RuntimeError('the profile installation is not the candidate with two ready managers')
        effective = self.run('effective-values', helm + ['get', 'values', 'ptah-operator', '-n', 'ptah-system', '-a', '-o', 'json'])
        self.run('installed-manifest', helm + ['get', 'manifest', 'ptah-operator', '-n', 'ptah-system'])
        updated = dict(env, E2E_KUBECONFIG=installer, E2E_OPERATOR_NAMESPACE='ptah-system', E2E_HELM_RELEASE='ptah-operator',
                       E2E_CONTROLLER_NAME=deployments[0]['metadata']['name'], E2E_CANDIDATE_VALUES_FILE=str(values_path),
                       CAPACITY_AUTHOR_KUBECONFIG=str(self.dir / 'author.kubeconfig'),
                       CAPACITY_APPROVER_KUBECONFIG=str(self.dir / 'approver.kubeconfig'),
                       CAPACITY_WORKLOAD_NAMESPACES=','.join(WORKLOAD_NAMESPACES), E2E_POD_SECURITY_VERSION=minor)
        write_environment(self.dir / 'profile.environment', updated)
        self.finish('install', actors=actors, deploymentUID=deployments[0]['metadata']['uid'],
                    valuesSHA256=sha(effective), chartSHA256=sha(Path(env['E2E_CHART_PACKAGE']).read_bytes()))

    # kindnet --------------------------------------------------------------

    def kindnet(self):
        self.begin('kindnet')
        env = self.env
        docker = ['docker', '--context', env['E2E_DOCKER_CONTEXT']]
        recipe = ROOT / 'support/qualification/kindnet'
        files = {p.name: sha(p.read_bytes()) for p in sorted(recipe.iterdir()) if p.is_file()}
        image = 'ptah-kindnet-qualification:' + sha(json.dumps(files, sort_keys=True).encode())[:16]
        architecture = json.loads(self.run('docker-info', docker + ['info', '--format', '{{json .}}']))['Architecture']
        platform = 'linux/' + {'x86_64': 'amd64', 'aarch64': 'arm64'}.get(architecture, architecture)
        self.run('kindnet-build', docker + ['build', '--progress=plain', '--platform', platform, '--tag', image,
                                           '--file', str(recipe / 'Dockerfile'), str(recipe)], timeout=1800)
        self.run('kindnet-load', ['kind', 'load', 'docker-image', '--name', env['E2E_KIND_CLUSTER_NAME'], image], timeout=600)
        original = self.read('kindnet-before', ['-n', 'kube-system', 'get', 'daemonset', 'kindnet', '-o', 'json'])
        containers = copy.deepcopy(original['spec']['template']['spec']['containers'])
        if len(containers) != 1:
            raise RuntimeError('kindnet does not run the single container this replaces')
        containers[0]['image'], containers[0]['imagePullPolicy'] = image, 'IfNotPresent'
        patch = {'metadata': {k: original['metadata'][k] for k in ('uid', 'resourceVersion')},
                 'spec': {'template': {'spec': {'containers': containers}}}}
        self.run('kindnet-patch', self.kubectl() + ['-n', 'kube-system', 'patch', 'daemonset', 'kindnet', '--type=merge',
                                                    '--patch', json.dumps(patch), '-o', 'json'])
        self.run('kindnet-rollout', self.kubectl() + ['-n', 'kube-system', 'rollout', 'status', 'daemonset/kindnet', '--timeout=300s'], timeout=330)
        inspected = json.loads(self.run('kindnet-image', docker + ['image', 'inspect', image]))[0]
        self.finish('kindnet', image=image, imageID=inspected['Id'], platform=platform, recipeFiles=files,
                    originalImage=original['spec']['template']['spec']['containers'][0]['image'])

    # fixtures -------------------------------------------------------------

    def fixtures(self):
        self.begin('fixtures')
        env = self.env
        binary = self.dir / 'ptah-e2e.test'
        self.run('e2e-test-build', ['go', 'test', '-tags', 'e2e', '-c', '-o', str(binary), './test/e2e'], timeout=900)
        work = Path(env['E2E_WORK_DIR'])
        driver = (ROOT / 'hack/e2e-kind.sh').read_text()
        postgres = re.search(r'E2E_POSTGRES_SOURCE_IMAGE=\$\{E2E_POSTGRES_SOURCE_IMAGE:-(.*?)\}', driver)[1]
        extra = dict(E2E_REGISTRY_PORT=env['E2E_REGISTRY_HOST_ADDRESS'].rsplit(':', 1)[1],
                     E2E_EXTERNAL_POSTGRES_OWNER=env['E2E_KIND_CLUSTER_NAME'], E2E_EXTERNAL_POSTGRES_IMAGE=postgres,
                     E2E_TLS_PROXY_SERVICE='e2e-registry-tls', E2E_TLS_PROXY_CA_FILE=str(work / 'tls-proxy/ca.crt'),
                     E2E_TLS_PROXY_CERT_FILE=str(work / 'tls-proxy/tls.crt'), E2E_TLS_PROXY_KEY_FILE=str(work / 'tls-proxy/tls.key'),
                     E2E_DATAPLANE_MODE='prepare', E2E_TIMING_LEDGER=str(self.dir / 'fixture-timings.jsonl'))
        self.run('fixture-prepare', [str(binary), '-test.v', '-e2e.phase=dataplane', '-e2e.completed=' + str(self.dir / 'fixture-prepared')],
                 cwd=ROOT / 'test/e2e', env=self.child_env(extra), timeout=1200)
        # The capacity driver copies these two Secrets from the test namespace.
        # The demonstration lab's own preparation would also start four demo
        # database Pods, which a measurement would carry as load, and under the
        # data plane's 64Mi LimitRange its MySQL server cannot start at all.
        credentials = json.loads(Path(env['E2E_REGISTRY_CREDENTIALS_FILE']).read_text())
        for secret in registry_secrets(env['E2E_TEST_NAMESPACE'], env['E2E_REGISTRY_HOST'], credentials):
            self.run('create-' + secret['metadata']['name'], self.kubectl() + ['create', '-f', '-', '-o', 'name'],
                     json.dumps(secret).encode())
        self.finish('fixtures')

    # prepare --------------------------------------------------------------

    def tools(self):
        """The pinned Ptah and Cosign the capacity inputs are published and signed with."""
        bin_dir = self.dir / 'bin'
        bin_dir.mkdir(mode=0o700, exist_ok=True)
        if not (bin_dir / 'cosign').exists():
            self.run('cosign-build', ['go', 'install', COSIGN_MODULE], env=self.child_env({'GOBIN': str(bin_dir), 'GOFLAGS': ''}), timeout=1200)
        if not (bin_dir / 'ptah').exists():
            commit = self.env['E2E_PTAH_REVISION']
            source = self.dir / 'ptah-source'
            if not source.exists():
                self.run('ptah-fetch', ['git', 'clone', '--filter=blob:none', '--no-checkout', 'https://github.com/stokaro/ptah', str(source)], timeout=600)
            self.run('ptah-checkout', ['git', '-C', str(source), 'checkout', '--detach', commit], timeout=300)
            if subprocess.check_output(['git', '-C', str(source), 'rev-parse', 'HEAD'], text=True).strip() != commit:
                raise RuntimeError('the Ptah checkout is not the pinned commit')
            self.run('ptah-build', ['go', '-C', str(source), 'build', '-trimpath', '-o', str(bin_dir / 'ptah'), './cmd/ptah'],
                     env=self.child_env({'GOFLAGS': ''}), timeout=1200)
        return {name: sha((bin_dir / name).read_bytes()) for name in ('cosign', 'ptah')}

    def prepare(self, workload_path):
        self.begin('prepare')
        workload_file = Path(workload_path).resolve()
        workload = json.loads(workload_file.read_text())
        varied = workload.get('soak') is not None or workload.get('approvalBacklog', False)
        env = self.env
        namespace = env['E2E_TEST_NAMESPACE']
        tools = self.tools()
        for name, filename in (('demo-verification-policy', 'verification-policy.yaml'),
                               ('demo-migration-verification-policy', 'migration-verification-policy.yaml')):
            existing = self.run('policy-' + name, self.kubectl() + ['-n', namespace, 'get', 'configmap', name, '-o', 'json', '--ignore-not-found'])
            if not existing.strip():
                policy = {'apiVersion': 'v1', 'kind': 'ConfigMap', 'metadata': {'name': name, 'namespace': namespace},
                          'immutable': True, 'data': {'policy.yaml': (ROOT / 'demo/policy' / filename).read_text()}}
                self.run('create-' + name, self.kubectl() + ['create', '-f', '-', '-o', 'json'], json.dumps(policy).encode())
        password = secrets.token_hex(32)
        (self.dir / 'signing-password').write_text(password)
        (self.dir / 'signing-password').chmod(0o600)
        self.run('signing-keygen', [str(self.dir / 'bin/cosign'), 'generate-key-pair', '--output-key-prefix', str(self.dir / 'signing')],
                 env=self.child_env({'COSIGN_PASSWORD': password}))
        capacity = dict(CAPACITY_REGISTRY_HOST=f'e2e-registry-tls.{namespace}.svc.cluster.local:5443',
                        CAPACITY_REGISTRY_CA_CONFIGMAP='e2e-registry-tls-ca',
                        CAPACITY_SIGNING_KEY=str(self.dir / 'signing.key'), CAPACITY_SIGNING_PUBLIC_KEY=str(self.dir / 'signing.pub'),
                        CAPACITY_COSIGN=str(self.dir / 'bin/cosign'), COSIGN_EXPERIMENTAL='1',
                        CAPACITY_VARIED_INPUTS='1' if varied else '0')
        write_environment(self.dir / 'profile.environment', dict(env, **capacity))
        child = self.child_env({'COSIGN_PASSWORD': password})
        sys.path.insert(0, str(PROBES))
        from capacity_bootstrap import Bootstrap
        os.environ.update(child)
        namespaces = Bootstrap(self.dir / 'capacity-bootstrap.json', child).prepare(workload)
        if namespaces != WORKLOAD_NAMESPACES:
            raise RuntimeError(f'the capacity bootstrap created {namespaces}')
        if varied:
            credentials = json.loads(Path(env['E2E_REGISTRY_CREDENTIALS_FILE']).read_text())
            os.environ.update(PTAH_OCI_USERNAME=credentials['username'], PTAH_OCI_PASSWORD=credentials['password'],
                              PTAH_OCI_REGISTRY=env['E2E_REGISTRY_HOST_ADDRESS'])
            from capacity_workload import Workload
            catalog = Workload(self.dir / 'capacity-bootstrap.json', self.dir / 'inputs').prepare()
            if len(catalog['schemas']) != workload['schemas'] or len(catalog['migrations']) != workload['migrations']:
                raise RuntimeError('the prepared inputs do not cover the workload')
        else:
            self.push_lab_inputs(workload['engine'] if 'engine' in workload else 'PostgreSQL')
        self.finish('prepare', workload=str(workload_file), workloadSHA256=sha(workload_file.read_bytes()), varied=varied, tools=tools)

    def push_lab_inputs(self, engine):
        """The two artifacts of each family a workload without populated inputs moves between."""
        env = self.env
        credentials = json.loads(Path(env['E2E_REGISTRY_CREDENTIALS_FILE']).read_text())
        child = self.child_env({'PTAH_OCI_USERNAME': credentials['username'], 'PTAH_OCI_PASSWORD': credentials['password']})
        mysql = engine == 'MySQL'
        schemas = ROOT / ('support/capacity/mysql/schemas' if mysql else 'demo/schemas')
        migrations = ROOT / ('support/capacity/mysql/migrations' if mysql else 'demo/migrations')
        dialect = 'mysql' if mysql else 'postgres'
        registry = env['E2E_REGISTRY_HOST_ADDRESS']
        tag = secrets.token_hex(3)
        refs = {}
        for version in ('v1', 'v2'):
            out = self.run(f'push-schema-{version}', ['ptah', 'schema', 'push', f'oci://{registry}/schemas/capacity:{version}-{tag}',
                                                      '--schema-file', str(schemas / f'{version}.sql'), '--dialect', dialect, '--plain-http'], env=child).decode()
            refs[f'schema-{version}'] = re.search(r'^Digest: (sha256:[0-9a-f]{64})$', out, re.M)[1]
        v2 = self.dir / 'migrations-v2'
        shutil.copytree(migrations, v2)
        (v2 / '0000000003_add_note.up.sql').write_text('ALTER TABLE shipments ADD COLUMN note TEXT;\n')
        (v2 / '0000000003_add_note.down.sql').write_text('ALTER TABLE shipments DROP COLUMN note;\n')
        for version, directory in (('v1', migrations), ('v2', v2)):
            out = self.run(f'push-migration-{version}', ['ptah', 'migrations', 'push', f'oci://{registry}/migrations/capacity:{version}-{tag}',
                                                         '--migrations-dir', str(directory), '--dir-format', 'ptah', '--version', f'{version}-{tag}',
                                                         '--plain-http'], env=child).decode()
            refs[f'migration-{version}'] = re.search(r'^Digest: (sha256:[0-9a-f]{64})$', out, re.M)[1]
        host = env['E2E_REGISTRY_HOST']
        self.state['labInputs'] = {key: f'oci://{host}/{"schemas" if key.startswith("schema") else "migrations"}/capacity@{digest}'
                                   for key, digest in refs.items()}
        self.save()

    # network --------------------------------------------------------------

    def network(self):
        self.begin('network')
        self.run('network-profile', [sys.executable, str(PROBES / 'profile_network.py'), '--lab', str(self.dir)], timeout=1800)
        summary = json.loads((self.dir / 'network/summary.json').read_text())
        if summary['conclusion'] != 'pass':
            raise RuntimeError('the network controls did not pass')
        self.finish('network', controls=len(summary['results']), summarySHA256=sha((self.dir / 'network/summary.json').read_bytes()))

    # measure --------------------------------------------------------------

    def preflight(self):
        env = self.env
        docker = ['docker', '--context', env['E2E_DOCKER_CONTEXT']]
        host = json.loads(self.run('host', docker + ['info', '--format',
            '{"dockerID":{{json .ID}},"name":{{json .Name}},"cpus":{{.NCPU}},"memoryBytes":{{.MemTotal}},'
            '"architecture":{{json .Architecture}},"os":{{json .OSType}},"observedAt":{{json .SystemTime}}}']))
        nodes = self.read('preflight-nodes', ['get', 'nodes', '-o', 'json'])['items']
        configz = {n['metadata']['name']: self.read('configz-' + n['metadata']['name'],
                   ['get', '--raw', f"/api/v1/nodes/{n['metadata']['name']}/proxy/configz"]) for n in nodes}
        namespaces = {name: self.read('namespace-' + name, ['get', 'namespace', name, '-o', 'json'])
                      for name in ['ptah-system'] + WORKLOAD_NAMESPACES}
        quotas = {name: self.read('quota-' + name, ['-n', name, 'get', 'resourcequota', 'capacity', '-o', 'json'])
                  for name in WORKLOAD_NAMESPACES}
        managers = self.read('managers', ['-n', 'ptah-system', 'get', 'pods', '-l', 'app.kubernetes.io/component=controller', '-o', 'json'])['items']
        kindnet = self.read('kindnet-pods', ['-n', 'kube-system', 'get', 'pods', '-l', 'app=kindnet', '-o', 'json'])['items']
        failures = preflight_failures(host, nodes, configz, namespaces, quotas, managers, kindnet,
                                      self.state['steps']['kindnet']['image'], env['E2E_POD_SECURITY_VERSION'])
        if failures:
            raise RuntimeError('the lab is not the frozen profile: ' + '; '.join(failures))
        return host

    def toolbox(self):
        """The driver image: the capacity tool built from this checkout, Python, kubectl and the pinned Ptah."""
        env = self.env
        docker = ['docker', '--context', env['E2E_DOCKER_CONTEXT']]
        harness = self.state['steps']['bootstrap']['harnessCommit']
        tag = 'ptah-capacity-toolbox:' + harness[:12]
        node_image = json.loads(self.run('node-inspect', docker + ['inspect', env['E2E_KIND_CLUSTER_NAME'] + '-control-plane']))[0]['Config']['Image']
        # The context is the committed tree of the harness commit and nothing
        # else in this checkout: no worktree, lab directory or local file.
        context = subprocess.run(['git', 'archive', '--format=tar', harness], cwd=ROOT, capture_output=True, check=True).stdout
        self.run('toolbox-build', docker + ['build', '--progress=plain', '--tag', tag,
                                           '--build-arg', 'PTAH_COMMIT=' + env['E2E_PTAH_REVISION'], '--build-arg', 'NODE_IMAGE=' + node_image,
                                           '--file', 'support/qualification/probes/profile_toolbox.Dockerfile', '-'], context, timeout=2400)
        return tag, json.loads(self.run('toolbox-inspect', docker + ['image', 'inspect', tag]))[0]['Id']

    def measure(self):
        self.begin('measure')
        env = self.env
        prepared = self.state['steps']['prepare']
        workload = json.loads(Path(prepared['workload']).read_text())
        host = self.preflight()
        tag, image_id = self.toolbox()
        docker = ['docker', '--context', env['E2E_DOCKER_CONTEXT']]
        node = env['E2E_KIND_CLUSTER_NAME'] + '-control-plane'
        files = {'work/evidence/capacity-bootstrap.json': (self.dir / 'capacity-bootstrap.json').read_bytes(),
                 'work/evidence/host.json': json.dumps(host).encode(),
                 'work/evidence/workload.json': Path(prepared['workload']).read_bytes(),
                 'work/native.py': INNER.encode()}
        bootstrap = self.state['steps']['bootstrap']
        if bootstrap.get('profile'):
            files['work/evidence/profile-freeze.json'] = json.dumps(dict(bootstrap['profile'], harnessCommit=bootstrap['harnessCommit'],
                                                                         development=bootstrap['development'])).encode()
        for role, key in (('installer', 'E2E_KUBECONFIG'), ('author', 'CAPACITY_AUTHOR_KUBECONFIG'), ('approver', 'CAPACITY_APPROVER_KUBECONFIG')):
            config = json.loads(self.run('flatten-' + role, ['kubectl', '--kubeconfig', env[key], 'config', 'view', '--raw', '--flatten', '-o', 'json']))
            config['clusters'][0]['cluster']['server'] = 'https://127.0.0.1:6443'
            files['access/' + role + '.json'] = json.dumps(config).encode()
        argv = ['/usr/local/bin/capacity', '-kubeconfig', '/access/installer.json',
                '-author-kubeconfig', '/access/author.json', '-approver-kubeconfig', '/access/approver.json',
                '-host-info', '/work/evidence/host.json', '-workload', '/work/evidence/workload.json',
                '-namespace', ','.join(WORKLOAD_NAMESPACES), '-operator-namespace', 'ptah-system',
                '-checkpoint-state', '/work/evidence/capacity-bootstrap.json',
                '-registry-egress-policies', 'ptah-schema-operations-registry,ptah-migration-operations-registry',
                '-registry-ca-configmap', env['CAPACITY_REGISTRY_CA_CONFIGMAP'],
                '-fixture-image', env['E2E_FIXTURE_IMAGE'], '-out', '/work/evidence']
        verify = None
        if prepared['varied']:
            inputs = self.dir / 'inputs'
            for path in sorted(inputs.rglob('*')):
                if path.is_symlink():
                    raise RuntimeError('an input is a symlink')
                if path.is_file() and 'checkpoints' not in path.relative_to(inputs).parts:
                    files['work/evidence/inputs/' + path.relative_to(inputs).as_posix()] = path.read_bytes()
            argv += ['-inputs', '/work/evidence/inputs/catalog.json',
                     '-checkpoint-probe', '/src/support/qualification/probes/capacity_workload.py']
            soak = workload.get('soak') or {}
            verify = {'changed': workload['changeBatch'], 'round': soak.get('rounds', 1)}
        else:
            refs = self.state['labInputs']
            argv += ['-schema-v1', refs['schema-v1'], '-schema-v2', refs['schema-v2'],
                     '-migration-v1', refs['migration-v1'], '-migration-v2', refs['migration-v2']]
        files['access/configuration.json'] = json.dumps({'environment': {'E2E_KUBECONFIG': '/access/installer.json',
            'CAPACITY_AUTHOR_KUBECONFIG': '/access/author.json', 'CAPACITY_APPROVER_KUBECONFIG': '/access/approver.json',
            'E2E_PTAH_REVISION': env['E2E_PTAH_REVISION'], 'CAPACITY_REGISTRY_CA_CONFIGMAP': env['CAPACITY_REGISTRY_CA_CONFIGMAP']},
            'argv': argv, 'verify': verify, 'unrelated': bool(workload.get('unrelatedObjects'))}).encode()
        name = 'ptah-capacity-' + uuid.uuid4().hex[:12]
        record = {'container': name, 'toolbox': tag, 'toolboxImageID': image_id, 'startedAt': now()}
        container, retained = None, False
        output = self.dir / 'measurement'
        try:
            container = self.run('toolbox-run', docker + ['run', '-d', '--name', name, '--label', 'operator.ptah.run/capacity-toolbox=' + name,
                                                         '--network', 'container:' + node, tag, 'sleep', 'infinity']).decode().strip()
            self.run('toolbox-copy', docker + ['exec', '-i', container, 'tar', '-xzf', '-', '-C', '/'], archive(files), timeout=300)
            self.run('toolbox-modes', docker + ['exec', container, 'chmod', '0700', '/access', '/work', '/work/evidence'])
            with (self.dir / 'measurement.log').open('wb') as log:
                result = subprocess.run(docker + ['exec', container, 'python3', '/work/native.py'], stdout=log, stderr=subprocess.STDOUT,
                                        env=self.child_env())
            record['exitCode'] = result.returncode
            raw = self.run('toolbox-export', docker + ['exec', '-i', container, 'python3', '-'], EXPORT.encode(), timeout=600)
            inventory = receive(raw, output)
            (self.dir / 'measurement.tar.gz').write_bytes(raw)
            record.update(evidenceSHA256=sha(raw), verifiedFiles=len(inventory))
            retained = True
        finally:
            if container and retained:
                subprocess.run(docker + ['rm', '-f', container], capture_output=True, env=self.child_env())
                record['containerRemoved'] = True
            record['finishedAt'] = now()
            (self.dir / 'measurement-run.json').write_text(json.dumps(record, indent=2) + '\n')
        if record['exitCode']:
            raise RuntimeError(f'the measurement exited {record["exitCode"]}; evidence in {output}')
        self.finish('measure', **record)

    # down -----------------------------------------------------------------

    def down(self):
        env = self.env
        environment = self.dir / 'environment'
        result = subprocess.run([str(ROOT / 'demo/bin/lab'), 'down'], cwd=ROOT,
                                env=dict(os.environ, LAB_ENVIRONMENT=str(environment), DOCKER_CONTEXT=env['E2E_DOCKER_CONTEXT']),
                                capture_output=True)
        (self.dir / 'down.log').write_bytes(result.stdout + result.stderr)
        self.state['down'] = {'exitCode': result.returncode, 'at': now()}
        self.save()
        if result.returncode:
            raise RuntimeError('the lab teardown failed; see down.log')


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('step', choices=STEPS + ('down',))
    parser.add_argument('--lab', required=True, help='Private lab directory')
    parser.add_argument('--context', help='bootstrap: Docker context')
    parser.add_argument('--kubernetes', help='bootstrap: Kubernetes version, such as 1.37.0')
    parser.add_argument('--release-manifest', help='bootstrap: release-manifest.txt of the prepared release')
    parser.add_argument('--workload', help='prepare: capacity workload file')
    parser.add_argument('--development', action='store_true', help='bootstrap: this checkout\'s own build; qualifies nothing')
    args = parser.parse_args()
    os.umask(0o077)
    lab = Lab(args.lab)
    if args.step == 'bootstrap':
        if not (args.context and args.kubernetes) or args.context in ('default', 'orbstack'):
            parser.error('bootstrap needs --context and --kubernetes on a remote context')
        if bool(args.release_manifest) == args.development:
            parser.error('bootstrap needs --release-manifest, or --development for a lab that qualifies nothing')
        lab.bootstrap(args.context, args.kubernetes, args.release_manifest, args.development)
    elif args.step == 'prepare':
        if not args.workload:
            parser.error('prepare needs --workload')
        lab.prepare(args.workload)
    else:
        getattr(lab, args.step)()
    return 0


if __name__ == '__main__':
    sys.exit(main())
