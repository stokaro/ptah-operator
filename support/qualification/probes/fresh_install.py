#!/usr/bin/env python3
"""Execute the published installation guide on a fresh cluster.

Issue #582 asks for the installation and the first schema to work as written,
with the released artifacts and nothing the acceptance lab sets up. This reads
the guide's own fenced commands out of the documentation at the release commit
and runs them in order: verify the release, install by digest, confirm the
installation, then the first schema on the engine the run names. Each engine
runs on its own fresh cluster, the way a reader picks one.

What a reader brings is provided as a fixture and kept apart from the guide: a
database server for that engine with an owner login, an anonymous registry the
cluster can read, and the schema published to it with the pinned Ptah. Every
place the guide asks the reader to substitute a value is listed in the
transcript beside the value used. A command that differs from the published
text is a finding, not a step.

With --upgrade-from, the walkthrough first installs the published predecessor
through its own guide and converges the first schema on it. It then verifies
the target release and runs its installation command as the upgrade the
operations guide describes, and requires the runtime to come up on the
target's image with the converged schema still converged: refreshed by the new
manager, with the applied record it had and no plan asking for approval.
"""

import argparse
import datetime
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[3]
FENCE = re.compile(r'^```(\w*)\s*$')
HEADING = re.compile(r'^(#{1,6})\s+(.*?)\s*(\{#[^}]*\})?\s*$')


def blocks(markdown, heading, language='sh'):
    """Return the fenced blocks of one language under a heading.

    A section ends at the next heading of the same or a higher level, so a
    subsection's commands belong to the section that contains it. A heading
    given as `#anchor` matches the heading with that explicit anchor, for a
    page whose titles repeat.
    """
    found, level, inside, fence, current = [], None, False, None, []
    for line in markdown.splitlines():
        if fence is not None:
            if line.strip() == '```':
                if inside and fence == language:
                    found.append('\n'.join(current) + '\n')
                fence, current = None, []
            else:
                current.append(line)
            continue
        match = HEADING.match(line)
        if match:
            depth, title = len(match[1]), match[2]
            if inside and depth <= level:
                inside = False
            if (match[3] == '{' + heading + '}') if heading.startswith('#') else title == heading:
                inside, level = True, depth
            continue
        opened = FENCE.match(line)
        if opened:
            fence = opened[1]
    if not found:
        raise ValueError(f'no {language} block under "{heading}"')
    return found


# What each engine's server needs as a fixture: its image variable, port, data
# directory and the administrator password variable its image reads.
SERVERS = {
    'postgresql': {'name': 'postgres', 'image': 'GUIDE_POSTGRES_IMAGE', 'port': 5432,
                   'data': '/var/lib/postgresql/data', 'password': 'POSTGRES_PASSWORD'},
    'mysql': {'name': 'mysql', 'image': 'GUIDE_MYSQL_IMAGE', 'port': 3306,
              'data': '/var/lib/mysql', 'password': 'MYSQL_ROOT_PASSWORD'},
}


def fixture_deployment(namespace, name, image, port, env, data=None):
    """One fixture server and its Service.

    A database keeps its data on an emptyDir, as the acceptance suite's do, and
    is ready only once its port accepts connections. Without the probe a
    rollout reports a server ready while its image is still initializing.
    """
    labels = {'app': name}
    container = {'name': name, 'image': image, 'env': env, 'ports': [{'containerPort': port}],
                 'readinessProbe': {'tcpSocket': {'port': port}, 'periodSeconds': 2}}
    spec = {'containers': [container]}
    if data:
        container['volumeMounts'] = [{'name': 'data', 'mountPath': data}]
        spec['volumes'] = [{'name': 'data', 'emptyDir': {}}]
    return [
        {'apiVersion': 'apps/v1', 'kind': 'Deployment', 'metadata': {'name': name, 'namespace': namespace},
         'spec': {'replicas': 1, 'selector': {'matchLabels': labels},
                  'template': {'metadata': {'labels': labels}, 'spec': spec}}},
        {'apiVersion': 'v1', 'kind': 'Service', 'metadata': {'name': name, 'namespace': namespace},
         'spec': {'selector': labels, 'ports': [{'port': port, 'targetPort': port}]}},
    ]


def timestamp(value):
    return datetime.datetime.fromisoformat(value.replace('Z', '+00:00'))


