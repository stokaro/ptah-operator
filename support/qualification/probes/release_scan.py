#!/usr/bin/env python3
"""Report the known vulnerabilities in the bytes a release ships.

Report only: the run lists what the scanners find and decides nothing. It reads
a release manifest and scans each platform image of the operator and of the
Ptah executor the manifest names with Trivy, every binary inside them and the
client binaries released beside the manifest with govulncheck in binary mode,
and the operator source in source mode. The source scan runs inside the
operator Dockerfile's own builder stage, built from the release source, so it
loads the Go and the modules that compiled the binaries. Raw scanner output is
kept beside a summary.

A stripped binary carries no symbol table, so binary mode reports what a binary
contains, not what it reaches; the source scan is the one that reads
reachability. The executor is Ptah's own release image, so its source is Ptah's
to scan.
"""

import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tarfile

ROOT = Path(__file__).resolve().parents[3]
PLATFORMS = ('linux/amd64', 'linux/arm64')
BINARIES = {'image': ['manager', 'ptah-runner', 'ptah-cert-rotator', 'ptah-crd-manager'],
            'executor': ['usr/local/bin/ptah']}
TRIVY = 'aquasec/trivy:0.74.0'
GOVULNCHECK = 'golang.org/x/vuln/cmd/govulncheck@v1.8.0'


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def manifest_fields(path):
    fields = {}
    for line in Path(path).read_text().splitlines():
        key, separator, value = line.partition('=')
        if separator:
            if key in fields:
                raise ValueError(f'the manifest repeats {key}')
            fields[key] = value
    for key in ('source-sha', 'image', 'executor'):
        if not fields.get(key):
            raise ValueError(f'the manifest names no {key}')
    for key in ('image', 'executor'):
        if not re.fullmatch(r'[^@\s]+@sha256:[0-9a-f]{64}', fields[key]):
            raise ValueError(f'the manifest {key} is not pinned by digest')
    return fields


def client_assets(fields, directory):
    """The client binaries a manifest names, read from the directory it was downloaded to."""
    paths = []
    for name in [name for name in fields.get('client-assets', '').split(',') if name]:
        if Path(name).name != name:
            raise ValueError(f'the manifest names the client asset {name!r} with a path')
        path = Path(directory) / name
        if not path.is_file():
            raise ValueError(f'the client asset {name} is not beside the manifest')
        paths.append(path)
    return paths


def govulncheck_findings(lines):
    """OSV IDs a govulncheck JSON stream reports, split by how far the trace reaches.

    A finding whose trace reaches a function is reachable; one that names only a
    package or a module is not, and is kept apart rather than dropped.
    """
    reached, imported = set(), set()
    for message in lines:
        finding = message.get('finding')
        if not finding:
            continue
        trace = finding.get('trace') or [{}]
        if trace[0].get('function'):
            reached.add(finding['osv'])
        else:
            imported.add(finding['osv'])
    return sorted(reached), sorted(imported - reached)


def decode_stream(raw):
    decoder, items, text = json.JSONDecoder(), [], raw.decode()
    while text.strip():
        text = text.lstrip()
        item, end = decoder.raw_decode(text)
        items.append(item)
        text = text[end:]
    return items


def trivy_findings(report):
    found = set()
    for result in report.get('Results') or []:
        for vulnerability in result.get('Vulnerabilities') or []:
            found.add(vulnerability['VulnerabilityID'])
    return sorted(found)


