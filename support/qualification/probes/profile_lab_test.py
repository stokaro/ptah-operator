import base64
import copy
import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest

from profile_lab import (QUOTA, archive, database_service, preflight_failures, profile_freeze, profile_values,
                         read_environment, receive, registry_secrets, write_environment)
from profile_network import bind_policy, task_expectations


def candidate():
    return {'fullnameOverride': 'ptah-operator-e2e', 'image': {'repository': 'registry/ptah-operator', 'digest': 'sha256:' + 'a' * 64},
            'imagePullSecrets': [{'name': 'pull'}], 'execution': {'executorImage': 'e', 'runnerImage': 'r', 'ptahVersion': 'v'},
            'replicaCount': 2, 'certificateRotation': {'interval': '168h'}, 'podDisruptionBudget': {'enabled': False},
            'applyPolicyGuard': {'exemptGroups': ['kubeadm:cluster-admins']}}


def healthy():
    host = {'os': 'linux', 'cpus': 16, 'memoryBytes': 58 * 1024**3}
    nodes = [{'metadata': {'name': f'n{i}', 'labels': {'node-role.kubernetes.io/control-plane': ''} if i < 3 else {}}} for i in range(4)]
    configz = {f'n{i}': {'kubeletconfig': {'containerLogMaxSize': '10Mi'}} for i in range(4)}
    labels = {}
    for mode in ('enforce', 'warn', 'audit'):
        labels['pod-security.kubernetes.io/' + mode] = 'restricted'
        labels['pod-security.kubernetes.io/' + mode + '-version'] = 'v1.37'
    namespaces = {name: {'metadata': {'labels': dict(labels)}} for name in ('ptah-system', 'ptah-qualification-a', 'ptah-qualification-b')}
    quotas = {name: {'spec': {'hard': dict(QUOTA)}} for name in ('ptah-qualification-a', 'ptah-qualification-b')}
    managers = [{'status': {'containerStatuses': [{'ready': True, 'restartCount': 0}]}} for _ in range(2)]
    kindnet = [{'metadata': {}, 'spec': {'containers': [{'image': 'kindnet:fixed'}]},
                'status': {'containerStatuses': [{'ready': True}]}} for _ in range(4)]
    return [host, nodes, configz, namespaces, quotas, managers, kindnet, 'kindnet:fixed', 'v1.37']


class ProfileTest(unittest.TestCase):
    def test_the_profile_keeps_the_candidate_images_and_adds_only_its_own_settings(self):
        values = profile_values(candidate())
        self.assertEqual(set(values), {'image', 'imagePullSecrets', 'execution', 'replicaCount', 'approvals', 'applyPolicyGuard'})
        self.assertEqual(values['image'], candidate()['image'])
        self.assertEqual(values['approvals'], {'requireDistinctApprover': True})
        self.assertEqual(values['applyPolicyGuard'], {'enabled': True, 'exemptGroups': ['system:masters']})
        single = candidate()
        single['replicaCount'] = 1
        with self.assertRaises(ValueError):
            profile_values(single)

    def test_each_engine_has_its_own_database_server(self):
        self.assertEqual(database_service('PostgreSQL'), ('capacity-postgres', 5432))
        self.assertEqual(database_service('MySQL'), ('capacity-mysql', 3306))
        with self.assertRaises(ValueError):
            database_service('Oracle')

    def test_the_registry_secrets_match_what_the_demonstration_lab_writes(self):
        opaque, pull = registry_secrets('ns', 'e2e-registry.ns.svc.cluster.local:5000', {'username': 'u', 'password': 'p:w'})
        self.assertEqual((opaque['metadata'], opaque['type']), ({'name': 'demo-registry', 'namespace': 'ns'}, 'Opaque'))
        self.assertEqual(opaque['stringData'], {'username': 'u', 'password': 'p:w', 'registry': 'e2e-registry.ns.svc.cluster.local:5000',
                                                'allowPlainHTTP': 'true'})
        self.assertEqual((pull['metadata']['name'], pull['type']), ('demo-registry-pull', 'kubernetes.io/dockerconfigjson'))
        entry = json.loads(pull['stringData']['.dockerconfigjson'])['auths']['e2e-registry.ns.svc.cluster.local:5000']
        self.assertEqual(base64.b64decode(entry['auth']), b'u:p:w')
        self.assertEqual((entry['username'], entry['password']), ('u', 'p:w'))

    def test_a_lab_meeting_the_profile_passes_the_preflight(self):
        self.assertEqual(preflight_failures(*healthy()), [])

    def test_the_preflight_names_each_way_a_lab_can_fall_short(self):
        cases = {
            'below 4 CPUs and 16 GiB': lambda a: a[0].update(memoryBytes=int(15.6 * 1024**3)),
            'three control-plane nodes': lambda a: a.__setitem__(1, a[1][:3]),
            'default 10Mi log size': lambda a: a[2]['n3']['kubeletconfig'].update(containerLogMaxSize='64Mi'),
            'enforce restricted Pod Security': lambda a: a[3]['ptah-system']['metadata']['labels'].update({'pod-security.kubernetes.io/enforce': 'baseline'}),
            'audit restricted Pod Security': lambda a: a[3]['ptah-qualification-b']['metadata']['labels'].update({'pod-security.kubernetes.io/audit-version': 'v1.35'}),
            'profile quota': lambda a: a[4]['ptah-qualification-a']['spec']['hard'].update(pods='48'),
            'ready and unrestarted': lambda a: a[5][1]['status']['containerStatuses'][0].update(restartCount=1),
            'corrected kindnet': lambda a: a[6][2]['spec']['containers'][0].update(image='kindnet:original'),
        }
        for want, mutate in cases.items():
            arguments = copy.deepcopy(healthy())
            mutate(arguments)
            failures = preflight_failures(*arguments)
            self.assertTrue(any(want in failure for failure in failures), (want, failures))

    def test_a_terminating_kindnet_pod_is_not_counted(self):
        arguments = healthy()
        arguments[6].append({'metadata': {'deletionTimestamp': 'x'}, 'spec': {'containers': [{'image': 'kindnet:original'}]},
                             'status': {'containerStatuses': []}})
        self.assertEqual(preflight_failures(*arguments), [])


