#!/usr/bin/env python3
"""Cold recovery from a disposable source kind cluster into a new cluster."""
import argparse
import base64
import copy
import json
import os
from pathlib import Path
import signal
import ssl
import subprocess
import time
from urllib.parse import urlsplit

from operator_restore import OPERATOR_CRDS, OperatorProbe, REPO, db


class ClusterRestoreProbe(OperatorProbe):
    def __init__(self, engine, family, environment, root, loss, timing='idle', result_delivery=False):
        if loss not in ('operator', 'combined'):
            raise ValueError('Cold cluster recovery requires operator or combined loss')
        super().__init__(engine, family, environment, root, loss, timing)
        if not self.envs['E2E_KIND_CLUSTER_NAME'].startswith('ptah-e2e-'):
            raise ValueError('Only the explicitly recorded disposable e2e cluster may be destroyed')
        if self.report['sourceCommit'] != self.envs['E2E_CONTROLLER_REVISION']:
            raise ValueError('Source and replacement clusters must use the current committed source')
        self.source_environment = environment
        self.source_envs = dict(self.envs)
        self.target_environment = root / 'target-environment'
        self.archived_workloads = {'jobs': [], 'pods': []}
        self.target_active = False
        self.result_delivery = result_delivery
        self.api_tunnels = []
        self.report.update(scope='Development-image cold-cluster recovery. The original kind control plane is destroyed, a separate cluster is provisioned after loss, and namespace state is rebuilt from an encrypted backup. Final-profile, in-flight and final-artifact acceptance remain required.',
                           procedureSHA256=db.digest(Path(__file__).read_bytes()),
                           operatorProcedureSHA256=db.digest((REPO / 'support/qualification/probes/operator_restore.py').read_bytes()))

    @staticmethod
    def api_ready(environment):
        try:
            result = subprocess.run(['kubectl', '--kubeconfig', environment['E2E_KUBECONFIG'],
                                     '--request-timeout=3s', 'get', '--raw=/readyz'],
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10)
        except subprocess.TimeoutExpired:
            return False
        return result.returncode == 0 and result.stdout.strip() == b'ok'

    def ensure_api_connection(self, environment, name):
        if self.api_ready(environment):
            return
        # Bootstrap's background SSH process can die when its invoking shell
        # exits. Own a replacement for this procedure, without trusting a saved
        # PID that could already belong to another process.
        config = json.loads(self.command('read the recorded API endpoint', [
            'kubectl', '--kubeconfig', environment['E2E_KUBECONFIG'],
            'config', 'view', '--minify', '-o', 'json']).stdout)
        server = urlsplit(config['clusters'][0]['cluster']['server'])
        endpoint = urlsplit(environment['E2E_DOCKER_ENDPOINT'])
        forward = environment.get('E2E_TUNNEL_FORWARD', '')
        if (server.scheme != 'https' or server.hostname != '127.0.0.1' or not server.port
                or forward != f'127.0.0.1:{server.port}:127.0.0.1:{server.port}'
                or endpoint.scheme != 'ssh' or not endpoint.hostname or endpoint.password):
            raise RuntimeError('Cannot reopen an API tunnel outside the recorded loopback SSH endpoint')
        target = (endpoint.username + '@' if endpoint.username else '') + endpoint.hostname
        args = ['ssh', '-N', '-o', 'BatchMode=yes', '-o', 'ExitOnForwardFailure=yes',
                '-o', 'ServerAliveInterval=15', '-o', 'ServerAliveCountMax=3', '-L', forward]
        if endpoint.port:
            args += ['-p', str(endpoint.port)]
        args += ['--', target]
        with open(self.root / (name + '-api-tunnel.private.log'), 'wb') as output:
            process = subprocess.Popen(args, stdout=output, stderr=subprocess.STDOUT, start_new_session=True)
        self.api_tunnels.append(process)
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            if process.poll() is not None:
                raise RuntimeError('Owned API tunnel exited; private diagnostics retained')
            if self.api_ready(environment):
                self.report.setdefault('reopenedAPITunnels', []).append(name)
                self.persist()
                return
            time.sleep(1)
        raise RuntimeError('API did not become ready through its owned tunnel')

    def close_api_tunnels(self):
        for process in getattr(self, 'api_tunnels', []):
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=10)

    def release_state(self, environment):
        helm = ['helm', '--kubeconfig', environment['E2E_KUBECONFIG'], '--namespace', environment['E2E_OPERATOR_NAMESPACE']]
        release = environment['E2E_HELM_RELEASE']
        status = json.loads(self.command('record the installed Helm release state', helm + ['status', release, '-o', 'json']).stdout)
        self.check('the archived Helm release is deployed', status.get('info', {}).get('status') == 'deployed')
        values = json.loads(self.command('record all effective release values', helm + ['get', 'values', release, '--all', '-o', 'json']).stdout)
        manifest = self.command('archive the stored release manifests and hooks', helm + ['get', 'all', release]).stdout.decode()
        self.check('the release archive includes effective values and stored manifests', bool(values) and bool(manifest.strip()))
        return {'name': release, 'namespace': environment['E2E_OPERATOR_NAMESPACE'],
                'status': status, 'values': values, 'storedRelease': manifest}

    def namespace_backup(self):
        archive = super().namespace_backup()
        cluster_uid = self.read('namespaces', 'kube-system')['metadata']['uid']
        nodes = self.read('nodes', False)['items']
        versions = {r['status']['nodeInfo']['kubeletVersion'] for r in nodes}
        platforms = {r['status']['nodeInfo']['operatingSystem'] + '/' + r['status']['nodeInfo']['architecture'] for r in nodes}
        self.check('the source cluster has four nodes with one recorded version and platform',
                   len(nodes) == 4 and len(versions) == len(platforms) == 1)
        self.source_cluster = {'kindName': self.envs['E2E_KIND_CLUSTER_NAME'], 'uid': cluster_uid,
                               'kubernetesVersion': versions.pop().removeprefix('v'), 'platform': platforms.pop(),
                               'nodeNames': sorted(r['metadata']['name'] for r in nodes)}
        archive['sourceCluster'] = self.source_cluster
        archive['release'] = self.release_state(self.source_envs)
        self.source_result_delivery = archive['release']['values'].get('resultDelivery', {}).get('enabled', False)
        self.check('requested durable delivery is enabled before the backup',
                   not self.result_delivery or self.source_result_delivery)
        self.report['sourceReleaseArchiveSHA256'] = db.digest(json.dumps(archive['release'], sort_keys=True).encode())
        self.report['sourceCluster'] = self.source_cluster
        return archive

    def lose_namespace(self):
        barriers = self.barrier('source-cluster-loss-boundary')
        for resource in ('jobs', 'pods'):
            self.archived_workloads[resource] = super().watched_objects(resource, barriers[resource])
        for resource, (process, output, path, _) in self.watches.items():
            process.terminate()
            process.wait(timeout=10)
            output.close()
            path.rename(self.root / ('source-' + path.name))
        self.watches.clear()
        cluster = self.source_envs['E2E_KIND_CLUSTER_NAME']
        containers = self.command('bind source API node names to the recorded Docker cluster',
                                  db.DOCKER + ['ps', '--all', '--filter', 'label=io.x-k8s.kind.cluster=' + cluster,
                                               '--format', '{{.Names}}']).stdout.decode().splitlines()
        self.check('the source API nodes belong to the exact cluster being destroyed',
                   len(containers) == 5 and set(self.source_cluster['nodeNames']) < set(containers))
        self.report['sourceDestructionStartedAt'] = db.now()
        self.command('destroy the recorded source kind cluster',
                     ['env', 'DOCKER_CONTEXT=' + db.DOCKER_CONTEXT, 'KIND_EXPERIMENTAL_PROVIDER=docker',
                      'kind', 'delete', 'cluster', '--name', cluster])
        remaining = self.command('verify no source cluster containers survive',
                                 db.DOCKER + ['ps', '--all', '--quiet', '--filter', 'label=io.x-k8s.kind.cluster=' + cluster])
        self.check('the original control plane and workload nodes were destroyed', not remaining.stdout.strip())
        self.created_namespace = False
        self.report['sourceDestroyedAt'] = db.now()
        self.persist()

    def provision_target(self):
        environment = {k: v for k, v in os.environ.items() if not k.startswith('E2E_')}
        environment.update(DOCKER_CONTEXT=db.DOCKER_CONTEXT,
                           K8S_VERSION=self.source_cluster['kubernetesVersion'],
                           E2E_RUN_ID='cold-restore-' + self.prefix.removeprefix('ptah-020-restore-'),
                           E2E_STOP_AFTER='bootstrap', E2E_ENVIRONMENT_FILE=str(self.target_environment),
                           E2E_TIMING_LEDGER=str(self.root / 'target-timings.jsonl'),
                           E2E_TIMING_CONTEXT=str(self.root / 'target-context.json'))
        if os.environ.get('E2E_PTAH_SOURCE_DIR'):
            environment['E2E_PTAH_SOURCE_DIR'] = os.environ['E2E_PTAH_SOURCE_DIR']
        row = {'action': 'provision a separate recovery cluster after source loss', 'startedAt': db.now()}
        self.report['targetProvisionStartedAt'] = row['startedAt']
        self.persist()
        with open(self.root / 'target-bootstrap.private.log', 'wb') as output:
            process = subprocess.Popen(['make', 'e2e'], cwd=REPO, env=environment, stdout=output,
                                       stderr=subprocess.STDOUT, start_new_session=True)
            try:
                result = process.wait(timeout=1100)
            except BaseException:
                try:
                    os.killpg(process.pid, signal.SIGTERM)
                except ProcessLookupError:
                    pass
                try:
                    process.wait(timeout=30)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait()
                raise
        row.update(completedAt=db.now(), exitCode=result)
        self.report['steps'].append(row)
        self.persist()
        if result:
            raise RuntimeError('Replacement cluster provisioning failed; private bootstrap diagnostics retained')
        target = dict(line.split('=', 1) for line in self.target_environment.read_text().splitlines() if '=' in line)
        self.check('replacement uses the same explicit Docker endpoint and operator revision',
                   target['E2E_DOCKER_ENDPOINT'] == self.source_envs['E2E_DOCKER_ENDPOINT'] and
                   target['E2E_CONTROLLER_REVISION'] == self.source_envs['E2E_CONTROLLER_REVISION'] and
                   target['E2E_PTAH_REVISION'] == self.source_envs['E2E_PTAH_REVISION'])
        self.envs = target
        self.ensure_api_connection(target, 'replacement')
        if self.source_result_delivery:
            self.enable_result_delivery(target)
        target_uid = self.read('namespaces', 'kube-system')['metadata']['uid']
        self.check('restoration uses a newly provisioned separate control plane',
                   target_uid != self.source_cluster['uid'] and
                   target['E2E_KIND_CLUSTER_NAME'] != self.source_cluster['kindName'])
        nodes = self.read('nodes', False)['items']
        self.check('the new cluster runs the same recorded Kubernetes version and native platform',
                   len(nodes) == 4 and all(r['status']['nodeInfo']['kubeletVersion'] == 'v' + self.source_cluster['kubernetesVersion'] and
                                          r['status']['nodeInfo']['operatingSystem'] + '/' + r['status']['nodeInfo']['architecture'] == self.source_cluster['platform']
                                          for r in nodes))
        self.report['targetCluster'] = {'kindName': target['E2E_KIND_CLUSTER_NAME'], 'uid': target_uid,
                                        'operatorRevision': target['E2E_CONTROLLER_REVISION'],
                                        'operatorImage': target['E2E_CONTROLLER_IMAGE'],
                                        'executorImage': target['E2E_EXECUTOR_IMAGE']}
        release = json.dumps(self.release_state(target), sort_keys=True).encode()
        key, wrong = self.root / 'restore-identity.private', self.root / 'wrong-identity.private'
        recipient = self.command('read the existing recovery evidence recipient', ['age-keygen', '-y', str(key)]).stdout.decode().strip()
        self.encrypted('target-release-evidence', release, recipient, key, wrong)
        self.report['targetProvisionCompletedAt'] = db.now()
        self.target_active = True
        self.persist()

    def create(self, value):
        if self.target_active:
            if not self.watches:
                self.begin_watch()
                credential = json.loads(Path(self.envs['E2E_REGISTRY_CREDENTIALS_FILE']).read_text())
                registry = self.envs['E2E_EXECUTOR_IMAGE'].split('/')[0]
                auth = base64.b64encode((credential['username'] + ':' + credential['password']).encode()).decode()
                config = json.dumps({'auths': {registry: {'auth': auth}}}).encode()
                secret = self.obj('Secret', 'restore-replacement-pull', type='kubernetes.io/dockerconfigjson',
                                  data={'.dockerconfigjson': base64.b64encode(config).decode()})
                installed = super().create(secret)
                self.report['replacementPullSecretUID'] = installed['metadata']['uid']
            value = copy.deepcopy(value)
            if value['kind'] == self.kind:
                pulls = value['spec'].setdefault('execution', {}).setdefault('imagePullSecrets', [])
                pulls.append({'name': 'restore-replacement-pull'})
            elif value['kind'] == 'Job' and value['metadata']['name'].startswith('restore-publish-'):
                value['spec']['template']['spec']['imagePullSecrets'].append({'name': 'restore-replacement-pull'})
        return super().create(value)

    def restore_namespace(self, checkpoint_path, key):
        self.provision_target()
        super().restore_namespace(checkpoint_path, key)
        self.report['rebuild']['controlledSpecEdits']['execution.imagePullSecrets.add'] = 'restore-replacement-pull'
        self.persist()

    def watched_objects(self, resource, barrier):
        return self.archived_workloads[resource] + super().watched_objects(resource, barrier)

    @staticmethod
    def comparable_contract(items, namespace, controller):
        result = copy.deepcopy(items)
        crds = [r for r in result if r['kind'] == 'CustomResourceDefinition']
        webhooks = [r for r in result if r['kind'] in ('MutatingWebhookConfiguration', 'ValidatingWebhookConfiguration')]
        if (len(crds) != len(OPERATOR_CRDS) or {r['name'] for r in crds} != OPERATOR_CRDS
                or len(webhooks) != 2 or len(result) != len(OPERATOR_CRDS) + 2):
            raise RuntimeError('Cold restore must account for every operator CRD and both admission configurations')
        expected_service = controller[:55].rstrip('-') + '-webhook'
        controller_user = 'system:serviceaccount:' + namespace + ':' + controller
        for item in result:
            if not item.pop('uid', None):
                raise RuntimeError('An installed API contract has no recorded identity')
            for webhook in item.get('webhooks', []):
                config = webhook['clientConfig']
                service = config['service']
                if service['namespace'] != namespace or service['name'] != expected_service:
                    raise RuntimeError('Admission points outside its recorded installation')
                service.update(namespace='RECOVERY_OPERATOR_NAMESPACE', name='RECOVERY_OPERATOR_SERVICE')
                try:
                    ca = base64.b64decode(config['caBundle'], validate=True).decode()
                    ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT).load_verify_locations(cadata=ca)
                except (ValueError, UnicodeError, ssl.SSLError) as error:
                    raise RuntimeError('Admission has no valid recorded certificate authority') from error
                config['caBundle'] = 'REGENERATED_CERTIFICATE_AUTHORITY'
                for condition in webhook.get('matchConditions', []):
                    condition['expression'] = condition['expression'].replace(controller_user, 'RECOVERY_CONTROLLER_USER')
            if item['kind'] != 'CustomResourceDefinition' and not item.get('webhooks'):
                raise RuntimeError('An admission configuration has no webhooks')
        return result

    def verify_restored_contract(self, original):
        current = self.preserved_contract()
        before = self.comparable_contract(original, self.source_envs['E2E_OPERATOR_NAMESPACE'], self.source_envs['E2E_CONTROLLER_NAME'])
        after = self.comparable_contract(current, self.envs['E2E_OPERATOR_NAMESPACE'], self.envs['E2E_CONTROLLER_NAME'])
        self.check('replacement reinstalls the original CRD and admission contracts', before == after)
        old_uids = {r['uid'] for r in original}
        new_uids = {r['uid'] for r in current}
        self.check('every installed contract has a new control-plane identity',
                   len(old_uids) == len(new_uids) == len(OPERATOR_CRDS) + 2 and old_uids.isdisjoint(new_uids))
        self.report['reinstalledContractSHA256'] = db.digest(json.dumps(current, sort_keys=True).encode())

    def enable_result_delivery(self, environment):
        helm = ['helm', '--kubeconfig', environment['E2E_KUBECONFIG'], '--namespace', environment['E2E_OPERATOR_NAMESPACE']]
        self.command('enable durable delivery on the recorded recovery installation', helm + [
            'upgrade', environment['E2E_HELM_RELEASE'], environment['E2E_CHART_PACKAGE'],
            '--reuse-values', '--set', 'resultDelivery.enabled=true', '--wait', '--timeout', '5m'])
        values = json.loads(self.command('verify effective recovery delivery mode', helm + [
            'get', 'values', environment['E2E_HELM_RELEASE'], '--all', '-o', 'json']).stdout)
        self.check('recovery installation uses durable delivery', values.get('resultDelivery', {}).get('enabled') is True)

    def cleanup_namespace(self):
        # The finally block removes both entire owned clusters. A namespace-only
        # delete would wait for result retention just before destroying its store.
        self.report['namespaceCleanup'] = 'Included in required source and replacement cluster teardown'
        return True

    def run(self):
        try:
            self.ensure_api_connection(self.source_envs, 'source')
            if getattr(self, 'result_delivery', False):
                self.enable_result_delivery(self.source_envs)
            super().run()
        except BaseException as exc:
            self.report.update(status='FAIL', failure=type(exc).__name__ + ': ' + str(exc))
            self.persist()
            raise
        finally:
            self.close_api_tunnels()
            outcomes = []
            for name, path in (('replacement', self.target_environment), ('source', self.source_environment)):
                if not path.exists():
                    continue
                with open(self.root / (name + '-cluster-cleanup.private.log'), 'wb') as output:
                    try:
                        result = subprocess.run(['demo/bin/lab', 'down'], cwd=REPO,
                                                env={**os.environ, 'LAB_ENVIRONMENT': str(path)},
                                                stdout=output, stderr=subprocess.STDOUT, timeout=180)
                        outcome = {'cluster': name, 'exitCode': result.returncode}
                    except subprocess.TimeoutExpired:
                        outcome = {'cluster': name, 'exitCode': 124, 'error': 'cleanup timed out'}
                    except OSError:
                        outcome = {'cluster': name, 'exitCode': 127, 'error': 'cleanup could not start'}
                outcomes.append(outcome)
            self.report.update(clusterCleanup=outcomes, completedAt=db.now())
            if any(r['exitCode'] for r in outcomes):
                self.report.update(status='FAIL', cleanupSucceeded=False, failure='Owned cluster cleanup failed')
            self.persist()
            if any(r['exitCode'] for r in outcomes):
                raise RuntimeError('Owned cluster cleanup failed; private diagnostics retained')


if __name__ == '__main__':
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('engine', choices=db.IMAGES)
    parser.add_argument('family', choices=['schema', 'migration'])
    parser.add_argument('environment', type=Path, help='Environment of the disposable source cluster that will be destroyed')
    parser.add_argument('output', type=Path)
    parser.add_argument('--loss', choices=['operator', 'combined'], required=True)
    parser.add_argument('--timing', choices=['idle', 'operator-lag'], default='idle')
    parser.add_argument('--result-delivery', action='store_true', help='Enable durable delivery before backup and on the replacement installation')
    args = parser.parse_args()
    probe = ClusterRestoreProbe(args.engine, args.family, args.environment, args.output, args.loss, args.timing, args.result_delivery)
    probe.run()
    print(json.dumps({k: probe.report.get(k) for k in ('engine', 'family', 'lossType', 'status', 'functionalRestore', 'profileRPO', 'recoverySeconds', 'cleanupSucceeded')}))
    raise SystemExit(0 if probe.report.get('status') == 'PASS' else 2)