class Scan:
    def __init__(self, args):
        self.manifest = Path(args.manifest).resolve()
        self.fields = manifest_fields(self.manifest)
        self.clients = client_assets(self.fields, self.manifest.parent)
        self.out = Path(args.out).resolve()
        self.out.mkdir(mode=0o700, parents=True, exist_ok=False)
        self.docker = ['docker', '--context', args.context]
        self.images = []
        self.summary = {'schemaVersion': 3, 'startedAt': datetime.datetime.now(datetime.timezone.utc).isoformat(),
                        'manifestSHA256': sha(self.manifest.read_bytes()), 'releaseSource': self.fields['source-sha'],
                        'targets': [], 'clients': [], 'sources': []}

    def run(self, name, argv, check=True, timeout=1800, stdin=None):
        result = subprocess.run(argv, capture_output=True, timeout=timeout, input=stdin)
        (self.out / (name + '.stdout')).write_bytes(result.stdout)
        (self.out / (name + '.stderr')).write_bytes(result.stderr)
        if check and result.returncode:
            raise RuntimeError(f'{name} exited {result.returncode}')
        return result

    def govulncheck(self, name, argv, stdin=None):
        result = self.run(name, argv, check=False, stdin=stdin)
        if result.returncode not in (0, 3):
            raise RuntimeError(f'{name} exited {result.returncode}')
        return govulncheck_findings(decode_stream(result.stdout))

    def scanners(self):
        """Trivy, and the operator's builder stage with the pinned govulncheck on top."""
        self.run('trivy-pull', self.docker + ['pull', TRIVY])
        digest = json.loads(self.run('trivy-inspect', self.docker + ['image', 'inspect', TRIVY]).stdout)[0]['RepoDigests']
        version = json.loads(self.run('trivy-version', self.docker + ['run', '--rm', TRIVY, 'version', '--format', 'json']).stdout)
        self.trivy = next(d for d in digest if d.startswith('aquasec/trivy@'))
        source = self.out / 'operator-source'
        source.mkdir(mode=0o700)
        archive = subprocess.run(['git', '-C', str(ROOT), 'archive', self.fields['source-sha']], capture_output=True, check=True).stdout
        subprocess.run(['tar', '-x', '-C', str(source)], input=archive, check=True)
        tag = f"ptah-release-scan:{self.fields['source-sha'][:12]}"
        self.run('build-builder', self.docker + ['build', '--target', 'builder', '-f', str(source / 'Dockerfile'),
                                                 '-t', tag + '-builder', str(source)], timeout=3600)
        self.images.append(tag + '-builder')
        self.run('build-scanner', self.docker + ['build', '-t', tag, '-'],
                 stdin=f'FROM {tag}-builder\nRUN go install {GOVULNCHECK}\n'.encode())
        self.images.append(tag)
        self.scanner = tag
        govulncheck = self.run('govulncheck-version', self.docker + ['run', '--rm', tag, 'govulncheck', '-version']).stdout.decode()
        self.summary['scanners'] = {'govulncheck': GOVULNCHECK, 'govulncheckVersion': govulncheck.strip().splitlines(),
                                    'trivy': self.trivy, 'trivyVersion': version}

    def binary_findings(self, name, binary):
        """Every binary-mode finding for one file, read by the pinned scanner over stdin."""
        reached, imported = self.govulncheck(name, self.docker + ['run', '--rm', '-i', self.scanner, 'sh', '-c',
                                                                  'cat > /tmp/binary && govulncheck -mode=binary -format json /tmp/binary'],
                                             stdin=binary.read_bytes())
        return sorted(set(reached) | set(imported))

    def binaries(self, role, reference, platform):
        slug = f"{role}-{platform.replace('/', '-')}"
        self.run(f'pull-{slug}', self.docker + ['pull', '--platform', platform, reference])
        container = self.run(f'create-{slug}', self.docker + ['create', '--platform', platform, reference]).stdout.decode().strip()
        try:
            archive = self.out / f'{slug}.tar'
            with archive.open('wb') as stream:
                subprocess.run(self.docker + ['export', container], stdout=stream, check=True)
        finally:
            subprocess.run(self.docker + ['rm', container], capture_output=True)
        extracted = {}
        directory = self.out / slug
        directory.mkdir(mode=0o700)
        with tarfile.open(archive) as tar:
            for path in BINARIES[role]:
                member = tar.getmember(path)
                if not member.isfile():
                    raise RuntimeError(f'{reference} {platform} has no regular file {path}')
                target = directory / Path(path).name
                target.write_bytes(tar.extractfile(member).read())
                extracted[path] = target
        archive.unlink()
        return slug, extracted

    def scan_target(self, role, reference, platform):
        slug, binaries = self.binaries(role, reference, platform)
        record = {'role': role, 'reference': reference, 'platform': platform, 'binaries': []}
        for path, binary in binaries.items():
            record['binaries'].append({'path': path, 'sha256': sha(binary.read_bytes()),
                                       'findings': self.binary_findings(f'govulncheck-{slug}-{binary.name}', binary)})
        trivy = self.run(f'trivy-{slug}', self.docker + ['run', '--rm', self.trivy, 'image', '--quiet', '--format', 'json',
                                                        '--platform', platform, reference], timeout=3600)
        report = json.loads(trivy.stdout)
        record['trivy'] = {'findings': trivy_findings(report), 'os': report.get('Metadata', {}).get('OS')}
        self.summary['targets'].append(record)

    def scan_clients(self):
        for binary in self.clients:
            self.summary['clients'].append({'asset': binary.name, 'sha256': sha(binary.read_bytes()),
                                            'findings': self.binary_findings(f'govulncheck-client-{binary.name}', binary)})

    def scan_source(self):
        """Reachability in the operator source, loaded by its builder stage for each target platform."""
        toolchain = self.run('go-version', self.docker + ['run', '--rm', self.scanner, 'go', 'env', 'GOVERSION']).stdout.decode().strip()
        for platform in PLATFORMS:
            goos, goarch = platform.split('/')
            reached, imported = self.govulncheck(
                f'govulncheck-source-{goos}-{goarch}',
                self.docker + ['run', '--rm', '-e', 'GOOS=' + goos, '-e', 'GOARCH=' + goarch, self.scanner,
                               'govulncheck', '-format', 'json', './...'])
            self.summary['sources'].append({'commit': self.fields['source-sha'], 'platform': platform, 'toolchain': toolchain,
                                            'reached': reached, 'moduleOrPackage': imported})

    def run_all(self):
        try:
            self.scanners()
            for role in ('image', 'executor'):
                for platform in PLATFORMS:
                    self.scan_target(role, self.fields[role], platform)
            self.scan_clients()
            self.scan_source()
        finally:
            for image in reversed(self.images):
                subprocess.run(self.docker + ['rmi', image], capture_output=True)
        self.summary['finishedAt'] = datetime.datetime.now(datetime.timezone.utc).isoformat()
        (self.out / 'summary.json').write_text(json.dumps(self.summary, indent=2) + '\n')
        print(json.dumps({
            'reachedInSource': sorted({osv for source in self.summary['sources'] for osv in source['reached']}),
            'inBinaries': sorted({osv for target in self.summary['targets'] for b in target['binaries'] for osv in b['findings']} |
                                 {osv for client in self.summary['clients'] for osv in client['findings']}),
            'inImages': sorted({osv for target in self.summary['targets'] for osv in target['trivy']['findings']}),
        }))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--manifest', required=True, help='release-manifest.txt, with the client assets beside it')
    parser.add_argument('--out', required=True)
    parser.add_argument('--context', required=True, help='Docker context that builds the scanner, pulls the images and runs Trivy')
    args = parser.parse_args()
    if args.context in ('default', 'orbstack'):
        parser.error('name an explicit remote Docker context')
    os.umask(0o077)
    Scan(args).run_all()
    return 0


if __name__ == '__main__':
    sys.exit(main())
