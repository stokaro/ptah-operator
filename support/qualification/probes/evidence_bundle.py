#!/usr/bin/env python3
"""Assemble, store and verify the 0.2.0 evidence bundles.

The profile keeps evidence in two places. The public bundle carries reviewed,
sanitized evidence: identities, checksums, assertions, aggregate measurements
and the fixture's expected database contents. It is published as assets of an
immutable GitHub release, so nobody can replace it, and it counts as available
only after an anonymous download matches its checksums. The restricted bundle
carries backups, kubeconfigs, keys and unsanitized logs. It is encrypted with
age before it leaves this machine and kept in a 0700 directory on the evidence
host. It counts as available only after an authorized download decrypts with
the retained identity and an unprivileged local user on that host is refused.

The age identity never enters a bundle, an argument or a record: records name
its path and public recipient only.
"""

import argparse
import datetime
import gzip
import hashlib
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys
import tarfile
import tempfile
import urllib.request
import zipfile

REPOSITORY = 'stokaro/ptah-operator'
SUMS = 'SHA256SUMS'
INDEX = 'index.json'

# Material the public bundle must not carry. Each pattern names what it found
# so a refusal says which file to sanitize and why.
SECRETS = {
    'private key': re.compile(rb'-----BEGIN [A-Z ]*PRIVATE KEY-----'),
    'age identity': re.compile(rb'AGE-SECRET-KEY-1[0-9A-Z]{20,}'),
    'kubeconfig credential': re.compile(rb'"?(client-key-data|client-certificate-data)"?\s*:\s*"?[A-Za-z0-9+/=]{16,}'),
    'GitHub token': re.compile(rb'(gh[opsu]_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{40,})'),
    'bearer token': re.compile(rb'eyJhbGciOi[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}'),
    'database URL with a password': re.compile(rb'(postgres(ql)?|mysql)://[^:/\s@"]+:[^@\s/"]+@'),
}
SECRET_NAMES = re.compile(r'(^|/)(kubeconfig[^/]*|[^/]*\.key|id_(rsa|ed25519|ecdsa)[^/]*|[^/]*\.age|[^/]*identity[^/]*)$')


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def collect(members):
    """Map bundle paths to bytes from NAME=PATH pairs; a directory keeps its layout under NAME."""
    files = {}
    for member in members:
        name, separator, source = member.partition('=')
        if not separator or not name or not source:
            raise ValueError(f'{member!r} is not NAME=PATH')
        prefix = PurePosixPath(name)
        if prefix.is_absolute() or '..' in prefix.parts:
            raise ValueError(f'{name} leaves the bundle')
        source = Path(source)
        if source.is_symlink():
            raise ValueError(f'{source} is a symlink')
        if source.is_dir():
            paths = sorted(p for p in source.rglob('*') if not p.is_dir())
            if not paths:
                raise ValueError(f'{source} holds no files')
            for path in paths:
                if path.is_symlink() or not path.is_file():
                    raise ValueError(f'{path} is not a regular file')
                files[str(prefix / path.relative_to(source).as_posix())] = path.read_bytes()
        elif source.is_file():
            files[str(prefix)] = source.read_bytes()
        else:
            raise ValueError(f'{source} does not exist')
    if not files:
        raise ValueError('the bundle has no members')
    for reserved in (INDEX, SUMS):
        if reserved in files:
            raise ValueError(f'{reserved} is written by the bundle itself')
    return files


def nested(name, raw):
    """Yield (path, bytes) for an archive member's own contents, so a scan reaches them."""
    try:
        if name.endswith(('.tar.gz', '.tgz', '.tar')):
            with tarfile.open(fileobj=io.BytesIO(raw)) as tar:
                for member in tar.getmembers():
                    if member.isfile():
                        yield f'{name}!{member.name}', tar.extractfile(member).read()
        elif name.endswith('.zip'):
            with zipfile.ZipFile(io.BytesIO(raw)) as archive:
                for info in archive.infolist():
                    if not info.is_dir():
                        yield f'{name}!{info.filename}', archive.read(info)
        elif name.endswith('.gz'):
            yield f'{name}!', gzip.decompress(raw)
    except (tarfile.TarError, zipfile.BadZipFile, OSError, EOFError) as error:
        raise ValueError(f'{name} cannot be read for scanning: {error}') from error


