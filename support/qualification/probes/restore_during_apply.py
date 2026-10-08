"""Database-loss boundary for the existing encrypted recovery procedure.

The backup is quiescent. Loss occurs only after the approved runner has sent
the fixture DDL to the original database and is waiting on our bounded lock.
"""
import json
import re
import subprocess
import time


def terminal_apply_pods(pods, job_uids, require_success=True):
    if not job_uids or len(pods) != len(job_uids):
        return False
    seen = set()
    for pod in pods:
        metadata, spec, status = pod['metadata'], pod['spec'], pod.get('status', {})
        owners = metadata.get('ownerReferences', [])
        if (not metadata.get('uid') or len(owners) != 1 or owners[0].get('kind') != 'Job' or
                owners[0].get('controller') is not True or owners[0].get('uid') not in job_uids or
                owners[0]['uid'] in seen or status.get('phase') not in
                (('Succeeded',) if require_success else ('Succeeded', 'Failed'))):
            return False
        seen.add(owners[0]['uid'])
        if not spec.get('containers'):
            return False
        for declared, observed in (('containers', 'containerStatuses'), ('initContainers', 'initContainerStatuses')):
            names = {container['name'] for container in spec.get(declared, [])}
            states = status.get(observed, [])
            if len(states) != len(names) or {s.get('name') for s in states} != names:
                return False
            for state in states:
                terminal = state.get('state', {}).get('terminated') or {}
                if (state.get('restartCount') != 0 or not terminal.get('finishedAt') or
                        type(terminal.get('exitCode')) is not int or
                        require_success and terminal['exitCode'] != 0):
                    return False
    return seen == set(job_uids)


def running_apply(resource, job, pod):
    """Bind a live executor to the persisted claim, not just an Apply label."""
    claim = resource.get('status', {}).get('activeOperation') or {}
    jm, pm = job['metadata'], pod['metadata']
    owners = jm.get('ownerReferences', [])
    pod_owners = pm.get('ownerReferences', [])
    statuses = pod.get('status', {}).get('containerStatuses', [])
    containers = pod['spec'].get('containers', [])
    return bool(resource['metadata'].get('uid') and claim.get('id') and
        claim.get('type') == 'Apply' and claim.get('jobUID') == jm.get('uid') and
        claim.get('jobName') == jm.get('name') and jm.get('uid') and pm.get('uid') and
        jm.get('namespace') == pm.get('namespace') == resource['metadata'].get('namespace') and
        jm.get('annotations', {}).get('operator.ptah.run/operation-id') == claim['id'] and
        len(owners) == 1 and owners[0].get('uid') == resource['metadata']['uid'] and
        owners[0].get('controller') is True and len(pod_owners) == 1 and
        pod_owners[0].get('uid') == jm['uid'] and pod_owners[0].get('controller') is True and
        not jm.get('deletionTimestamp') and not pm.get('deletionTimestamp') and
        pod.get('status', {}).get('phase') == 'Running' and pod['status'].get('podIP') and
        len(containers) == len(statuses) == 1 and statuses[0].get('name') == containers[0].get('name') and
        statuses[0].get('restartCount') == 0 and statuses[0].get('state', {}).get('running'))


def fixture_waiter(row, pod_ip, holder, engine):
    """Require the actual fixture ALTER from this Pod, held at the server."""
    query = ' '.join((row.get('query') or '').replace('"', '').replace('`', '').strip().rstrip(';').split())
    if (type(row.get('session')) is not int or row['session'] <= 0 or row['session'] == holder or
            not pod_ip or row.get('client') != pod_ip or ';' in query or
            not re.fullmatch(r'ALTER TABLE (?:public\.)?recovery_canary ADD (?:COLUMN )?recovered .+', query, re.I)):
        return False
    if engine == 'postgresql':
        return holder in row.get('blockers', [])
    if engine == 'mysql':
        return row.get('state', '').lower() == 'waiting for table metadata lock'
    return False


