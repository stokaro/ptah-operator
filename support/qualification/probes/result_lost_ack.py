"""Prove native migration SQL is not replayed after a lost result acknowledgment.

Run only against an owned disposable lab. The fixture temporarily replaces the
receiver Service route. It retains evidence and restores that route in finally.
"""
import base64, copy, datetime as dt, hashlib, json, os, pathlib, re, socket, ssl, subprocess, time, urllib.parse, urllib.request
from result_first_harvest import publication, pod_binding_record

def verify_evidence(value):
    """Reject evidence that could pass without a lost ACK or a single SQL effect."""

    def require(condition, message):
        if not condition:
            raise ValueError(message)
    binding = value['binding']
    proxy = value['proxy']
    version = value.get('evidenceVersion', 1)
    require(version in (1, 2, 3, 4, 5), 'Unknown evidence version')
    # Version 1 was the PostgreSQL-only probe. New executions name their engine.
    if version >= 2:
        require(value.get('engine') in ('PostgreSQL', 'MySQL'), 'Missing supported engine')
        calibration = value['rollbackCalibration']
        expected = '0:1:true'
        require(calibration['afterRollback'] == expected
                and calibration['afterReset'] == '0:1:false', 'Ineffective rollback witness')
        if value['engine'] == 'MySQL':
            require(calibration['storageEngine'] == 'InnoDB', 'Nontransactional MySQL witness')
    require(re.fullmatch('[0-9a-f]{40}', value['commit']), 'Missing source revision')
    require(re.fullmatch('[0-9a-f]{64}', value['procedureSHA256']), 'Missing procedure identity')
    require(binding['kind'] == 'PtahMigration' and binding['operation'] == 'migration-apply', 'The receipt does not describe migration Apply')
    for field, target in [('namespace', 'namespace'), ('uid', 'resourceUID'), ('jobUID', 'jobUID'), ('podUID', 'podUID')]:
        require(bool(binding[field]) and binding[field] == value[target], 'Operation binding changed: ' + field)
    require(binding['generation'] > 0 and bool(value['intentUID']) and bool(value['receiptUID']), 'Missing publication identity')
    require(value['converged'] is True and value['applyJobs'] == 1 and (value['podRestarts'] == 0) and (value['podPhase'] == 'Succeeded'), 'The original execution did not finish exactly once')
    require(value['executionPods'] == 1 and value['runnerAPICredentials'] is False, 'Replacement Pod or runner API credentials')
    require(value['databaseBeforeExecution'] == '0:1:false', 'Database witness was not empty')
    require(value['databaseBeforeRelease'] == '1:1:true' and value['databaseAfterRelease'] == '1:1:true', 'SQL was absent, repeated, or retried and rolled back')
    require(proxy['dropped'] is True and proxy['released'] is True and proxy['preflights'] == 1, 'No single-execution acknowledgment-loss fault')
    if version >= 4:
        require(version == 5 or 'receiverRestart' not in value, 'Receiver restart needs version 5 evidence')
        require(value.get('authentication') == proxy.get('authentication') == 'pod-token', 'Missing Pod-token authentication')
        require(not proxy.get('clientCertificateDigest') and not value.get('credentialCertificateDigest'), 'Certificate evidence substituted for Pod-token authority')
        require(bool(value.get('credentialUID')) and proxy.get('jobUID') == binding['jobUID']
                and proxy.get('podUID') == binding['podUID'], 'Original authenticated Job or Pod changed')
        require(re.fullmatch('sha256:[0-9a-f]{64}', value.get('credentialIdentityDigest', ''))
                and proxy.get('identityDigest') == value['credentialIdentityDigest'], 'Public Pod binding changed')
        require(value.get('tokenProjection') == {'audience': 'operator.ptah.run/results', 'expirationSeconds': 3600,
                                                'path': 'token'}, 'Runner token is not receiver-only')
    else:
        require(re.fullmatch('sha256:[0-9a-f]{64}', proxy['clientCertificateDigest']) and proxy['clientCertificateDigest'] == value['credentialCertificateDigest'], 'Missing or changed original client certificate identity')
    attempts = proxy['attempts']
    require(len(attempts) == 2, 'Expected the original delivery and one retry')
    receipt = attempts[0]['receipt']
    require(receipt == attempts[1]['receipt'], 'Retry returned a different durable receipt')
    require(receipt['Name'] == value['receiptName'] and receipt['UID'] == value['receiptUID'] and (receipt['Digest'] == value['payloadDigest']) and (receipt['Size'] == value['payloadBytes'] > 0), 'Proxy evidence does not match persisted publication')
    times = [dt.datetime.fromisoformat(a['receivedAt'].replace('Z', '+00:00')) for a in attempts]
    require(times[0] < times[1] <= dt.datetime.fromisoformat(value['completedAt']), 'Retry timing does not precede completed acceptance')
    if version in (3, 5):
        restart = value['receiverRestart']
        old, new = restart['before'], restart['after']
        require(len(old) == len(new) == 2 and len(set(old)) == len(set(new)) == 2
                and all(old) and all(new) and set(old).isdisjoint(new), 'Receiver processes were not replaced')
        require(restart['oldPodsAbsent'] is True and restart['newPodsReady'] is True,
                'Receiver replacement was incomplete')
        require(restart['receiptUIDBeforeRestart'] == value['receiptUID']
                and restart['databaseBeforeRestart'] == '1:1:true', 'No durable SQL result before restart')
        require(proxy.get('retryGateEnabled') is True and proxy.get('retryWaits', 0) > 0,
                'No retry held before receiver replacement')
        removed = dt.datetime.fromisoformat(restart['oldPodsAbsentAt'])
        ready = dt.datetime.fromisoformat(restart['newPodsReadyAt'])
        resumed = dt.datetime.fromisoformat(proxy['retryResumedAt'].replace('Z', '+00:00'))
        require(times[0] < removed <= ready < resumed < times[1], 'Retry reached receivers before replacement')
    require(any((c['type'] == 'Ready' and c['status'] == 'True' and (c.get('reason') == 'HistoryMatched') and (c.get('observedGeneration') == binding['generation']) for c in value['conditions'])), 'No current-generation database convergence')
    return {'deliveries': 2, 'sqlExecutions': 1, 'receiptUID': receipt['UID']}

