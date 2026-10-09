#!/usr/bin/env python3
"""Scan the bytes a release ships, on every platform it ships them for.

#583 asks for every shipped binary and image to be scanned with the release
toolchain and a current vulnerability database, and for reachable findings to
be separated from module-only ones. Source CI scans the source; this scans what
was built from it: each platform image of the operator and the executor named
in a release manifest, the binaries inside them, and the two sources they came
from for reachability. Raw scanner output is kept beside a summary that names
each finding with the targets it appears in.

A stripped binary carries no symbol table, so govulncheck reports every
vulnerable package its modules contain. The source scan of the same build is
what says whether a vulnerable symbol is reached; the summary keeps both.
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
    for key in ('source-sha', 'image', 'executor', 'executor-ptah-commit'):
        if not fields.get(key):
            raise ValueError(f'the manifest names no {key}')
    for key in ('image', 'executor'):
        if not re.fullmatch(r'[^@\s]+@sha256:[0-9a-f]{64}', fields[key]):
            raise ValueError(f'the manifest {key} is not pinned by digest')
    return fields


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
        self.fields = manifest_fields(args.manifest)
        self.out = Path(args.out).resolve()
        self.out.mkdir(mode=0o700, parents=True, exist_ok=False)
        self.context = args.context
        self.docker = ['docker', '--context', args.context]
        self.summary = {'schemaVersion': 1, 'startedAt': datetime.datetime.now(datetime.timezone.utc).isoformat(),
                        'manifestSHA256': sha(Path(args.manifest).read_bytes()), 'releaseSource': self.fields['source-sha'],
                        'targets': [], 'sources': []}

    def run(self, name, argv, env=None, check=True, timeout=1800, cwd=None):
        result = subprocess.run(argv, capture_output=True, env=env, timeout=timeout, cwd=cwd)
        (self.out / (name + '.stdout')).write_bytes(result.stdout)
        (self.out / (name + '.stderr')).write_bytes(result.stderr)
        if check and result.returncode:
            raise RuntimeError(f'{name} exited {result.returncode}')
        return result

    def scanners(self):
        govulncheck = self.run('govulncheck-version', ['govulncheck', '-version']).stdout.decode()
        self.run('trivy-pull', self.docker + ['pull', TRIVY])
        digest = json.loads(self.run('trivy-inspect', self.docker + ['image', 'inspect', TRIVY]).stdout)[0]['RepoDigests']
        version = json.loads(self.run('trivy-version', self.docker + ['run', '--rm', TRIVY, 'version', '--format', 'json']).stdout)
        self.trivy = next(d for d in digest if d.startswith('aquasec/trivy@'))
        self.summary['scanners'] = {'govulncheck': govulncheck.strip().splitlines(), 'trivy': self.trivy, 'trivyVersion': version}

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
                raw = tar.extractfile(member).read()
                target = directory / Path(path).name
                target.write_bytes(raw)
                extracted[path] = target
        archive.unlink()
        return slug, extracted

    def scan_target(self, role, reference, platform):
        slug, binaries = self.binaries(role, reference, platform)
        record = {'role': role, 'reference': reference, 'platform': platform, 'binaries': []}
        for path, binary in binaries.items():
            name = f'govulncheck-{slug}-{binary.name}'
            result = self.run(name, ['govulncheck', '-mode=binary', '-format', 'json', str(binary)], check=False)
            if result.returncode not in (0, 3):
                raise RuntimeError(f'{name} exited {result.returncode}')
            reached, imported = govulncheck_findings(decode_stream(result.stdout))
            record['binaries'].append({'path': path, 'sha256': sha(binary.read_bytes()), 'reached': reached, 'moduleOrPackage': imported})
        trivy = self.run(f'trivy-{slug}', self.docker + ['run', '--rm', self.trivy, 'image', '--quiet', '--format', 'json',
                                                        '--platform', platform, reference], timeout=3600)
        report = json.loads(trivy.stdout)
        record['trivy'] = {'findings': trivy_findings(report), 'os': report.get('Metadata', {}).get('OS')}
        self.summary['targets'].append(record)

    def scan_sources(self):
        """Reachability in the two sources the release built, with each build's own modules."""
        checkout = self.out / 'operator-source'
        archive = subprocess.run(['git', '-C', str(ROOT), 'archive', self.fields['source-sha']], capture_output=True, check=True).stdout
        checkout.mkdir(mode=0o700)
        subprocess.run(['tar', '-x', '-C', str(checkout)], input=archive, check=True)
        result = self.run('govulncheck-operator-source', ['govulncheck', '-format', 'json', './...'], cwd=checkout, check=False)
        if result.returncode not in (0, 3):
            raise RuntimeError('the operator source scan failed')
        reached, imported = govulncheck_findings(decode_stream(result.stdout))
        self.summary['sources'].append({'role': 'operator', 'commit': self.fields['source-sha'], 'reached': reached, 'moduleOrPackage': imported})

        # The executor is Ptah at the pinned commit, built with the module
        # override Dockerfile.executor applies; scan it with the same modules.
        commit = self.fields['executor-ptah-commit']
        executor = self.out / 'executor-source'
        self.run('ptah-clone', ['git', 'clone', '--filter=blob:none', '--no-checkout', 'https://github.com/stokaro/ptah', str(executor)])
        self.run('ptah-checkout', ['git', '-C', str(executor), 'checkout', '--detach', commit])
        if subprocess.check_output(['git', '-C', str(executor), 'rev-parse', 'HEAD'], text=True).strip() != commit:
            raise RuntimeError('the Ptah checkout is not the executor commit')
        override = re.search(r'-require=(golang\.org/x/net@v[0-9.]+)', (ROOT / 'Dockerfile.executor').read_text())
        modfile = self.out / 'ptah-build.mod'
        (self.out / 'ptah-build.sum').write_bytes((executor / 'go.sum').read_bytes())
        modfile.write_bytes((executor / 'go.mod').read_bytes())
        environment = dict(os.environ, GOFLAGS='-modfile=' + str(modfile))
        if override:
            self.run('ptah-override', ['go', 'mod', 'edit', '-modfile=' + str(modfile), '-require=' + override[1]], cwd=executor)
            self.run('ptah-override-download', ['go', 'mod', 'download', '-modfile=' + str(modfile), override[1].split('@')[0]], cwd=executor)
        result = self.run('govulncheck-executor-source', ['govulncheck', '-format', 'json', './cmd/ptah'], cwd=executor, env=environment, check=False)
        if result.returncode not in (0, 3):
            raise RuntimeError('the executor source scan failed')
        reached, imported = govulncheck_findings(decode_stream(result.stdout))
        self.summary['sources'].append({'role': 'executor', 'commit': commit, 'moduleOverride': override[1] if override else None,
                                        'reached': reached, 'moduleOrPackage': imported})

    def run_all(self):
        self.scanners()
        for role in ('image', 'executor'):
            for platform in PLATFORMS:
                self.scan_target(role, self.fields[role], platform)
        self.scan_sources()
        reached = sorted({osv for target in self.summary['targets'] for b in target['binaries'] for osv in b['reached']} |
                         {osv for source in self.summary['sources'] for osv in source['reached']})
        self.summary.update(reachedFindings=reached, finishedAt=datetime.datetime.now(datetime.timezone.utc).isoformat())
        (self.out / 'summary.json').write_text(json.dumps(self.summary, indent=2) + '\n')
        print(json.dumps({'reached': reached, 'targets': len(self.summary['targets'])}))
        return not reached


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--manifest', required=True)
    parser.add_argument('--out', required=True)
    parser.add_argument('--context', required=True, help='Docker context that pulls the images and runs Trivy')
    args = parser.parse_args()
    if args.context in ('default', 'orbstack'):
        parser.error('name an explicit remote Docker context')
    os.umask(0o077)
    return 0 if Scan(args).run_all() else 2


if __name__ == '__main__':
    sys.exit(main())
