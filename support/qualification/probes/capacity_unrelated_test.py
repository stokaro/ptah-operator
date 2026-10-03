"""Refusal controls for the exact background population and real Job evidence."""
import copy
import unittest

from capacity_unrelated import COUNT, LABEL, PAYLOAD, inventory, objects

IMAGE = 'registry.example/postgres@sha256:' + 'a' * 64


def fixture():
    configs, jobs, pods, expected = [], [], [], []
    for i in range(COUNT):
        c, j = objects('background-' + str(i % 2), i, 'run', IMAGE)
        # API identities differ even though a ConfigMap and Job share a name.
        c['metadata'] = copy.deepcopy(c['metadata']); j['metadata'] = copy.deepcopy(j['metadata'])
        c['metadata']['uid'] = 'config-' + str(i); j['metadata']['uid'] = 'job-' + str(i)
        j['status'] = {'succeeded': 1, 'completionTime': '2026-10-01T00:00:01Z',
                       'conditions': [{'type': 'Complete', 'status': 'True'}]}
        pod = {'metadata': {'uid': 'pod-' + str(i), 'namespace': j['metadata']['namespace'],
                           'ownerReferences': [{'apiVersion': 'batch/v1', 'kind': 'Job',
                                                'controller': True, 'uid': j['metadata']['uid']}]},
               'spec': copy.deepcopy(j['spec']['template']['spec']),
               'status': {'phase': 'Succeeded', 'containerStatuses': [{
                   'name': 'complete', 'restartCount': 0, 'imageID': 'docker-pullable://' + IMAGE,
                   'state': {'terminated': {'exitCode': 0, 'startedAt': '2026-10-01T00:00:00Z',
                                           'finishedAt': '2026-10-01T00:00:01Z'}}}]}}
        configs.append(c); jobs.append(j); pods.append(pod)
        expected.extend(copy.deepcopy([c, j]))
    return configs, jobs, pods, expected


class UnrelatedTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.population = fixture()

    def test_exact_profile_population(self):
        c, j, p, expected = self.population
        result = inventory(c, j, p, expected, IMAGE, 'run')
        self.assertEqual(result, {'configMaps': 1000, 'payloadBytesPerConfigMap': 1024,
                                  'completedJobs': 1000, 'successfulPods': 1000})
        for namespace in ('background-0', 'background-1'):
            self.assertEqual(sum(o['metadata']['namespace'] == namespace for o in c), 500)
            self.assertEqual(sum(o['metadata']['namespace'] == namespace for o in j), 500)

    def test_refuses_partial_replaced_or_invented_execution(self):
        for mode in ('no configs', 'missing job', 'missing pod', 'replaced config', 'duplicate config',
                     'short payload', 'extra binary data', 'workload label', 'managed label',
                     'missing completion', 'failed job', 'active job', 'wrong owner', 'duplicate owner',
                     'wrong namespace', 'pending pod', 'failed exit', 'no image identity', 'changed image',
                     'restarted container', 'no execution timestamp'):
            with self.subTest(mode=mode):
                c, j, p, expected = copy.deepcopy(self.population)
                if mode == 'no configs': c.clear()
                elif mode == 'missing job': j.pop()
                elif mode == 'missing pod': p.pop()
                elif mode == 'replaced config': c[0]['metadata']['uid'] = 'replacement'
                elif mode == 'duplicate config': c[0] = c[1]
                elif mode == 'short payload': c[0]['data']['payload'] = PAYLOAD[:-1]
                elif mode == 'extra binary data': c[0]['binaryData'] = {'extra': 'YQ=='}
                elif mode == 'workload label': j[0]['metadata']['labels']['operator.ptah.run/capacity'] = 'soak'
                elif mode == 'managed label': j[0]['metadata']['labels']['app.kubernetes.io/managed-by'] = 'ptah-operator'
                elif mode == 'missing completion': j[0]['status']['conditions'] = []
                elif mode == 'failed job': j[0]['status']['failed'] = 1
                elif mode == 'active job': j[0]['status']['active'] = 1
                elif mode == 'wrong owner': p[0]['metadata']['ownerReferences'][0]['uid'] = 'unrelated'
                elif mode == 'duplicate owner': p[0]['metadata']['ownerReferences'] = p[1]['metadata']['ownerReferences']
                elif mode == 'wrong namespace': p[0]['metadata']['namespace'] = 'other'
                elif mode == 'pending pod': p[0]['status']['phase'] = 'Pending'
                elif mode == 'failed exit': p[0]['status']['containerStatuses'][0]['state']['terminated']['exitCode'] = 1
                elif mode == 'no image identity': p[0]['status']['containerStatuses'][0].pop('imageID')
                elif mode == 'changed image': p[0]['spec']['containers'][0]['image'] = 'other'
                elif mode == 'restarted container': p[0]['status']['containerStatuses'][0]['restartCount'] = 1
                elif mode == 'no execution timestamp': p[0]['status']['containerStatuses'][0]['state']['terminated'].pop('finishedAt')
                with self.assertRaises(ValueError): inventory(c, j, p, expected, IMAGE, 'run')

    def test_background_jobs_have_no_workload_privileges_or_live_service(self):
        c, j = objects('background', 0, 'run', IMAGE)
        self.assertEqual(len(c['data']['payload'].encode()), 1024)
        self.assertTrue(c['immutable'])
        spec = j['spec']['template']['spec']; container = spec['containers'][0]
        self.assertFalse(spec['automountServiceAccountToken'])
        self.assertTrue(spec['securityContext']['runAsNonRoot'])
        self.assertEqual(spec['securityContext']['seccompProfile']['type'], 'RuntimeDefault')
        self.assertFalse(container['securityContext']['allowPrivilegeEscalation'])
        self.assertTrue(container['securityContext']['readOnlyRootFilesystem'])
        self.assertEqual(container['securityContext']['capabilities']['drop'], ['ALL'])
        self.assertEqual(container['command'], ['/bin/sh', '-c', 'exit 0'])
        self.assertEqual(j['spec']['backoffLimit'], 0)
        self.assertNotIn('ttlSecondsAfterFinished', j['spec'])
        self.assertEqual(j['metadata']['labels'], {LABEL: 'run'})
        self.assertTrue(container['resources']['requests'])
        self.assertTrue(container['resources']['limits'])



