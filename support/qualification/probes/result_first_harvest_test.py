import base64
import copy
import hashlib
import json
import pathlib
import unittest

from result_first_harvest import publication, size_fixture, oversized_refusal, pod_binding_record


def digest(data):
    return 'sha256:' + hashlib.sha256(data).hexdigest()


def record(name, uid, role, value, owner=None):
    raw = value if isinstance(value, bytes) else json.dumps(value).encode()
    metadata = {'name': name, 'uid': uid}
    if owner:
        metadata['ownerReferences'] = [{'uid': owner}]
    return {'metadata': metadata, 'spec': {
        'type': role, 'data': base64.b64encode(raw).decode()}}


class PodBindingTests(unittest.TestCase):
    def fixture(self):
        owner = {'apiVersion': 'operator.ptah.run/v1alpha1', 'kind': 'PtahSchema',
                 'name': 'schema', 'uid': 'schema-uid', 'controller': True}
        job = {'metadata': {'namespace': 'probe', 'name': 'ptah-plan-probe', 'uid': 'job-uid',
                            'ownerReferences': [owner]}}
        identity = {'binding': {'namespace': 'probe', 'kind': 'PtahSchema', 'name': 'schema',
                    'uid': 'schema-uid', 'generation': 1, 'executionBindingID': 'v1-' + 'a' * 32,
                    'inputFingerprint': 'sha256:' + 'b' * 64, 'operation': 'plan',
                    'operationID': 'operation', 'jobName': 'ptah-plan-probe',
                    'jobUID': '', 'podName': '', 'podUID': ''}, 'engine': 'postgresql'}
        pod = {'metadata': {'namespace': 'probe', 'name': 'original-pod', 'uid': 'pod-uid',
                            'ownerReferences': [{'uid': 'job-uid'}]},
               'spec': {'automountServiceAccountToken': False,
                        'containers': [{'env': [{'name': 'PTAH_RESULT_IDENTITY_TEMPLATE',
                                                'value': json.dumps(identity)}]}],
                        'volumes': [{'name': 'result-credentials', 'projected': {'sources': [
                            {'serviceAccountToken': {'audience': 'operator.ptah.run/results',
                                                     'expirationSeconds': 3600, 'path': 'token'}}]}}]}}
        return job, pod

    def test_enrolls_public_identity_without_reading_a_token(self):
        job, pod = self.fixture()
        bound = pod_binding_record(job, pod)
        identity = json.loads(base64.b64decode(bound['spec']['data']))
        self.assertEqual(set(identity), {'binding', 'engine'})
        self.assertEqual(identity['binding']['jobUID'], 'job-uid')
        self.assertEqual(identity['binding']['podUID'], 'pod-uid')
        self.assertEqual(identity['binding']['podName'], 'original-pod')
        self.assertEqual(bound['metadata']['ownerReferences'][0]['uid'], 'schema-uid')
        self.assertEqual(bound['metadata']['annotations']['operator.ptah.run/result-pod-uid'], 'pod-uid')
        self.assertEqual(json.loads(pod['spec']['containers'][0]['env'][0]['value'])['binding']['podUID'], '')

    def test_refuses_foreign_identity_and_api_token(self):
        for fault in ('owner', 'namespace', 'missing UID', 'automount', 'audience', 'extra token', 'template'):
            with self.subTest(fault=fault):
                job, pod = self.fixture()
                if fault == 'owner': pod['metadata']['ownerReferences'][0]['uid'] = 'foreign-job'
                if fault == 'namespace': pod['metadata']['namespace'] = 'foreign'
                if fault == 'missing UID': pod['metadata']['uid'] = ''
                if fault == 'automount': pod['spec']['automountServiceAccountToken'] = True
                if fault == 'audience':
                    pod['spec']['volumes'][0]['projected']['sources'][0]['serviceAccountToken']['audience'] = 'https://kubernetes.default.svc'
                if fault == 'extra token': pod['spec']['volumes'].append(copy.deepcopy(pod['spec']['volumes'][0]))
                if fault == 'template':
                    env = pod['spec']['containers'][0]['env'][0]
                    identity = json.loads(env['value'])
                    identity['binding']['uid'] = 'foreign-schema'
                    env['value'] = json.dumps(identity)
                with self.assertRaises(ValueError):
                    pod_binding_record(job, pod)


