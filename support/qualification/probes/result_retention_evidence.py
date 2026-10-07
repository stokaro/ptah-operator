#!/usr/bin/env python3
"""Replay the frozen retention cohort against successful API DELETE events."""
import argparse
import base64
import datetime as dt
import json
from pathlib import Path


def instant(value):
    parsed = dt.datetime.fromisoformat(value.replace('Z', '+00:00'))
    if parsed.tzinfo is None:
        raise ValueError('Evidence timestamps must carry a timezone')
    return parsed


def require(condition, message):
    if not condition:
        raise ValueError(message)


def verify(before, after, markers, audits):
    records = {r['name']: r for r in before['records']}
    require(len(records) == len(before['records']), 'Duplicate frozen record name')
    eligible, pinned = set(before['eligibleNames']), set(before['pinnedNames'])
    require(len(eligible) == len(before['eligibleNames'])
            and len(pinned) == len(before['pinnedNames'])
            and eligible and pinned and not eligible & pinned
            and eligible | pinned == records.keys(), 'Invalid frozen denominator')
    retained = {r['name']: r for r in after['remainingPinned']}
    require(set(retained) == pinned and all(retained[n] == records[n] for n in pinned),
            'Pinned evidence changed or disappeared')
    require(before['planDigests'] and before['planDigests'] == after['planDigests'],
            'Published plan identities or bytes changed')
    require(after['eligibleRecordsCollected'] == len(eligible), 'Incomplete record collection')
    secrets = before['secretOwners']
    eligible_secrets = eligible & secrets.keys()
    authentication = before.get('authentication', 'certificate')
    require(authentication in ('certificate', 'pod-token'), 'Unknown result authentication')
    if authentication == 'pod-token':
        require(after.get('authentication') == authentication and not secrets,
                'Pod-token evidence contains legacy Secret projections')
        require(before.get('collectorIdentity', '').startswith('system:serviceaccount:'),
                'Missing installed collector identity')
        credentials = {n for n, r in records.items() if r['type'] == 'credential'}
        require(credentials & eligible and credentials & pinned, 'Missing public Pod-binding census')
        for reading in (before, after):
            census = reading.get('credentialSecretCensus', {})
            require(census.get('expectedNames') == sorted(credentials)
                    and census.get('presentNames') == [], 'Credential Secret absence was not verified')
        require(all(records[n].get('binding', {}).get('jobUID') for n in credentials),
                'Public credential record has no Job binding')
    else:
        require(eligible_secrets and pinned & secrets.keys(), 'Missing eligible or pinned Secret census')
    require(after['eligibleSecretProjectionsCollected'] == len(eligible_secrets),
            'Incomplete projection collection')

    retired = {m['spec']['value']['binding']['jobUID']: m for m in markers}
    require(len(retired) == len(markers) and retired, 'Invalid retirement marker census')
    marker_names = {m['metadata']['name'] for m in markers}
    require(marker_names == {n for n, r in records.items() if r['type'] == 'retired'},
            'Retirement policies do not match the frozen cohort')
    intent_jobs = {r['uid']: r['binding']['jobUID'] for r in records.values() if r['type'] == 'intent'}
    credential_jobs = {m['spec']['value']['source']['name']: uid for uid, m in retired.items()
                       if m['spec']['value']['source']['type'] == 'credential'}
    if authentication == 'pod-token':
        credential_jobs.update({n: r['binding']['jobUID'] for n, r in records.items()
                                if r['type'] == 'credential'})
    for name, secret in secrets.items():
        owner = secret['ownerReferences']
        require(len(owner) == 1 and owner[0]['uid'] == records[name]['uid']
                and owner[0]['blockOwnerDeletion'] is False, 'Foreign Secret ownership')
        credential_jobs[name] = secret['annotations']['operator.ptah.run/result-job-uid']

    deadlines = {}
    for name in eligible:
        record = records[name]
        if record['type'] in ('intent', 'retired'):
            job = record['binding']['jobUID']
        elif record['type'] == 'credential':
            job = credential_jobs[name]
        else:
            job = intent_jobs[record['owners'][0]['uid']]
        marker = retired[job]
        value, metadata = marker['spec']['value'], marker['metadata']
        frozen = records[metadata['name']]
        require(metadata['uid'] == frozen['uid']
                and metadata['creationTimestamp'] == frozen['createdAt']
                and value['binding'] == frozen['binding'], 'Retirement identity changed')
        source = value['source']
        require(source['name'] in records
                and records[source['name']]['uid'] == source['uid']
                and records[source['name']]['type'] == source['type'], 'Retirement source changed')
        require(value['retentionSeconds'] >= 3600, 'Retention is shorter than one hour')
        deadline = max(instant(metadata['creationTimestamp']), instant(record['createdAt']))
        deadline += dt.timedelta(seconds=value['retentionSeconds'])
        require(deadline == instant(before['deadlines'][name]), 'Frozen deadline differs from API time and policy')
        deadlines[name] = deadline

    def events(resource, name, uid):
        namespace = secrets[name]['namespace'] if resource == 'secrets' else next(
            m['spec']['value']['binding']['namespace'] for m in markers)
        return [a for a in audits if a.get('verb') == 'delete'
                and a.get('stage') == 'ResponseComplete'
                and a.get('responseStatus', {}).get('code') in (200, 202)
                and a.get('objectRef', {}).get('namespace') == namespace
                and a['objectRef'].get('resource') == resource
                and a['objectRef'].get('name') == name
                and a.get('requestObject', {}).get('preconditions', {}).get('uid') == uid]

    witnesses = []
    for name in sorted(eligible):
        matches = events('ptahresultrecords', name, records[name]['uid'])
        require(matches, 'No exact-UID successful DELETE for ' + name)
        if authentication == 'pod-token':
            require(all(a.get('user', {}).get('username') == before['collectorIdentity'] for a in matches),
                    'Result record was not deleted by the installed collector')
        require(all(instant(a['requestReceivedTimestamp']) >= deadlines[name] for a in matches),
                'Record deleted before its retention deadline: ' + name)
        witnesses.append({'name': name, 'uid': records[name]['uid'],
                          'notBefore': deadlines[name].isoformat(),
                          'deletedAt': min(a['requestReceivedTimestamp'] for a in matches),
                          'auditIDs': sorted({a['auditID'] for a in matches})})
    actors = set()
    for name in sorted(eligible_secrets):
        matches = events('secrets', name, secrets[name]['uid'])
        require(matches, 'No exact-UID Secret GC event for ' + name)
        require(all(instant(a['requestReceivedTimestamp']) >= deadlines[name] for a in matches),
                'Secret deleted before its credential retention deadline')
        actual = {a['user']['username'] for a in matches}
        require(actual <= {'system:kube-controller-manager',
                           'system:serviceaccount:kube-system:generic-garbage-collector'},
                'Secret deletion was not performed by Kubernetes garbage collection')
        actors.update(actual)
    return {'status': 'passed', 'eligibleRecords': len(eligible), 'pinnedRecords': len(pinned),
            'collectedSecretProjections': len(eligible_secrets), 'preservedPlanObjects': len(before['planDigests']),
            'garbageCollectorIdentities': sorted(actors), 'recordDeletes': witnesses}