class UnrelatedBootstrapTests(unittest.TestCase):
    def test_owned_namespaces_and_complete_readback_survive_until_cleanup(self):
        import json
        from pathlib import Path
        import tempfile
        import threading
        from capacity_bootstrap_test import FakeBootstrap
        from capacity_unrelated import verify

        class BackgroundBootstrap(FakeBootstrap):
            def __init__(self, path):
                super().__init__(path)
                self.env['E2E_POSTGRES_IMAGE'] = IMAGE
                self.creation_lock = threading.Lock()

            def create(self, value):
                with self.creation_lock:
                    result = super().create(value)
                    if value['kind'] == 'Job':
                        key = ('Job', result['metadata']['namespace'], result['metadata']['name'])
                        result['status'] = {'succeeded': 1, 'completionTime': '2026-10-01T00:00:01Z',
                                            'conditions': [{'type': 'Complete', 'status': 'True'}]}
                        self.objects[key] = copy.deepcopy(result)
                        super().create({'apiVersion': 'v1', 'kind': 'Pod',
                                        'metadata': {'name': result['metadata']['name'] + '-pod',
                                                     'namespace': result['metadata']['namespace'],
                                                     'labels': {LABEL: self.state['runID']},
                                                     'ownerReferences': [{'apiVersion': 'batch/v1', 'kind': 'Job',
                                                                          'controller': True, 'uid': result['metadata']['uid']}]},
                                        'spec': copy.deepcopy(result['spec']['template']['spec']),
                                        'status': {'phase': 'Succeeded', 'containerStatuses': [{
                                            'name': 'complete', 'restartCount': 0, 'imageID': IMAGE,
                                            'state': {'terminated': {'exitCode': 0, 'startedAt': '2026-10-01T00:00:00Z',
                                                                    'finishedAt': '2026-10-01T00:00:01Z'}}}]}})
                    return result

            def command(self, args, value=None, timeout=45):
                if args[0] == '-n' and args[2] == 'get':
                    kind = {'configmaps': 'ConfigMap', 'jobs': 'Job', 'pods': 'Pod'}[args[3]]
                    return json.dumps({'items': [o for (k, ns, _), o in self.objects.items()
                                                if k == kind and ns == args[1] and LABEL in o['metadata'].get('labels', {})]}).encode()
                return super().command(args, value, timeout)

        with tempfile.TemporaryDirectory() as directory:
            b = BackgroundBootstrap(Path(directory) / 'state.json')
            workload = b.prepare({'schemas': 10, 'migrations': 10, 'unrelatedObjects': True})
            owned = b.state['unrelated']['namespaces']
            self.assertEqual(len(owned), 2)
            self.assertTrue(all(ns['name'] not in workload for ns in owned))
            self.assertEqual(b.state['unrelated']['prepared']['completedJobs'], 1000)
            verify(b, 'before')
            proof = verify(b, 'after')
            self.assertEqual(proof['successfulPods'], 1000)
            self.assertTrue((Path(directory) / proof['path']).is_file())
            for ns in owned:
                self.assertEqual(b.objects['Namespace', '', ns['name']]['metadata']['labels']['pod-security.kubernetes.io/enforce'], 'restricted')
            b.cleanup()
            self.assertTrue(b.state['cleaned'])
            self.assertEqual(b.deleted[:2], [ns['name'] for ns in reversed(owned)])
            self.assertEqual(b.deleted[-1], b.state['fixtureNamespace'])

if __name__ == '__main__': unittest.main()
