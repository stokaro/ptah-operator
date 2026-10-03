"""A partial native Apply loses its result, then needs a person and a new approval."""
import base64
import copy
import datetime as dt
import hashlib
import json
import pathlib
import re
import time
from result_runner_loss import snapshot_migration


def has_condition(obj, kind, reason=None):
    return any(c['type'] == kind and c['status'] == 'True' and (reason is None or c.get('reason') == reason)
               for c in obj.get('status', {}).get('conditions', []))


def as_person(k, identity, obj):
    return json.loads(k('create', '-f', '-', '-o', 'json', '--as=' + identity['person'],
                        '--as-group=' + identity['group'], data=json.dumps(obj)))


def approve(k, get, identity, resource, plan, name):
    return as_person(k, identity, {'apiVersion': 'operator.ptah.run/v1alpha1', 'kind': 'PtahMigrationApproval',
                     'metadata': {'name': name, 'namespace': resource['metadata']['namespace']},
                     'spec': {'migrationRef': {'name': resource['metadata']['name'], 'uid': resource['metadata']['uid']},
                              'planRef': {'name': plan['metadata']['name'], 'uid': plan['metadata']['uid']},
                              'planFingerprint': plan['spec']['fingerprint']}})


def authorize_initial(k, get, create, wait, resource):
    ns = resource['metadata']['namespace']
    identity = {'person': 'qualification-approver@example.test', 'group': 'qualification:partial-result'}
    roles = [r for r in get('clusterroles')['items'] if r['metadata']['name'].endswith('-approver')
             and r['metadata'].get('labels', {}).get('app.kubernetes.io/name') == 'ptah-operator']
    if len(roles) != 1:
        raise ValueError('Expected the installed approver role')
    create({'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'RoleBinding', 'metadata': {'name': 'partial-result-approver', 'namespace': ns},
            'roleRef': {'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'ClusterRole', 'name': roles[0]['metadata']['name']},
            'subjects': [{'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'Group', 'name': identity['group']}]})

    def allowed():
        review = create({'apiVersion': 'authorization.k8s.io/v1', 'kind': 'SubjectAccessReview',
                         'spec': {'user': identity['person'], 'groups': [identity['group']],
                                  'resourceAttributes': {'namespace': ns, 'verb': 'create', 'group': 'operator.ptah.run', 'resource': 'ptahmigrationapprovals'}}})
        return review['status'].get('allowed')
    wait(allowed, 30)

    def planned():
        current = get('ptahmigration', resource['metadata']['name'])
        return current if current.get('status', {}).get('phase') == 'AwaitingApproval' and current['status'].get('plan') else None
    current = wait(planned, 180)
    plan = get('ptahmigrationplan', current['status']['plan']['name'])
    approval = approve(k, get, identity, resource, plan, 'authorize-original')
    return {'identity': identity, 'plan': plan, 'approval': approval, 'approverRole': roles[0]['metadata']['name']}


def verify_evidence(e):
    def require(ok, message):
        if not ok:
            raise ValueError(message)
    require(e['engine'] in ('PostgreSQL', 'MySQL') and e['policy'] == 'OnApproval', 'Missing engine or approval policy')
    require(e['calibration']['afterRollback'] == '0:1:true' and e['calibration']['afterReset'] == '0:1:false', 'Uncalibrated SQL witness')
    require(e['beforeLoss']['witness'] == '1:1:true' and e['beforeLoss']['dirtyRevisions'] == 1
            and e['beforeLoss']['podPhase'] == 'Running' and e['beforeLoss']['publications'] == 0, 'No unpublished partial execution')
    require(e['originalJobAbsent'] and e['originalPodAbsent'], 'Original execution survived')
    unknown = e['unknown']['status']['unresolvedRun']
    require(unknown['operationID'] == e['operationID'] and unknown['jobUID'] == e['jobUID']
            and unknown['outcome'] == 'Unknown', 'Missing exact unknown run')
    require(json.loads(e['unknown']['metadata']['annotations']['operator.ptah.run/unresolved-run']) == unknown, 'Missing unresolved copy')
    for key in ('beforeAcknowledgment', 'beforeFreshApproval'):
        rows = e[key]
        require(len(rows) >= 2 and rows[-1]['at'] - rows[0]['at'] >= 90, 'Short or empty refusal window')
        require(all(b['at'] - a['at'] <= 10 for a, b in zip(rows, rows[1:])), 'Unobserved refusal window')
        expected = '1:1:true' if key == 'beforeAcknowledgment' else '0:1:true'
        require(all(r['witness'] == expected and r['applyJobUIDs'] == [] for r in rows), 'Unapproved SQL or Apply replay')
        if key == 'beforeAcknowledgment':
            require(all(r['unresolvedOperation'] == e['operationID'] and r['dirty'] for r in rows), 'Unknown partial run disappeared')
    require(e['afterManualRepair'] == '0:1:true', 'Manual repair reset or failed to undo the SQL witness')
    ack = e['acknowledgment']; resolved = e['acknowledged']['status']['resolvedRun']
    require(ack['spec']['operationID'] == e['operationID'] and ack['spec']['migrationRef']['uid'] == e['resourceUID'], 'Acknowledgment names another run')
    require(resolved['resolution'] == 'Acknowledged' and resolved['operationID'] == e['operationID']
            and resolved['acknowledgmentRef']['uid'] == ack['metadata']['uid']
            and resolved['acknowledgedBy']['username'] == e['initial']['identity']['person'], 'No authenticated human resolution')
    require(has_condition(ack, 'Consumed'), 'Acknowledgment was not consumed')
    original, fresh = e['initial']['approval'], e['freshApproval']
    require(original['metadata']['uid'] != fresh['metadata']['uid'] and has_condition(original, 'Consumed'), 'Original approval was not consumed once')
    require(original['spec']['planRef']['uid'] != fresh['spec']['planRef']['uid']
            and original['spec']['planFingerprint'] != fresh['spec']['planFingerprint'], 'Approval did not bind a fresh plan')
    require(e['freshJob']['metadata']['creationTimestamp'] >= fresh['metadata']['creationTimestamp'], 'New Apply predates its approval')
    require(e['finalWitness'] == '1:2:true' and e['originalPublications'] == 0 and e['freshApplyJobs'] == 1, 'Missing authorized retry or extra SQL execution')
    require(has_condition(e['final'], 'Ready', 'HistoryMatched') and not e['final']['status'].get('unresolvedRun')
            and any(c['type'] == 'Ready' and c['status'] == 'True' and c.get('observedGeneration') == e['final']['metadata']['generation']
                    for c in e['final']['status']['conditions']), 'No resolved current-generation convergence')
    require(e['final']['metadata']['uid'] == e['resourceUID'], 'Resource was replaced')
    return {'originalPartialExecutions': 1, 'humanAcknowledgments': 1, 'freshApprovedExecutions': 1, 'unapprovedExecutions': 0}


def run(*, k, get, create, wait, save, witness, records, open_gate, resource, pod,
        operation, engine, environment, calibration, sql, migration_sql, publish_template, initial, resume=None):
    ns, name = resource['metadata']['namespace'], resource['metadata']['name']
    job_name, job_uid = operation['jobName'], operation['jobUID']
    pod_name = pod['metadata'].get('name', '')
    quota_created = False
    passed = False
    evidence = {'engine': engine, 'commit': environment['E2E_CONTROLLER_REVISION'], 'namespace': ns,
                'resourceUID': resource['metadata']['uid'], 'jobUID': job_uid, 'podUID': pod['metadata']['uid'],
                'operationID': operation['id'], 'policy': resource['spec']['policy']['apply'], 'calibration': calibration,
                'initial': initial, 'procedureSHA256': {n: hashlib.sha256(pathlib.Path(__file__).with_name(n).read_bytes()).hexdigest()
                    for n in ('result_partial_loss.py', 'result_lost_ack.py')}}

    def publications():
        return [r for r in records() if r['spec']['type'] == 'intent' and json.loads(base64.b64decode(r['spec']['data']))['binding']['jobUID'] == job_uid]

    def apply_jobs():
        return [j for j in get('jobs')['items'] if j['metadata']['name'].startswith('ptah-m-apply-')]

    def hold(dirty):
        rows = []
        while True:
            current = get('ptahmigration', name)
            row = {'at': time.monotonic(), 'witness': witness(), 'applyJobUIDs': [j['metadata']['uid'] for j in apply_jobs()],
                   'unresolvedOperation': current['status'].get('unresolvedRun', {}).get('operationID'),
                   'dirty': current['status'].get('history', {}).get('dirty', False)}
            rows.append(row)
            expected = '1:1:true' if dirty else '0:1:true'
            assert row['witness'] == expected and not row['applyJobUIDs'], 'Unapproved replay during hold'
            if dirty:
                assert row['unresolvedOperation'] == operation['id'] and row['dirty']
            if row['at'] - rows[0]['at'] >= 90:
                return rows
            time.sleep(2)

    try:
        if resume is None:
            key = 'count/ptahresultrecords.operator.ptah.run'
            census = len(records())
            assert census > 0 and not publications()
            quota = create({'apiVersion': 'v1', 'kind': 'ResourceQuota', 'metadata': {'name': 'hold-partial-result', 'namespace': ns}, 'spec': {'hard': {key: str(census)}}})
            quota_created = True
            def quota_ready():
                status = get('resourcequota', 'hold-partial-result').get('status', {})
                return status.get('used', {}).get(key) == str(census) and status.get('hard', {}).get(key) == str(census)
            wait(quota_ready, 30)
            evidence['quota'] = {'uid': quota['metadata']['uid'], 'hard': census}
            open_gate()
            wait(lambda: witness() == '1:1:true' and sql("SELECT count(*) FROM schema_migrations WHERE state <> 'applied';").splitlines()[-1] == '1', 90)
            current_pod = get('pod', pod_name)
            evidence['beforeLoss'] = {'witness': witness(), 'dirtyRevisions': int(sql("SELECT count(*) FROM schema_migrations WHERE state <> 'applied';").splitlines()[-1]), 'podPhase': current_pod['status']['phase'], 'publications': len(publications())}
            assert current_pod['metadata']['uid'] == pod['metadata']['uid'] and current_pod['status']['phase'] == 'Running' and not publications()
            assert get('job', job_name)['metadata']['uid'] == job_uid
            k('delete', 'job', job_name, '--cascade=background', '--wait=false')
            k('delete', 'pod', pod_name, '--ignore-not-found', '--grace-period=0', '--force', '--wait=true', '--timeout=60s')
            evidence['originalJobAbsent'] = not k('get', 'job', job_name, '--ignore-not-found', '-o', 'name').strip()
            evidence['originalPodAbsent'] = not k('get', 'pod', pod_name, '--ignore-not-found', '-o', 'name').strip()

            def unknown():
                current = get('ptahmigration', name)
                u = current.get('status', {}).get('unresolvedRun', {})
                return current if u.get('jobUID') == job_uid and u.get('outcome') == 'Unknown' else None
            evidence['unknown'] = snapshot_migration(wait(unknown, 120))
            k('delete', 'resourcequota', 'hold-partial-result')
            quota_created = False
            wait(lambda: get('ptahmigration', name).get('status', {}).get('history', {}).get('dirty'), 120)
            print('Partial SQL and dirty revision retained after original Job/Pod loss; holding against replay', flush=True)
            evidence['beforeAcknowledgment'] = hold(True)
            # A person's explicit repair removes only the failed operation's effect;
            # it does not reset the calibrated allocation counter.
            sql("DELETE FROM delivery_probe_calls WHERE n=1; DELETE FROM schema_migrations WHERE state <> 'applied';")
            evidence['afterManualRepair'] = witness()
            assert evidence['afterManualRepair'] == '0:1:true'
        else:
            current = get('ptahmigration', name)
            unresolved = current['status']['unresolvedRun']
            assert current['metadata']['uid'] == resume['resourceUID'] and unresolved['operationID'] == operation['id']
            assert unresolved['jobUID'] == job_uid and witness() == '0:1:true' and not publications() and not apply_jobs()
            current_procedures = evidence['procedureSHA256']
            evidence = copy.deepcopy(resume)
            evidence['interruptionProcedureSHA256'] = evidence['procedureSHA256']
            evidence['procedureSHA256'] = current_procedures
            evidence['resume'] = {'checkpointSHA256': hashlib.sha256(pathlib.Path(environment['RESULT_PROBE_PARTIAL_CHECKPOINT']).read_bytes()).hexdigest(),
                                  'sourceCommit': environment['RESULT_PROBE_PARTIAL_CHECKPOINT_COMMIT'],
                                  'observedAt': dt.datetime.now(dt.timezone.utc).isoformat(),
                                  'resourceGeneration': current['metadata']['generation'],
                                  'databaseWitness': witness(), 'cause': 'The first recovery publisher was refused by the write-once tag policy. Resume uses a distinct tag and the same unresolved execution.'}
            k('patch', 'ptahmigration', name, '--type=json', '-p', json.dumps([
                {'op': 'test', 'path': '/metadata/uid', 'value': current['metadata']['uid']},
                {'op': 'test', 'path': '/metadata/resourceVersion', 'value': current['metadata']['resourceVersion']},
                {'op': 'add', 'path': '/spec/suspend', 'value': False}]))
            print('Resumed the saved partial-loss checkpoint with the same Unknown execution and calibrated counter', flush=True)
        config = get('configmap', 'ack-migrations')
        config['data']['0000000001_record_delivery.up.sql'] = migration_sql + '\n'
        k('replace', '-f', '-', data=json.dumps(config))
        template = copy.deepcopy(publish_template)
        template['metadata'] = {}
        args = template['spec']['containers'][0]['args']
        prefix = 'oci://' + environment['E2E_REGISTRY_HOST'] + '/migrations/demo:'
        tags = [i for i, arg in enumerate(args) if arg.startswith(prefix)]
        assert len(tags) == 1
        args[tags[0]] = prefix + ns + '-repaired'
        args[args.index('--version') + 1] = ns + '-repaired'
        create({'apiVersion': 'batch/v1', 'kind': 'Job', 'metadata': {'name': 'repaired-publish-final', 'namespace': ns}, 'spec': {'backoffLimit': 0, 'activeDeadlineSeconds': 300, 'template': template}})
        def published():
            job = get('job', 'repaired-publish-final')
            if has_condition(job, 'Failed'):
                raise RuntimeError('The repaired artifact publisher failed')
            return has_condition(job, 'Complete')
        wait(published, 300)
        digest = re.search('^Digest: (sha256:[0-9a-f]{64})$', k('logs', 'job/repaired-publish-final'), re.M).group(1)
        ref = 'oci://' + environment['E2E_REGISTRY_HOST'] + '/migrations/demo@' + digest
        k('patch', 'ptahmigration', name, '--type=merge', '-p', json.dumps({'spec': {'artifact': {'ociRef': ref}}}))
        wait(lambda: get('ptahmigration', name)['status'].get('artifact', {}).get('digest') == digest, 120)
        assert get('ptahmigration', name)['status']['unresolvedRun']['operationID'] == operation['id']
        ack = as_person(k, initial['identity'], {'apiVersion': 'operator.ptah.run/v1alpha1', 'kind': 'PtahMigrationRunAcknowledgment',
                        'metadata': {'name': 'account-for-partial-run', 'namespace': ns},
                        'spec': {'migrationRef': {'name': name, 'uid': resource['metadata']['uid']}, 'operationID': operation['id']}})

        def acknowledged():
            current = get('ptahmigration', name)
            resolved = current['status'].get('resolvedRun', {})
            return current if resolved.get('operationID') == operation['id'] and resolved.get('resolution') == 'Acknowledged' and not current['status'].get('unresolvedRun') else None
        evidence['acknowledged'] = snapshot_migration(wait(acknowledged, 120))
        wait(lambda: has_condition(get('ptahmigrationrunacknowledgment', ack['metadata']['name']), 'Consumed'), 60)
        evidence['acknowledgment'] = get('ptahmigrationrunacknowledgment', ack['metadata']['name'])

        def planned():
            current = get('ptahmigration', name)
            return current if current['status'].get('phase') == 'AwaitingApproval' and current['status'].get('plan') else None
        current = wait(planned, 180)
        evidence['beforeFreshApproval'] = hold(False)
        current = wait(planned, 60)
        plan = get('ptahmigrationplan', current['status']['plan']['name'])
        evidence['freshPlan'] = plan
        evidence['initial']['approval'] = get('ptahmigrationapproval', initial['approval']['metadata']['name'])
        evidence['freshApproval'] = approve(k, get, initial['identity'], resource, plan, 'authorize-repaired-plan')
        print('Human acknowledgment did not dispatch Apply; fresh plan approved separately', flush=True)

        def converged():
            current = get('ptahmigration', name)
            return current if has_condition(current, 'Ready', 'HistoryMatched') and not current['status'].get('activeOperation') and any(c.get('observedGeneration') == current['metadata']['generation'] and c['type'] == 'Ready' and c['status'] == 'True' for c in current['status']['conditions']) else None
        final = wait(converged, 240)
        jobs = apply_jobs()
        assert len(jobs) == 1
        evidence.update(final=snapshot_migration(final), finalWitness=witness(), freshApplyJobs=len(jobs), freshJob=jobs[0], originalPublications=len(publications()), completedAt=dt.datetime.now(dt.timezone.utc).isoformat())
        evidence['verification'] = verify_evidence(evidence)
        save('partial-loss.json', evidence)
        passed = True
        print('PASS: partial lost result required human acknowledgment and one fresh approved Apply', flush=True)
    finally:
        save('partial-loss.json', evidence)
        if not passed:
            k('patch', 'ptahmigration', name, '--type=merge', '-p', '{"spec":{"suspend":true}}')
        if quota_created:
            k('delete', 'resourcequota', 'hold-partial-result', '--ignore-not-found')
