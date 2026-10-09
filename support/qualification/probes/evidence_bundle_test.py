import gzip
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
import zipfile

from evidence_bundle import (INDEX, SUMS, build, collect, deterministic_tar, read_sums, secret_findings,
                             verify_archive, verify_directory)


def tree(root, files):
    for name, raw in files.items():
        path = Path(root) / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(raw)


class BuildTest(unittest.TestCase):
    def test_a_bundle_is_reproducible_and_verifies(self):
        with tempfile.TemporaryDirectory() as directory:
            tree(directory, {'runs/ci.json': b'{"conclusion":"success"}', 'record.json': b'{}'})
            members = [f'ci={directory}/runs', f'acceptance/0.2.0.json={directory}/record.json']
            first = build(members, Path(directory) / 'a', 'evidence', 'test')
            second = build(members, Path(directory) / 'b', 'evidence', 'test')
            self.assertEqual(first['sha256'], second['sha256'])
            self.assertEqual(first['members'], 2)
            self.assertEqual(verify_directory(Path(directory) / 'a'), 2)

    def test_member_names_cannot_leave_the_bundle_or_take_reserved_names(self):
        with tempfile.TemporaryDirectory() as directory:
            tree(directory, {'f': b'x'})
            for member in (f'../escape={directory}/f', f'/abs={directory}/f', f'{INDEX}={directory}/f',
                           f'{SUMS}={directory}/f', 'nopath', f'x={directory}/missing'):
                with self.subTest(member), self.assertRaises(ValueError):
                    collect([member])
            with self.assertRaises(ValueError):
                collect([])

    def test_a_symlinked_member_is_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            tree(directory, {'real': b'x'})
            (Path(directory) / 'link').symlink_to(Path(directory) / 'real')
            with self.assertRaises(ValueError):
                collect([f'x={directory}/link'])


class SanitizationTest(unittest.TestCase):
    def test_clean_evidence_passes(self):
        self.assertEqual(secret_findings({'report.json': b'{"token": "redacted", "tokenAudience": "ptah"}',
                                          'log.txt': b'connected to postgres://capacity-postgres:5432/app'}), [])

    def test_each_kind_of_credential_is_named(self):
        cases = {
            'private key': b'-----BEGIN EC PRIVATE KEY-----\nabc',
            'age identity': b'AGE-SECRET-KEY-1' + b'Q' * 58,
            'kubeconfig credential': b'    client-key-data: ' + b'A' * 40,
            'GitHub token': b'ghp_' + b'a' * 36,
            'bearer token': b'Authorization: Bearer eyJhbGciOiJSUzI1NiIs.' + b'b' * 20 + b'.' + b'c' * 20,
            'database URL with a password': b'mysql://root:hunter2@capacity-mysql:3306/app',
        }
        for label, raw in cases.items():
            with self.subTest(label):
                self.assertEqual(secret_findings({'evidence.log': raw}), [f'evidence.log: {label}'])

    def test_credential_file_names_are_refused(self):
        for name in ('lab/kubeconfig', 'keys/server.key', 'backup.tar.gz.age', 'restore-identity.txt', 'id_ed25519'):
            with self.subTest(name):
                self.assertEqual(secret_findings({name: b'harmless'}), [f'{name}: a credential file name'])

    def test_credentials_inside_archives_are_found(self):
        inner = io.BytesIO()
        with zipfile.ZipFile(inner, 'w', compression=zipfile.ZIP_DEFLATED) as archive:
            archive.writestr('job/log.txt', 'ghp_' + 'z' * 36)
        tarred = deterministic_tar({'nested/kubeconfig': b'apiVersion: v1'})
        compressed = gzip.compress(b'-----BEGIN RSA PRIVATE KEY-----')
        findings = secret_findings({'logs.zip': inner.getvalue(), 'timing.tar.gz': tarred, 'raw.gz': compressed})
        self.assertEqual(findings, ['logs.zip!job/log.txt: GitHub token', 'raw.gz!: private key',
                                    'timing.tar.gz!nested/kubeconfig: a credential file name'])

    def test_build_refuses_a_bundle_carrying_a_credential(self):
        with tempfile.TemporaryDirectory() as directory:
            tree(directory, {'lab/kubeconfig': b'x'})
            with self.assertRaises(ValueError) as refusal:
                build([f'lab={directory}/lab'], Path(directory) / 'out', 'evidence', 'test')
            self.assertIn('lab/kubeconfig', str(refusal.exception))
            self.assertFalse((Path(directory) / 'out').exists())

    def test_an_unreadable_archive_is_refused_rather_than_skipped(self):
        with self.assertRaises(ValueError):
            secret_findings({'broken.tar.gz': b'not a tar'})


class VerificationTest(unittest.TestCase):
    def bundle(self, directory):
        tree(directory, {'src/a.json': b'{}', 'src/b.log': b'ok'})
        build([f'x={directory}/src'], Path(directory) / 'out', 'evidence', 'test')
        return Path(directory) / 'out'

    def test_a_changed_asset_fails_its_checksum(self):
        with tempfile.TemporaryDirectory() as directory:
            out = self.bundle(directory)
            (out / 'evidence.index.json').write_text('{}')
            with self.assertRaises(ValueError):
                verify_directory(out)

    def test_an_archive_that_disagrees_with_its_index_is_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            out = self.bundle(directory)
            index = (out / 'evidence.index.json').read_bytes()
            listed = json.loads(index)
            for name, members in {
                'a changed member': {'x/a.json': b'{"changed":1}', 'x/b.log': b'ok'},
                'a missing member': {'x/a.json': b'{}'},
                'an extra member': {'x/a.json': b'{}', 'x/b.log': b'ok', 'x/c': b''},
            }.items():
                with self.subTest(name), self.assertRaises(ValueError):
                    verify_archive(deterministic_tar(dict(members, **{INDEX: index})), index)
            with self.assertRaises(ValueError):
                verify_archive((out / 'evidence.tar.gz').read_bytes(), index.replace(b'"test"', b'"other"'))
            listed['members'] = []
            empty = json.dumps(listed).encode()
            with self.assertRaises(ValueError):
                verify_archive(deterministic_tar({INDEX: empty}), empty)

    def test_checksum_files_are_read_strictly(self):
        good = 'a' * 64 + '  evidence.tar.gz\n'
        self.assertEqual(read_sums(good), {'evidence.tar.gz': 'a' * 64})
        for bad in ('', 'a' * 63 + '  x\n', good + good, 'a' * 64 + '  ../x\n', 'a' * 64 + ' x\n'):
            with self.subTest(bad), self.assertRaises(ValueError):
                read_sums(bad)

    def test_the_archive_is_owner_neutral_and_dated_at_the_epoch(self):
        with tarfile.open(fileobj=io.BytesIO(deterministic_tar({'b': b'2', 'a': b'1'}))) as tar:
            members = tar.getmembers()
        self.assertEqual([m.name for m in members], ['a', 'b'])
        self.assertTrue(all(m.mtime == 0 and m.uid == 0 and m.uname == '' for m in members))


if __name__ == '__main__':
    unittest.main()