def secret_findings(files):
    """Every credential-shaped name or content in the files, archives included."""
    findings, queue = [], list(files.items())
    while queue:
        name, raw = queue.pop()
        if SECRET_NAMES.search(name.rsplit('!', 1)[-1] or name):
            findings.append(f'{name}: a credential file name')
        for label, pattern in SECRETS.items():
            if pattern.search(raw):
                findings.append(f'{name}: {label}')
        queue.extend(nested(name, raw))
    return sorted(findings)


def deterministic_tar(files):
    """A gzip tar whose bytes depend only on the member paths and contents."""
    output = io.BytesIO()
    with gzip.GzipFile(fileobj=output, mode='wb', mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode='w', format=tarfile.PAX_FORMAT) as tar:
            for name in sorted(files):
                member = tarfile.TarInfo(name)
                member.size, member.mode, member.mtime = len(files[name]), 0o644, 0
                member.uid = member.gid = 0
                member.uname = member.gname = ''
                tar.addfile(member, io.BytesIO(files[name]))
    return output.getvalue()


def build(members, out, name, description):
    files = collect(members)
    findings = secret_findings(files)
    if findings:
        raise ValueError('the public bundle carries credential material:\n  ' + '\n  '.join(findings))
    index = {'schemaVersion': 1, 'bundle': name, 'description': description,
             'members': [{'path': p, 'size': len(files[p]), 'sha256': sha(files[p])} for p in sorted(files)]}
    files[INDEX] = (json.dumps(index, indent=2) + '\n').encode()
    archive = deterministic_tar(files)
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    (out / f'{name}.tar.gz').write_bytes(archive)
    (out / f'{name}.index.json').write_bytes(files[INDEX])
    sums = f'{sha(archive)}  {name}.tar.gz\n{sha(files[INDEX])}  {name}.index.json\n'
    (out / SUMS).write_text(sums)
    return {'archive': f'{name}.tar.gz', 'sha256': sha(archive), 'members': len(index['members'])}


def read_sums(text):
    sums = {}
    for line in text.splitlines():
        digest, separator, name = line.partition('  ')
        if not separator or not re.fullmatch(r'[0-9a-f]{64}', digest) or not name or '/' in name:
            raise ValueError(f'malformed checksum line {line!r}')
        if name in sums:
            raise ValueError(f'{name} is listed twice')
        sums[name] = digest
    if not sums:
        raise ValueError('the checksum file lists nothing')
    return sums


def verify_archive(raw, index_raw):
    """Check an archive against the index it carries and the index published beside it."""
    with tarfile.open(fileobj=io.BytesIO(raw)) as tar:
        members = {m.name: tar.extractfile(m).read() for m in tar.getmembers() if m.isfile()}
    if members.get(INDEX) != index_raw:
        raise ValueError('the archive index differs from the published index')
    index = json.loads(index_raw)
    listed = {m['path']: m['sha256'] for m in index['members']}
    actual = {p: sha(r) for p, r in members.items() if p != INDEX}
    if not listed:
        raise ValueError('the index lists no members')
    if listed != actual:
        missing, extra = sorted(set(listed) - set(actual)), sorted(set(actual) - set(listed))
        changed = sorted(p for p in set(listed) & set(actual) if listed[p] != actual[p])
        raise ValueError(f'the archive disagrees with its index: missing {missing}, extra {extra}, changed {changed}')
    return len(listed)


def verify_directory(directory):
    sums = read_sums((directory / SUMS).read_text())
    for name, digest in sums.items():
        if sha((directory / name).read_bytes()) != digest:
            raise ValueError(f'{name} does not match {SUMS}')
    archives = [n for n in sums if n.endswith('.tar.gz')]
    if len(archives) != 1:
        raise ValueError('the checksums name more or fewer than one archive')
    index = archives[0][:-len('.tar.gz')] + '.index.json'
    if index not in sums:
        raise ValueError(f'the checksums do not name {index}')
    return verify_archive((directory / archives[0]).read_bytes(), (directory / index).read_bytes())


def gh(*argv, **kwargs):
    return subprocess.run(['gh', *argv], check=True, capture_output=True, text=True, **kwargs).stdout