class FreezeTest(unittest.TestCase):
    def freeze(self, files):
        entries = [{'path': p, 'sha256': hashlib.sha256(r).hexdigest()} for p, r in files.items()]
        return json.dumps({'revision': 41, 'sourceFiles': entries[:-1], 'functionalMatrix': entries[-1]}).encode()

    def test_a_lab_records_the_revision_whose_inputs_match(self):
        files = {'profile.md': b'targets', 'coverage.md': b'cells', 'matrix.md': b'54'}
        raw = self.freeze(files)
        self.assertEqual(profile_freeze(raw, files.__getitem__),
                         {'revision': 41, 'sha256': hashlib.sha256(raw).hexdigest(), 'inputs': 3})

    def test_a_changed_input_or_an_empty_freeze_is_refused(self):
        files = {'profile.md': b'targets', 'matrix.md': b'54'}
        raw = self.freeze(files)
        for path in files:
            with self.subTest(path), self.assertRaises(ValueError) as refusal:
                profile_freeze(raw, lambda p: b'changed' if p == path else files[p])
            self.assertIn(path, str(refusal.exception))
        with self.assertRaises(ValueError):
            profile_freeze(self.freeze({'matrix.md': b'54'}), files.__getitem__)

    def test_the_real_freeze_matches_this_checkout(self):
        root = Path(__file__).resolve().parents[3]
        result = profile_freeze((root / 'support/qualification/0.2.0-freeze.json').read_bytes(), lambda p: (root / p).read_bytes())
        self.assertGreaterEqual(result['revision'], 41)