def main():
    if not __debug__:
        raise RuntimeError('Run without Python optimization; acceptance assertions are required')
    E = os.environ
    restart_receiver = E.get('RESULT_PROBE_RESTART_RECEIVER', '0')
    if restart_receiver not in ('0', '1'):
        raise ValueError('RESULT_PROBE_RESTART_RECEIVER must be 0 or 1')
    restart_receiver = restart_receiver == '1'
    runner_loss = E.get('RESULT_PROBE_RUNNER_LOSS', '0')
    if runner_loss not in ('0', '1') or (runner_loss == '1' and restart_receiver):
        raise ValueError('Select either runner loss or receiver replacement')
    runner_loss = runner_loss == '1'
    partial_loss = E.get('RESULT_PROBE_PARTIAL_LOSS', '0')
    if partial_loss not in ('0', '1') or (partial_loss == '1' and not runner_loss):
        raise ValueError('Partial loss requires the runner-loss case')
    partial_loss = partial_loss == '1'
    first_publication = E.get('RESULT_PROBE_FIRST_PUBLICATION', '0')
    if first_publication not in ('0', '1'):
        raise ValueError('RESULT_PROBE_FIRST_PUBLICATION must be 0 or 1')
    first_publication = first_publication == '1'
    concurrent = E.get('RESULT_PROBE_CONCURRENT', '1' if first_publication else '0')
    if concurrent not in ('0', '1') or (concurrent == '1' and (runner_loss or restart_receiver)):
        raise ValueError('Concurrent delivery must run separately from replacement or loss')
    concurrent = concurrent == '1'
    authority = E.get('RESULT_PROBE_AUTHORITY', '0')
    if authority not in ('0', '1') or (authority == '1' and (not concurrent or first_publication)):
        raise ValueError('Authority refusals require committed concurrent setup without first-publication mode')
    authority = authority == '1'
    if first_publication and not concurrent:
        raise ValueError('First publication requires concurrent delivery')
    upload_budget = E.get('RESULT_PROBE_UPLOAD_BUDGET', '0')
    if upload_budget not in ('0', '1') or (upload_budget == '1' and (runner_loss or restart_receiver or concurrent)):
        raise ValueError('Upload budget must run separately from other faults')
    upload_budget = upload_budget == '1'
    family = E.get('RESULT_PROBE_FAMILY', 'PtahMigration')
    if family not in ('PtahMigration', 'PtahSchema') or (family == 'PtahSchema' and not upload_budget):
        raise ValueError('Schema probes require the upload-budget case')
    schema_budget = family == 'PtahSchema'
    authentication = E.get('RESULT_PROBE_AUTH', 'certificate')
    if authentication not in ('certificate', 'pod-token'):
        raise ValueError('RESULT_PROBE_AUTH must be certificate or pod-token')
    pod_token = authentication == 'pod-token'
    if authority and not pod_token:
        raise ValueError('Authority refusals require Pod-token authentication')
    if pod_token and (runner_loss or first_publication):
        raise ValueError('Pod-token mode measures lost ACK, receiver replacement, concurrent redelivery, and upload bounds')
    apply_prefix = 'ptah-apply-' if schema_budget else 'ptah-m-apply-'
    empty_witness = {'columns': [], 'primaryKeyColumns': []} if schema_budget else '0:1:false'
    engine = E['RESULT_PROBE_ENGINE']
    if engine not in ('PostgreSQL', 'MySQL'):
        raise ValueError('RESULT_PROBE_ENGINE must be PostgreSQL or MySQL')
    ns = E['RESULT_PROBE_NAMESPACE']
    database = E['RESULT_PROBE_DATABASE']
    source = E['E2E_TEST_NAMESPACE']
    opns = E['E2E_OPERATOR_NAMESPACE']
    out = pathlib.Path(E['RESULT_PROBE_EVIDENCE_DIR'])
    out.mkdir(parents=True, exist_ok=True)
    assert re.fullmatch('[a-z][a-z0-9-]{1,45}', ns) and re.fullmatch('[a-z][a-z0-9_]{1,45}', database)
    fixture = E['RESULT_PROBE_FIXTURE_IMAGE']
    assert re.fullmatch(re.escape(E['E2E_REGISTRY_HOST']) + '/e2e-fixture@sha256:[0-9a-f]{64}', fixture)

    def run(argv, data=None):
        p = subprocess.run(argv, input=data, text=True, capture_output=True, timeout=360)
        if p.returncode:
            raise RuntimeError(argv[0] + ' failed: ' + p.stderr[-1400:])
        return p.stdout

    def k(*args, data=None, namespace=ns):
        return run(['kubectl', '--kubeconfig', E['E2E_KUBECONFIG'], '--request-timeout=30s', '-n', namespace, *args], data)

    def get(kind, name=None, namespace=ns):
        return json.loads(k('get', kind, *([name] if name else []), '-o', 'json', namespace=namespace))

    def create(o):
        return json.loads(k('create', '-f', '-', '-o', 'json', data=json.dumps(o), namespace=o.get('metadata', {}).get('namespace', ns)))

    def wait(fn, seconds=240):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            value = fn()
            if value:
                return value
            time.sleep(0.2)
        raise RuntimeError('bounded wait failed: ' + fn.__name__)

    def save(name, value):
        (out / name).write_text(json.dumps(value, indent=2) + '\n')

    def dec(r):
        return json.loads(base64.b64decode(r['spec']['data'], validate=True))

    def records():
        return get('ptahresultrecords')['items']

    def sql(query, db=None):
        if engine == 'MySQL':
            return k('exec', '-i', 'deployment/demo-mysql', '--', 'sh', '-ec',
                     'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql --protocol=TCP -h 127.0.0.1 -u root --batch --skip-column-names "$1"',
                     'probe', db or database, data=query + '\n', namespace=source).strip()
        return run(['docker', '--context', E['E2E_DOCKER_CONTEXT'], 'exec', '-i', E['E2E_EXTERNAL_POSTGRES_CONTAINER_ID'], 'sh', '-ec', 'PGPASSWORD="$POSTGRES_PASSWORD" exec psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$1" -At -v ON_ERROR_STOP=1', 'probe', db or database], query + '\n').strip()

    def witness(db=None):
        if schema_budget:
            from result_upload_budget import schema_witness
            return schema_witness(sql, engine, db or database)
        if engine == 'MySQL':
            # Read the allocated counter, including rolled-back attempts. Cached
            # information_schema statistics would hide a replay.
            return sql("SET SESSION information_schema_stats_expiry=0; "
                       "SELECT CONCAT((SELECT COUNT(*) FROM delivery_probe_calls), ':', "
                       "GREATEST(AUTO_INCREMENT - 1, 1), ':', "
                       "IF(AUTO_INCREMENT > 1, 'true', 'false')) "
                       "FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() "
                       "AND TABLE_NAME='delivery_probe_calls';", db).splitlines()[-1]
        return sql("SELECT (SELECT count(*) FROM delivery_probe_calls)::text || ':' || last_value::text || ':' || is_called::text FROM delivery_probe_sequence;", db).splitlines()[-1]
    endpoint = run(['docker', '--context', E['E2E_DOCKER_CONTEXT'], 'context', 'inspect', E['E2E_DOCKER_CONTEXT'], '--format', '{{.Endpoints.docker.Host}}']).strip()
    assert endpoint == E['E2E_DOCKER_ENDPOINT']
    nodes = get('nodes')['items']
    assert nodes
    for node in nodes:
        name = node['metadata']['name']
        owner = run(['docker', '--context', E['E2E_DOCKER_CONTEXT'], 'inspect', '--format', '{{index .Config.Labels "io.x-k8s.kind.cluster"}}', name]).strip()
        assert owner == E['E2E_KIND_CLUSTER_NAME']
        config = json.loads(k('get', '--raw', '/api/v1/nodes/' + name + '/proxy/configz'))
        assert config['kubeletconfig']['containerLogMaxSize'] == '10Mi'
    if E.get('RESULT_PROBE_PARTIAL_CHECKPOINT'):
        if not partial_loss:
            raise ValueError('A checkpoint requires the partial-loss case')
        checkpoint = json.loads(pathlib.Path(E['RESULT_PROBE_PARTIAL_CHECKPOINT']).read_text())
        source_commit = E['RESULT_PROBE_PARTIAL_CHECKPOINT_COMMIT']
        assert re.fullmatch('[0-9a-f]{40}', source_commit)
        assert checkpoint['namespace'] == ns and checkpoint['engine'] == engine and checkpoint['commit'] == E['E2E_CONTROLLER_REVISION']
        assert set(checkpoint['procedureSHA256']) == {'result_partial_loss.py', 'result_lost_ack.py'}
        for filename, expected in checkpoint['procedureSHA256'].items():
            source_bytes = subprocess.check_output(['git', 'show', source_commit + ':support/qualification/probes/' + filename])
            assert hashlib.sha256(source_bytes).hexdigest() == expected
        assert get('namespace', ns)['metadata']['labels']['operator.ptah.run/acceptance-owner'] == E['E2E_KIND_CLUSTER_NAME']
        resource = get('ptahmigration', 'lost-ack')
        assert resource['metadata']['uid'] == checkpoint['resourceUID']
        operation = {'id': checkpoint['operationID'], 'jobUID': checkpoint['jobUID'],
                     'jobName': checkpoint['unknown']['status']['unresolvedRun']['jobName']}
        from result_partial_loss import run as run_partial
        run_partial(k=k, get=get, create=create, wait=wait, save=save, witness=witness, records=records,
                    open_gate=None, resource=resource, pod={'metadata': {'uid': checkpoint['podUID']}},
                    operation=operation, engine=engine, environment=E, calibration=checkpoint['calibration'],
                    sql=sql, migration_sql=get('configmap', 'ack-migrations')['data']['0000000001_record_delivery.up.sql'].strip(),
                    publish_template=get('job', 'repaired-publish')['spec']['template'],
                    initial=checkpoint['initial'], resume=checkpoint)
        return
    services = [s for s in get('services', namespace=opns)['items'] if any((p.get('targetPort') == 'results' for p in s['spec']['ports']))]
    assert len(services) == 1
    service = services[0]
    service_name = service['metadata']['name']
    original_selector = service['spec']['selector']
    save('receiver-service-before.json', service)
    trusts = [s for s in get('secrets', namespace=opns)['items'] if s['metadata'].get('labels', {}).get('operator.ptah.run/result-trust') == 'projection']
    assert len(trusts) == 1
    trust = trusts[0]
    manager = get('deployment', E['E2E_CONTROLLER_NAME'], opns)
    selector = ','.join((a + '=' + b for a, b in original_selector.items()))
    managers = json.loads(k('get', 'pods', '-l', selector, '-o', 'json', namespace=opns))['items']
    assert len(managers) == 2 and all((any((c['type'] == 'Ready' and c['status'] == 'True' for c in p['status']['conditions'])) for p in managers))
    backend = managers[0]
    host = service_name + '.' + opns + '.svc'
    create({'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': ns, 'labels': {'operator.ptah.run/acceptance-owner': E['E2E_KIND_CLUSTER_NAME']}}})
    for kind, name in [('secret', 'demo-registry'), ('secret', 'demo-registry-pull'), ('configmap', 'demo-verification-policy' if schema_budget else 'demo-migration-verification-policy')]:
        o = get(kind, name, source)
        o['metadata'] = {'name': name, 'namespace': ns}
        create(o)
    k('patch', 'serviceaccount', 'default', '--type=merge', '-p', json.dumps({'imagePullSecrets': [{'name': 'demo-registry-pull'}]}))
    if engine == 'MySQL':
        creds = {key: base64.b64decode(value).decode() for key, value in get('secret', 'demo-mysql-database', source)['data'].items()}
        assert creds['username'] == 'demo'
        sql('CREATE DATABASE `' + database + '`; GRANT ALL ON `' + database + '`.* TO \'demo\'@\'%\';', 'mysql')
        migration_sql = 'INSERT INTO delivery_probe_calls VALUES (NULL);'
        if not schema_budget:
            sql('CREATE TABLE delivery_probe_calls(n BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY) ENGINE=InnoDB;')
            storage_engine = sql("SELECT ENGINE FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='delivery_probe_calls';")
            assert storage_engine == 'InnoDB'
    else:
        creds = json.loads(pathlib.Path(E['E2E_EXTERNAL_POSTGRES_CREDENTIALS_FILE']).read_text())
        owner = '"' + creds['username'].replace('"', '""') + '"'
        sql('CREATE DATABASE ' + database + ' OWNER ' + owner + ';', urllib.parse.urlsplit(creds['url']).path[1:])
        migration_sql = "INSERT INTO delivery_probe_calls(n) VALUES (nextval('delivery_probe_sequence'));"
        if not schema_budget:
            sql('SET ROLE ' + owner + '; CREATE SEQUENCE delivery_probe_sequence; CREATE TABLE delivery_probe_calls(n bigint NOT NULL);')
            storage_engine = 'PostgreSQL sequence'
    assert witness() == empty_witness
    if schema_budget:
        calibration = {'before': witness()}
    else:
        sql('BEGIN; ' + migration_sql + ' ROLLBACK;')
        calibration = {'storageEngine': storage_engine, 'afterRollback': witness()}
        assert calibration['afterRollback'] == '0:1:true'
        sql('TRUNCATE delivery_probe_calls;' if engine == 'MySQL' else 'ALTER SEQUENCE delivery_probe_sequence RESTART WITH 1;')
        calibration['afterReset'] = witness()
        assert calibration['afterReset'] == '0:1:false'
    server_version = sql('SELECT VERSION();')
    url = urllib.parse.urlunsplit(urllib.parse.urlsplit(creds['url'])._replace(path='/' + database))
    create({'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'ack-database', 'namespace': ns}, 'stringData': {'url': url}})
    artifact_sql = ('-- +ptah no_transaction\n' + migration_sql + '\nINSERT INTO qualification_missing_table VALUES (1);\n') if partial_loss else migration_sql + '\n'
    create({'apiVersion': 'v1', 'kind': 'ConfigMap', 'metadata': {'name': 'ack-migrations', 'namespace': ns}, 'data': ({'schema.sql': 'CREATE TABLE qualification_probe (id bigint NOT NULL PRIMARY KEY, email text NOT NULL);\n'} if schema_budget else {'0000000001_record_delivery.up.sql': artifact_sql, '0000000001_record_delivery.down.sql': 'DELETE FROM delivery_probe_calls;\n'})})
    if pod_token:
        security = {'allowPrivilegeEscalation': False, 'readOnlyRootFilesystem': True,
                    'capabilities': {'drop': ['ALL']}}
        container = {'name': 'publisher', 'image': E['E2E_EXECUTOR_IMAGE'], 'securityContext': security,
                     'resources': {'requests': {'cpu': '100m', 'memory': '32Mi'},
                                   'limits': {'cpu': '1', 'memory': '256Mi'}},
                     'env': [{'name': 'HOME', 'value': '/work'}, {'name': 'TMPDIR', 'value': '/work'}] + [
                         {'name': 'PTAH_OCI_' + name.upper(), 'valueFrom': {'secretKeyRef': {
                         'name': 'demo-registry', 'key': name}}} for name in ('registry', 'username', 'password')],
                     'volumeMounts': [{'name': 'schema', 'mountPath': '/schema', 'readOnly': True},
                                      {'name': 'work', 'mountPath': '/work'}]}
        template = {'spec': {'restartPolicy': 'Never', 'automountServiceAccountToken': False,
                    'imagePullSecrets': [{'name': 'demo-registry-pull'}],
                    'securityContext': {'runAsNonRoot': True, 'runAsUser': 65532, 'runAsGroup': 65532,
                                        'fsGroup': 65532, 'seccompProfile': {'type': 'RuntimeDefault'}},
                    'containers': [container], 'volumes': [
                        {'name': 'schema', 'configMap': {'name': 'ack-migrations'}},
                        {'name': 'work', 'emptyDir': {'sizeLimit': '64Mi'}}]}}
    else:
        original = get('job', 'result-schema-publish', source)
        template = copy.deepcopy(original['spec']['template'])
        template['metadata'] = {}
        container = template['spec']['containers'][0]
    container['command'] = ['/bin/sh', '-ec']
    container['args'] = ['mkdir -p /work/migrations; cp -L /schema/*.sql /work/migrations/; exec /usr/local/bin/ptah "$@"', 'publisher', 'migrations', 'push', 'oci://' + E['E2E_REGISTRY_HOST'] + '/migrations/demo:' + ns, '--migrations-dir', '/work/migrations', '--dir-format', 'ptah', '--version', ns, '--plain-http']
    if schema_budget:
        container['command'] = ['/usr/local/bin/ptah']
        container['args'] = ['schema', 'push', 'oci://' + E['E2E_REGISTRY_HOST'] + '/schemas/demo:' + ns, '--schema-file', '/schema/schema.sql', '--dialect', 'mysql' if engine == 'MySQL' else 'postgres', '--plain-http']
    for v in template['spec']['volumes']:
        if v['name'] == 'schema':
            v['configMap'] = {'name': 'ack-migrations'}
    create({'apiVersion': 'batch/v1', 'kind': 'Job', 'metadata': {'name': 'ack-publish', 'namespace': ns}, 'spec': {'backoffLimit': 0, 'activeDeadlineSeconds': 300, 'template': template}})
    k('wait', '--for=condition=complete', 'job/ack-publish', '--timeout=300s')
    digest = re.search('^Digest: (sha256:[0-9a-f]{64})$', k('logs', 'job/ack-publish'), re.M).group(1)
    gate = ns + '-gate'
    gated = False
    routed = False
    pf = None
    copied = False
    proxy_created = False
    backend_service = None
    receiver_restart = None
    concurrent_client = None
    publication_webhook = None
    publication_port_added = False
    proxy_name = ns + '-proxy'
    client_secret = ns + '-client'
    scheduling_node = None
    scheduling_label_present = False

    def restore_service():
        assert get('service', service_name, opns)['metadata']['uid'] == service['metadata']['uid']
        k('patch', 'service', service_name, '--type=json', '-p', json.dumps([{'op': 'replace', 'path': '/spec/selector', 'value': original_selector}, {'op': 'replace', 'path': '/spec/ports', 'value': service['spec']['ports']}]), namespace=opns)

    def endpoints_are(expected, name=service_name):
        slices = json.loads(k('get', 'endpointslices', '-l', 'kubernetes.io/service-name=' + name, '-o', 'json', namespace=opns))['items']
        uids = {e.get('targetRef', {}).get('uid') for s in slices for e in s.get('endpoints', []) if e.get('conditions', {}).get('ready')}
        return uids == expected
    try:
        gate_message = 'Acceptance probe holds migration Apply credentials'
        gate_resource = 'secrets'
        gate_expression = ("!has(object.metadata.annotations) || !('operator.ptah.run/result-pod-name' in object.metadata.annotations) || "
                           "!object.metadata.annotations['operator.ptah.run/result-pod-name'].startsWith('" + apply_prefix + "')")
        if pod_token:
            gate_resource = 'pods'
            gate_message = 'Acceptance probe holds migration Apply Pods'
            gate_expression = ("!has(object.metadata.labels) || "
                               "!('operator.ptah.run/operation' in object.metadata.labels) || "
                               "object.metadata.labels['operator.ptah.run/operation'] != 'apply'")
        create({'apiVersion': 'admissionregistration.k8s.io/v1', 'kind': 'ValidatingAdmissionPolicy',
                'metadata': {'name': gate}, 'spec': {'failurePolicy': 'Fail', 'matchConstraints': {
                    'resourceRules': [{'apiGroups': [''], 'apiVersions': ['v1'], 'operations': ['CREATE'],
                                       'resources': [gate_resource]}]},
                    'validations': [{'expression': gate_expression, 'message': gate_message}]}})
        gated = True
        create({'apiVersion': 'admissionregistration.k8s.io/v1', 'kind': 'ValidatingAdmissionPolicyBinding', 'metadata': {'name': gate}, 'spec': {'policyName': gate, 'validationActions': ['Deny'], 'matchResources': {'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': ns}}}}})

        def gate_ready():
            o = {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'gate-probe', 'namespace': ns, 'annotations': {'operator.ptah.run/result-pod-name': apply_prefix + 'probe'}}}
            if pod_token:
                o = {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': 'gate-probe', 'namespace': ns,
                     'labels': {'operator.ptah.run/operation': 'apply'}},
                     'spec': {'automountServiceAccountToken': False, 'restartPolicy': 'Never',
                              'containers': [{'name': 'probe', 'image': fixture}]}}
            p = subprocess.run(['kubectl', '--kubeconfig', E['E2E_KUBECONFIG'], 'create', '--dry-run=server', '-f', '-'], input=json.dumps(o), text=True, capture_output=True, timeout=30)
            return p.returncode and gate_message in p.stderr
        wait(gate_ready, 30)
        if pod_token:
            migration = {'apiVersion': 'operator.ptah.run/v1alpha1', 'kind': 'PtahMigration', 'spec': {
                'target': {'engine': engine, 'coordinationKey': 'acceptance/' + ns,
                           'urlFrom': {'name': 'ack-database', 'key': 'url'}},
                'artifact': {'ociRef': 'oci://' + E['E2E_REGISTRY_HOST'] + '/migrations/demo@' + digest,
                             'registryAuthFrom': {'name': 'demo-registry', 'mode': 'Environment',
                                                  'usernameKey': 'username', 'passwordKey': 'password'},
                             'verificationPolicyFrom': {'name': 'demo-migration-verification-policy', 'key': 'policy.yaml'},
                             'transport': {'plainHTTP': True}},
                'policy': {'apply': 'Always', 'lockTimeout': '30s'}, 'interval': '2h',
                'execution': {'activeDeadlineSeconds': 900, 'failureRetryInterval': '10s', 'connectTimeout': '30s'}}}
        else:
            env = E.copy()
            env.update(APPLY='OnApproval' if partial_loss else 'Always', INTERVAL='30s' if runner_loss else '2h')
            manifest = subprocess.run(['demo/bin/lab', 'manifest', 'shipments', digest], env=env, text=True, capture_output=True, check=True).stdout
            migration = json.loads(k('create', '--dry-run=client', '--validate=false', '-f', '-', '-o', 'json', data=manifest, namespace=source))
        if schema_budget:
            if pod_token:
                migration['kind'] = 'PtahSchema'
                migration['spec']['desired'] = migration['spec'].pop('artifact')
                migration['spec']['desired']['verificationPolicyFrom']['name'] = 'demo-verification-policy'
                migration['spec']['policy'] = {'apply': 'Always', 'allowDestructive': False, 'driftSeverity': 'all'}
            else:
                original_schema = get('ptahschema', 'storefront', source)
                migration = {'apiVersion': original_schema['apiVersion'], 'kind': 'PtahSchema', 'spec': copy.deepcopy(original_schema['spec'])}
            migration['spec']['desired']['ociRef'] = 'oci://' + E['E2E_REGISTRY_HOST'] + '/schemas/demo@' + digest
            migration['spec']['policy']['apply'] = 'Always'
            migration['spec']['interval'] = '2h'
            migration['spec']['suspend'] = False
        migration['metadata'] = {'name': 'lost-ack', 'namespace': ns}
        migration['spec']['target']['coordinationKey'] = 'acceptance/' + ns
        migration['spec']['target']['urlFrom']['name'] = 'ack-database'
        migration['spec']['target']['engine'] = engine
        migration['spec']['execution']['activeDeadlineSeconds'] = 900
        if pod_token and upload_budget:
            from result_upload_budget import scheduling_node as select_node, set_scheduling_label, SCHEDULING_LABEL
            # Resolve/Plan run normally. Once the Apply Job reaches the Pod gate,
            # remove this task-owned label to hold that original Pod before SQL.
            node_selector = migration['spec']['execution'].setdefault('nodeSelector', {})
            assert SCHEDULING_LABEL not in node_selector
            scheduling_node = select_node(get('nodes')['items'], node_selector)
            set_scheduling_label(k, scheduling_node, ns)
            scheduling_label_present = True
            node_selector[SCHEDULING_LABEL] = ns
        resource = create(migration)
        if partial_loss:
            from result_partial_loss import authorize_initial
            initial_authorization = authorize_initial(k, get, create, wait, resource)

        if pod_token:
            def held_job():
                op = get(family.lower(), 'lost-ack').get('status', {}).get('activeOperation') or {}
                if op.get('jobName', '').startswith(apply_prefix) and op.get('jobUID'):
                    return op
                return None
            operation = wait(held_job, 300)
            job_name, job_uid = operation['jobName'], operation['jobUID']
            job = get('job', job_name)
            assert job['metadata']['uid'] == job_uid
            assert not any(any(o['uid'] == job_uid for o in p['metadata'].get('ownerReferences', []))
                           for p in get('pods')['items']), 'Apply Pod escaped the gate'
            credential, certificate_digest = None, None
        else:
            def held():
                matches = [r for r in records() if r['spec']['type'] == 'credential' and r['metadata'].get('annotations', {}).get('operator.ptah.run/result-pod-name', '').startswith(apply_prefix)]
                return matches[0] if len(matches) == 1 else None
            credential = wait(held, 300)
            certificate_der = ssl.PEM_cert_to_DER_cert(base64.b64decode(dec(credential)['tls.crt']).decode())
            certificate_digest = 'sha256:' + hashlib.sha256(certificate_der).hexdigest()
            pod_name = credential['metadata']['annotations']['operator.ptah.run/result-pod-name']
            pod = get('pod', pod_name)
            assert pod['status']['phase'] == 'Pending'
            assert pod['spec']['automountServiceAccountToken'] is False
            assert not any('serviceAccountToken' in source for volume in pod['spec'].get('volumes', []) for source in volume.get('projected', {}).get('sources', []))
            operation = get(family.lower(), 'lost-ack')['status']['activeOperation']
            job_name = operation['jobName']
            job_uid = operation['jobUID']
        assert witness() == empty_witness
        def open_execution_gate():
            nonlocal gated, scheduling_label_present
            if scheduling_node and not scheduling_label_present:
                set_scheduling_label(k, scheduling_node, ns)
                scheduling_label_present = True
            k('delete', 'validatingadmissionpolicybinding', gate)
            k('delete', 'validatingadmissionpolicy', gate)
            gated = False

        if upload_budget:
            from result_upload_budget import run as run_budget
            token_args = {}
            if pod_token:
                set_scheduling_label(k, scheduling_node, ns, remove=True)
                scheduling_label_present = False
                assert not any(n['metadata'].get('labels', {}).get(SCHEDULING_LABEL) == ns
                               for n in get('nodes')['items'])
                k('patch', 'validatingadmissionpolicy', gate, '--type=json', '-p', json.dumps([
                    {'op': 'replace', 'path': '/spec/validations/0/expression', 'value': 'true'}]))
                def pending_pod_and_binding():
                    pods = [p for p in get('pods')['items'] if any(o['uid'] == job_uid for o in p['metadata'].get('ownerReferences', []))]
                    assert len(pods) <= 1, 'Apply created a replacement Pod'
                    bindings = [r for r in records() if r['spec']['type'] == 'credential'
                                and r['metadata'].get('annotations', {}).get('operator.ptah.run/result-job-uid') == job_uid]
                    assert len(bindings) <= 1
                    return (pods[0], bindings[0]) if pods and bindings else None
                pod, credential = wait(pending_pod_and_binding, 60)
                assert pod['status']['phase'] == 'Pending' and not pod['spec'].get('nodeName')
                assert credential['spec'] == pod_binding_record(job, pod)['spec']
                assert witness() == empty_witness
                request = {'apiVersion': 'authentication.k8s.io/v1', 'kind': 'TokenRequest', 'spec': {
                    'audiences': ['operator.ptah.run/results'], 'expirationSeconds': 600,
                    'boundObjectRef': {'apiVersion': 'v1', 'kind': 'Pod', 'name': pod['metadata']['name'],
                                       'uid': pod['metadata']['uid']}}}
                # The privileged harness issues a receiver-only token for the
                # held Pod. It never writes that token to a file or command line.
                issued = json.loads(k('create', '--raw', '/api/v1/namespaces/' + ns + '/serviceaccounts/' +
                                     pod['spec']['serviceAccountName'] + '/token', '-f', '-', data=json.dumps(request)))
                token_args = {'pod_token': issued['status']['token'],
                              'identity': base64.b64decode(credential['spec']['data'], validate=True),
                              'scheduling_gate': {'nodeUID': scheduling_node['metadata']['uid'],
                                                  'podUID': pod['metadata']['uid'], 'pendingBeforeUploads': True}}
                del issued
                save('pod-binding.json', credential)
            run_budget(k=k, get=get, create=create, wait=wait, save=save, witness=witness,
                       records=records, open_gate=open_execution_gate, resource=resource, pod=pod,
                       operation=operation, engine=engine, environment=E, calibration=calibration,
                       managers=managers, host=host,
                       credential={'ca.crt': trust['data']['ca.crt']} if pod_token else dec(credential), service=service,
                       restore_service=restore_service, endpoints_are=endpoints_are, gate=gate,
                       sql=sql, creds=creds, database=database, migration_sql=migration_sql, **token_args)
            token_args.clear()
            return
        if partial_loss:
            from result_partial_loss import run as run_partial
            run_partial(k=k, get=get, create=create, wait=wait, save=save, witness=witness,
                        records=records, open_gate=open_execution_gate, resource=resource, pod=pod,
                        operation=operation, engine=engine, environment=E, calibration=calibration,
                        sql=sql, migration_sql=migration_sql, publish_template=template,
                        initial=initial_authorization)
            return
        if runner_loss:
            from result_runner_loss import run as run_loss
            run_loss(k=k, get=get, create=create, wait=wait, save=save, witness=witness,
                     records=records, open_gate=open_execution_gate, resource=resource, pod=pod,
                     operation=operation, engine=engine, environment=E, calibration=calibration)
            return
        if not pod_token:
            create({'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': client_secret, 'namespace': opns}, 'type': 'kubernetes.io/tls', 'immutable': True, 'data': dec(credential)})
            copied = True
        spec = {'restartPolicy': 'Never', 'automountServiceAccountToken': False, 'securityContext': {'runAsNonRoot': True, 'runAsUser': 65532, 'runAsGroup': 65532, 'fsGroup': 65532, 'seccompProfile': {'type': 'RuntimeDefault'}}, 'imagePullSecrets': manager['spec']['template']['spec'].get('imagePullSecrets', []), 'containers': [{'name': 'proxy', 'image': fixture, 'command': ['/e2e-handcraft-oci'], 'args': ['result-ack-proxy', '--backend-address=' + backend['status']['podIP'] + ':9444', '--server-name=' + host, '--trust-directory=/trust', '--credential-directory=/credential'], 'ports': [{'name': 'results', 'containerPort': 9444}], 'securityContext': {'allowPrivilegeEscalation': False, 'readOnlyRootFilesystem': True, 'capabilities': {'drop': ['ALL']}}, 'resources': {'requests': {'cpu': '20m', 'memory': '32Mi'}, 'limits': {'cpu': '500m', 'memory': '256Mi'}}, 'volumeMounts': [{'name': 'trust', 'mountPath': '/trust', 'readOnly': True}, {'name': 'credential', 'mountPath': '/credential', 'readOnly': True}]}], 'volumes': [{'name': 'trust', 'secret': {'secretName': trust['metadata']['name'], 'defaultMode': 288, 'items': [{'key': key, 'path': key} for key in ['tls.crt', 'tls.key', 'client-trust.crt']]}}, {'name': 'credential', 'secret': {'secretName': client_secret, 'defaultMode': 288}}]}
        if pod_token:
            spec['containers'][0]['args'][-1] = '--job-uid=' + job_uid
            spec['containers'][0]['volumeMounts'] = [m for m in spec['containers'][0]['volumeMounts'] if m['name'] == 'trust']
            spec['volumes'] = [v for v in spec['volumes'] if v['name'] == 'trust']
            spec['volumes'][0]['secret']['items'] = [{'key': key, 'path': key} for key in ('tls.crt', 'tls.key', 'ca.crt')]
        if restart_receiver:
            backend_service = create({'apiVersion': 'v1', 'kind': 'Service',
                                      'metadata': {'name': ns + '-backend', 'namespace': opns},
                                      'spec': {'selector': original_selector,
                                               'ports': [{'port': 9444, 'targetPort': 'results'}]}})
            wait(lambda: endpoints_are({p['metadata']['uid'] for p in managers}, backend_service['metadata']['name']), 30)
            spec['containers'][0]['args'][1] = '--backend-address=' + backend_service['spec']['clusterIP'] + ':9444'
            spec['containers'][0]['args'].append('--pause-retry')
        if concurrent:
            from result_concurrent import Client
            if not pod_token:
                concurrent_client = Client(E, managers, host, dec(credential))
            spec['containers'][0]['args'].append('--pause-first-namespace=' + ns if first_publication else '--pause-retry')
        proxy = create({'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': proxy_name, 'namespace': opns, 'labels': {'acceptance-proxy': ns}}, 'spec': spec})
        proxy_created = True
        k('wait', '--for=condition=Ready', 'pod/' + proxy_name, '--timeout=120s', namespace=opns)
        sock = socket.socket()
        sock.bind(('127.0.0.1', 0))
        port = sock.getsockname()[1]
        sock.close()
        pf_log = open(out / 'proxy-port-forward.log', 'w')
        pf = subprocess.Popen(['kubectl', '--kubeconfig', E['E2E_KUBECONFIG'], '-n', opns, 'port-forward', 'pod/' + proxy_name, str(port) + ':8081'], stdout=pf_log, stderr=subprocess.STDOUT)

        def admin(path, post=False):
            try:
                with urllib.request.urlopen(urllib.request.Request('http://127.0.0.1:' + str(port) + path, data=b'' if post else None), timeout=2) as r:
                    return json.loads(r.read()) if not post else r.status
            except (OSError, ValueError):
                return None
        wait(lambda: admin('/evidence'), 15)
        k('patch', 'service', service_name, '--type=json', '-p', json.dumps([{'op': 'replace', 'path': '/spec/selector', 'value': {'acceptance-proxy': ns}}]), namespace=opns)
        routed = True
        wait(lambda: endpoints_are({proxy['metadata']['uid']}), 30)
        if first_publication:
            ports = service['spec']['ports'] + [{'name': 'publication-barrier', 'port': 9445, 'targetPort': 9445}]
            k('patch', 'service', service_name, '--type=json', '-p', json.dumps([{'op': 'replace', 'path': '/spec/ports', 'value': ports}]), namespace=opns)
            publication_port_added = True
            publication_webhook = ns + '-publication'
            create({'apiVersion': 'admissionregistration.k8s.io/v1', 'kind': 'MutatingWebhookConfiguration',
                    'metadata': {'name': publication_webhook}, 'webhooks': [{
                        'name': 'first-publication.ptah.run', 'admissionReviewVersions': ['v1'],
                        'sideEffects': 'None', 'failurePolicy': 'Fail', 'timeoutSeconds': 10,
                        'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': ns}},
                        'matchConditions': [{'name': 'intent-only', 'expression': 'object.spec.type == "intent"'}],
                        'rules': [{'apiGroups': ['operator.ptah.run'], 'apiVersions': ['v1alpha1'],
                                   'operations': ['CREATE'], 'resources': ['ptahresultrecords'], 'scope': 'Namespaced'}],
                        'clientConfig': {'caBundle': dec(credential)['ca.crt'],
                                         'service': {'namespace': opns, 'name': service_name,
                                                     'port': 9445, 'path': '/first-publication'}}}]})
            probe_record = next(r for r in records() if r['spec']['type'] == 'intent')
            probe_record['metadata'] = {key: value for key, value in probe_record['metadata'].items()
                                        if key in ('name', 'namespace', 'labels', 'ownerReferences')}
            def publication_barrier_ready():
                check = subprocess.run(['kubectl', '--kubeconfig', E['E2E_KUBECONFIG'],
                                        '-n', ns, 'create', '--dry-run=server', '-f', '-'],
                                       input=json.dumps(probe_record), text=True, capture_output=True, timeout=15)
                return check.returncode and 'first-publication.ptah.run' in check.stderr
            wait(publication_barrier_ready, 30)
        open_execution_gate()
        print('Apply released through the ACK-loss proxy:', job_name, flush=True)
        if pod_token:
            def original_pod_and_binding():
                pods = [p for p in get('pods')['items'] if any(o['uid'] == job_uid for o in p['metadata'].get('ownerReferences', []))]
                assert len(pods) <= 1, 'Apply created a replacement Pod'
                bindings = [r for r in records() if r['spec']['type'] == 'credential'
                            and r['metadata'].get('annotations', {}).get('operator.ptah.run/result-job-uid') == job_uid]
                assert len(bindings) <= 1
                return (pods[0], bindings[0]) if pods and bindings else None
            pod, credential = wait(original_pod_and_binding, 60)
            expected = pod_binding_record(job, pod)
            assert credential['spec'] == expected['spec'], 'Stored Pod binding differs from the admitted original Pod'
            assert credential['metadata']['name'] not in {s['metadata']['name'] for s in get('secrets')['items']}
            pod_name = pod['metadata']['name']
            save('pod-binding.json', credential)

        if restart_receiver:
            def retry_paused():
                value = admin('/evidence')
                return value if value and value.get('retryWaits', 0) > 0 and value['dropped'] and len(value['attempts']) == 1 else None
            paused = wait(retry_paused, 60)
            assert witness() == '1:1:true'
            _, committed, _ = publication({r['metadata']['name']: r for r in records()}, job_uid)
            assert committed['metadata']['uid'] == paused['attempts'][0]['receipt']['UID']
            old_uids = {p['metadata']['uid'] for p in managers}
            for p in managers:
                assert get('pod', p['metadata']['name'], opns)['metadata']['uid'] == p['metadata']['uid']
            k('delete', 'pods', *[p['metadata']['name'] for p in managers], '--wait=true', '--timeout=90s', namespace=opns)
            absent_at = dt.datetime.now(dt.timezone.utc).isoformat()
            def replacements_ready():
                pods = json.loads(k('get', 'pods', '-l', selector, '-o', 'json', namespace=opns))['items']
                return pods if len(pods) == 2 and old_uids.isdisjoint({p['metadata']['uid'] for p in pods}) and all(
                    not p['metadata'].get('deletionTimestamp') and any(c['type'] == 'Ready' and c['status'] == 'True'
                        for c in p.get('status', {}).get('conditions', [])) for p in pods) else None
            managers = wait(replacements_ready, 90)
            wait(lambda: endpoints_are({p['metadata']['uid'] for p in managers}, backend_service['metadata']['name']), 20)
            receiver_restart = {'before': sorted(old_uids), 'after': sorted(p['metadata']['uid'] for p in managers),
                                'oldPodsAbsent': True, 'newPodsReady': True, 'oldPodsAbsentAt': absent_at,
                                'newPodsReadyAt': dt.datetime.now(dt.timezone.utc).isoformat(),
                                'receiptUIDBeforeRestart': committed['metadata']['uid'], 'databaseBeforeRestart': '1:1:true'}
            save('receiver-restart.json', receiver_restart)
            assert admin('/resume-retry', True) == 204
            print('Both receiving managers replaced before retry forwarding', flush=True)

        if first_publication:
            from result_first_publication import run as run_first_publication
            run_first_publication(client=concurrent_client, admin=admin, records=records,
                                  save=save, wait=wait, job_uid=job_uid)
            k('delete', 'mutatingwebhookconfiguration', publication_webhook)
            publication_webhook = None
            assert admin('/resume-first', True) == 204

        if concurrent and not first_publication:
            from result_concurrent import verify_evidence as verify_concurrent, verify_authority_evidence, authority_cases, attempt_name
            def concurrent_retry_paused():
                value = admin('/evidence')
                return value if value and value.get('retryWaits', 0) > 0 and value['dropped'] and len(value['attempts']) == 1 else None
            paused = wait(concurrent_retry_paused, 60)
            if pod_token:
                original = get('pod', pod_name)
                assert original['metadata']['uid'] == pod['metadata']['uid'] and original['status']['phase'] == 'Running'
                main = original['spec']['containers'][0]
                mounts = [m for m in main['volumeMounts'] if m['name'] == 'result-credentials' and m.get('readOnly')]
                assert len(mounts) == 1
                # This is privileged test setup. Keep the original receiver-only
                # token in memory; neither subprocess arguments nor reports carry it.
                token = k('exec', 'pod/' + pod_name, '-c', main['name'], '--', '/bin/cat', mounts[0]['mountPath'] + '/token')
                concurrent_client = Client(E, managers, host, {'ca.crt': trust['data']['ca.crt']}, pod_token=token,
                                           identity=base64.b64decode(credential['spec']['data'], validate=True))
                del token
            rs_before = {r['metadata']['name']: r for r in records()}
            saved_intent, saved_complete, saved_data = publication(rs_before, job_uid)
            receipt = paused['attempts'][0]['receipt']
            assert saved_complete['metadata']['uid'] == receipt['UID']
            if authority:
                publishers = [p for p in get('pods')['items'] if p['metadata'].get('labels', {}).get('job-name') == 'ack-publish']
                assert len(publishers) == 1 and publishers[0]['metadata']['uid'] != pod['metadata']['uid']
                foreign = publishers[0]
                request = {'apiVersion': 'authentication.k8s.io/v1', 'kind': 'TokenRequest', 'spec': {
                    'audiences': ['operator.ptah.run/results'], 'expirationSeconds': 600,
                    'boundObjectRef': {'apiVersion': 'v1', 'kind': 'Pod', 'name': foreign['metadata']['name'],
                                       'uid': foreign['metadata']['uid']}}}
                issued = json.loads(k('create', '--raw', '/api/v1/namespaces/' + ns + '/serviceaccounts/' +
                                     foreign['spec']['serviceAccountName'] + '/token', '-f', '-', data=json.dumps(request)))
                review = json.loads(k('create', '--raw', '/apis/authentication.k8s.io/v1/tokenreviews', '-f', '-',
                    data=json.dumps({'apiVersion': 'authentication.k8s.io/v1', 'kind': 'TokenReview',
                                     'spec': {'token': issued['status']['token'], 'audiences': ['operator.ptah.run/results']}})))['status']
                assert review['authenticated'] and review['audiences'] == ['operator.ptah.run/results']
                assert review['user']['extra']['authentication.kubernetes.io/pod-uid'] == [foreign['metadata']['uid']]
                foreign_control = {'uid': foreign['metadata']['uid'], 'namespace': ns,
                                   'tokenReviewAuthenticated': True, 'audiences': review['audiences']}
                concurrent_result = concurrent_client.run_authority(saved_data, saved_intent['metadata']['name'], receipt,
                                                                     issued['status']['token'], foreign_control)
                del issued, review
                assert concurrent_result['identity'] == dec(credential)
            else:
                concurrent_result = concurrent_client.run(saved_data, saved_intent['metadata']['name'], receipt)
            if pod_token and not authority:
                assert concurrent_result['binding'] == dec(saved_intent)['binding']
                assert concurrent_result['identityDigest'] == paused['identityDigest']
            rs_after = {r['metadata']['name']: r for r in records()}
            after_intent, after_complete, after_data = publication(rs_after, job_uid)
            owned = {name: r for name, r in rs_before.items() if name == saved_intent['metadata']['name']
                     or any(o['uid'] == saved_intent['metadata']['uid'] for o in r['metadata'].get('ownerReferences', []))}
            assert owned and all(rs_after[name] == r for name, r in owned.items())
            assert after_intent == saved_intent and after_complete == saved_complete and after_data == saved_data
            concurrent_result['publicationUnchanged'] = True
            if authority:
                assert all(attempt_name(claim) == saved_intent['metadata']['name'] or attempt_name(claim) not in rs_after
                           for _, claim in authority_cases(concurrent_result['identity']))
                save('authority.json', concurrent_result)
                verify_authority_evidence(concurrent_result)
            else:
                save('concurrent.json', concurrent_result)
                verify_concurrent(concurrent_result)
            assert admin('/resume-retry', True) == 204
            print('PASS: installed Pod-token authority refusals' if authority else
                  'PASS: concurrent identical and conflicting redeliveries through both receivers', flush=True)

        def retry_saved():
            value = admin('/evidence')
            return value if value and value['dropped'] and (len(value['attempts']) == 2) else None
        evidence = wait(retry_saved, 180)
        before = witness()
        assert before == '1:1:true'
        rs = {r['metadata']['name']: r for r in records()}
        intent, complete, data = publication(rs, job_uid)
        assert complete['metadata']['uid'] == evidence['attempts'][0]['receipt']['UID']
        assert evidence['attempts'][0]['receipt'] == evidence['attempts'][1]['receipt']
        assert dec(intent)['digest'] == evidence['attempts'][0]['receipt']['Digest']
        saved_publication = {name: record for name, record in rs.items() if record['spec']['type'] in ['intent', 'chunk', 'complete'] and (name == intent['metadata']['name'] or any(owner['uid'] == intent['metadata']['uid'] for owner in record['metadata'].get('ownerReferences', [])))}
        save('publication.json', saved_publication)
        running = get('pod', pod_name)
        assert running['metadata']['uid'] == pod['metadata']['uid'] and running['status']['phase'] == 'Running'
        assert not get('job', job_name).get('status', {}).get('succeeded')
        save('held-retry.json', {'proxy': evidence, 'jobUID': job_uid, 'podUID': pod['metadata']['uid'], 'intentUID': intent['metadata']['uid'], 'receiptUID': complete['metadata']['uid'], 'databaseWitness': before, 'observedAt': dt.datetime.now(dt.timezone.utc).isoformat()})
        restore_service()
        routed = False
        wait(lambda: endpoints_are({p['metadata']['uid'] for p in managers}), 20)
        assert admin('/release', True) == 204
        k('wait', '--for=condition=complete', 'job/' + job_name, '--timeout=90s')

        def converged():
            r = get('ptahmigration', 'lost-ack')
            return r if any((c['type'] == 'Ready' and c['status'] == 'True' and (c.get('reason') == 'HistoryMatched') and (c.get('observedGeneration') == r['metadata']['generation']) for c in r.get('status', {}).get('conditions', []))) and (not r.get('status', {}).get('activeOperation')) else None
        final = wait(converged, 300)
        after = witness()
        assert after == before
        apply_jobs = [j for j in get('jobs')['items'] if j['metadata']['name'].startswith('ptah-m-apply-')]
        assert len(apply_jobs) == 1 and apply_jobs[0]['metadata']['uid'] == job_uid
        execution_pods = [p for p in get('pods')['items'] if any(owner['uid'] == job_uid for owner in p['metadata'].get('ownerReferences', []))]
        assert len(execution_pods) == 1 and execution_pods[0]['metadata']['uid'] == pod['metadata']['uid']
        finalpod = get('pod', pod_name)
        assert finalpod['metadata']['uid'] == pod['metadata']['uid'] and finalpod['status']['phase'] == 'Succeeded' and all((c['restartCount'] == 0 for c in finalpod['status']['containerStatuses']))
        finalproxy = admin('/evidence')
        assert finalproxy['released'] and finalproxy['attempts'] == evidence['attempts']
        result = {'commit': E['E2E_CONTROLLER_REVISION'], 'fixtureImage': fixture, 'credentialCertificateDigest': certificate_digest, 'credentialUID': credential['metadata']['uid'], 'namespace': ns, 'database': database, 'resourceUID': resource['metadata']['uid'], 'binding': dec(intent)['binding'], 'jobUID': job_uid, 'podUID': pod['metadata']['uid'], 'intentUID': intent['metadata']['uid'], 'receiptUID': complete['metadata']['uid'], 'payloadDigest': dec(intent)['digest'], 'receiptName': complete['metadata']['name'], 'payloadBytes': len(data), 'proxy': finalproxy, 'databaseBeforeRelease': before, 'databaseAfterRelease': after, 'databaseBeforeExecution': '0:1:false', 'applyJobs': 1, 'podRestarts': 0, 'podPhase': finalpod['status']['phase'], 'executionPods': len(execution_pods), 'runnerAPICredentials': False, 'converged': True, 'conditions': final['status']['conditions'], 'procedureSHA256': hashlib.sha256(pathlib.Path(__file__).read_bytes()).hexdigest(), 'completedAt': dt.datetime.now(dt.timezone.utc).isoformat()}
        result.update(evidenceVersion=2, engine=engine, rollbackCalibration=calibration, databaseServerVersion=server_version)
        if restart_receiver:
            result.update(evidenceVersion=3, receiverRestart=receiver_restart)
        if pod_token:
            result.pop('credentialCertificateDigest')
            result.update(evidenceVersion=5 if restart_receiver else 4, authentication='pod-token',
                          credentialIdentityDigest='sha256:' + hashlib.sha256(base64.b64decode(credential['spec']['data'], validate=True)).hexdigest(),
                          tokenProjection={'audience': 'operator.ptah.run/results', 'expirationSeconds': 3600, 'path': 'token'})
        verify_evidence(result)
        save('lost-ack.json', result)
        print('PASS: identical receipt redelivered after lost ACK; one native migration SQL execution', flush=True)
    finally:
        if scheduling_label_present:
            set_scheduling_label(k, scheduling_node, ns, remove=True)
        if publication_webhook:
            k('delete', 'mutatingwebhookconfiguration', publication_webhook, '--ignore-not-found')
        if concurrent_client:
            concurrent_client.close()
        if routed or publication_port_added:
            restore_service()
        if gated:
            k('delete', 'validatingadmissionpolicybinding', gate, '--ignore-not-found')
            k('delete', 'validatingadmissionpolicy', gate, '--ignore-not-found')
        if pf:
            pf.terminate()
            try:
                pf.wait(timeout=10)
            except subprocess.TimeoutExpired:
                pf.kill()
                pf.wait()
        if proxy_created:
            k('delete', 'pod', proxy_name, '--ignore-not-found', '--wait=true', '--timeout=60s', namespace=opns)
        if backend_service:
            k('delete', 'service', backend_service['metadata']['name'], '--ignore-not-found', namespace=opns)
        if copied:
            k('delete', 'secret', client_secret, '--ignore-not-found', namespace=opns)
if __name__ == '__main__':
    main()
