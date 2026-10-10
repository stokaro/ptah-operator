#!/usr/bin/env python3
"""Run the declared 36-cell recovery matrix, one cell at a time.

Each cell is one engine, one resource family, one loss type and one timing, run
by the procedure 0.2.0-recovery.md names for it. Database-only loss at idle or
during Apply keeps the operator, so those eight cells share one bootstrapped
lab. Every other cell destroys or replaces its source, so it gets a lab of its
own. Every cell installs the selected release assets before its workload.

A failed cell is recorded and the run continues: one failure must not hide the
cells behind it. The summary keeps each cell's exit status, the procedure's own
verdict and the hashes of the files it retained.
"""

import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[3]
PROBES = ROOT / 'support/qualification/probes'
ENGINES = ('postgresql', 'mysql')
FAMILIES = ('schema', 'migration')
LOSSES = ('database', 'operator', 'combined')
TIMINGS = ('idle', 'during-apply', 'operator-lag')


def cells():
    for loss in LOSSES:
        for timing in TIMINGS:
            for engine in ENGINES:
                for family in FAMILIES:
                    yield {'engine': engine, 'family': family, 'loss': loss, 'timing': timing}


def cell_name(cell):
    return f"{cell['engine']}-{cell['family']}-{cell['loss']}-{cell['timing']}"


def shares_lab(cell):
    """The operator survives database-only loss at idle or during Apply."""
    return cell['loss'] == 'database' and cell['timing'] in ('idle', 'during-apply')


def command(cell, environment, output, release):
    if shares_lab(cell):
        argv = [sys.executable, str(PROBES / 'operator_restore.py'), cell['engine'], cell['family'],
                str(environment), str(output), '--loss', 'database', '--timing', cell['timing']]
    else:
        argv = [sys.executable, str(PROBES / 'cluster_restore.py'), cell['engine'], cell['family'],
                str(environment), str(output), '--loss', cell['loss'], '--timing', cell['timing'], '--result-delivery']
    argv += ['--release-assets', str(release['assets'])]
    if release.get('preparedManifestSHA256'):
        argv += ['--prepared-manifest-sha256', release['preparedManifestSHA256']]
    return argv


def digest_tree(directory):
    files = {}
    for path in sorted(Path(directory).rglob('*')):
        if path.is_file() and not path.is_symlink():
            files[path.relative_to(directory).as_posix()] = hashlib.sha256(path.read_bytes()).hexdigest()
    return files


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


class Matrix:
    def __init__(self, args):
        selected = set(args.cell or [])
        unknown = selected - {cell_name(c) for c in cells()}
        if unknown:
            raise ValueError(f'unknown cells: {sorted(unknown)}')
        self.out = Path(args.out).resolve()
        self.out.mkdir(mode=0o700, parents=True, exist_ok=False)
        self.context = args.context
        self.kubernetes = args.kubernetes
        self.release = {'assets': str(Path(args.release_assets).resolve())}
        if args.prepared_manifest_sha256:
            self.release['preparedManifestSHA256'] = args.prepared_manifest_sha256
        self.cells = [c for c in cells() if not selected or cell_name(c) in selected]
        self.summary = {'schemaVersion': 1, 'startedAt': now(), 'dockerContext': args.context,
                        'kubernetes': args.kubernetes, 'release': self.release, 'cells': []}
        self.shared = None

    def save(self):
        (self.out / 'matrix.json').write_text(json.dumps(self.summary, indent=2) + '\n')

    def bootstrap(self, name):
        directory = self.out / 'labs' / name
        directory.mkdir(mode=0o700, parents=True)
        environment = directory / 'environment'
        env = dict(os.environ, DOCKER_CONTEXT=self.context, K8S_VERSION=self.kubernetes,
                   E2E_RUN_ID=f'restore-{secrets.token_hex(3)}', E2E_STOP_AFTER='bootstrap',
                   E2E_ENVIRONMENT_FILE=str(environment), E2E_TIMING_LEDGER=str(directory / 'timings.jsonl'),
                   E2E_TIMING_CONTEXT=str(directory / 'context.json'))
        with (directory / 'bootstrap.log').open('w') as log:
            result = subprocess.run(['make', 'e2e'], cwd=ROOT, env=env, stdout=log, stderr=subprocess.STDOUT)
        if result.returncode or not environment.is_file():
            raise RuntimeError(f'bootstrap {name} failed; see {directory / "bootstrap.log"}')
        return environment

    def teardown(self, environment):
        if environment is None:
            return True
        with (environment.parent / 'teardown.log').open('w') as log:
            result = subprocess.run([str(ROOT / 'demo/bin/lab'), 'down'], cwd=ROOT,
                                    env=dict(os.environ, LAB_ENVIRONMENT=str(environment), DOCKER_CONTEXT=self.context),
                                    stdout=log, stderr=subprocess.STDOUT)
        return result.returncode == 0

    def run_cell(self, cell):
        name = cell_name(cell)
        record = dict(cell, name=name, startedAt=now())
        environment, own = None, False
        try:
            if shares_lab(cell):
                if self.shared is None:
                    self.shared = self.bootstrap('shared-database-loss')
                environment = self.shared
            else:
                environment, own = self.bootstrap(name), True
            output = self.out / 'cells' / name
            output.parent.mkdir(mode=0o700, exist_ok=True)
            argv = command(cell, environment, output, self.release)
            record['command'] = argv
            with (self.out / 'cells' / f'{name}.log').open('w') as log:
                result = subprocess.run(argv, cwd=ROOT, env=dict(os.environ, DOCKER_CONTEXT=self.context),
                                        stdout=subprocess.PIPE, stderr=log, text=True)
                log.write(result.stdout)
            lines = [line for line in result.stdout.splitlines() if line.startswith('{')]
            record['exitCode'] = result.returncode
            record['verdict'] = json.loads(lines[-1]) if lines else None
            record['status'] = 'PASS' if result.returncode == 0 and record['verdict'] and record['verdict'].get('status') == 'PASS' else 'FAIL'
            if output.is_dir():
                record['retainedFiles'] = digest_tree(output)
        except Exception as error:  # noqa: BLE001 - recorded, and the matrix continues.
            record.update(status='FAIL', error=str(error))
        finally:
            if own:
                record['teardownSucceeded'] = self.teardown(environment)
            record['finishedAt'] = now()
            self.summary['cells'].append(record)
            self.save()
        return record['status'] == 'PASS'

    def run(self):
        passed = 0
        try:
            for cell in self.cells:
                passed += self.run_cell(cell)
        finally:
            if self.shared is not None:
                self.summary['sharedTeardownSucceeded'] = self.teardown(self.shared)
            self.summary.update(finishedAt=now(), passed=passed, total=len(self.cells))
            self.save()
        return passed == len(self.cells)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--context', required=True, help='Docker context the labs and recovery clusters run on')
    parser.add_argument('--kubernetes', required=True, help='Kubernetes version, such as 1.37.0')
    parser.add_argument('--out', required=True, help='New private output directory')
    parser.add_argument('--release-assets', required=True, help='Complete downloaded release asset set')
    parser.add_argument('--prepared-manifest-sha256', help='Select a signed draft by its manifest digest')
    parser.add_argument('--cell', action='append', help='Run only this cell, such as mysql-migration-combined-during-apply; repeatable')
    args = parser.parse_args()
    if args.context in ('default', 'orbstack'):
        parser.error('name an explicit remote Docker context')
    os.umask(0o077)
    return 0 if Matrix(args).run() else 2


if __name__ == '__main__':
    sys.exit(main())
