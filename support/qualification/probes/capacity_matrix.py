#!/usr/bin/env python3
"""Run the frozen capacity matrix for one host, one lab per measurement.

The profile measures each architecture, engine and supported Kubernetes minor
separately, and each of those cells runs the soak, the within-fleet approval
backlog and the overload probe. One host is one architecture, so this runs the
other two dimensions on it. Every measurement gets a fresh frozen-profile lab
from profile_lab.py and removes it afterwards; a failed measurement is recorded
and the matrix continues, so one failure cannot hide the cells behind it.
"""

import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[3]
PROBES = ROOT / 'support/qualification/probes'
WORKLOADS = {('PostgreSQL', 'soak'): 'soak.json', ('MySQL', 'soak'): 'soak-mysql.json',
             ('PostgreSQL', 'backlog'): 'backlog.json', ('MySQL', 'backlog'): 'backlog-mysql.json',
             ('PostgreSQL', 'overload'): 'overload.json', ('MySQL', 'overload'): 'overload-mysql.json'}
STEPS = ('bootstrap', 'install', 'kindnet', 'fixtures', 'prepare', 'network', 'measure')


def supported_versions():
    matrix = json.loads(subprocess.check_output(['go', 'run', './hack/verify-kubernetes-support.go', '-output=matrix'], cwd=ROOT))
    return [row['kubernetes_version'] for row in matrix]


def cells(versions, engines=('PostgreSQL', 'MySQL'), kinds=('soak', 'backlog', 'overload')):
    for version in versions:
        for engine in engines:
            for kind in kinds:
                yield {'kubernetes': version, 'engine': engine, 'workload': kind,
                       'file': f'support/capacity/{WORKLOADS[(engine, kind)]}'}


def cell_name(cell):
    return f"{cell['kubernetes']}-{cell['engine'].lower()}-{cell['workload']}"


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def step_argv(step, lab, cell, context, manifest):
    argv = [sys.executable, str(PROBES / 'profile_lab.py'), step, '--lab', str(lab)]
    if step == 'bootstrap':
        argv += ['--context', context, '--kubernetes', cell['kubernetes'], '--release-manifest', str(manifest)]
    elif step == 'prepare':
        argv += ['--workload', str(ROOT / cell['file'])]
    return argv


def run_cell(cell, out, context, manifest):
    name = cell_name(cell)
    lab = out / 'labs' / name
    record = dict(cell, name=name, lab=str(lab), startedAt=now(), steps={})
    bootstrapped = False
    try:
        for step in STEPS:
            with (out / 'logs' / f'{name}-{step}.log').open('w') as log:
                result = subprocess.run(step_argv(step, lab, cell, context, manifest), cwd=ROOT, stdout=log, stderr=subprocess.STDOUT)
            record['steps'][step] = result.returncode
            if step == 'bootstrap' and (lab / 'environment').is_file():
                bootstrapped = True
            if result.returncode:
                record['status'] = 'FAIL'
                record['failedStep'] = step
                break
        else:
            record['status'] = 'PASS'
            evidence = lab / 'measurement.tar.gz'
            record['evidence'] = str(evidence)
            record['evidenceSHA256'] = hashlib.sha256(evidence.read_bytes()).hexdigest()
    finally:
        if bootstrapped:
            with (out / 'logs' / f'{name}-down.log').open('w') as log:
                down = subprocess.run([sys.executable, str(PROBES / 'profile_lab.py'), 'down', '--lab', str(lab)],
                                      cwd=ROOT, stdout=log, stderr=subprocess.STDOUT)
            record['teardownSucceeded'] = down.returncode == 0
        record['finishedAt'] = now()
    return record


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--context', required=True, help='Docker context of the host, one architecture')
    parser.add_argument('--release-manifest', required=True, help='release-manifest.txt of the prepared release')
    parser.add_argument('--out', required=True, help='New private output directory')
    parser.add_argument('--kubernetes', action='append', help='Restrict to these versions; default every supported minor')
    parser.add_argument('--engine', action='append', choices=('PostgreSQL', 'MySQL'))
    parser.add_argument('--workload', action='append', choices=('soak', 'backlog', 'overload'))
    args = parser.parse_args()
    if args.context in ('default', 'orbstack'):
        parser.error('name an explicit remote Docker context')
    os.umask(0o077)
    out = Path(args.out).resolve()
    (out / 'logs').mkdir(mode=0o700, parents=True, exist_ok=False)
    manifest = Path(args.release_manifest).resolve()
    selected = list(cells(args.kubernetes or supported_versions(), tuple(args.engine or ('PostgreSQL', 'MySQL')),
                          tuple(args.workload or ('soak', 'backlog', 'overload'))))
    summary = {'schemaVersion': 1, 'context': args.context, 'releaseManifestSHA256': hashlib.sha256(manifest.read_bytes()).hexdigest(),
               'harnessCommit': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip(),
               'startedAt': now(), 'cells': []}
    for cell in selected:
        summary['cells'].append(run_cell(cell, out, args.context, manifest))
        (out / 'matrix.json').write_text(json.dumps(summary, indent=2) + '\n')
    summary.update(finishedAt=now(), passed=sum(c['status'] == 'PASS' for c in summary['cells']), total=len(selected))
    (out / 'matrix.json').write_text(json.dumps(summary, indent=2) + '\n')
    return 0 if summary['passed'] == summary['total'] else 2


if __name__ == '__main__':
    sys.exit(main())