def verify_abandoned(interrupted, before, after, markers, audits, refusals):
    """Require an actual unfinished publication before replaying its collection."""
    intent = interrupted['intent']
    manifest = json.loads(base64.b64decode(intent['spec']['data'], validate=True))
    binding = manifest['binding']
    pod_token = before.get('authentication') == 'pod-token'
    require(binding['operation'] == ('plan' if pod_token else 'resolve')
            and manifest.get('chunks') and 'inline' not in manifest, 'Missing unfinished read-only publication')
    if pod_token:
        require(manifest.get('size', 0) > 262144
                and sum(chunk['size'] for chunk in manifest['chunks']) == manifest['size'],
                'Plan does not require chunked delivery')
    resource, job, pod = interrupted['resource'], interrupted['job'], interrupted['pod']
    require(resource['metadata']['uid'] == binding['uid']
            and resource['status']['activeOperation']['id'] == binding['operationID']
            and job['metadata']['uid'] == binding['jobUID']
            and pod['metadata']['uid'] == binding['podUID'], 'Wrong interrupted execution identity')
    rows = interrupted['records']
    require(len(rows) == 2 and {r['type'] for r in rows} == {'intent', 'credential'},
            'Publication was complete or its interruption census is missing')
    require(next(r['uid'] for r in rows if r['type'] == 'intent') == intent['metadata']['uid'],
            'Interrupted intent was replaced')
    frozen = {r['name']: r for r in before['records']}
    require(all(r['name'] in frozen and frozen[r['name']] == r for r in rows),
            'Abandoned publication changed before retirement')
    expected = {r['name'] for r in rows} | {m['metadata']['name'] for m in markers}
    require(set(before['eligibleNames']) == expected and len(expected) == 3,
            'Retirement cohort does not cover exactly the abandoned publication')
    quota = interrupted['quota']
    key = 'count/ptahresultrecords.operator.ptah.run'
    preceding = interrupted.get('precedingRecords', []) if pod_token else []
    require(len({r['name'] for r in preceding}) == len(preceding)
            and not {r['name'] for r in preceding} & {r['name'] for r in rows},
            'Invalid preceding publication census')
    limit = str(2 + len(preceding))
    require(quota['status']['hard'][key] == limit and quota['status']['used'][key] == limit,
            'Quota did not fill at the interrupted publication')
    matches = [a for a in refusals if a.get('verb') == 'create' and a.get('stage') == 'ResponseComplete'
               and a.get('responseStatus', {}).get('code') == 403
               and ('exceeded quota: ' + quota['metadata']['name']) in a.get('responseStatus', {}).get('message', '')
               and a.get('objectRef', {}).get('namespace') == binding['namespace']
               and a['objectRef'].get('resource') == 'ptahresultrecords'
               and a['objectRef'].get('name') == intent['metadata']['name'] + '-000']
    require(matches, 'No actual API refusal of the first chunk')
    report = verify(before, after, markers, audits)
    report['abandonedPublication'] = {'intentUID': intent['metadata']['uid'],
                                     'jobUID': binding['jobUID'], 'recordsBeforeRetirement': 2,
                                     'quotaRefusalAuditIDs': sorted({a['auditID'] for a in matches})}
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('directory', type=Path)
    args = parser.parse_args()
    def read(name):
        return json.loads((args.directory / name).read_text())
    report = verify(read('retention-before.json'), read('retention-after.json'),
                    read('retention-markers.json'), read('retention-delete-audit.json'))
    print(json.dumps(report, indent=2))


if __name__ == '__main__':
    main()