class EvidenceTest(unittest.TestCase):
    def transfer(self, files, manifest=None):
        manifest = manifest if manifest is not None else {name: hashlib.sha256(raw).hexdigest() for name, raw in files.items()}
        return archive(dict(files, **{'transfer-manifest.json': json.dumps(manifest).encode()}))

    def test_an_archive_matching_its_manifest_is_received(self):
        with tempfile.TemporaryDirectory() as directory:
            raw = self.transfer({'report.json': b'{}', 'inputs/catalog.json': b'[]'})
            inventory = receive(raw, Path(directory) / 'out')
            self.assertEqual(set(inventory), {'report.json', 'inputs/catalog.json'})
            self.assertEqual((Path(directory) / 'out/inputs/catalog.json').read_bytes(), b'[]')

    def test_an_archive_that_disagrees_with_its_manifest_is_refused(self):
        files = {'report.json': b'{}', 'driver.log': b'ok'}
        for name, manifest in {
            'a changed file': {'report.json': hashlib.sha256(b'{}').hexdigest(), 'driver.log': hashlib.sha256(b'other').hexdigest()},
            'a file the manifest omits': {'report.json': hashlib.sha256(b'{}').hexdigest()},
            'a file the archive lacks': dict({n: hashlib.sha256(r).hexdigest() for n, r in files.items()}, extra='0' * 64),
            'an empty manifest': {},
        }.items():
            with tempfile.TemporaryDirectory() as directory, self.subTest(name):
                with self.assertRaises(ValueError):
                    receive(self.transfer(files, manifest), Path(directory) / 'out')

    def test_an_archive_escaping_its_directory_is_refused(self):
        output = io.BytesIO()
        with tarfile.open(fileobj=output, mode='w:gz') as tar:
            for name, raw in (('../escape', b'x'), ('transfer-manifest.json', json.dumps({'../escape': hashlib.sha256(b'x').hexdigest()}).encode())):
                member = tarfile.TarInfo(name)
                member.size = len(raw)
                tar.addfile(member, io.BytesIO(raw))
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaises(ValueError):
                receive(output.getvalue(), Path(directory) / 'out')
        with self.assertRaises(ValueError):
            archive({'/absolute': b'x'})

    def test_an_environment_round_trips_and_refuses_repeated_keys(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'environment'
            values = {'E2E_KUBECONFIG': '/a b/kubeconfig', 'EMPTY': '', 'QUOTED': "it's"}
            write_environment(path, values)
            self.assertEqual(read_environment(path), values)
            path.write_text('A=1\nA=2\n')
            with self.assertRaises(ValueError):
                read_environment(path)


class NetworkTest(unittest.TestCase):
    def test_each_operation_reaches_only_what_it_needs(self):
        self.assertEqual(task_expectations('schema', 'resolve'), {'dns': 'open', 'database': 'closed', 'registry': 'open', 'api': 'closed', 'receiver': 'open'})
        self.assertEqual(task_expectations('schema', 'apply')['registry'], 'closed')
        self.assertEqual(task_expectations('schema', 'observe')['database'], 'open')
        self.assertEqual(task_expectations('migration', 'apply'), {'dns': 'open', 'database': 'open', 'registry': 'open', 'api': 'closed', 'receiver': 'open'})
        for family, operations in (('schema', ('resolve', 'verify', 'observe', 'plan', 'apply')), ('migration', ('resolve', 'verify', 'history', 'apply'))):
            for operation in operations:
                self.assertEqual(task_expectations(family, operation)['api'], 'closed')

    def test_binding_points_each_policy_at_the_lab_without_changing_what_it_selects(self):
        example = Path(__file__).resolve().parents[3] / 'examples/networkpolicy-egress.yaml'
        self.assertTrue(example.is_file())
        database = {'metadata': {'name': 'ptah-migration-operations-database'},
                    'spec': {'podSelector': {'matchLabels': {'app.kubernetes.io/component': 'migration-operation'}},
                             'egress': [{'to': [{'podSelector': {'matchLabels': {'app.kubernetes.io/name': 'application-database'}}}],
                                         'ports': [{'protocol': 'TCP', 'port': 5432}]}]}}
        bound = bind_policy(database, 'source', {'app': 'registry'}, 'fixtures', {'app.kubernetes.io/name': 'capacity-mysql'}, 3306, 'ptah-system', {'app': 'manager'})
        rule = bound['spec']['egress'][0]
        self.assertEqual(rule['ports'], [{'protocol': 'TCP', 'port': 3306}])
        self.assertEqual(rule['to'][0]['namespaceSelector'], {'matchLabels': {'kubernetes.io/metadata.name': 'fixtures'}})
        self.assertEqual(rule['to'][0]['podSelector'], {'matchLabels': {'app.kubernetes.io/name': 'capacity-mysql'}})
        self.assertEqual(bound['spec']['podSelector'], database['spec']['podSelector'])
        self.assertEqual(database['spec']['egress'][0]['ports'][0]['port'], 5432, 'binding changed the documented example')


if __name__ == '__main__':
    unittest.main()
