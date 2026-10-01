"""Ownership and workload-placement controls for the capacity lab bootstrap."""

import copy
import json
import os
import shutil
import subprocess
import sys
from pathlib import Path
import tempfile
import unittest
from urllib.parse import urlsplit

from capacity_bootstrap import Bootstrap


class FakeBootstrap(Bootstrap):
    def __init__(self, state):
        super().__init__(state, {'E2E_KUBECONFIG': '/unused', 'E2E_TEST_NAMESPACE': 'shared-lab',
                               'E2E_POSTGRES_IMAGE': 'postgres@sha256:fixture'})
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

    def command(self, args, value=None, timeout=45):
        self.commands.append((list(args), value))
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
        b.cleanup()
        self.assertEqual(b.deleted, [namespaces[1], namespaces[0], b.state['fixtureNamespace']])
        self.assertTrue(b.state['cleaned'])
        self.assertEqual(b.state['namespaces'], [])
        b.cleanup()  # A terminal cleanup is safe to retry.
        self.assertEqual(len(b.deleted), 3)

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
    def test_relative_output_and_failed_run_keep_cleanup_bound_to_original_journal(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / 'repository'
            caller = Path(directory) / 'caller'
            bin_path = root / 'tools'
            for path in (root / 'hack', root / 'demo/bin', root / 'demo/migrations', root / 'support/capacity', caller, bin_path):
                path.mkdir(parents=True, exist_ok=True)
            source = Path(__file__).resolve().parents[3] / 'hack/capacity.sh'
            shutil.copyfile(source, root / 'hack/capacity.sh')
            (caller / 'workload.json').write_text('{}')
            environment = caller / 'environment'
            environment.write_text('E2E_KUBECONFIG=/unused\nE2E_OPERATOR_NAMESPACE=operator\nE2E_REGISTRY_HOST=registry\nE2E_REGISTRY_IP=127.0.0.1\n')
            def executable(path, body):
                path.write_text(body)
                path.chmod(0o700)
            executable(root / 'demo/bin/lab', '#!/bin/sh\ncase "$1" in\ncredentials) echo "PTAH_OCI_USERNAME=user PTAH_OCI_PASSWORD=fixture PTAH_OCI_REGISTRY=registry" ;;\ntools) echo "' + str(bin_path) + '" ;;\nesac\n')
            executable(bin_path / 'ptah', '#!/bin/sh\necho Digest: sha256:' + 'a' * 64 + '\n')
            executable(bin_path / 'python3', '#!' + sys.executable + '\n' + r'''import json,pathlib,sys
args=sys.argv[1:]; path=pathlib.Path(args[args.index('--state')+1])
assert path.is_absolute(), 'journal changes meaning after chdir'
if args[1]=='prepare':
 workload=pathlib.Path(args[args.index('--workload')+1]); assert workload.is_absolute() and workload.exists()
 path.write_text('{}'); print('work-a,work-b')
else:
 assert path.exists(), 'cleanup lost the original journal'
 path.write_text('{"cleaned":true}')
''')
            executable(bin_path / 'go', '#!' + sys.executable + '\n' + r'''import pathlib,sys
args=sys.argv[1:]; assert args[args.index('-namespace')+1]=='work-a,work-b'
assert pathlib.Path(args[args.index('-workload')+1]).exists()
pathlib.Path(args[args.index('-out')+1],'go-ran').write_text('yes')
sys.exit(42)
''')
            env = dict(os.environ, PATH=str(bin_path) + os.pathsep + os.environ['PATH'],
                       LAB_ENVIRONMENT=str(environment), CAPACITY_WORKLOAD='workload.json', CAPACITY_OUT_DIR='evidence')
            result = subprocess.run(['bash', str(root / 'hack/capacity.sh')], cwd=caller, env=env,
                                    capture_output=True, timeout=20)
            self.assertEqual(result.returncode, 42, result.stderr.decode())
            self.assertTrue((caller / 'evidence/go-ran').exists())
            journal = caller / 'evidence/bootstrap-state.json'
            self.assertEqual(json.loads(journal.read_text()), {'cleaned': True})
            journal.write_text('earlier evidence')
            result = subprocess.run(['bash', str(root / 'hack/capacity.sh')], cwd=caller, env=env,
                                    capture_output=True, timeout=20)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('ownership journal already exists', result.stderr.decode())
            self.assertEqual(journal.read_text(), 'earlier evidence')

if __name__ == '__main__':
    unittest.main()