def runtime_findings(deployments, pods, digest):
    """What keeps the release namespace from running only the candidate image.

    Every Deployment the chart installs has to be rolled out, and every
    container of every Pod they select, init containers included, has to run
    the candidate digest. A Deployment with no Pod examined is a finding rather
    than a pass. Returns the findings and the Pods examined.
    """
    findings, examined = [], []
    for deployment in deployments:
        name, spec = deployment['metadata']['name'], deployment['spec']
        status = deployment.get('status', {})
        wanted = spec.get('replicas', 1)
        if status.get('observedGeneration') != deployment['metadata'].get('generation') or not (
                status.get('replicas') == status.get('updatedReplicas') == status.get('availableReplicas') == wanted):
            findings.append(f'deployment {name} is not rolled out: {status}')
        template = spec['template']['spec']
        for container in template.get('initContainers', []) + template['containers']:
            if not container['image'].endswith('@' + digest):
                findings.append(f'deployment {name} container {container["name"]} names {container["image"]}')
        selector = spec['selector']['matchLabels']
        selected = [pod for pod in pods if all(pod['metadata'].get('labels', {}).get(k) == v for k, v in selector.items())]
        if not selected:
            findings.append(f'deployment {name} selects no Pod')
        examined += selected
        for pod in selected:
            pod_status = pod.get('status', {})
            statuses = pod_status.get('initContainerStatuses', []) + pod_status.get('containerStatuses', [])
            if not statuses:
                findings.append(f'pod {pod["metadata"]["name"]} reports no container')
            for container in statuses:
                if not container.get('imageID', '').endswith('@' + digest):
                    findings.append(f'pod {pod["metadata"]["name"]} container {container["name"]} runs {container.get("imageID")}')
    return findings, examined


def converged_after(schema, before, started):
    """Whether a schema reading shows a refresh that began on the new runtime
    and found nothing to do.

    The refresh has to have finished after the newest runtime Pod started, so
    the reading is the new manager's rather than the predecessor's. The schema
    has to be in sync for the applied record it had before the upgrade, under
    the same execution epoch: a release that changes only the manager and its
    runner invalidates nothing.
    """
    status = schema.get('status', {})
    finished = status.get('lastSuccessfulReconciliation')
    if not finished or timestamp(finished) <= started:
        return False
    in_sync = [c for c in status.get('conditions', []) if c['type'] == 'InSync']
    epoch = (status.get('executionBinding') or {}).get('epoch')
    return (status.get('phase') == 'InSync' and status.get('applied') == before['applied'] and bool(in_sync)
            and epoch is not None and epoch == before['executionBinding']['epoch']
            and in_sync[0]['status'] == 'True' and in_sync[0]['reason'] == 'ScopedConverged')


def upgrade_command(install, operations):
    """The release's installation command as the operations guide upgrades:
    with the flag it names for an upgrade that changes the manager image."""
    upgrade = blocks(operations, '#upgrade-run')
    if 'An upgrade that changes the manager image needs `--force-conflicts`' not in operations or '--force-conflicts' not in upgrade[0]:
        raise ValueError('the operations guide no longer upgrades with --force-conflicts')
    last = '  --set-string "execution.ptahVersion=$ptah_version"\n'
    return substitute(install, [(last, last.rstrip('\n') + ' \\\n  --force-conflicts\n')])


def substitute(text, replacements):
    """Replace each placeholder exactly once, and refuse one that is missing."""
    for old, new in replacements:
        if text.count(old) != 1:
            raise ValueError(f'the guide no longer contains exactly one {old!r}')
        text = text.replace(old, new)
    return text


class Transcript:
    def __init__(self, directory):
        self.directory = Path(directory)
        self.directory.mkdir(mode=0o700, parents=True, exist_ok=False)
        self.path = self.directory / 'transcript.jsonl'
        self.index = 0

    def record(self, **entry):
        entry['at'] = datetime.datetime.now(datetime.timezone.utc).isoformat()
        with self.path.open('a') as stream:
            stream.write(json.dumps(entry, sort_keys=True) + '\n')

    def run(self, step, script, cwd, env, timeout=1800, check=True, source=None):
        self.index += 1
        started = time.monotonic()
        result = subprocess.run(['bash', '-c', 'set -euo pipefail\n' + script], cwd=cwd, env=env,
                                capture_output=True, text=True, timeout=timeout)
        output = self.directory / f'{self.index:03d}-{step}.out'
        output.write_text(result.stdout + ('\n--- stderr ---\n' + result.stderr if result.stderr else ''))
        self.record(step=step, source=source, script=script, exitCode=result.returncode,
                    seconds=round(time.monotonic() - started, 3), output=output.name)
        if check and result.returncode:
            raise RuntimeError(f'{step} exited {result.returncode}; see {output}')
        return result