class ApplyLoss:
    def __init__(self, probe, docker):
        self.probe, self.docker = probe, docker
        self.process = None

    def lock(self, source):
        p = self.probe
        argv = self.docker + ['exec', '-i']
        if p.engine == 'mysql':
            argv += ['--env-file', str(p.mysql_client), source, 'mysql', '--unbuffered',
                     '--protocol=TCP', '--host=127.0.0.1', '--user=root', '--batch', '--skip-column-names', 'drill']
            sql = ('SET SESSION lock_wait_timeout=10; LOCK TABLES recovery_canary READ; '
                   'SELECT CONNECTION_ID(); DO SLEEP(180); UNLOCK TABLES;\n')
        else:
            argv += [source, 'psql', '-X', '-qAt', '-h', '127.0.0.1', '-d', 'drill', '-v', 'ON_ERROR_STOP=1']
            sql = ("BEGIN; SET LOCAL lock_timeout='10s'; SET LOCAL statement_timeout='200s'; "
                   'LOCK TABLE recovery_canary IN SHARE MODE; SELECT pg_backend_pid(); '
                   'SELECT pg_sleep(180); ROLLBACK;\n')
        output = p.root / 'apply-lock.private.txt'
        with output.open('xb') as stream, (p.root / 'apply-lock.stderr.private').open('xb') as error:
            self.process = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=stream, stderr=error)
        self.process.stdin.write(sql.encode())
        self.process.stdin.close()
        end = time.monotonic() + 20
        while time.monotonic() < end:
            if self.process.poll() is not None:
                raise RuntimeError('Apply lock exited before database loss')
            rows = output.read_text().splitlines()
            if rows and rows[0].isdigit() and int(rows[0]) > 0:
                self.holder = int(rows[0])
                return
            time.sleep(.2)
        raise RuntimeError('The database did not confirm the recovery lock')

    def waiters(self, source):
        p = self.probe
        if p.engine == 'mysql':
            query = ("SELECT JSON_OBJECT('session',ID,'query',INFO,'state',STATE,"
                     "'client',SUBSTRING_INDEX(HOST,':',1)) FROM information_schema.PROCESSLIST "
                     "WHERE USER='operator_writer' AND DB='drill';")
        else:
            query = ("SELECT json_build_object('session',pid,'query',query,'blockers',pg_blocking_pids(pid),"
                     "'client',host(client_addr)) FROM pg_stat_activity "
                     "WHERE datname='drill' AND usename='operator_writer';")
        return [json.loads(line) for line in p.sql(source, query).stdout.decode().splitlines()]

    def arm(self, source, source_field, original_jobs, recipient, key, wrong):
        p = self.probe
        reference = p.publish(2)
        p.patch({'spec': {source_field: p.source_spec(reference), 'suspend': False}})
        ready = p.settled('AwaitingApproval')
        self.plan = p.read(p.kind.lower() + 'plans', ready['status']['plan']['name'])
        p.check('in-flight loss has no unapproved Apply',
                p.watched_apply_jobs(p.barrier('in-flight-unapproved')) == set(original_jobs))
        self.lock(source)
        approval = json.loads(p.approve(ready, self.plan, 'restore-in-flight').stdout)
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            if self.process.poll() is not None:
                raise RuntimeError('The recovery lock ended before an active Apply was witnessed')
            jobs = p.apply_jobs()
            added = set(jobs) - set(original_jobs)
            if len(added) > 1:
                raise RuntimeError('The in-flight approval dispatched more than one Apply')
            if len(added) == 1:
                job = jobs[added.pop()]
                pods = [pod for pod in p.read('pods', False)['items']
                        if any(o.get('uid') == job['metadata']['uid'] for o in pod['metadata'].get('ownerReferences', []))]
                resource = p.read()
                if len(pods) == 1 and running_apply(resource, job, pods[0]):
                    matches = [row for row in self.waiters(source)
                               if fixture_waiter(row, pods[0]['status']['podIP'], self.holder, p.engine)]
                    if len(matches) > 1:
                        raise RuntimeError('More than one session matches the in-flight Apply')
                    if len(matches) == 1:
                        self.job, self.pod, self.resource = job, pods[0], resource
                        self.expected_jobs = set(original_jobs) | {job['metadata']['uid']}
                        self.operation = resource['status']['activeOperation']['id']
                        self.evidence = {'resource': resource, 'plan': self.plan, 'approval': approval,
                                         'job': job, 'pod': pods[0], 'serverWait': matches[0], 'holder': self.holder,
                                         'outcomeAtLoss': 'Unknown: the approved DDL is still running'}
                        p.encrypted('in-flight-execution', json.dumps(self.evidence).encode(), recipient, key, wrong)
                        p.check('in-flight watch accounts for exactly the approved executor',
                                p.watched_apply_jobs(p.barrier('in-flight-loss-boundary')) == self.expected_jobs)
                        return {**original_jobs, job['metadata']['uid']: job}
            time.sleep(1)
        raise RuntimeError('No exact approved Apply reached the database DDL lock')

    def verify_loss_boundary(self, source):
        p = self.probe
        job = p.read('jobs', self.job['metadata']['name'])
        pod = p.read('pods', self.pod['metadata']['name'])
        p.check('original Apply is still running at the database-loss boundary',
                self.process.poll() is None and job['metadata']['uid'] == self.job['metadata']['uid'] and
                pod['metadata']['uid'] == self.pod['metadata']['uid'] and running_apply(p.read(), job, pod) and
                any(row == self.evidence['serverWait'] for row in self.waiters(source)))
        p.report['inFlightLoss'] = {'operationID': self.operation, 'jobUID': job['metadata']['uid'],
            'podUID': pod['metadata']['uid'], 'planUID': self.plan['metadata']['uid'],
            'approvalUID': self.evidence['approval']['metadata']['uid'], 'serverSession': self.evidence['serverWait']['session'],
            'outcomeAtLoss': self.evidence['outcomeAtLoss']}
        p.persist()

    def stopped(self, source_field, original_source):
        p = self.probe
        pods = p.stopped_apply_pods({self.job['metadata']['uid']}, require_success=False)
        p.check('the original in-flight Pod stopped without replacement',
                len(pods) == 1 and pods[0]['metadata']['uid'] == self.pod['metadata']['uid'])
        # Do not change generation while the original runner may still be
        # delivering its result. It is now terminal; preserve the evidence first.
        (p.root / 'in-flight-stopped.private.json').write_text(json.dumps(pods, indent=2))
        p.patch({'spec': {'suspend': True, source_field: original_source}})
        paused = p.settled('Suspended')
        (p.root / 'in-flight-accounted.private.json').write_text(json.dumps(paused, indent=2))
        p.check('the stopped Apply was not replayed before restoration',
                p.watched_apply_jobs(p.barrier('in-flight-stopped-boundary')) == self.expected_jobs)
        return pods

    def diagnose(self, restored):
        p = self.probe
        p.check('restoration recovered the pre-Apply schema', p.sql(restored,
            "SELECT count(*) FROM information_schema.columns WHERE table_name='recovery_canary' "
            "AND column_name='recovered' AND table_schema=" + ("'public'" if p.engine == 'postgresql' else "'drill'") + ';').stdout.strip() == b'0')
        if p.family == 'migration':
            p.check('restoration recovered only the committed migration history',
                    p.sql(restored, 'SELECT version FROM schema_migrations ORDER BY version;').stdout.strip() == b'1')
            current = p.read()
            unresolved = current.get('status', {}).get('unresolvedRun')
            if unresolved:
                p.check('the unresolved record names the lost-database Apply',
                        unresolved.get('operationID') == self.operation and
                        unresolved.get('jobUID') == self.job['metadata']['uid'] and
                        unresolved.get('planRef', {}).get('uid') == self.plan['metadata']['uid'])
                acknowledgment = p.create(p.obj('PtahMigrationRunAcknowledgment', 'restore-lost-database',
                    {'migrationRef': {'name': p.name, 'uid': p.uid}, 'operationID': self.operation}, api=p.api))
                p.report['inFlightLoss']['acknowledgmentUID'] = acknowledgment['metadata']['uid']
        p.report['inFlightLoss']['diagnosis'] = 'The original executor stopped. The restored backup has the pre-Apply schema and history. No old approval may authorize the missing change.'
        p.persist()

    def verify_resolution(self, resource):
        p = self.probe
        acknowledgment = p.report['inFlightLoss'].get('acknowledgmentUID')
        if acknowledgment:
            resolution = resource.get('status', {}).get('resolvedRun') or {}
            p.check('the restored resource consumed the exact recovery acknowledgment',
                    not resource['status'].get('unresolvedRun') and
                    resolution.get('operationID') == self.operation and
                    resolution.get('resolution') == 'Acknowledged' and
                    resolution.get('acknowledgmentRef', {}).get('uid') == acknowledgment)
            p.report['inFlightLoss']['resolution'] = resolution
            p.persist()

    def close(self):
        # Destroying the source DB ends the server-side lock. On any earlier
        # failure its 180-second server bound applies even if Docker loses contact.
        if self.process is not None:
            if self.process.poll() is None:
                self.process.terminate()
            try:
                self.process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5)
