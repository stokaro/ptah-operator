"""Read back bounded, uncompressed Ptah workload artifacts by their digest."""

import base64
import hashlib
import json
from pathlib import Path
import re
import urllib.request

MAX_PAYLOAD = 8 * 1024 * 1024
MAX_MANIFEST = 1024 * 1024
DIGEST = re.compile(r'sha256:[0-9a-f]{64}')
TYPES = {'schema': 'application/vnd.stokaro.ptah.schema.v1',
         'migration': 'application/vnd.stokaro.ptah.migrations.v1'}
LAYERS = {'schema': 'application/vnd.stokaro.ptah.schema.hcl.v1',
          'migration': 'application/vnd.stokaro.ptah.migration.file.v1'}


def digest(raw):
    return 'sha256:' + hashlib.sha256(raw).hexdigest()


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, msg, headers, newurl):
        raise ValueError('the private lab registry must serve its own artifact bytes')


class ArtifactReader:
    def __init__(self, registry, username, password, directory):
        if not re.fullmatch(r'(?:localhost|127\.0\.0\.1):[0-9]{1,5}', registry):
            raise ValueError('artifact readback requires the lab registry tunnel on loopback')
        self.registry = registry
        self.authorization = 'Basic ' + base64.b64encode((username + ':' + password).encode()).decode()
        self.root = Path(directory)
        self.root.mkdir(mode=0o700)
        (self.root / 'manifests').mkdir(mode=0o700)
        (self.root / 'blobs').mkdir(mode=0o700)
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())

    def fetch(self, repository, component, identity, bound):
        # Neither a redirect nor a process argument may carry the credential.
        request = urllib.request.Request(f'http://{self.registry}/v2/{repository}/{component}/{identity}',
            headers={'Authorization': self.authorization, 'Accept': 'application/vnd.oci.image.manifest.v1+json',
                     'Accept-Encoding': 'identity'})
        with self.opener.open(request, timeout=30) as response:
            if response.headers.get('Content-Encoding', 'identity') != 'identity':
                raise ValueError('unexpected HTTP content encoding')
            raw = response.read(bound + 1)
        if len(raw) > bound or digest(raw) != identity:
            raise ValueError('registry bytes exceed the bound or differ from their requested digest')
        return raw

    def blob(self, repository, descriptor):
        identity, size = descriptor.get('digest'), descriptor.get('size')
        if not isinstance(identity, str) or not DIGEST.fullmatch(identity) or type(size) is not int or not 0 <= size <= MAX_PAYLOAD:
            raise ValueError('artifact has an invalid blob descriptor')
        path = self.root / 'blobs' / identity.removeprefix('sha256:')
        raw = path.read_bytes() if path.exists() else self.fetch(repository, 'blobs', identity, size)
        if len(raw) != size or digest(raw) != identity:
            raise ValueError('artifact blob differs from its declared size or digest')
        if not path.exists():
            path.write_bytes(raw)
        return raw

    def read(self, reference, kind, source_files):
        prefix = 'oci://' + self.registry + '/'
        if kind not in TYPES or not reference.startswith(prefix):
            raise ValueError('artifact reference does not name the selected lab registry and family')
        match = re.fullmatch(r'((?:schemas|migrations)/capacity-inputs)@(sha256:[0-9a-f]{64})', reference[len(prefix):])
        if not match or match[1] != kind + 's/capacity-inputs':
            raise ValueError('artifact reference is not the immutable workload repository')
        repository, identity = match.groups()
        raw = self.fetch(repository, 'manifests', identity, MAX_MANIFEST)
        if digest(raw) != identity:
            raise ValueError('artifact manifest differs from its requested digest')
        manifest = json.loads(raw)
        if manifest.get('schemaVersion') != 2 or manifest.get('mediaType') != 'application/vnd.oci.image.manifest.v1+json' or manifest.get('artifactType') != TYPES[kind]:
            raise ValueError('artifact manifest has the wrong format or family')
        layers = manifest.get('layers')
        if not isinstance(layers, list) or not layers:
            raise ValueError('artifact has no file layers')
        names, total = set(), 0
        for layer in layers:
            name = layer.get('annotations', {}).get('org.opencontainers.image.title', '')
            size = layer.get('size')
            if not name or '/' in name or '\\' in name or name in ('.', '..') or name in names or layer.get('mediaType') != LAYERS[kind]:
                raise ValueError('artifact file layers are duplicate, unsafe, or not uncompressed workload files')
            if type(size) is not int or size < 0:
                raise ValueError('artifact file size is invalid')
            names.add(name)
            total += size
        if total > MAX_PAYLOAD:
            raise ValueError('artifact exceeds the frozen unpacked-content limit')
        if kind == 'schema' and names != {'schema.hcl'}:
            raise ValueError('schema artifact does not contain exactly its canonical HCL file')
        expected = {Path(f['path']).name: f for f in source_files}
        if kind == 'migration' and (len(expected) != len(source_files) or names != set(expected)):
            raise ValueError('migration artifact file inventory differs from the published source')
        config = self.blob(repository, manifest['config'])
        records = []
        for layer in layers:
            payload = self.blob(repository, layer)
            name = layer['annotations']['org.opencontainers.image.title']
            actual = {'name': name, 'bytes': len(payload), 'digest': digest(payload), 'mediaType': layer['mediaType']}
            if kind == 'migration' and (actual['bytes'] != expected[name]['bytes'] or actual['digest'] != 'sha256:' + expected[name]['sha256']):
                raise ValueError('published migration file differs from the source bytes')
            records.append(actual)
        (self.root / 'manifests' / (identity.removeprefix('sha256:') + '.json')).write_bytes(raw)
        # Ptah file layers are stored verbatim, so the stored and unpacked
        # payload sizes are equal. SQL source size is not canonical HCL size.
        return {'manifestDigest': identity, 'manifestBytes': len(raw), 'artifactType': TYPES[kind],
                'configBytes': len(config), 'compression': 'none', 'storedPayloadBytes': total,
                'unpackedPayloadBytes': total, 'files': records}