class SizeFixtureTests(unittest.TestCase):
    def test_captured_native_calibration_hits_exact_limit(self):
        for engine in ('postgresql', 'mysql'):
            with self.subTest(engine=engine):
                dialect, repeated, suffix, checksum = size_fixture(engine)
                path = pathlib.Path(__file__).resolve().parents[3] / (
                    'testdata/e2e/readings/plan-size-small-' + dialect + '.json')
                raw = path.read_bytes()
                self.assertEqual(checksum, hashlib.sha256(raw).hexdigest())
                envelope = len(raw) - 32 * len(b'\\u003c')
                self.assertEqual(envelope + repeated * 6 + suffix, 8 * 1024 * 1024)
                self.assertGreater(repeated, 1_000_000)
                self.assertGreaterEqual(suffix, 0)
                self.assertLess(suffix, 6)

    def test_refuses_an_unrecognized_engine(self):
        for engine in ('', 'postgres', 'MySQL', 'sqlite'):
            with self.subTest(engine=engine), self.assertRaises(ValueError):
                size_fixture(engine)


class OversizedRefusalTests(unittest.TestCase):
    def fixture(self):
        identity = 'sha256:' + 'a' * 64
        schema = {'metadata': {'uid': 'original', 'generation': 1}, 'status': {
            'phase': 'Failed', 'source': {'digest': identity, 'verified': True},
            'target': {'coordinationDigest': identity, 'identityDigest': identity},
            'conditions': [{'type': 'ReconciliationFailed', 'status': 'True',
                            'reason': 'OperationFailed', 'observedGeneration': 1}]}}
        payload = {'operation': 'plan', 'operationId': 'original-operation', 'childExitCode': 0,
                   'coordinationDigest': identity, 'targetIdentityDigest': identity,
                   'error': {'code': 'invalid_plan_output', 'message':
                       'plan output exceeds the configured plan limit: saved file has 8388609 bytes; limit is 8388608'}}
        return schema, payload, identity

    def test_accepts_exact_bound_and_bindings(self):
        schema, payload, identity = self.fixture()
        oversized_refusal(schema, payload, identity, 'original-operation')
        for engine in ('postgresql', 'mysql'):
            _, repeated, suffix, _ = size_fixture(engine)
            _, oversize_repeated, oversize_suffix, _ = size_fixture(engine, 8388609)
            self.assertEqual(oversize_repeated * 6 + oversize_suffix, repeated * 6 + suffix + 1)
        for size in (0, 8388607, 8388610):
            with self.assertRaises(ValueError):
                size_fixture('mysql', size)

    def test_refuses_wrong_size_target_dispatch_and_leaked_plan(self):
        changes = [(key, value) for key, value in (
            ('operation', 'apply'), ('operationId', 'replacement'), ('childExitCode', 1),
            ('coordinationDigest', 'sha256:' + 'b' * 64),
            ('targetIdentityDigest', 'sha256:' + 'b' * 64),
            ('stdout', 'SQL'), ('planContentDigest', 'sha256:' + 'a' * 64),
            ('planOutcome', 'changes'), ('mutationStarted', True), ('uncertain', True),
            ('truncation', {'stdout': True}), ('truncation', {}), ('error', {'code': 'other'}))]
        schema, payload, identity = self.fixture()
        for key, value in changes:
            with self.subTest(key=key), self.assertRaises(ValueError):
                oversized_refusal(schema, {**payload, key: value}, identity, 'original-operation')
        for size in ('8388608', '8388610'):
            bad = copy.deepcopy(payload)
            bad['error']['message'] = bad['error']['message'].replace('8388609', size)
            with self.subTest(size=size), self.assertRaises(ValueError):
                oversized_refusal(schema, bad, identity, 'original-operation')

    def test_accepts_native_refusal_with_an_undispatched_retry_claim(self):
        folder = pathlib.Path(__file__).resolve().parent / 'testdata/results/result-oversized-2026-10-03/postgresql'
        schema = json.loads((folder / 'refused-resource.json').read_text())
        payload = json.loads((folder / 'refused-result.json').read_text())
        manifest = json.loads((folder / 'manifest.json').read_text())
        self.assertEqual(schema['status']['activeOperation']['attempt'], 2)
        self.assertFalse(schema['status']['activeOperation'].get('jobUID'))
        oversized_refusal(schema, payload, schema['status']['source']['digest'], manifest['binding']['operationID'])

    def test_refuses_unverified_stale_or_published_resource(self):
        mutations = {
            'missing UID': lambda s: s['metadata'].update(uid=''),
            'stale condition': lambda s: s['metadata'].update(generation=2),
            'unverified': lambda s: s['status']['source'].update(verified=False),
            'foreign artifact': lambda s: s['status']['source'].update(digest='foreign'),
            'published plan': lambda s: s['status'].update(plan={'name': 'unexpected'}),
            'missing refusal': lambda s: s['status'].update(conditions=[]),
        }
        for name, mutate in mutations.items():
            with self.subTest(name=name):
                schema, payload, identity = self.fixture()
                mutate(schema)
                with self.assertRaises(ValueError):
                    oversized_refusal(schema, payload, identity, 'original-operation')