def anonymous(url, timeout=120):
    request = urllib.request.Request(url, headers={'User-Agent': 'ptah-evidence-readback'})
    with urllib.request.urlopen(request, timeout=timeout) as response:
        return response.read()


def publish(directory, tag, target, title, notes):
    """Publish the bundle as an immutable release that is never marked latest, then read it back anonymously."""
    verify_directory(directory)
    sums = read_sums((directory / SUMS).read_text())
    assets = [str(directory / n) for n in sorted(sums)] + [str(directory / SUMS)]
    gh('release', 'create', tag, '--repo', REPOSITORY, '--target', target, '--draft', '--latest=false',
       '--title', title, '--notes-file', str(notes), *assets)
    gh('release', 'edit', tag, '--repo', REPOSITORY, '--draft=false', '--latest=false')
    return readback(tag, directory)


def readback(tag, directory):
    release = json.loads(anonymous(f'https://api.github.com/repos/{REPOSITORY}/releases/tags/{tag}'))
    if release.get('draft') or not release.get('immutable'):
        raise ValueError(f'release {tag} is not a published immutable release')
    sums = read_sums((directory / SUMS).read_text())
    published = {a['name']: a['browser_download_url'] for a in release['assets']}
    if set(published) != set(sums) | {SUMS}:
        raise ValueError(f'release {tag} assets {sorted(published)} are not the bundle')
    downloaded = read_sums(anonymous(published[SUMS]).decode())
    if downloaded != sums:
        raise ValueError(f'the published {SUMS} differs from the local one')
    with tempfile.TemporaryDirectory() as scratch:
        copy = Path(scratch)
        (copy / SUMS).write_text((directory / SUMS).read_text())
        for name in sums:
            (copy / name).write_bytes(anonymous(published[name]))
        count = verify_directory(copy)
    return {'tag': tag, 'releaseId': release['id'], 'url': release['html_url'], 'immutable': True,
            'publishedAt': release['published_at'], 'target': release['target_commitish'],
            'assets': {n: sums.get(n) for n in sorted(published)}, 'membersVerified': count, 'readBackAt': now()}


def ssh(host, command, check=True):
    return subprocess.run(['ssh', '-o', 'BatchMode=yes', host, command], capture_output=True, text=True, check=check)


def store_restricted(inputs, host, remote, identity, label):
    """Encrypt the inputs, store them on the evidence host, and prove each access rule there."""
    if not re.fullmatch(r'/[A-Za-z0-9._/-]+', remote) or '..' in remote:
        raise ValueError('the remote directory must be a plain absolute path')
    if not re.fullmatch(r'[A-Za-z0-9._-]+', label):
        raise ValueError('the label must be a plain file name')
    identity = Path(identity).expanduser()
    if not identity.exists():
        identity.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        subprocess.run(['age-keygen', '-o', str(identity)], check=True, capture_output=True)
    if identity.stat().st_mode & 0o077:
        raise ValueError(f'{identity} is readable beyond its owner')
    recipient = subprocess.run(['age-keygen', '-y', str(identity)], check=True, capture_output=True, text=True).stdout.strip()
    plaintext = deterministic_tar(collect(inputs))
    ciphertext = subprocess.run(['age', '-r', recipient], input=plaintext, check=True, capture_output=True).stdout
    name = f'{label}.tar.gz.age'
    record = {'schemaVersion': 1, 'host': host, 'directory': remote, 'file': name, 'recipient': recipient,
              'identityPath': str(identity), 'plaintextSHA256': sha(plaintext), 'ciphertextSHA256': sha(ciphertext),
              'storedAt': now()}
    ssh(host, f'umask 077 && mkdir -p {remote} && chmod 0700 {remote} && test ! -e {remote}/{name}')
    with tempfile.TemporaryDirectory() as scratch:
        local = Path(scratch) / name
        local.write_bytes(ciphertext)
        subprocess.run(['scp', '-q', '-o', 'BatchMode=yes', str(local), f'{host}:{remote}/{name}'], check=True)
    ssh(host, f'chmod 0400 {remote}/{name}')
    record['remoteModes'] = ssh(host, f'stat -c "%a %U %n" {remote} {remote}/{name}').stdout.split('\n')[:2]
    record['remoteSHA256'] = ssh(host, f'sha256sum {remote}/{name}').stdout.split()[0]
    if record['remoteSHA256'] != record['ciphertextSHA256']:
        raise ValueError('the stored ciphertext differs from what was sent')
    record.update(verify_restricted(host, remote, name, identity, record['plaintextSHA256'], record['ciphertextSHA256']))
    return record


