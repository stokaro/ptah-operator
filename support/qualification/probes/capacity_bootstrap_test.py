"""Ownership and workload-placement controls for the capacity lab bootstrap."""

import copy
import base64
import hashlib
import json
import os
import shutil
import subprocess
import sys
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from urllib.parse import urlsplit

from capacity_bootstrap import Bootstrap, database_account


class CommandTests(unittest.TestCase):
    quota_conflict = (b'Error from server (Conflict): error when creating "STDIN": '
                      b'Operation cannot be fulfilled on resourcequotas "background": '
                      b'the object has been modified; please apply your changes to the latest version and try again\n')

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.bootstrap = Bootstrap(Path(self.temp.name) / 'state.json', {'E2E_KUBECONFIG': '/unused'})
        self.bootstrap.state['namespaces'] = [{'name': 'owned', 'uid': 'namespace-uid'}]
        self.value = self.bootstrap.object('owned', 'ConfigMap', 'background-0001', data={'payload': 'x'})
        self.args = ['create', '-f', '-', '-o', 'json']

    def test_quota_admission_refusal_retries_same_create_and_keeps_diagnostics(self):
        refused = subprocess.CompletedProcess([], 1, b'', self.quota_conflict)
        stored = copy.deepcopy(self.value)
        stored['metadata']['uid'] = 'stored-uid'
        with patch('capacity_bootstrap.subprocess.run', side_effect=[refused, refused,
                   subprocess.CompletedProcess([], 0, json.dumps(stored).encode(), b'')]) as run, \
                patch('capacity_bootstrap.time.sleep') as sleep:
            self.assertEqual(self.bootstrap.create(self.value), stored)
        self.assertEqual(run.call_count, 3)
        self.assertTrue(all(c.kwargs['input'] == json.dumps(self.value).encode() for c in run.call_args_list))
        self.assertEqual(sleep.call_count, 2)
        diagnostic = self.bootstrap.path.with_suffix('.error.log')
        self.assertEqual(diagnostic.read_bytes(), self.quota_conflict * 2)
        self.assertEqual(diagnostic.stat().st_mode & 0o777, 0o600)

    def test_other_failures_and_writes_are_not_replayed(self):
        foreign = copy.deepcopy(self.value)
        foreign['metadata']['namespace'] = 'foreign'
        quota = self.bootstrap.object('owned', 'ResourceQuota', 'background')
        for args, value, stderr, stdout in [
                (self.args, foreign, self.quota_conflict, b''),
                (self.args, quota, self.quota_conflict, b''),
                (['exec', 'database', '--', 'psql'], b'SQL', self.quota_conflict, b''),
                (self.args, self.value, self.quota_conflict, b'partial response'),
                (self.args, self.value, self.quota_conflict.replace(b'resourcequotas', b'configmaps'), b''),
                (self.args, self.value, b'Error from server (Forbidden): exceeded quota: background', b''),
                (self.args, self.value, b'Error from server (AlreadyExists): object already exists', b''),
                (self.args, self.value, b'connection reset by peer', b'')]:
            with self.subTest(args=args, stderr=stderr, value=value), \
                    patch('capacity_bootstrap.subprocess.run', return_value=
                          subprocess.CompletedProcess([], 1, stdout, stderr)) as run, \
                    patch('capacity_bootstrap.time.sleep') as sleep:
                with self.assertRaisesRegex(RuntimeError, 'private diagnostics'):
                    self.bootstrap.command(args, value)
                self.assertEqual(run.call_count, 1)
                sleep.assert_not_called()

    def test_ambiguous_timeout_is_not_replayed(self):
        with patch('capacity_bootstrap.subprocess.run', side_effect=subprocess.TimeoutExpired('kubectl', 45)) as run:
            with self.assertRaises(subprocess.TimeoutExpired):
                self.bootstrap.create(self.value)
            self.assertEqual(run.call_count, 1)

    def test_quota_conflicts_have_a_fixed_attempt_limit(self):
        with patch('capacity_bootstrap.subprocess.run', return_value=
                   subprocess.CompletedProcess([], 1, b'', self.quota_conflict)) as run, \
                patch('capacity_bootstrap.time.sleep') as sleep:
            with self.assertRaisesRegex(RuntimeError, 'private diagnostics'):
                self.bootstrap.create(self.value)
            self.assertEqual(run.call_count, 5)
            self.assertEqual(sleep.call_count, 4)

    def test_retries_share_the_original_command_deadline(self):
        with patch('capacity_bootstrap.subprocess.run', return_value=
                   subprocess.CompletedProcess([], 1, b'', self.quota_conflict)) as run, \
                patch('capacity_bootstrap.time.monotonic', side_effect=[100, 100, 120, 121, 144.75]), \
                patch('capacity_bootstrap.time.sleep') as sleep:
            with self.assertRaisesRegex(RuntimeError, 'private diagnostics'):
                self.bootstrap.create(self.value)
            self.assertEqual([c.kwargs['timeout'] for c in run.call_args_list], [45, 24])
            self.assertEqual(sleep.call_count, 1)