class PublicationTests(unittest.TestCase):
    def test_inline_publication_requires_all_bytes_and_the_original_owner(self):
        payload = b'small durable result'
        manifest = {'version': 1, 'binding': {'jobUID': 'original-job', 'uid': 'resource'},
                    'size': len(payload), 'digest': digest(payload), 'chunks': None,
                    'inline': base64.b64encode(payload).decode()}
        root = record('attempt', 'intent-uid', 'intent', manifest)
        root['metadata']['ownerReferences'] = [{'uid': 'resource'}]
        rows = {'attempt': root}
        intent, receipt, loaded = publication(rows, 'original-job')
        self.assertEqual((intent, receipt, loaded), (root, root, payload))
        for fault in ('digest', 'size', 'chunks', 'empty', 'oversize', 'owner', 'UID', 'child'):
            with self.subTest(fault=fault):
                broken = copy.deepcopy(rows)
                changed = copy.deepcopy(manifest)
                if fault == 'digest': changed['digest'] = digest(b'foreign')
                if fault == 'size': changed['size'] += 1
                if fault == 'chunks': changed['chunks'] = [{'size': len(payload), 'digest': digest(payload)}]
                if fault == 'empty': changed['inline'] = ''
                if fault == 'oversize':
                    data = b'x' * 262145
                    changed.update(inline=base64.b64encode(data).decode(), size=len(data), digest=digest(data))
                if fault == 'owner': broken['attempt']['metadata']['ownerReferences'][0]['uid'] = 'foreign'
                if fault == 'UID': broken['attempt']['metadata']['uid'] = ''
                if fault == 'child':
                    child = record('attempt-000', 'chunk-uid', 'chunk', {})
                    child['metadata']['ownerReferences'] = [{'uid': 'intent-uid'}]
                    broken['attempt-000'] = child
                broken['attempt']['spec']['data'] = base64.b64encode(json.dumps(changed).encode()).decode()
                with self.assertRaises(ValueError): publication(broken, 'original-job')

    def fixture(self):
        parts = [b'first chunk', b'second chunk']
        manifest = {'binding': {'jobUID': 'original-job'},
                    'size': len(b''.join(parts)), 'digest': digest(b''.join(parts)),
                    'chunks': [{'size': len(p), 'digest': digest(p)} for p in parts]}
        intent = record('attempt', 'intent-uid', 'intent', manifest)
        completion = record('attempt-complete', 'receipt-uid', 'complete', {
            'manifestUID': 'intent-uid',
            'manifestDigest': digest(base64.b64decode(intent['spec']['data'])),
            'chunkUIDs': ['chunk-0', 'chunk-1']}, 'intent-uid')
        rows = [intent, completion]
        rows += [record(f'attempt-{i:03}', f'chunk-{i}', 'chunk', part, 'intent-uid')
                 for i, part in enumerate(parts)]
        return {r['metadata']['name']: r for r in rows}

    def test_reads_exact_committed_bytes(self):
        rows = self.fixture()
        intent, receipt, data = publication(rows, 'original-job')
        self.assertEqual(intent['metadata']['uid'], 'intent-uid')
        self.assertEqual(receipt['metadata']['uid'], 'receipt-uid')
        self.assertEqual(data, b'first chunksecond chunk')

    def test_refuses_missing_or_changed_members(self):
        mutations = {
            'empty census': lambda r: r.clear(),
            'missing completion': lambda r: r.pop('attempt-complete'),
            'missing chunk': lambda r: r.pop('attempt-001'),
            'replacement intent': lambda r: r['attempt']['metadata'].update(uid='replacement'),
            'replacement chunk': lambda r: r['attempt-000']['metadata'].update(uid='replacement'),
            'missing chunk UID': lambda r: r['attempt-000']['metadata'].update(uid=''),
            'wrong completion owner': lambda r: r['attempt-complete']['metadata'].update(ownerReferences=[{'uid': 'foreign'}]),
            'wrong chunk owner': lambda r: r['attempt-000']['metadata'].update(ownerReferences=[{'uid': 'foreign'}]),
            'wrong completion role': lambda r: r['attempt-complete']['spec'].update(type='chunk'),
            'wrong chunk role': lambda r: r['attempt-000']['spec'].update(type='intent'),
            'changed bytes': lambda r: r['attempt-000']['spec'].update(data=base64.b64encode(b'wrong chunk').decode()),
        }
        for name, mutate in mutations.items():
            with self.subTest(name=name):
                rows = self.fixture()
                mutate(rows)
                with self.assertRaises((ValueError, KeyError)):
                    publication(rows, 'original-job')

    def test_refuses_foreign_job_and_duplicate_intent(self):
        rows = self.fixture()
        with self.assertRaises(ValueError):
            publication(rows, 'replacement-job')
        rows['duplicate'] = copy.deepcopy(rows['attempt'])
        with self.assertRaises(ValueError):
            publication(rows, 'original-job')

    def test_refuses_inconsistent_completion_and_manifest(self):
        for target, field, value in (
            ('attempt-complete', 'manifestDigest', 'sha256:foreign'),
            ('attempt-complete', 'chunkUIDs', []),
            ('attempt-complete', 'chunkUIDs', ['chunk-1', 'chunk-0']),
            ('attempt', 'size', 1),
            ('attempt', 'digest', 'sha256:foreign'),
            ('attempt', 'chunks', []),
        ):
            with self.subTest(target=target, field=field, value=value):
                rows = self.fixture()
                obj = json.loads(base64.b64decode(rows[target]['spec']['data']))
                obj[field] = value
                rows[target]['spec']['data'] = base64.b64encode(json.dumps(obj).encode()).decode()
                # A malformed manifest with a matching commitment must still
                # fail its own size/digest/chunk checks.
                if target == 'attempt':
                    commit = json.loads(base64.b64decode(rows['attempt-complete']['spec']['data']))
                    commit['manifestDigest'] = digest(base64.b64decode(rows[target]['spec']['data']))
                    rows['attempt-complete']['spec']['data'] = base64.b64encode(json.dumps(commit).encode()).decode()
                with self.assertRaises(ValueError):
                    publication(rows, 'original-job')


if __name__ == '__main__':
    unittest.main()
