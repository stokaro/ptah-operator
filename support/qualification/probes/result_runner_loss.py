"""Installed migration recovery after SQL commit and loss of its unpublished result."""
import base64
import datetime as dt
import hashlib
import json
import pathlib
import re


def verify_evidence(value):
    def require(condition, message):
        if not condition:
            raise ValueError(message)

    require(all(value[key] for key in ['resourceUID', 'jobUID', 'podUID', 'operationID', 'namespace']),
            'Missing execution identity')
    require(value['engine'] in ('PostgreSQL', 'MySQL'), 'Unknown engine')
    require(re.fullmatch('[0-9a-f]{40}', value['commit']), 'Missing runtime revision')
    for digest in value['procedureSHA256'].values():
        require(re.fullmatch('[0-9a-f]{64}', digest), 'Missing procedure identity')
    require(set(value['procedureSHA256']) == {'result_lost_ack.py', 'result_runner_loss.py'},
            'Incomplete procedure identities')
    require(value['calibration']['afterRollback'] == '0:1:true'
            and value['calibration']['afterReset'] == '0:1:false', 'Ineffective SQL witness')
    require(value['beforeLoss']['databaseWitness'] == value['afterRecovery']['databaseWitness'] == '1:1:true',
            'SQL was absent or replayed')
    require(value['beforeLoss']['podPhase'] == 'Running' and value['beforeLoss']['podRestarts'] == 0,
            'Runner had already stopped before the fault')
    require(value['beforeLoss']['publications'] == value['afterRecovery']['publications'] == 0,
            'Original Apply result was published')
    quota = value['quota']
    require(quota['uid'] and quota['hard'] == quota['used'] > 0, 'Publication was not fenced')
    require(value['originalJobAbsent'] is True and value['originalPodAbsent'] is True,
            'Original execution survived')
    require(value['unknown']['metadata']['uid'] == value['resourceUID']
            and value['unknown']['metadata']['namespace'] == value['namespace'], 'Unknown record belongs to another resource')
    unknown = value['unknown']['status']['unresolvedRun']
    require(unknown['outcome'] == 'Unknown' and unknown['jobUID'] == value['jobUID']
            and unknown['operationID'] == value['operationID'], 'No exact unknown-run record')
    require(json.loads(value['unknown']['metadata']['annotations']['operator.ptah.run/unresolved-run']) == unknown,
            'Missing durable unresolved-run copy')
    final = value['afterRecovery']['migration']
    status = final['status']
    require(final['metadata']['uid'] == value['resourceUID'] and not status.get('unresolvedRun'),
            'Recovery did not settle the original resource')
    resolved = status['resolvedRun']
    require(resolved['operationID'] == value['operationID'] and resolved['outcome'] == 'Unknown'
            and resolved['resolution'] == 'HistoryRead', 'No history-based recovery of the unknown execution')
    require(dt.datetime.fromisoformat(resolved['resolvedAt'].replace('Z', '+00:00')) >
            dt.datetime.fromisoformat(unknown['recordedAt'].replace('Z', '+00:00')), 'Recovery used stale history')
    require(any(c['type'] == 'Ready' and c['status'] == 'True' and c['reason'] == 'HistoryMatched'
                and c.get('observedGeneration') == final['metadata']['generation']
                for c in status['conditions']), 'No current-generation convergence')
    require(value['afterRecovery']['replacementApplyJobs'] == 0, 'Another Apply was dispatched')
    return {'sqlExecutions': 1, 'originalResultPublished': False, 'resolution': 'HistoryRead'}


def snapshot_migration(m):
    metadata = {key: m['metadata'][key] for key in ['name', 'namespace', 'uid', 'generation']}
    # The API omits annotations after the controller removes its last copy.
    metadata['annotations'] = m['metadata'].get('annotations', {})
    return {'metadata': metadata, 'status': m['status']}