class Walkthrough:
    def __init__(self, args):
        self.tag, self.predecessor = args.tag, args.upgrade_from
        for tag in filter(None, (self.tag, self.predecessor)):
            if not re.fullmatch(r'v[0-9]+\.[0-9]+\.[0-9]+', tag):
                raise ValueError('the walkthrough installs a published release tag')
        if self.predecessor == self.tag:
            raise ValueError('an upgrade needs a predecessor other than the target')
        if args.engine not in SERVERS:
            raise ValueError(f'the walkthrough runs one of {sorted(SERVERS)}')
        self.engine = args.engine
        self.work = Path(args.work).resolve()
        self.work.mkdir(mode=0o700, parents=True, exist_ok=False)
        self.transcript = Transcript(args.evidence)
        self.env = dict(os.environ)
        self.fixtures = 'ptah-guide-fixtures'
        self.source = None

    def docs(self, page):
        if self.source is None:
            raise RuntimeError('the release commit is not known yet')
        return subprocess.run(['git', '-C', str(ROOT), 'show', f'{self.source}:docs/site/src/content/docs/{page}'],
                              check=True, capture_output=True, text=True).stdout

    def kubectl(self, *args, stdin=None, check=True):
        return subprocess.run(['kubectl', *args], input=stdin, capture_output=True, text=True, check=check, env=self.env)

    def verify_and_install(self, upgrade=False):
        """Run the release guide's verification and installation verbatim.

        As an upgrade, the installation carries the one flag the operations
        guide adds to an upgrade that changes the manager image.
        """
        source = subprocess.run(['gh', 'api', f'repos/stokaro/ptah-operator/commits/{self.tag}', '--jq', '.sha'],
                                check=True, capture_output=True, text=True, env=self.env).stdout.strip()
        if not re.fullmatch(r'[0-9a-f]{40}', source):
            raise ValueError('the release tag resolves to no commit')
        self.source = source
        releases = self.docs('support/releases.md')
        verify = ''.join(blocks(releases, 'Verify before installation'))
        install = ''.join(blocks(releases, 'Install by digest'))
        if f'tag={self.tag}\n' not in verify:
            raise ValueError(f'the published verification names another tag than {self.tag}')
        if upgrade:
            install = upgrade_command(install, self.docs('use/operations.md'))
            self.transcript.record(step='substitute-upgrade', source='use/operations.md#upgrade-run', added='--force-conflicts')
        # The blocks share one shell: the installation reads the image, executor
        # and version the verification established.
        script = verify + install + 'printf "image=%s\\nexecutor=%s\\nptah_version=%s\\n" "$image" "$executor" "$ptah_version"\n'
        # Each release downloads into a directory of its own: the guide's
        # download refuses a file an earlier release left there.
        directory = self.work / self.tag
        directory.mkdir(mode=0o700)
        result = self.transcript.run('upgrade' if upgrade else 'verify-and-install', script, directory, self.env,
                                     source='support/releases.md#verify-before-installation,#install-by-digest'
                                            + (' with use/operations.md#upgrade-run' if upgrade else ''))
        identities = dict(line.split('=', 1) for line in result.stdout.splitlines() if re.match(r'^(image|executor|ptah_version)=', line))
        self.transcript.record(step='release-identities', releaseSource=source, **identities)
        return identities

    def runtime_on(self, image, timeout=300):
        """Wait for the release namespace to run only the candidate image, and
        return when its newest Pod started."""
        digest = image.rsplit('@', 1)[1]
        deadline = time.monotonic() + timeout
        while True:
            deployments, pods = (json.loads(self.kubectl('-n', 'ptah-system', 'get', kind, '-o', 'json').stdout)['items']
                                 for kind in ('deployments', 'pods'))
            findings, pods = runtime_findings(deployments, pods, digest)
            if len(deployments) != 2:
                findings.append(f'expected the manager and certificate-rotator Deployments, found {len(deployments)}')
            if not findings:
                break
            if time.monotonic() > deadline:
                raise RuntimeError('the upgrade left the runtime off the candidate: ' + '; '.join(findings))
            time.sleep(5)
        started = max(timestamp(p['status']['startTime']) for p in pods)
        self.transcript.record(step='runtime-on-candidate', digest=digest, deployments=[d['metadata']['name'] for d in deployments],
                               pods=sorted(p['metadata']['name'] for p in pods), newestPodStarted=started.isoformat())
        return started

    def schema_reading(self):
        schema = json.loads(self.kubectl('-n', 'application', 'get', 'ptahschema', 'application', '-o', 'json').stdout)
        plans = json.loads(self.kubectl('-n', 'application', 'get', 'ptahschemaplans', '-o', 'json').stdout)['items']
        return schema, sorted(p['metadata']['uid'] for p in plans)

    def still_converged(self, before, started, timeout=1500):
        """Wait for the new manager to refresh the schema and find it converged.

        The schema refreshes at its interval, which the predecessor scheduled,
        so the wait covers one interval and a cycle.
        """
        schema, plans = before
        deadline, reading = time.monotonic() + timeout, schema
        while time.monotonic() < deadline:
            reading, now_plans = self.schema_reading()
            if converged_after(reading, schema['status'], started):
                if now_plans != plans:
                    raise RuntimeError(f'the upgrade added plans: before {plans}, after {now_plans}')
                break
            time.sleep(5)
        else:
            raise RuntimeError(f'the schema was not refreshed in sync after the upgrade: {json.dumps(reading.get("status"))}')
        if not self.has_customers(self.engine):
            raise RuntimeError(f'{self.engine}: the database lost the customers table across the upgrade')
        status = reading['status']
        self.transcript.record(step='converged-across-upgrade', applied=status['applied'], plans=plans,
                               epoch=status['executionBinding']['epoch'],
                               lastSuccessfulReconciliation=status['lastSuccessfulReconciliation'],
                               nextReconciliationTime=status.get('nextReconciliationTime'))

    def confirm_installed(self):
        install = self.docs('start/install.md')
        script = ''.join(blocks(install, 'Confirm it installed'))
        result = self.transcript.run('confirm-installed', script, self.work, self.env, source='start/install.md#confirm-it-installed')
        crds = [line.split('\t') for line in result.stdout.splitlines() if '\t' in line]
        established = [name for name, status in crds if status == 'True']
        expected = sorted(Path(p).name.split('_', 1)[1].removesuffix('.yaml') + '.operator.ptah.run'
                          for p in subprocess.run(['git', '-C', str(ROOT), 'ls-tree', '--name-only', self.source, 'config/crd/bases/'],
                                                  check=True, capture_output=True, text=True).stdout.split())
        if sorted(established) != expected or len(crds) != len(expected):
            raise RuntimeError(f'the installation established {sorted(established)}, expected {expected}')
        self.transcript.record(step='crds-established', count=len(established), names=sorted(established))

    def fixture(self):
        """Stand up what a reader brings: a database server and an anonymous registry."""
        server = SERVERS[self.engine]
        images = {server['name']: os.environ[server['image']], 'registry': os.environ['GUIDE_REGISTRY_IMAGE']}
        for value in images.values():
            if not re.fullmatch(r'[^@\s]+@sha256:[0-9a-f]{64}', value):
                raise ValueError('fixture images must be pinned by digest')
        self.passwords = {'admin': secrets.token_hex(16), 'application': secrets.token_hex(16)}
        manifests = [
            {'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': self.fixtures}},
            {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'admin', 'namespace': self.fixtures},
             'stringData': {'password': self.passwords['admin']}},
            *fixture_deployment(self.fixtures, server['name'], images[server['name']], server['port'],
                                [{'name': server['password'], 'valueFrom': {'secretKeyRef': {'name': 'admin', 'key': 'password'}}}],
                                data=server['data']),
            *fixture_deployment(self.fixtures, 'registry', images['registry'], 5000, []),
        ]
        self.kubectl('apply', '-f', '-', stdin='\n---\n'.join(json.dumps(m) for m in manifests))
        for name in (server['name'], 'registry'):
            result = self.kubectl('-n', self.fixtures, 'rollout', 'status', f'deployment/{name}', '--timeout=300s', check=False)
            if result.returncode:
                self.diagnose(name)
                raise RuntimeError(f'the {name} fixture did not become ready: {result.stderr}')
        self.transcript.record(step='fixture', note=f'A {self.engine} server and an anonymous plain-HTTP registry, as the reader brings them.',
                               images=images)

    def diagnose(self, deployment):
        """Keep what the fixture's Pods said, so a failed run explains itself."""
        report = []
        for args in (['get', 'pods', '-o', 'wide'], ['describe', f'deployment/{deployment}'], ['describe', 'pods', '-l', f'app={deployment}'],
                     ['logs', f'deployment/{deployment}', '--tail=200'], ['logs', f'deployment/{deployment}', '--previous', '--tail=200']):
            result = self.kubectl('-n', self.fixtures, *args, check=False)
            report.append(f'$ kubectl -n {self.fixtures} {" ".join(args)}\n{result.stdout}{result.stderr}')
        path = self.transcript.directory / f'fixture-{deployment}-diagnostics.txt'
        path.write_text('\n'.join(report))
        self.transcript.record(step='fixture-diagnostics', deployment=deployment, output=path.name)

    def admin_sql(self, engine, statements):
        if engine == 'postgresql':
            command = ['psql', '-v', 'ON_ERROR_STOP=1', '-U', 'postgres']
        else:
            # Over TCP: the server's socket path is whatever the image's my.cnf
            # says, which need not be where the client looks by default.
            command = ['sh', '-c', 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -h 127.0.0.1 -P 3306 -u root --batch --skip-column-names']
        deployment = SERVERS[engine]['name']
        for attempt in range(30):
            result = self.kubectl('-n', self.fixtures, 'exec', '-i', f'deploy/{deployment}', '--', *command, stdin=statements, check=False)
            if result.returncode == 0:
                return result.stdout
            time.sleep(5)
        self.diagnose(deployment)
        raise RuntimeError(f'{engine} administration failed: {result.stderr}')

    def database_url(self, engine):
        """Create the owner login the databases page asks for, and its URL."""
        password = self.passwords['application']
        if engine == 'postgresql':
            self.admin_sql(engine, f"CREATE ROLE application LOGIN PASSWORD '{password}';\nCREATE DATABASE application OWNER application;\n")
            return f'postgres://application:{password}@postgres.{self.fixtures}.svc.cluster.local:5432/application?sslmode=disable'
        self.admin_sql(engine, f"CREATE DATABASE application;\nCREATE USER 'application'@'%' IDENTIFIED BY '{password}';\n"
                               "GRANT ALL PRIVILEGES ON application.* TO 'application'@'%';\n")
        return f'mysql://application:{password}@tcp(mysql.{self.fixtures}.svc.cluster.local:3306)/application'

    def publish(self, engine):
        """Publish the reader's schema with the pinned Ptah, through a port-forward."""
        dialect = 'postgres' if engine == 'postgresql' else 'mysql'
        schema = self.work / f'schema-{engine}.sql'
        schema.write_text('CREATE TABLE customers (\n  id BIGINT PRIMARY KEY,\n  email VARCHAR(320) NOT NULL\n);\n')
        log = (self.work / f'registry-forward-{engine}.log').open('w')
        forward = subprocess.Popen(['kubectl', '-n', self.fixtures, 'port-forward', 'service/registry', '0:5000'],
                                   stdout=log, stderr=subprocess.STDOUT, env=self.env)
        try:
            port = None
            for _ in range(100):
                match = re.search(r'Forwarding from 127\.0\.0\.1:([0-9]+)', Path(log.name).read_text())
                if match:
                    port = match[1]
                    break
                time.sleep(0.2)
            if port is None:
                raise RuntimeError('the registry port-forward did not start')
            result = self.transcript.run(f'publish-{engine}',
                                         f'ptah schema push oci://127.0.0.1:{port}/application-schema-{engine}:v1 '
                                         f'--schema-file {shlex.quote(str(schema))} --dialect {dialect} --plain-http',
                                         self.work, self.env, source='fixture: the reader publishes their schema')
        finally:
            forward.terminate()
            forward.wait(timeout=10)
        digest = re.search(r'^Digest: (sha256:[0-9a-f]{64})$', result.stdout, re.M)
        if not digest:
            raise RuntimeError('the push printed no digest')
        return f'oci://registry.{self.fixtures}.svc.cluster.local:5000/application-schema-{engine}@{digest[1]}'

    def examples(self):
        """The examples come from the release commit, beside the chart."""
        archive = subprocess.run(['git', '-C', str(ROOT), 'archive', self.source, 'examples'], check=True, capture_output=True).stdout
        subprocess.run(['tar', '-x', '-C', str(self.work)], input=archive, check=True)

    def first_schema(self, engine):
        page = self.docs('start/first-schema.md')
        create = blocks(page, 'Create what the schema reads')
        stops = blocks(page, 'It stops, and waits for you')
        url_file = self.work / f'database-url-{engine}'
        url_file.write_text(self.database_url(engine))
        url_file.chmod(0o600)
        reference = self.publish(engine)
        manifest = 'examples/ptahschema.yaml' if engine == 'postgresql' else 'examples/ptahschema-mysql.yaml'
        path = self.work / manifest
        text = path.read_text()
        placeholder = re.search(r'ociRef: (oci://\S+@sha256:<digest>)', text)
        if not placeholder:
            raise ValueError(f'{manifest} has no artifact placeholder')
        # The two substitutions the guide asks for, and the one an anonymous
        # plain-HTTP registry needs, which the guide names beside them.
        text = text.replace(placeholder[1], reference, 1)
        text = substitute(text, [('    verificationPolicyFrom:\n', '    transport:\n      plainHTTP: true\n    verificationPolicyFrom:\n')])
        path.write_text(text)
        self.transcript.record(step=f'substitute-{engine}', manifest=manifest, ociRef=reference,
                               added='desired.transport.plainHTTP=true for the anonymous plain-HTTP fixture registry',
                               databaseURLFile='private, mode 0600')

        secret_block = substitute(create[0], [('DATABASE_URL_FILE=/path/to/private/database-url\n', f'DATABASE_URL_FILE={shlex.quote(str(url_file))}\n')])
        self.transcript.run(f'create-reads-{engine}', secret_block, self.work, self.env, source='start/first-schema.md#create-what-the-schema-reads')
        apply_index = 1 if engine == 'postgresql' else 2
        apply_block = create[apply_index]
        if f'kubectl apply -f {manifest}\n' not in apply_block or 'get ptahschema application -w' not in apply_block:
            raise ValueError(f'the guide no longer applies {manifest} and watches the schema')
        # A watch never returns; the reader stops it once the phase shows. This
        # runs the apply as written and then waits for that phase.
        self.transcript.run(f'apply-{engine}', apply_block.split('kubectl -n application get ptahschema application -w')[0],
                            self.work, self.env, source=f'start/first-schema.md (apply {manifest}; the watch is bounded)')
        self.wait_phase('AwaitingApproval', 'ApprovalRequired')

        self.transcript.run(f'install-plugin-{engine}', self.plugin_install(), self.work, self.env, source='use/read-a-plan.md#install')
        for index, block in enumerate(stops):
            self.transcript.run(f'waits-for-you-{engine}-{index}', block, self.work, self.env, source='start/first-schema.md#it-stops-and-waits-for-you')
            if 'kubectl -n application create -f -' in block:
                self.wait_condition('InSync', 'True', 'ScopedConverged')
        conditions = self.kubectl('-n', 'application', 'get', 'ptahschema', 'application', '-o', 'json')
        status = json.loads(conditions.stdout)['status']
        in_sync = [c for c in status.get('conditions', []) if c['type'] == 'InSync']
        if not in_sync or in_sync[0]['status'] != 'True' or in_sync[0]['reason'] != 'ScopedConverged':
            raise RuntimeError(f'{engine}: the schema did not converge: {in_sync}')
        if not self.has_customers(engine):
            raise RuntimeError(f'{engine}: the database has no customers table after convergence')
        self.transcript.record(step=f'database-{engine}', table='customers', present=True, inSync=in_sync[0])

    def has_customers(self, engine):
        tables = self.admin_sql(engine, "SELECT table_name FROM information_schema.tables WHERE table_name = 'customers';\n"
                                if engine == 'mysql' else "\\c application\nSELECT tablename FROM pg_tables WHERE tablename = 'customers';\n")
        return 'customers' in tables

    def plugin_install(self):
        """The plugin as a reader installs it, onto a directory on this PATH."""
        page = self.docs('use/read-a-plan.md')
        block = ''.join(blocks(page, 'Install it'))
        machine = os.uname().machine
        platform = {'x86_64': 'linux-amd64', 'aarch64': 'linux-arm64', 'arm64': 'linux-arm64'}.get(machine)
        if platform is None or sys.platform != 'linux':
            raise ValueError('the walkthrough runs on Linux amd64 or arm64')
        target = Path(os.environ['GUIDE_BIN']).resolve()
        replacements = [
            ('version=<release tag>\n', f'version={self.tag}\n'),
            ('platform=darwin-arm64   # or linux-amd64, linux-arm64, darwin-amd64\n', f'platform={platform}\n'),
            ('/usr/local/bin/kubectl-ptah', shlex.quote(str(target / 'kubectl-ptah'))),
        ]
        self.transcript.record(step='substitute-plugin', replacements=[[old, new] for old, new in replacements])
        return substitute(block, replacements)

    def wait_phase(self, phase, condition, timeout=600):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            result = self.kubectl('-n', 'application', 'get', 'ptahschema', 'application', '-o', 'json', check=False)
            if result.returncode == 0:
                status = json.loads(result.stdout).get('status', {})
                conditions = {c['type']: c['status'] for c in status.get('conditions', [])}
                if status.get('phase') == phase and conditions.get(condition) == 'True':
                    self.transcript.record(step='phase', phase=phase, condition=condition)
                    return
            time.sleep(5)
        raise RuntimeError(f'the schema did not reach {phase}')

    def wait_condition(self, kind, value, reason, timeout=900):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            result = self.kubectl('-n', 'application', 'get', 'ptahschema', 'application', '-o', 'json', check=False)
            if result.returncode == 0:
                for condition in json.loads(result.stdout).get('status', {}).get('conditions', []):
                    if condition['type'] == kind and condition['status'] == value and condition['reason'] == reason:
                        return
            time.sleep(5)
        raise RuntimeError(f'the schema did not reach {kind}={value} {reason}')

    # The guide's resource is `application` in namespace `application` for either
    # engine, and it says to choose one manifest. A reader does one engine per
    # installation, so each engine gets a fresh cluster. Reusing the namespace is
    # not an option: deleting it waits for the result records' one-hour
    # retention window before the namespace can go.
    def run(self):
        if self.predecessor:
            return self.upgrade()
        identities = self.verify_and_install()
        self.confirm_installed()
        self.fixture()
        self.examples()
        self.first_schema(self.engine)
        self.transcript.record(step='complete', tag=self.tag, engine=self.engine, releaseSource=self.source, **identities)

    def upgrade(self):
        target, self.tag = self.tag, self.predecessor
        installed = self.verify_and_install()
        self.confirm_installed()
        self.fixture()
        self.examples()
        self.first_schema(self.engine)
        before = self.schema_reading()
        self.transcript.record(step='predecessor-converged', tag=self.tag, releaseSource=self.source,
                               applied=before[0]['status']['applied'], epoch=before[0]['status']['executionBinding']['epoch'],
                               plans=before[1])
        self.tag = target
        identities = self.verify_and_install(upgrade=True)
        if identities['image'] == installed['image']:
            raise ValueError('the predecessor and the target run the same manager image; the drill measures an upgrade that changes it')
        self.confirm_installed()
        started = self.runtime_on(identities['image'])
        self.still_converged(before, started)
        self.transcript.record(step='complete', tag=self.tag, upgradedFrom=self.predecessor, engine=self.engine,
                               releaseSource=self.source, **identities)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--tag', required=True)
    parser.add_argument('--engine', required=True, choices=sorted(SERVERS))
    parser.add_argument('--upgrade-from', help='install this published release first and upgrade it to --tag')
    parser.add_argument('--work', required=True)
    parser.add_argument('--evidence', required=True)
    args = parser.parse_args()
    os.umask(0o077)
    Walkthrough(args).run()
    return 0


if __name__ == '__main__':
    sys.exit(main())
