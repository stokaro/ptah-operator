"""Artifact readback must measure actual bytes and fail on missing bindings."""

import copy
import io
import json
from pathlib import Path
import tempfile
import unittest

from capacity_artifacts import ArtifactReader, MAX_PAYLOAD, NoRedirect, digest


class Response(io.BytesIO):
    headers = {}


class Registry:
    def __init__(self, content):
        self.content = content
        self.requests = []

    def open(self, request, timeout):
        self.requests.append(request)
        return Response(self.content[request.full_url.rsplit('/', 1)[1]])


# These are observed wire types, deliberately independent of the reader's map.
WIRE_TYPES = {'schema': 'application/vnd.stokaro.ptah.schema.v1', 'migration': 'application/vnd.stokaro.ptah.migrations.v1'}
WIRE_LAYERS = {'schema': 'application/vnd.stokaro.ptah.schema.hcl.v1', 'migration': 'application/vnd.stokaro.ptah.migration.file.v1'}


class ArtifactTests(unittest.TestCase):
    def fixture(self, kind='migration'):
        files = {'schema.hcl': b'table "capacity_rows" {}\n'} if kind == 'schema' else {
            '0000000001_create.up.sql': b'CREATE TABLE capacity_rows (id BIGINT PRIMARY KEY);\n',
            '0000000001_create.down.sql': b'DROP TABLE capacity_rows;\n', 'ptah.sum': b'h1:fixture\n'}
        config = b'{}'
        content = {digest(config): config} | {digest(raw): raw for raw in files.values()}
        manifest = {'schemaVersion': 2, 'mediaType': 'application/vnd.oci.image.manifest.v1+json',
                    'artifactType': WIRE_TYPES[kind], 'config': {'digest': digest(config), 'size': len(config)},
                    'layers': [{'mediaType': WIRE_LAYERS[kind], 'digest': digest(raw), 'size': len(raw),
                                'annotations': {'org.opencontainers.image.title': name}} for name, raw in files.items()]}
        source = [{'path': 'migration-2-0/' + name, 'bytes': len(raw), 'sha256': digest(raw)[7:]} for name, raw in files.items()]
        return files, content, manifest, source

    def reader(self, root, kind, content, manifest):
        raw = json.dumps(manifest).encode();content = content | {digest(raw): raw}
        reader = ArtifactReader('127.0.0.1:1234', 'user', 'private', Path(root) / 'oci')
        reader.opener = Registry(content)
        return reader, f'oci://127.0.0.1:1234/{kind}s/capacity-inputs@{digest(raw)}'

    def test_readback_counts_every_file_and_retains_digest_verified_bytes(self):
        for kind in ('schema', 'migration'):
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as root:
                files, content, manifest, source = self.fixture(kind)
                reader, ref = self.reader(root, kind, content, manifest)
                result = reader.read(ref, kind, source)
                total = sum(map(len, files.values()))
                self.assertEqual(result['storedPayloadBytes'], total)
                self.assertEqual(result['unpackedPayloadBytes'], total)
                self.assertEqual(result['compression'], 'none')
                self.assertEqual({f['name'] for f in result['files']}, set(files))
                for raw in content.values():
                    self.assertEqual((reader.root / 'blobs' / digest(raw)[7:]).read_bytes(), raw)
                self.assertEqual(len(reader.opener.requests), len(files) + 2)
                reader.read(ref, kind, source)
                self.assertEqual(len(reader.opener.requests), len(files) + 3)
                self.assertTrue(all(r.get_header('Accept-encoding') == 'identity' for r in reader.opener.requests))

    def test_native_migration_manifest_and_files(self):
        fixture = Path(__file__).parent / 'testdata/capacity-artifact'
        raw = (fixture / 'manifest.json').read_bytes()
        manifest = json.loads(raw)
        content = {digest(raw): raw, digest(b'{}'): b'{}'}
        source = []
        for layer in manifest['layers']:
            name = layer['annotations']['org.opencontainers.image.title']
            payload = (fixture / name).read_bytes()
            self.assertEqual(digest(payload), layer['digest'])
            content[layer['digest']] = payload
            source.append({'path': name, 'bytes': len(payload), 'sha256': digest(payload)[7:]})
        with tempfile.TemporaryDirectory() as root:
            reader = ArtifactReader('localhost:1234', 'user', 'private', Path(root) / 'oci')
            reader.opener = Registry(content)
            result = reader.read('oci://localhost:1234/migrations/capacity-inputs@' + digest(raw), 'migration', source)
            self.assertEqual(result['manifestDigest'], digest(raw))
            self.assertEqual(len(result['files']), 5)
            self.assertEqual(result['unpackedPayloadBytes'], 613)

    def test_refuses_corrupt_blobs_even_when_cached(self):
        for cached in (False, True):
            with self.subTest(cached=cached), tempfile.TemporaryDirectory() as root:
                files, content, manifest, source = self.fixture()
                reader, ref = self.reader(root, 'migration', content, manifest)
                identity = manifest['layers'][0]['digest']
                if cached:
                    (reader.root / 'blobs' / identity[7:]).write_bytes(b'wrong')
                else:
                    reader.opener.content[identity] = b'wrong'
                with self.assertRaises(ValueError):
                    reader.read(ref, 'migration', source)
                self.assertEqual(list((reader.root / 'manifests').iterdir()), [])

    def test_refuses_malformed_empty_duplicate_compressed_and_oversize_manifests(self):
        changes = {
            'wrong family': lambda m: m.update(artifactType=WIRE_TYPES['schema']),
            'no files': lambda m: m.update(layers=[]),
            'duplicate': lambda m: m['layers'].append(copy.deepcopy(m['layers'][0])),
            'path traversal': lambda m: m['layers'][0]['annotations'].update({'org.opencontainers.image.title': '../bad'}),
            'compressed': lambda m: m['layers'][0].update(mediaType='application/vnd.oci.image.layer.v1.tar+gzip'),
            'oversize': lambda m: m['layers'][0].update(size=MAX_PAYLOAD + 1),
            'negative size': lambda m: m['layers'][0].update(size=-1),
            'boolean size': lambda m: m['layers'][0].update(size=True),
            'missing file': lambda m: m['layers'].pop(),
            'wrong digest syntax': lambda m: m['layers'][0].update(digest='sha256:not-a-digest'),
        }
        for name, change in changes.items():
            with self.subTest(name=name), tempfile.TemporaryDirectory() as root:
                _, content, manifest, source = self.fixture();change(manifest)
                reader, ref = self.reader(root, 'migration', content, manifest)
                with self.assertRaises(ValueError):
                    reader.read(ref, 'migration', source)
                self.assertEqual(list((reader.root / 'manifests').iterdir()), [])

    def test_refuses_changed_source_and_wrong_schema_inventory(self):
        for kind in ('schema', 'migration'):
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as root:
                _, content, manifest, source = self.fixture(kind)
                if kind == 'schema':
                    manifest['layers'][0]['annotations']['org.opencontainers.image.title'] = 'wrong.hcl'
                else:
                    source[0]['sha256'] = 'a' * 64
                reader, ref = self.reader(root, kind, content, manifest)
                with self.assertRaises(ValueError):
                    reader.read(ref, kind, source)

    def test_fetch_refuses_wrong_digest_and_an_excess_body(self):
        with tempfile.TemporaryDirectory() as root:
            reader = ArtifactReader('localhost:1234', 'user', 'private', Path(root) / 'oci')
            expected = digest(b'correct')
            for raw, bound in ((b'wrong', 100), (b'correct', 2)):
                reader.opener = Registry({expected: raw})
                with self.assertRaises(ValueError):
                    reader.fetch('schemas/capacity-inputs', 'manifests', expected, bound)

    def test_never_redirects_credentials_or_accepts_another_registry(self):
        with self.assertRaises(ValueError):
            NoRedirect().redirect_request(None, None, 307, '', {}, 'https://elsewhere.invalid')
        for host in ('elsewhere.invalid:1234', 'user:pass@localhost:1234', 'localhost:1234/path', 'localhost:1234?query'):
            with tempfile.TemporaryDirectory() as root, self.assertRaises(ValueError):
                ArtifactReader(host, 'user', 'private', Path(root) / 'oci')
        with tempfile.TemporaryDirectory() as root:
            _, content, manifest, source = self.fixture()
            reader, ref = self.reader(root, 'migration', content, manifest)
            for bad in (ref.replace('127.0.0.1', 'elsewhere.invalid'), ref.replace('migrations/', '../'), ref.replace('@sha256:', ':tag@sha256:')):
                with self.assertRaises(ValueError):
                    reader.read(bad, 'migration', source)
            self.assertEqual(reader.opener.requests, [])


if __name__ == '__main__':
    unittest.main()
