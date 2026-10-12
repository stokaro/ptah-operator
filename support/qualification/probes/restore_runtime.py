"""Keep a cold restore on the authenticated release bytes selected before loss."""
import copy
import hashlib
import json
from pathlib import Path
import re


REPOSITORY = 'stokaro/ptah-operator'
# The executor is Ptah's own release image, run as Ptah published it. The
# authenticated manifest names it by digest; its signature is Ptah's, not this
# repository's release workflow's.
EXECUTOR_REPOSITORY = 'stokaro/ptah'
ROOT = Path(__file__).resolve().parents[3]


def release_runtime_paths(root=ROOT):
    """The paths a release's runtime is built from, as the e2e driver declares them.

    The manifest-backed acceptance run refuses a harness that changes any of
    them relative to the release source. Recovery holds its harness to the same
    list rather than to a second copy of it.
    """
    match = re.search(r"^RELEASE_RUNTIME_PATHS='([^']+)'$", (root / 'hack/e2e-kind.sh').read_text(), re.M)
    if not match:
        raise ValueError('hack/e2e-kind.sh declares no release runtime paths')
    return match.group(1).split()


class ReleaseRuntime:
    def __init__(self, probe, directory, prepared_manifest_sha256=None):
        self.probe = probe
        self.directory = Path(directory).resolve()
        self.manifest = self.directory / 'release-manifest.txt'
        # The manifest is parsed from the bytes that are hashed, so the release
        # it names is the one whose digest is checked from here on.
        raw = self.manifest.read_bytes()
        self.fields = {}
        for line in raw.decode().splitlines():
            key, separator, value = line.partition('=')
            if not separator or not value or key in self.fields:
                raise ValueError('Invalid or duplicate release manifest record')
            self.fields[key] = value
        version = self.fields.get('version', '')
        if not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+', version):
            raise ValueError('The release manifest names no release version')
        self.tag = 'v' + version
        self.chart = self.directory / ('ptah-operator-' + version + '.tgz')
        self.hashes = {self.manifest.name: hashlib.sha256(raw).hexdigest()}
        self.hashes.update({path.name: self.sha(path) for path in (self.chart, self.directory / 'SHA256SUMS')})
        self.prepared_manifest_sha256 = prepared_manifest_sha256
        if prepared_manifest_sha256 is not None and (
                not re.fullmatch(r'[0-9a-f]{64}', prepared_manifest_sha256)
                or prepared_manifest_sha256 != self.hashes[self.manifest.name]):
            raise ValueError('Prepared recovery assets must match the explicitly selected manifest digest')
        self.source_ref = self.fields.get('source-ref')
        if self.source_ref not in ('refs/tags/' + self.tag, 'refs/heads/master'):
            raise ValueError('Recovery requires the exact release tag or protected master build ref')
        expected = {'source-repository': REPOSITORY,
                    'chart-asset': self.chart.name, 'chart-asset-sha256': self.hashes[self.chart.name]}
        if any(self.fields.get(key) != value for key, value in expected.items()):
            raise ValueError('Recovery release assets do not match the selected source and chart')
        # The release source is what the authenticated manifest names. The
        # harness running the recovery is checked against it in authenticate().
        self.source = self.fields.get('source-sha', '')
        if not re.fullmatch(r'[0-9a-f]{40}', self.source):
            raise ValueError('The release manifest names no source commit')
        for key, image in (('image', REPOSITORY), ('executor', EXECUTOR_REPOSITORY)):
            if not re.fullmatch('ghcr.io/' + re.escape(image) + r'@sha256:[0-9a-f]{64}', self.fields.get(key, '')):
                raise ValueError('Recovery requires the release repository and exact image digests')
        self.authenticated = False

    @staticmethod
    def sha(path):
        return hashlib.sha256(path.read_bytes()).hexdigest()

    def unchanged(self):
        if any(self.sha(self.directory / name) != value for name, value in self.hashes.items()):
            raise RuntimeError('The authenticated recovery release assets changed')

    def check_harness(self):
        """Hold the harness to the rule the manifest-backed acceptance run uses.

        The harness may be a later commit than the release, so a defect in the
        recovery procedure is fixed without cutting a release. It must descend
        from the release source and change none of the paths the release's
        runtime is built from; what runs in the cluster is the release's own
        authenticated bytes either way.
        """
        p = self.probe
        harness = p.report['sourceCommit']
        if harness == self.source:
            return
        p.command('confirm the harness descends from the release source', [
            'git', '-C', str(ROOT), 'merge-base', '--is-ancestor', self.source, harness])
        changed = p.command('list release runtime sources the harness changed', [
            'git', '-C', str(ROOT), 'diff', '--name-only', self.source, harness, '--'] + release_runtime_paths()).stdout.decode().split()
        if changed:
            raise RuntimeError('The harness changes runtime sources of the release: ' + ' '.join(changed))

    def authenticate(self):
        p = self.probe
        self.unchanged()
        self.check_harness()
        # Use the existing release contract and consumer commands. No local
        # signature, relaxed source identity, or unsigned mode stands in for it.
        source = self.source
        p.command('validate the complete recovery release asset inventory', [
            'go', 'run', './hack/releaseverify', '-tag', self.tag,
            '-source-sha', source, '-source-ref', self.source_ref, '-manifest', str(self.manifest),
            '-checksums', str(self.directory / 'SHA256SUMS'), '-chart', str(self.chart)])
        if self.prepared_manifest_sha256 is None:
            tagged_source = p.command('resolve the immutable recovery release source', [
                'gh', 'api', 'repos/' + REPOSITORY + '/commits/' + self.tag, '--jq', '.sha']).stdout.decode().strip()
            if tagged_source != source:
                raise RuntimeError('The published recovery release has a different source commit')
            p.command('verify the immutable recovery release', ['gh', 'release', 'verify', self.tag, '--repo', REPOSITORY])
        else:
            self.verify_draft()
        identity = ['--repo', REPOSITORY, '--source-ref', self.source_ref,
                    '--source-digest', source, '--signer-workflow', REPOSITORY + '/.github/workflows/release.yml']
        for path in (self.manifest, self.chart, self.directory / 'SHA256SUMS'):
            if self.prepared_manifest_sha256 is None:
                p.command('verify recovery asset ' + path.name, [
                    'gh', 'release', 'verify-asset', self.tag, str(path), '--repo', REPOSITORY])
            p.command('authenticate recovery asset ' + path.name, ['gh', 'attestation', 'verify', str(path)] + identity)
        # The operator image is this release's own build. The executor is
        # Ptah's image, named by digest in the manifest authenticated above,
        # and its index is held to that digest below.
        reference = self.fields['image']
        p.command('authenticate recovery image', ['gh', 'attestation', 'verify', 'oci://' + reference] + identity)
        p.command('verify recovery image signature', [
            'cosign', 'verify', '--certificate-identity',
            'https://github.com/' + REPOSITORY + '/.github/workflows/release.yml@' + self.source_ref,
            '--certificate-oidc-issuer', 'https://token.actions.githubusercontent.com', reference])
        self.image_digests = {key: self.read_image_digests(self.fields[key]) for key in ('image', 'executor')}
        self.unchanged()
        self.authenticated = True
        p.report['releaseRuntime'] = {'source': source, 'harness': p.report['sourceCommit'],
            'sourceRef': self.source_ref, 'tag': self.tag, 'assets': dict(self.hashes),
            'operatorImage': self.fields['image'], 'runnerImage': self.fields['image'],
            'executorImage': self.fields['executor'], 'installations': [],
            'publicationState': 'signed-draft' if self.prepared_manifest_sha256 else 'immutable-release',
            'scope': 'Authenticated runtime identity only; restore, RPO/RTO and profile acceptance require their own results.'}
        p.persist()

    def verify_draft(self):
        self.unchanged()
        release_id = self.probe.command('locate the authenticated draft by its intended tag', [
            'gh', 'release', 'view', self.tag, '--repo', REPOSITORY,
            '--json', 'databaseId', '--jq', '.databaseId']).stdout.decode().strip()
        if not re.fullmatch(r'[1-9][0-9]*', release_id):
            raise RuntimeError('The selected draft has no valid release ID')
        release = json.loads(self.probe.command('read the selected prepared recovery release', [
            'gh', 'api', '-H', 'X-GitHub-Api-Version: 2026-03-10',
            'repos/' + REPOSITORY + '/releases/' + release_id]).stdout)
        if (release.get('draft') is not True or release.get('immutable') is not False
                or release.get('tag_name') != self.tag or release.get('body') != self.manifest.read_text()
                or release.get('target_commitish') != self.source):
            raise RuntimeError('The prepared release state or manifest differs from the selected recovery kit')
        names = {self.chart.name, self.manifest.name, 'SHA256SUMS', 'acceptance-evidence.tar.gz',
                 'kubectl-ptah-darwin-amd64', 'kubectl-ptah-darwin-arm64',
                 'kubectl-ptah-linux-amd64', 'kubectl-ptah-linux-arm64'}
        assets = release.get('assets', [])
        if len(assets) != len(names) or {a['name'] for a in assets} != names:
            raise RuntimeError('The prepared release asset inventory is incomplete or has extra assets')
        checksums = {name: digest for digest, name in
                     (line.split('  ', 1) for line in (self.directory / 'SHA256SUMS').read_text().splitlines())}
        hashes = {}
        for asset in assets:
            path = self.directory / asset['name']
            digest = self.sha(path)
            if (asset.get('state') != 'uploaded' or asset.get('size') != path.stat().st_size
                    or asset.get('digest') != 'sha256:' + digest
                    or (path.name in self.hashes and self.hashes[path.name] != digest)
                    or (path.name != 'SHA256SUMS' and checksums.get(path.name) != digest)):
                raise RuntimeError('A prepared release asset differs from its authenticated checksum or release readback')
            hashes[path.name] = digest
        self.unchanged()
        self.hashes.update(hashes)

    def install(self, environment, name):
        if not self.authenticated:
            raise RuntimeError('Recovery release authentication must precede installation')
        self.unchanged()
        p = self.probe
        helm = ['helm', '--kubeconfig', environment['E2E_KUBECONFIG'], '--namespace', environment['E2E_OPERATOR_NAMESPACE']]
        release = environment['E2E_HELM_RELEASE']
        values = json.loads(p.command('read recovery bootstrap values', helm + ['get', 'values', release, '--all', '-o', 'json']).stdout)
        values = self.values(values)
        path = p.root / (name + '-release-values.private.json')
        path.write_text(json.dumps(values))
        self.unchanged()
        p.command('install the selected recovery release bytes in ' + name, helm + [
            'upgrade', release, str(self.chart), '-f', str(path), '--force-conflicts', '--wait', '--timeout', '150s'])
        actual = json.loads(p.command('verify installed recovery release values', helm + ['get', 'values', release, '--all', '-o', 'json']).stdout)
        if actual != values:
            raise RuntimeError('The installed recovery release changed the selected effective values')
        kubectl = ['kubectl', '--kubeconfig', environment['E2E_KUBECONFIG'], '--request-timeout=30s',
                   '--namespace', environment['E2E_OPERATOR_NAMESPACE']]
        pods = json.loads(p.command('read installed recovery runtime Pods', kubectl + [
            'get', 'pods', '-l', 'app.kubernetes.io/instance=' + release, '-o', 'json']).stdout)['items']
        identities = self.runtime_pods(pods, values, self.image_digests['image'])
        self.unchanged()
        environment.update(E2E_CONTROLLER_IMAGE=self.fields['image'], E2E_RUNNER_IMAGE=self.fields['image'],
                           E2E_CONTROLLER_REVISION=self.fields['source-sha'], E2E_EXECUTOR_IMAGE=self.fields['executor'],
                           E2E_PTAH_REVISION=self.fields['executor-ptah-commit'], E2E_PTAH_VERSION=self.fields['executor-ptah-version'],
                           E2E_CHART_PACKAGE=str(self.chart))
        p.report['releaseRuntime']['installations'].append({'cluster': name, 'chartSHA256': self.hashes[self.chart.name],
            'operatorImage': self.fields['image'], 'runnerImage': self.fields['image'], 'executorImage': self.fields['executor'],
            'valuesSHA256': hashlib.sha256(json.dumps(actual, sort_keys=True).encode()).hexdigest(), 'pods': identities})
        p.persist()

    def read_image_digests(self, reference):
        # imageID may identify the index or the selected platform manifest.
        # Hash the fetched index before accepting any of its child identities.
        from database_restore import DOCKER
        raw = self.probe.command('read the authenticated runtime image index', DOCKER + [
            'buildx', 'imagetools', 'inspect', '--raw', reference]).stdout
        index_digest = reference.split('@')[1]
        if 'sha256:' + hashlib.sha256(raw).hexdigest() != index_digest:
            raise RuntimeError('Registry image index differs from the authenticated release')
        index = json.loads(raw)
        accepted = {index_digest}
        accepted.update(m['digest'] for m in index.get('manifests', [])
                        if m.get('platform', {}).get('os') == 'linux'
                        and m.get('platform', {}).get('architecture') in ('amd64', 'arm64'))
        return accepted

    def values(self, original):
        values = copy.deepcopy(original)
        repository, digest = self.fields['image'].split('@')
        values['image'].update(repository=repository, digest=digest, tag=self.fields['version'])
        values['execution'].update(runnerImage=self.fields['image'], executorImage=self.fields['executor'],
                                   ptahVersion=self.fields['executor-ptah-version'])
        # Release images are anonymously readable. The bootstrap's private
        # fixture-registry credential must never be offered to GHCR.
        values['imagePullSecrets'] = []
        return values

    def runtime_pods(self, pods, values, accepted):
        selected = {role: [p for p in pods if p['metadata'].get('labels', {}).get('app.kubernetes.io/component') == role]
                    for role in ('controller', 'certificate-rotation')}
        if len(selected['controller']) != values['replicaCount'] or len(selected['certificate-rotation']) != 1:
            raise RuntimeError('The selected recovery runtime replicas are missing or duplicated')
        identities = []
        for pod in selected['controller'] + selected['certificate-rotation']:
            meta, spec, status = pod['metadata'], pod['spec'], pod.get('status', {})
            if meta.get('deletionTimestamp') or status.get('phase') != 'Running':
                raise RuntimeError('Recovery runtime Pod is not running')
            for kind, states in (('containers', 'containerStatuses'), ('initContainers', 'initContainerStatuses')):
                containers, statuses = spec.get(kind, []), status.get(states, [])
                if {c['name'] for c in containers} != {c['name'] for c in statuses} or len(containers) != len(statuses):
                    raise RuntimeError('Recovery runtime container status is incomplete')
                if any(c['image'] != self.fields['image'] for c in containers):
                    raise RuntimeError('Recovery runtime uses a different image')
                for c in statuses:
                    if c.get('restartCount') != 0 or c.get('imageID', '').split('@')[-1] not in accepted:
                        raise RuntimeError('Recovery runtime image identity or restart count differs')
                    if kind == 'containers' and not c.get('ready'):
                        raise RuntimeError('Recovery runtime container is not ready')
                    if kind == 'initContainers' and c.get('state', {}).get('terminated', {}).get('exitCode') != 0:
                        raise RuntimeError('Recovery runtime init container did not succeed')
            if not spec.get('containers'):
                raise RuntimeError('Recovery runtime Pod has no containers')
            identities.append({'uid': meta['uid'], 'name': meta['name'],
                               'imageIDs': [c['imageID'] for c in status['containerStatuses']]})
        return identities

    def verify_apply_pods(self, pods):
        if not pods:
            raise RuntimeError('No Apply Pod establishes the release runtime identity')
        self.unchanged()
        identities = []
        for pod in pods:
            containers = pod['spec'].get('containers', []) + pod['spec'].get('initContainers', [])
            statuses = pod['status'].get('containerStatuses', []) + pod['status'].get('initContainerStatuses', [])
            if {c['name'] for c in containers} != {s['name'] for s in statuses} or len(containers) != len(statuses):
                raise RuntimeError('Apply runtime image status is incomplete')
            if not {'install-runner', 'ptah'} <= {c['name'] for c in containers}:
                raise RuntimeError('Apply Pod lacks the runner or executor')
            for container in containers:
                key = 'image' if container['name'] == 'install-runner' else 'executor'
                status = next(s for s in statuses if s['name'] == container['name'])
                if (container['image'] != self.fields[key]
                        or status.get('imageID', '').split('@')[-1] not in self.image_digests[key]):
                    raise RuntimeError('Apply Pod ran a different runner or executor image')
            identities.append({'uid': pod['metadata']['uid'], 'imageIDs': [s['imageID'] for s in statuses]})
        self.probe.report['releaseRuntime'].setdefault('applyPods', []).extend(identities)
        self.probe.persist()