def run(*, k, get, create, wait, save, witness, records, open_gate, resource, pod,
        operation, engine, environment, calibration):
    """Use only an owned disposable lab; retain the unknown record before recovery."""
    ns = resource['metadata']['namespace']
    name = resource['metadata']['name']
    job_name, job_uid = operation['jobName'], operation['jobUID']
    pod_name, pod_uid = pod['metadata']['name'], pod['metadata']['uid']
    quota_name = 'hold-unpublished-result'
    quota_created = False
    passed = False

    def publications():
        return [r for r in records() if r['spec']['type'] == 'intent' and
                json.loads(base64.b64decode(r['spec']['data']))['binding']['jobUID'] == job_uid]

    try:
        census = len(records())
        assert census > 0 and not publications()
        key = 'count/ptahresultrecords.operator.ptah.run'
        quota = create({'apiVersion': 'v1', 'kind': 'ResourceQuota', 'metadata': {'name': quota_name, 'namespace': ns},
                        'spec': {'hard': {key: str(census)}}})
        quota_created = True

        def quota_ready():
            current = get('resourcequota', quota_name)
            return current if current.get('status', {}).get('used', {}).get(key) == str(census) and current['status'].get('hard', {}).get(key) == str(census) else None

        ready_quota = wait(quota_ready, 30)
        open_gate()
        wait(lambda: witness() == '1:1:true', 90)
        current_pod = get('pod', pod_name)
        assert current_pod['metadata']['uid'] == pod_uid and current_pod['status']['phase'] == 'Running'
        assert all(c['restartCount'] == 0 for c in current_pod['status']['containerStatuses'])
        assert not publications()
        before = {'databaseWitness': '1:1:true', 'podPhase': 'Running', 'podRestarts': 0, 'publications': 0,
                  'observedAt': dt.datetime.now(dt.timezone.utc).isoformat()}
        save('before-loss.json', before)
        assert get('job', job_name)['metadata']['uid'] == job_uid
        k('delete', 'job', job_name, '--cascade=background', '--wait=false')
        k('delete', 'pod', pod_name, '--ignore-not-found', '--grace-period=0', '--force', '--wait=true', '--timeout=60s')
        assert not k('get', 'job', job_name, '--ignore-not-found', '-o', 'name').strip()
        assert not k('get', 'pod', pod_name, '--ignore-not-found', '-o', 'name').strip()
        print('Original Job and Pod removed after one committed SQL effect, with publication fenced', flush=True)

        def unknown_recorded():
            m = get('ptahmigration', name)
            u = m.get('status', {}).get('unresolvedRun', {})
            return m if u.get('jobUID') == job_uid and u.get('outcome') == 'Unknown' else None

        unknown = wait(unknown_recorded, 120)
        save('unknown.json', snapshot_migration(unknown))
        assert witness() == '1:1:true' and not publications()
        k('delete', 'resourcequota', quota_name)
        quota_created = False

        def recovered():
            m = get('ptahmigration', name)
            resolved = m.get('status', {}).get('resolvedRun', {})
            return m if resolved.get('operationID') == operation['id'] and resolved.get('resolution') == 'HistoryRead' and any(
                c['type'] == 'Ready' and c['status'] == 'True' and c.get('reason') == 'HistoryMatched'
                and c.get('observedGeneration') == m['metadata']['generation']
                for c in m.get('status', {}).get('conditions', [])) else None

        final = wait(recovered, 240)
        apply_jobs = [j for j in get('jobs')['items'] if j['metadata']['name'].startswith('ptah-m-apply-')]
        result = {'engine': engine, 'commit': environment['E2E_CONTROLLER_REVISION'],
                  'namespace': ns, 'resourceUID': resource['metadata']['uid'],
                  'jobUID': job_uid, 'podUID': pod_uid, 'operationID': operation['id'],
                  'calibration': calibration, 'beforeLoss': before,
                  'quota': {'uid': quota['metadata']['uid'], 'hard': int(ready_quota['status']['hard'][key]),
                            'used': int(ready_quota['status']['used'][key])},
                  'originalJobAbsent': True, 'originalPodAbsent': True, 'unknown': snapshot_migration(unknown),
                  'afterRecovery': {'databaseWitness': witness(), 'publications': len(publications()),
                                    'replacementApplyJobs': len(apply_jobs), 'migration': snapshot_migration(final)},
                  'procedureSHA256': {p: hashlib.sha256(pathlib.Path(__file__).with_name(p).read_bytes()).hexdigest()
                                      for p in ['result_lost_ack.py', 'result_runner_loss.py']},
                  'completedAt': dt.datetime.now(dt.timezone.utc).isoformat()}
        verify_evidence(result)
        save('runner-loss.json', result)
        passed = True
        print('PASS: missing Apply result recovered through fresh native history without another SQL execution', flush=True)
    finally:
        if not passed:
            k('patch', 'ptahmigration', name, '--type=merge', '-p', '{"spec":{"suspend":true}}')
        if quota_created:
            k('delete', 'resourcequota', quota_name, '--ignore-not-found')
