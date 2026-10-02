import base64
import copy
import hashlib
import json
import pathlib
import unittest

from result_first_harvest import publication, size_fixture


def digest(data):
    return 'sha256:' + hashlib.sha256(data).hexdigest()


def record(name, uid, role, value, owner=None):
    raw = value if isinstance(value, bytes) else json.dumps(value).encode()
    metadata = {'name': name, 'uid': uid}
    if owner:
        metadata['ownerReferences'] = [{'uid': owner}]
    return {'metadata': metadata, 'spec': {
        'type': role, 'data': base64.b64encode(raw).decode()}}


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


class PublicationTests(unittest.TestCase):
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