class FakeBootstrap(Bootstrap):
    def __init__(self, state):
        super().__init__(state, {'E2E_KUBECONFIG': '/unused', 'E2E_TEST_NAMESPACE': 'shared-lab',
                               'E2E_CONTROLLER_NAME': 'lab-controller',
                               'E2E_POSTGRES_IMAGE': 'postgres@sha256:fixture',
                               'E2E_MYSQL_IMAGE': 'mysql@sha256:fixture'})
        self.objects = {}
        self.commands = []
        self.sql = []
        self.deleted = []
        self.fail_kind = None
        self.fail_wait = False
        self.lose_response = None
        self.serial = 0
        self.shared = []

    def read(self, resource, name, namespace):
        value = {'apiVersion': 'v1', 'kind': 'Secret' if resource == 'secret' else 'ConfigMap',
                 'metadata': {'name': name, 'namespace': namespace, 'uid': 'shared',
                              'ownerReferences': [{'uid': 'shared-owner'}],
                              'annotations': {'kubectl.kubernetes.io/last-applied-configuration': 'private'}},
                 'data': {'fixture': 'bytes'}}
        if resource == 'configmap':
            value['immutable'] = True
        self.shared.append(copy.deepcopy(value))
        return value

    def create(self, value):
        if value['kind'] == self.fail_kind:
            raise RuntimeError('injected creation failure')
        value = copy.deepcopy(value)
        key = (value['kind'], value['metadata'].get('namespace', ''), value['metadata']['name'])
        if key in self.objects:
            raise RuntimeError('AlreadyExists')
        self.serial += 1
        value['metadata']['uid'] = f'owned-{self.serial}'
        self.objects[key] = value
        if value['kind'] == self.lose_response:
            raise RuntimeError('CREATE response lost')
        return copy.deepcopy(value)

    def command(self, args, value=None, timeout=45, kubeconfig=None):
        self.commands.append((list(args), value))
        if args[:2] == ['auth', 'whoami']:
            if kubeconfig not in ('/approver-config', '/author-config'):
                raise AssertionError('identity must come from its separate kubeconfig')
            return json.dumps({'status': {'userInfo': {'username': 'capacity-' + ('author' if kubeconfig == '/author-config' else 'approver')}}}).encode()
        if args[:2] == ['create', '--dry-run=client']:
            role = {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'Role',
                    'metadata': {'name': 'ptah-desired-state-author', 'namespace': 'application'},
                    'rules': [{'apiGroups': ['operator.ptah.run'], 'resources': ['ptahschemas', 'ptahmigrations'],
                               'verbs': ['get', 'list', 'watch', 'create', 'update', 'patch', 'delete']}]}
            return (json.dumps(role) + '\n' + json.dumps({'kind': 'RoleBinding'})).encode()
        if args[:2] == ['get', 'clusterrole']:
            return b'clusterrole.rbac.authorization.k8s.io/lab-controller-approver'
        if args[0] == 'version':
            return b'{"serverVersion":{"gitVersion":"v1.37.0"}}'
        if args[:2] == ['get', 'namespace']:
            result = self.objects.get(('Namespace', '', args[2]))
            return json.dumps(result).encode() if result else b''
        if args[:2] == ['delete', '--raw']:
            name = args[2].rsplit('/', 1)[1]
            key = ('Namespace', '', name)
            options = json.loads(Path(args[4]).read_text())
            if options['preconditions']['uid'] != self.objects[key]['metadata']['uid']:
                raise RuntimeError('UID conflict')
            self.deleted.append(name)
            del self.objects[key]
            return b'{}'
        if args[0] == 'wait':
            if self.fail_wait:
                raise RuntimeError('finalizers have not settled')
            return b''
        if 'exec' in args:
            self.sql.append(value.decode())
            return b''
        if args[0] == '-n' and args[2] in ('patch', 'rollout', 'wait'):
            return b''
        raise AssertionError(args)


class BootstrapTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.bootstrap = FakeBootstrap(Path(self.temp.name) / 'state.json')

    def test_https_registry_binds_the_copied_ca_and_requires_signed_digests(self):
        b = self.bootstrap
        b.env.update(CAPACITY_REGISTRY_HOST='tls.fixture.svc.cluster.local:5443',
                     CAPACITY_REGISTRY_CA_CONFIGMAP='registry-ca',
                     CAPACITY_SIGNING_KEY='/signing.key', CAPACITY_SIGNING_PUBLIC_KEY='/signing.pub')
        ca = {'apiVersion': 'v1', 'kind': 'ConfigMap', 'immutable': True,
              'metadata': {'name': 'registry-ca'}, 'data': {'ca.pem': 'exact fixture CA bytes\n'}}
        original_read = b.read
        def read(resource, name, namespace):
            value = copy.deepcopy(ca) if name == 'registry-ca' else original_read(resource, name, namespace)
            if name == 'demo-registry':
                value['data']['allowPlainHTTP'] = base64.b64encode(b'true').decode()
            return value
        with patch.object(b, 'read', side_effect=read):
            namespaces = b.prepare({'schemas': 1, 'migrations': 1})
        for namespace in namespaces:
            credential = b.objects['Secret', namespace, 'demo-registry']['data']
            self.assertNotIn('allowPlainHTTP', credential)
            self.assertEqual(base64.b64decode(credential['registry']).decode(), b.env['CAPACITY_REGISTRY_HOST'])
            expected = 'sha256:' + hashlib.sha256(ca['data']['ca.pem'].encode()).hexdigest()
            self.assertEqual(base64.b64decode(credential['caSHA256']).decode(), expected)
            self.assertEqual(b.objects['ConfigMap', namespace, 'registry-ca']['data'], ca['data'])
            for name, kind in [('demo-verification-policy', 'schema'), ('demo-migration-verification-policy', 'migrations')]:
                policy = json.loads(b.objects['ConfigMap', namespace, name]['data']['policy.yaml'])
                self.assertTrue(policy['require_digest_pin'])
                self.assertTrue(policy['require_signature'])
                self.assertEqual(policy['artifact_types'], ['application/vnd.stokaro.ptah.' + kind + '.v1'])

    def test_incomplete_registry_or_signing_configuration_creates_nothing(self):
        for setting in ('CAPACITY_REGISTRY_HOST', 'CAPACITY_REGISTRY_CA_CONFIGMAP', 'CAPACITY_SIGNING_KEY', 'CAPACITY_SIGNING_PUBLIC_KEY'):
            with self.subTest(setting=setting):
                b = FakeBootstrap(Path(self.temp.name) / 'state.json')
                b.env[setting] = 'incomplete'
                with self.assertRaises(ValueError):
                    b.prepare({'schemas': 1, 'migrations': 1})
                self.assertFalse(b.objects)
                self.assertFalse(b.path.exists())

    def test_distinct_approver_is_bound_only_in_owned_workload_namespaces(self):
        b = self.bootstrap
        b.env['CAPACITY_APPROVER_KUBECONFIG'] = '/approver-config'
        namespaces = b.prepare({'schemas': 1, 'migrations': 1})
        bindings = [obj for obj in b.objects.values() if obj['kind'] in ('RoleBinding', 'ClusterRoleBinding')]
        self.assertEqual(len(bindings), 2)
        self.assertEqual({obj['metadata']['namespace'] for obj in bindings}, set(namespaces))
        for binding in bindings:
            self.assertEqual(binding['kind'], 'RoleBinding')
            self.assertEqual(binding['roleRef'], {'apiGroup': 'rbac.authorization.k8s.io',
                             'kind': 'ClusterRole', 'name': 'lab-controller-approver'})
            self.assertEqual(binding['subjects'], [{'apiGroup': 'rbac.authorization.k8s.io',
                             'kind': 'User', 'name': 'capacity-approver'}])
        self.assertEqual(b.state['approver']['username'], 'capacity-approver')

    def test_author_receives_only_the_documented_role_in_owned_workload_namespaces(self):
        b = self.bootstrap
        b.env.update(CAPACITY_AUTHOR_KUBECONFIG='/author-config', CAPACITY_APPROVER_KUBECONFIG='/approver-config')
        namespaces = b.prepare({'schemas': 1, 'migrations': 1})
        roles = [obj for obj in b.objects.values() if obj['kind'] == 'Role']
        bindings = [obj for obj in b.objects.values() if obj['kind'] == 'RoleBinding' and obj['metadata']['name'] == 'capacity-author']
        self.assertEqual(len(roles), 2)
        self.assertEqual(len(bindings), 2)
        self.assertEqual({obj['metadata']['namespace'] for obj in roles}, set(namespaces))
        for obj in bindings:
            self.assertIn(obj['metadata']['namespace'], namespaces)
            self.assertEqual(obj['roleRef']['kind'], 'Role')
            self.assertEqual(obj['roleRef']['name'], 'ptah-desired-state-author')
            self.assertEqual(obj['subjects'], [{'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'User', 'name': 'capacity-author'}])
        self.assertFalse(any(obj['kind'] == 'ClusterRoleBinding' for obj in b.objects.values()))
        self.assertEqual(b.state['author']['username'], 'capacity-author')
        self.assertEqual(len(b.state['author']['exampleSHA256']), 64)

    def test_unproven_or_privileged_author_creates_nothing(self):
        for mode in ('missing approver', 'same user', 'administrator', 'missing username'):
            with self.subTest(mode=mode):
                b = FakeBootstrap(Path(self.temp.name) / 'state.json')
                b.env['CAPACITY_AUTHOR_KUBECONFIG'] = '/author-config'
                if mode != 'missing approver':
                    b.env['CAPACITY_APPROVER_KUBECONFIG'] = '/approver-config'
                original = b.command
                def command(args, value=None, timeout=45, kubeconfig=None):
                    if args[:2] == ['auth', 'whoami'] and kubeconfig == '/author-config':
                        identity = {'username': 'capacity-approver' if mode == 'same user' else 'capacity-author'}
                        if mode == 'administrator':
                            identity['groups'] = ['system:masters']
                        if mode == 'missing username':
                            identity.pop('username')
                        return json.dumps({'status': {'userInfo': identity}}).encode()
                    return original(args, value, timeout, kubeconfig)
                with patch.object(b, 'command', side_effect=command), self.assertRaises(ValueError):
                    b.prepare({'schemas': 1, 'migrations': 1})
                self.assertFalse(b.objects)
                self.assertFalse(b.path.exists())

    def test_default_workload_serviceaccount_receives_no_approval_binding(self):
        b = self.bootstrap
        b.prepare({'schemas': 1, 'migrations': 1})
        self.assertFalse(any(obj['kind'] in ('RoleBinding', 'ClusterRoleBinding') for obj in b.objects.values()))

    def test_mysql_uses_database_scoped_users_and_matching_service(self):
        b = self.bootstrap
        namespaces = b.prepare({'engine': 'MySQL', 'schemas': 10, 'migrations': 10})
        self.assertEqual(b.state['engine'], 'MySQL')
        fixture = b.state['fixtureNamespace']
        container = b.objects['Deployment', fixture, 'capacity-mysql']['spec']['template']['spec']['containers'][0]
        self.assertEqual(container['image'], b.env['E2E_MYSQL_IMAGE'])
        self.assertEqual(container['env'][0]['name'], 'MYSQL_ROOT_PASSWORD')
        self.assertNotIn('value', container['env'][0])
        self.assertEqual(container['ports'][0]['containerPort'], 3306)
        self.assertIn('SELECT 1', container['readinessProbe']['exec']['command'][-1])
        service = b.objects['Service', fixture, 'capacity-mysql']
        self.assertEqual(service['spec']['ports'][0]['targetPort'], container['ports'][0]['name'])
        policy = b.objects['NetworkPolicy', fixture, 'capacity-database-ingress']['spec']
        self.assertEqual(policy['ingress'][0]['ports'][0]['port'], 3306)
        self.assertEqual(len(b.sql), 21)
        urls = set()
        for row, sql in zip(b.state['databases'], b.sql):
            self.assertNotIn('_', row['database'])
            self.assertNotIn('%', row['database'])
            self.assertIn(f"GRANT ALL PRIVILEGES ON `{row['database']}`.* TO '{row['username']}'@'%';", sql)
            self.assertNotIn('GRANT OPTION', sql)
            self.assertNotIn('ON *.*', sql)
            url = b.objects['Secret', row['namespace'], 'capacity-db-' + str(row['index'])]['stringData']['url']
            self.assertTrue(url.startswith('mysql://' + row['username'] + ':'))
            self.assertTrue(url.endswith(f'@tcp(capacity-mysql.{fixture}.svc.cluster.local:3306)/' + row['database']))
            self.assertIn(row['namespace'], namespaces)
            urls.add(url)
        self.assertEqual(len(urls), 21)
        self.assertTrue(all('MYSQL_PWD="$MYSQL_ROOT_PASSWORD"' in args[-1] for args, _ in b.commands if 'exec' in args))

    def test_invalid_engine_and_missing_image_create_nothing(self):
        for engine in ('mysql', 'SQLite', '', None):
            with self.assertRaisesRegex(ValueError, 'unsupported workload engine'):
                self.bootstrap.prepare({'engine': engine, 'schemas': 1, 'migrations': 1})
        del self.bootstrap.env['E2E_MYSQL_IMAGE']
        with self.assertRaisesRegex(ValueError, 'missing database image'):
            self.bootstrap.prepare({'engine': 'MySQL', 'schemas': 1, 'migrations': 1})
        self.assertEqual(self.bootstrap.objects, {})
        self.assertFalse(self.bootstrap.path.exists())

    def test_account_input_cannot_add_sql_or_expand_a_grant(self):
        for engine, index, credential in [('MySQL', -1, 'a' * 48), ('MySQL', True, 'a' * 48),
                                          ('MySQL', '0;DROP DATABASE x', 'a' * 48),
                                          ('PostgreSQL', 0, "x';ALTER ROLE x SUPERUSER;--"),
                                          ('SQLite', 0, 'a' * 48)]:
            with self.assertRaises(ValueError):
                database_account(engine, index, credential)

    def test_two_namespaces_have_distinct_owned_databases_and_local_dependencies(self):
        b = self.bootstrap
        namespaces = b.prepare({'schemas': 10, 'migrations': 10})
        self.assertEqual(len(namespaces), 2)
        self.assertNotIn('shared-lab', namespaces)
        self.assertNotIn(b.state['fixtureNamespace'], namespaces)
        placements = {}
        users, databases, passwords = set(), set(), set()
        for row in b.state['databases']:
            secret = b.objects['Secret', row['namespace'], 'capacity-db-' + str(row['index'])]
            url = urlsplit(secret['stringData']['url'])
            self.assertEqual(url.username, row['username'])
            self.assertNotEqual(url.username, 'postgres')
            self.assertEqual(url.path, '/' + row['database'])
            users.add(url.username)
            databases.add(url.path)
            passwords.add(url.password)
            placements[row['namespace'], row['family']] = placements.get((row['namespace'], row['family']), 0) + 1
        self.assertEqual((len(users), len(databases), len(passwords)), (21, 21, 21))
        for ns in namespaces:
            self.assertEqual(placements[ns, 'schema'], 5)
            self.assertEqual(placements[ns, 'migration'], 5)
            for kind, name in [('Secret', 'demo-registry'), ('Secret', 'demo-registry-pull'),
                               ('ConfigMap', 'demo-verification-policy'), ('ConfigMap', 'demo-migration-verification-policy')]:
                copied = b.objects[kind, ns, name]
                self.assertNotIn('ownerReferences', copied['metadata'])
                self.assertNotIn('annotations', copied['metadata'])
                self.assertNotEqual(copied['metadata']['uid'], 'shared')
                if kind == 'ConfigMap':
                    self.assertTrue(copied['immutable'])
            labels = b.objects['Namespace', '', ns]['metadata']['labels']
            self.assertEqual(labels['pod-security.kubernetes.io/enforce'], 'restricted')
            self.assertEqual(labels['pod-security.kubernetes.io/enforce-version'], 'v1.37')
            quota = b.objects['ResourceQuota', ns, 'capacity']['spec']['hard']
            self.assertEqual(quota['pods'], '24')
            self.assertEqual(quota['count/ptahschemaplanchunks.operator.ptah.run'], '32768')
        self.assertEqual(len(b.sql), 21)
        for row, sql in zip(b.state['databases'], b.sql):
            self.assertIn('NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS', sql)
            self.assertIn('REVOKE ALL ON DATABASE ' + row['database'] + ' FROM PUBLIC', sql)
            self.assertIn('OWNER ' + row['username'], sql)
        journal = b.path.read_text()
        arguments = json.dumps([args for args, _ in b.commands])
        for password in passwords:
            self.assertNotIn(password, journal)
            self.assertNotIn(password, arguments)
        self.assertEqual(b.path.stat().st_mode & 0o777, 0o600)
        self.assertEqual(placements[namespaces[0], 'approval-fixture'], 1)
        self.assertTrue(all('ownerReferences' in value['metadata'] for value in b.shared))

    def test_cleanup_waits_for_workload_finalizers_before_removing_database(self):
        b = self.bootstrap
        namespaces = b.prepare({'schemas': 1, 'migrations': 1})
        b.commands.clear()
        b.cleanup()
        self.assertEqual(b.deleted, [namespaces[1], namespaces[0], b.state['fixtureNamespace']])
        actions = [args for args, _ in b.commands if args[0] in ('delete', 'wait')]
        self.assertEqual([args[0] for args in actions], ['delete', 'delete', 'wait', 'wait', 'delete', 'wait'])
        self.assertTrue(b.state['cleaned'])
        self.assertEqual(b.state['namespaces'], [])
        b.cleanup()  # A terminal cleanup is safe to retry.
        self.assertEqual(len(b.deleted), 3)

    def test_retention_window_fits_inside_cleanup_wait(self):
        b = self.bootstrap
        b.prepare({'schemas': 1, 'migrations': 1})
        command = b.command

        def retained(args, value=None, timeout=45):
            if args[0] == 'wait' and args[2] != 'namespace/' + b.state['fixtureNamespace']:
                seconds = int(next(arg for arg in args if arg.startswith('--timeout=')).split('=')[1][:-1])
                if seconds <= 3600 or timeout <= seconds:
                    raise RuntimeError('cleanup expires before result retention and collection')
            return command(args, value, timeout)

        b.command = retained
        b.cleanup()
        self.assertTrue(b.state['cleaned'])

    def test_partial_preparation_cleans_only_created_namespace_uids(self):
        b = self.bootstrap
        b.fail_kind = 'ResourceQuota'
        with self.assertRaisesRegex(RuntimeError, 'injected'):
            b.prepare({'schemas': 1, 'migrations': 1})
        created = list(b.state['namespaces'])
        self.assertEqual(len(created), 2)
        b.cleanup()
        self.assertEqual(b.deleted, [row['name'] for row in reversed(created)])
        self.assertNotIn('shared-lab', b.deleted)

    def test_replaced_namespace_is_never_deleted(self):
        b = self.bootstrap
        ns = b.prepare({'schemas': 1, 'migrations': 1})[-1]
        b.objects['Namespace', '', ns]['metadata']['uid'] = 'replacement'
        with self.assertRaisesRegex(RuntimeError, 'replaced'):
            b.cleanup()
        self.assertEqual(b.deleted, [])
        self.assertEqual(len(b.state['namespaces']), 3)

    def test_unsettled_finalizers_preserve_database_and_cleanup_can_resume(self):
        b = self.bootstrap
        b.prepare({'schemas': 1, 'migrations': 1})
        b.fail_wait = True
        with self.assertRaisesRegex(RuntimeError, 'finalizers'):
            b.cleanup()
        self.assertNotIn(b.state['fixtureNamespace'], b.deleted)
        self.assertEqual(len(b.state['namespaces']), 3)
        b.fail_wait = False
        b.cleanup()  # The previous DELETE succeeded; its response was not enough.
        self.assertEqual(len(b.deleted), 3)
        self.assertTrue(b.state['cleaned'])

    def test_lost_create_response_recovers_only_matching_run_identity(self):
        for foreign in (False, True):
            with self.subTest(foreign=foreign):
                path = Path(self.temp.name) / ('foreign.json' if foreign else 'lost.json')
                b = FakeBootstrap(path)
                b.lose_response = 'Namespace'
                with self.assertRaisesRegex(RuntimeError, 'response lost'):
                    b.prepare({'schemas': 1, 'migrations': 1})
                ns = b.state['pendingNamespace']
                if foreign:
                    b.objects['Namespace', '', ns]['metadata']['labels']['operator.ptah.run/capacity-bootstrap'] = 'other-run'
                    with self.assertRaisesRegex(RuntimeError, 'not owned'):
                        b.cleanup()
                    self.assertEqual(b.deleted, [])
                else:
                    b.cleanup()
                    self.assertEqual(b.deleted, [ns])

    def test_existing_journal_is_not_overwritten(self):
        b = self.bootstrap
        b.path.write_text('earlier evidence')
        with self.assertRaisesRegex(RuntimeError, 'already exists'):
            b.prepare({'schemas': 1, 'migrations': 1})
        self.assertEqual(b.path.read_text(), 'earlier evidence')
        self.assertEqual(b.commands, [])



class WrapperTests(unittest.TestCase):
    def test_disposable_lab_teardown_keeps_report_and_workload_exit_status(self):
        for workload_exit in (0, 42):
            with self.subTest(workload_exit=workload_exit):
                self.run_wrapper_cleanup_case('PostgreSQL', remove_lab=True, workload_exit=workload_exit)

    def test_disposable_lab_teardown_failure_preserves_ownership(self):
        self.run_wrapper_cleanup_case('PostgreSQL', remove_lab=True, workload_exit=0, teardown_exit=44)

    def test_disposable_lab_refuses_to_delete_its_report(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            workload = root / 'workload.json'
            workload.write_text('{}')
            environment = root / 'environment'
            environment.write_text('E2E_KUBECONFIG=/unused\nE2E_WORK_DIR=' + str(root / 'lab-work') + '\n')
            output = root / 'lab-work/report'
            script = Path(__file__).resolve().parents[3] / 'hack/capacity.sh'
            env = dict(os.environ, CAPACITY_WORKLOAD=str(workload), CAPACITY_VARIED_INPUTS='0',
                       CAPACITY_REMOVE_LAB='1', CAPACITY_OUT_DIR=str(output), LAB_ENVIRONMENT=str(environment))
            result = subprocess.run(['bash', str(script)], env=env, capture_output=True, timeout=10)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('reports must be outside directories removed by lab teardown', result.stderr.decode())
            self.assertTrue(environment.exists())
            self.assertFalse((output / 'bootstrap-state.json').exists())

    def test_relative_output_and_failed_run_keep_cleanup_bound_to_original_journal(self):
        for engine in ('PostgreSQL', 'MySQL'):
            with self.subTest(engine=engine):
                self.run_wrapper_cleanup_case(engine)

    def test_unrelated_inventory_checks_bracket_execution_and_fail_closed(self):
        for checkpoint in ('', 'before', 'after'):
            with self.subTest(checkpoint=checkpoint):
                self.run_wrapper_cleanup_case('PostgreSQL', unrelated=True, fail_checkpoint=checkpoint)

    def test_backlog_requires_populated_inputs_before_bootstrap(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            workload = root / 'backlog.json'
            workload.write_text(json.dumps({'approvalBacklog': True}))
            script = Path(__file__).resolve().parents[3] / 'hack/capacity.sh'
            env = dict(os.environ, CAPACITY_WORKLOAD=str(workload),
                       CAPACITY_VARIED_INPUTS='0', CAPACITY_OUT_DIR=str(root / 'evidence'),
                       LAB_ENVIRONMENT=str(root / 'must-not-open'))
            result = subprocess.run(['bash', str(script)], env=env, capture_output=True, timeout=10)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('approval backlog workloads require CAPACITY_VARIED_INPUTS=1', result.stderr.decode())
            self.assertFalse((root / 'evidence').exists())

    def run_wrapper_cleanup_case(self, engine, unrelated=False, fail_checkpoint='',
                                 remove_lab=False, workload_exit=None, teardown_exit=0):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / 'repository'
            caller = Path(directory) / 'caller'
            bin_path = root / 'tools'
            for path in (root / 'hack', root / 'demo/bin', root / 'demo/migrations', root / 'support/capacity', root / 'support/qualification/probes', caller, bin_path):
                path.mkdir(parents=True, exist_ok=True)
            source = Path(__file__).resolve().parents[3] / 'hack/capacity.sh'
            shutil.copyfile(source, root / 'hack/capacity.sh')
            (caller / 'workload.json').write_text(json.dumps({'engine': engine, 'unrelatedObjects': unrelated}))
            repository = source.parents[1]
            for relative in ('demo/schemas', 'demo/migrations', 'support/capacity/mysql'):
                shutil.copytree(repository / relative, root / relative, dirs_exist_ok=True)
            environment = caller / 'environment'
            environment.write_text('E2E_KUBECONFIG=/unused\nE2E_DOCKER_CONTEXT=capacity-test\nE2E_DOCKER_CONFIG=/capacity-test-config\nE2E_OPERATOR_NAMESPACE=operator\nE2E_REGISTRY_HOST=registry\nE2E_REGISTRY_IP=127.0.0.1\n')
            with environment.open('a') as file:
                file.write('E2E_WORK_DIR=' + str(root / 'lab-work') + '\n')
            def executable(path, body):
                path.write_text(body)
                path.chmod(0o700)
            executable(bin_path / 'docker', '#!' + sys.executable + '\n' + r'''import json,os,sys
assert os.environ.get('DOCKER_CONFIG')=='/capacity-test-config', 'host reading lost the task-specific Docker config'
assert sys.argv[1:4]==['--context','capacity-test','info'], 'host reading used another daemon'
print(json.dumps({'dockerID':'test-daemon','name':'test-host','cpus':4,'memoryBytes':17179869184,'architecture':'x86_64','os':'linux','observedAt':'2026-10-01T09:43:00Z'}))
''')
            executable(root / 'demo/bin/lab', '#!' + sys.executable + '\n' + r'''import os,pathlib,sys
if sys.argv[1]=='credentials':
 print('PTAH_OCI_USERNAME=user PTAH_OCI_PASSWORD=fixture PTAH_OCI_REGISTRY=registry')
elif sys.argv[1]=='tools':
 print(pathlib.Path(__file__).resolve().parents[2]/'tools')
elif sys.argv[1]=='down':
 environment=pathlib.Path(os.environ['LAB_ENVIRONMENT'])
 assert environment==pathlib.Path(os.environ['CAPACITY_TEST_ENVIRONMENT']), 'teardown selected another lab'
 with (environment.parent/'evidence/actions.log').open('a') as output: output.write('lab-down\n')
 code=int(os.environ['CAPACITY_TEST_TEARDOWN_EXIT'])
 if code==0: environment.unlink()
 sys.exit(code)
else:
 sys.exit('unexpected lab command')
''')
            executable(bin_path / 'ptah', '#!' + sys.executable + '\n' + r'''import os,pathlib,sys
args=sys.argv[1:]; engine=os.environ['CAPACITY_TEST_ENGINE']
root=pathlib.Path(__file__).resolve().parents[1]
if args[0]=='schema':
 source=pathlib.Path(args[args.index('--schema-file')+1]).resolve()
 expected=root/('support/capacity/mysql/schemas' if engine=='MySQL' else 'demo/schemas')
 assert source.parent==expected and source.is_file(), 'schema source crossed engines'
 assert args[args.index('--dialect')+1]==('mysql' if engine=='MySQL' else 'postgres')
else:
 source=pathlib.Path(args[args.index('--migrations-dir')+1]).resolve()
 base=root/('support/capacity/mysql/migrations' if engine=='MySQL' else 'demo/migrations')
 for path in base.iterdir():
  assert (source/path.name).read_bytes()==path.read_bytes(), 'published migration prefix changed'
 version=args[args.index('--version')+1]
 assert len(list(source.glob('*.up.sql')))==(3 if version.startswith('v2-') else 2)
print('Digest: sha256:'+'a'*64)
''')
            executable(root / 'support/qualification/probes/capacity_bootstrap.py', '#!' + sys.executable + '\n' + r'''import json,os,pathlib,sys
args=sys.argv[1:]; path=pathlib.Path(args[args.index('--state')+1])
action=args[0]+(':'+args[args.index('--checkpoint')+1] if args[0]=='verify-unrelated' else '')
with (path.parent/'actions.log').open('a') as output: output.write(action+'\n')
assert path.is_absolute(), 'journal changes meaning after chdir'
if args[0]=='prepare':
 workload=pathlib.Path(args[args.index('--workload')+1]); assert workload.is_absolute() and workload.exists()
 path.write_text(workload.read_text()); print('work-a,work-b')
elif args[0]=='engine':
 print(json.loads(path.read_text())['engine'])
elif args[0]=='verify-unrelated':
 assert json.loads(path.read_text())['unrelatedObjects']
 if args[args.index('--checkpoint')+1]==os.environ.get('CAPACITY_TEST_FAIL_CHECKPOINT'): sys.exit(43)
else:
 assert args[0]=='cleanup'
 assert path.exists(), 'cleanup lost the original journal'
 path.write_text('{"cleaned":true}')
''')
            executable(bin_path / 'go', '#!' + sys.executable + '\n' + r'''import json,os,pathlib,sys
args=sys.argv[1:]; assert args[args.index('-namespace')+1]=='work-a,work-b'
assert pathlib.Path(args[args.index('-workload')+1]).exists()
assert pathlib.Path(args[args.index('-checkpoint-state')+1])==pathlib.Path(args[args.index('-out')+1],'bootstrap-state.json')
assert json.loads(pathlib.Path(args[args.index('-host-info')+1]).read_text())['dockerID']=='test-daemon'
pathlib.Path(args[args.index('-out')+1],'go-ran').write_text('yes')
pathlib.Path(args[args.index('-out')+1],'report.json').write_text('{"measurement":"retained"}')
with pathlib.Path(args[args.index('-out')+1],'actions.log').open('a') as output: output.write('go\n')
sys.exit(int(os.environ['CAPACITY_TEST_GO_EXIT']))
''')
            go_exit = workload_exit if workload_exit is not None else (0 if unrelated else 42)
            env = dict(os.environ, PATH=str(bin_path) + os.pathsep + os.environ['PATH'],
                       LAB_ENVIRONMENT=str(environment), CAPACITY_WORKLOAD='workload.json', CAPACITY_OUT_DIR='evidence', CAPACITY_TEST_ENGINE=engine, CAPACITY_VARIED_INPUTS='0',
                       CAPACITY_TEST_GO_EXIT=str(go_exit), CAPACITY_TEST_FAIL_CHECKPOINT=fail_checkpoint,
                       CAPACITY_REMOVE_LAB='1' if remove_lab else '0', CAPACITY_TEST_ENVIRONMENT=str(environment),
                       CAPACITY_TEST_TEARDOWN_EXIT=str(teardown_exit))
            result = subprocess.run(['bash', str(root / 'hack/capacity.sh')], cwd=caller, env=env,
                                    capture_output=True, timeout=20)
            expected_exit = 1 if teardown_exit else (43 if fail_checkpoint else go_exit)
            self.assertEqual(result.returncode, expected_exit, result.stderr.decode())
            self.assertEqual((caller / 'evidence/go-ran').exists(), fail_checkpoint != 'before')
            expected = ['prepare', 'engine']
            if unrelated: expected.append('verify-unrelated:before')
            if fail_checkpoint != 'before':
                expected.append('go')
                if unrelated: expected.append('verify-unrelated:after')
            expected.append('lab-down' if remove_lab else 'cleanup')
            self.assertEqual((caller / 'evidence/actions.log').read_text().splitlines(), expected)
            journal = caller / 'evidence/bootstrap-state.json'
            if remove_lab:
                self.assertEqual(json.loads((caller / 'evidence/report.json').read_text()), {'measurement': 'retained'})
                self.assertEqual(json.loads(journal.read_text())['engine'], engine)
                self.assertEqual(environment.exists(), teardown_exit != 0)
                if teardown_exit:
                    self.assertIn('lab teardown failed', result.stderr.decode())
                return
            self.assertEqual(json.loads(journal.read_text()), {'cleaned': True})
            journal.write_text('earlier evidence')
            result = subprocess.run(['bash', str(root / 'hack/capacity.sh')], cwd=caller, env=env,
                                    capture_output=True, timeout=20)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('ownership journal already exists', result.stderr.decode())
            self.assertEqual(journal.read_text(), 'earlier evidence')
            journal.unlink()
            host = caller / 'evidence/host.json'
            before = host.read_bytes()
            result = subprocess.run(['bash', str(root / 'hack/capacity.sh')], cwd=caller, env=env,
                                    capture_output=True, timeout=20)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('host evidence already exists', result.stderr.decode())
            self.assertEqual(host.read_bytes(), before)
            self.assertFalse(journal.exists())

if __name__ == '__main__':
    unittest.main()
