"""Verify retained native audit, including namespace-controller collection."""
import datetime as dt
import json
import gzip
from pathlib import Path


def instant(value):
    return dt.datetime.fromisoformat(value.replace('Z', '+00:00'))


def read_audit(root):
    compressed = root / 'record-audit.json.gz'
    if compressed.exists():
        return json.loads(gzip.decompress(compressed.read_bytes()))
    return json.loads((root / 'record-audit.json').read_text())


def verify(report, audit):
    assert report['status'] == 'FAIL'
    assert report['failure'].startswith('AssertionError: no eligible deletion or finalizer completion for ptah-result-key-')
    assert sorted(report['removedNamespaces']) == sorted(report['cases']) and len(report['cases']) == 2
    rows = []
    for namespace, case in report['cases'].items():
        events = [e for e in audit if e.get('objectRef', {}).get('namespace') == namespace and e['objectRef'].get('resource') == 'ptahresultrecords' and e['stage'] == 'ResponseComplete']
        successful = [e for e in events if 200 <= e.get('responseStatus', {}).get('code', 0) < 300]
        # NamespaceLifecycle forbids replacement. Confirm the audit has no
        # successful CREATE, so a later GET cannot refer to a replacement UID.
        assert not any(e['verb'] == 'create' for e in successful)
        assert len(case['before']) == 4
        for record in case['before']:
            name = record['name']
            timing = case['retirement'][name]
            deadline = instant(timing['deadline'])
            assert deadline >= max(instant(timing['retiredAt']), instant(record['createdAt'])) + dt.timedelta(hours=1)
            named = [e for e in successful if e['objectRef'].get('name') == name]
            reads = [e for e in named if e['verb'] == 'get' and instant(e['requestReceivedTimestamp']) >= deadline]
            assert reads, 'missing retained-object GET after the full deadline'
            mutations = [e for e in named if e['verb'] in ('delete', 'patch', 'update')]
            for event in mutations:
                if instant(event['requestReceivedTimestamp']) < deadline:
                    assert record['type'] in ('intent', 'credential') and event['verb'] == 'delete'
                    assert event['requestObject']['propagationPolicy'] == 'Foreground'
                    assert event['requestObject']['preconditions']['uid'] == record['uid']
            after = [e for e in mutations if instant(e['requestReceivedTimestamp']) >= deadline]
            collections = [e for e in successful if e['verb'] == 'deletecollection']
            for event in collections:
                assert instant(event['requestReceivedTimestamp']) >= deadline
                assert event['requestURI'] == '/apis/operator.ptah.run/v1alpha1/namespaces/' + namespace + '/ptahresultrecords'
                assert event['user']['username'] == 'system:serviceaccount:kube-system:namespace-controller'
            method = 'named mutation'
            if not after:
                assert record['type'] == 'credential' and collections
                after = collections
                method = 'namespace-controller DeleteCollection'
            rows.append({'namespace': namespace, 'name': name, 'uid': record['uid'], 'notBefore': timing['deadline'], 'retainedAfterDeadlineAuditIDs': [e['auditID'] for e in reads], 'firstEligibleMutationAt': min(e['requestReceivedTimestamp'] for e in after), 'mutationAuditIDs': [e['auditID'] for e in after], 'method': method})
    assert len(rows) == 8
    return {'status': 'passed', 'originalProbeStatus': report['status'], 'correction': 'The original verifier omitted DeleteCollection. Post-deadline successful GETs prove all original records survived the window; no successful CREATE permits UID replacement. Credentials were then collected by the namespace controller.', 'namespacesRemoved': report['removedNamespaces'], 'records': rows}


if __name__ == '__main__':
    root = Path(__file__).resolve().parent
    result = verify(json.loads((root / 'report.json').read_text()), read_audit(root))
    (root / 'verification.json').write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps({'status': result['status'], 'records': len(result['records'])}))
