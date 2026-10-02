"""Force two installed receivers into CREATE before either intent exists."""
import base64
import datetime as dt
import hashlib
import pathlib
import time

from result_concurrent import digest, verify_evidence as verify_concurrent
from result_first_harvest import publication


def verify_evidence(value):
    def require(condition, message):
        if not condition:
            raise ValueError(message)
    require(value['evidenceVersion'] == 1, 'Unknown first-publication evidence version')
    concurrent = value['concurrent']
    verify_concurrent(concurrent)
    require(bool(value['intentUID']) and concurrent['receipt']['Name'] == value['intentName'] + '-complete',
            'Missing or foreign publication identity')
    before = value['before']
    census = before['records']
    require(len(census) > 0 and len({r['name'] for r in census}) == len(census)
            and all(r['uid'] for r in census), 'Empty or duplicated initial census')
    require(all(r['name'] != value['intentName'] and r['name'] != concurrent['receipt']['Name'] for r in census),
            'Publication already existed before the race')
    gate = value['barrier']
    require(gate['firstGateEnabled'] is True and gate['firstWaits'] == 1
            and not gate.get('firstResumedAt') and gate['preflights'] == 1
            and not gate['attempts'] and not gate['dropped'], 'Original upload escaped before the race')
    arrivals = gate['firstAdmissions']
    require(len(arrivals) == 2 and len({a['requestUID'] for a in arrivals}) == 2
            and all(a['requestUID'] for a in arrivals), 'Missing distinct CREATE requests')
    require({a['managerPodUID'] for a in arrivals} == set(concurrent['receivers']),
            'Admissions came from different receivers')
    require(all(a['intentName'] == value['intentName'] and a['payloadDigest'] == concurrent['originalDigest']
                for a in arrivals), 'Admissions did not describe the original bytes')
    require(all(isinstance(a.get('releasedAt'), str) for a in arrivals), 'Admission did not release')
    parse = lambda value: dt.datetime.fromisoformat(value.replace('Z', '+00:00'))
    require(before['readCompletedNs'] < min(r['headersSentNs'] for r in concurrent['rounds'][0]['requests'])
            and max(parse(a['arrivedAt']) for a in arrivals) <= min(parse(a['releasedAt']) for a in arrivals),
            'Intent writes were not held together after the absent reading')
    return {'simultaneousIntentCreates': 2, 'receiptUID': concurrent['receipt']['UID']}


def run(*, client, admin, records, save, wait, job_uid):
    pending = wait(lambda: admin('/pending-first'), 60)
    payload = base64.b64decode(pending['payload'], validate=True)
    assert digest(payload) == pending['digest']
    name = pending['path'].removeprefix('/v1/results/')
    assert pending['path'] == '/v1/results/' + name and name.startswith('ptah-result-')
    before_records = records()
    assert not any(r['metadata']['name'] in (name, name + '-complete') for r in before_records)
    before = {'observedAt': dt.datetime.now(dt.timezone.utc).isoformat(),
              'readCompletedNs': time.monotonic_ns(),
              'records': [{'name': r['metadata']['name'], 'uid': r['metadata']['uid'],
                           'type': r['spec']['type']} for r in before_records]}
    save('before-first-publication.json', before)
    result = client.run(payload, name, None)
    barrier = admin('/evidence')
    rs = {r['metadata']['name']: r for r in records()}
    intent, complete, actual = publication(rs, job_uid)
    assert intent['metadata']['name'] == name and complete['metadata']['uid'] == result['receipt']['UID']
    assert actual == payload and digest(actual) == result['originalDigest']
    result['publicationUnchanged'] = True
    save('concurrent.json', result)
    value = {'evidenceVersion': 1, 'before': before, 'barrier': barrier, 'concurrent': result,
             'intentName': name, 'intentUID': intent['metadata']['uid'],
             'procedureSHA256': hashlib.sha256(pathlib.Path(__file__).read_bytes()).hexdigest()}
    save('first-publication.json', value)
    verify_evidence(value)
    print('PASS: both receiving managers reached intent CREATE before publication', flush=True)