def verify_restricted(host, remote, name, identity, plaintext_sha, ciphertext_sha):
    """Authorized download and decryption succeed; an unprivileged local user is refused."""
    with tempfile.TemporaryDirectory() as scratch:
        local = Path(scratch) / name
        subprocess.run(['scp', '-q', '-o', 'BatchMode=yes', f'{host}:{remote}/{name}', str(local)], check=True)
        downloaded = local.read_bytes()
    if sha(downloaded) != ciphertext_sha:
        raise ValueError('the authorized download differs from the stored ciphertext')
    decrypted = subprocess.run(['age', '-d', '-i', str(identity)], input=downloaded, capture_output=True, check=True).stdout
    if sha(decrypted) != plaintext_sha:
        raise ValueError('the decrypted bundle differs from what was encrypted')
    refusals = {}
    for what, command in (('read', f'cat {remote}/{name}'), ('list', f'ls {remote}')):
        result = ssh(host, f'sudo -n -u nobody {command} >/dev/null', check=False)
        if result.returncode == 0 or 'Permission denied' not in result.stderr:
            raise ValueError(f'an unprivileged user was not refused the {what}: {result.returncode} {result.stderr.strip()}')
        refusals[what] = result.stderr.strip()
    return {'authorizedDownloadSHA256': sha(downloaded), 'decryptedSHA256': sha(decrypted),
            'unauthorizedRefusals': refusals, 'verifiedAt': now()}


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    commands = parser.add_subparsers(dest='command', required=True)
    build_parser = commands.add_parser('build', help='Assemble a sanitized public bundle')
    build_parser.add_argument('--out', required=True, help='New output directory')
    build_parser.add_argument('--name', required=True, help='Archive base name, such as ptah-operator-0.2.0-evidence')
    build_parser.add_argument('--description', required=True)
    build_parser.add_argument('--member', action='append', required=True, help='NAME=PATH; a directory keeps its layout under NAME')
    verify_parser = commands.add_parser('verify', help='Check a bundle directory against its checksums and index')
    verify_parser.add_argument('--dir', required=True)
    publish_parser = commands.add_parser('publish', help='Publish a built bundle as an immutable release and read it back')
    publish_parser.add_argument('--dir', required=True)
    publish_parser.add_argument('--tag', required=True)
    publish_parser.add_argument('--target', required=True, help='Commit the release tag names')
    publish_parser.add_argument('--title', required=True)
    publish_parser.add_argument('--notes', required=True, help='Release notes file')
    readback_parser = commands.add_parser('readback', help='Read a published bundle back anonymously')
    readback_parser.add_argument('--dir', required=True)
    readback_parser.add_argument('--tag', required=True)
    store_parser = commands.add_parser('store-restricted', help='Encrypt restricted evidence and store it on the evidence host')
    store_parser.add_argument('--input', action='append', required=True, help='NAME=PATH')
    store_parser.add_argument('--host', required=True)
    store_parser.add_argument('--remote-dir', required=True)
    store_parser.add_argument('--identity', required=True, help='age identity file; created with mode 0600 when absent')
    store_parser.add_argument('--label', required=True)
    store_parser.add_argument('--record', required=True, help='Where to write the custody record')
    args = parser.parse_args()
    os.umask(0o077)
    if args.command == 'build':
        result = build(args.member, Path(args.out), args.name, args.description)
    elif args.command == 'verify':
        result = {'membersVerified': verify_directory(Path(args.dir))}
    elif args.command == 'publish':
        result = publish(Path(args.dir), args.tag, args.target, args.title, args.notes)
        (Path(args.dir) / 'publication.json').write_text(json.dumps(result, indent=2) + '\n')
    elif args.command == 'readback':
        result = readback(args.tag, Path(args.dir))
    else:
        result = store_restricted(args.input, args.host, args.remote_dir, args.identity, args.label)
        Path(args.record).write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(result, indent=2))
    return 0


if __name__ == '__main__':
    sys.exit(main())
