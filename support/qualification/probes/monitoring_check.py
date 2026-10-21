#!/usr/bin/env python3
"""Deliver an alert through the chart's own monitoring, installed from the guide.

The CI alerting suites scrape the managers with a Prometheus configuration of
their own, so they prove the rules and the alert routes and say nothing about
the ServiceMonitor the chart renders. v0.2.4 shipped one that selected no
Service (#673): `monitoring.serviceMonitor.enabled` scraped nothing, and every
alert test passed while the shipped chart could not deliver an alert. This
check is the evidence for that half of PA-09 (#675).

It installs a Prometheus Operator stack whose Alertmanager posts every
notification to an in-cluster receiver, then the release with the published
guide's verification and installation commands, adding the profile's
monitoring values. Prometheus has to scrape every manager Pod through the
ServiceMonitor the chart rendered, with no other ServiceMonitor in the release
namespace, and load the chart's rules. Then every manager is removed: the
lost-view alert has to reach the receiver, and once the managers are back its
resolution has to arrive too.

A notification counts only for an incident that started after the managers
were removed, and a resolution only once it arrived after they came back. The
alert can fire while Prometheus is still starting, before anything is scraped,
and that incident's notifications must satisfy neither step.
"""

import argparse
import datetime
import json
import os
import re
import sys
import time
from urllib.parse import quote

from fresh_install import ROOT, Walkthrough, timestamp

RELEASE_NAMESPACE = 'ptah-system'
MONITORING_NAMESPACE = 'monitoring'
ALERT = 'PtahOperatorUnresolvedViewNotSynced'
# The stack's chart, pinned by digest; Helm refuses a pull whose digest differs.
STACK_CHART = ('oci://ghcr.io/prometheus-community/charts/kube-prometheus-stack:92.11.0'
               '@sha256:5b9b0215085f00f61c0fb2aa01ec402609d91c0a6a91bc84ca85cce2ed69e635')
STACK_VALUES = ROOT / 'support/qualification/monitoring-stack.yaml'
PROFILE_VALUES = ROOT / 'support/qualification/0.2.0-monitoring.yaml'
RECEIVER_IMAGE = 'python:3.13-slim@sha256:98ce6fb13b3803744fbca11434f00818f1e300e4e48a4f204e07b2c928b6218e'
# Logs every notification Alertmanager posts, one JSON document per line, so
# delivery is read from the receiver itself rather than from Alertmanager.
RECEIVER = '''import datetime, http.server, json
class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        body['receivedAt'] = datetime.datetime.now(datetime.timezone.utc).isoformat()
        print(json.dumps(body), flush=True)
        self.send_response(200)
        self.end_headers()
    def log_message(self, *args):
        pass
http.server.HTTPServer(('', 8080), Handler).serve_forever()
'''


def receiver_manifests():
    labels = {'app': 'ptah-receiver'}
    return [
        {'apiVersion': 'v1', 'kind': 'ConfigMap', 'metadata': {'name': 'ptah-receiver', 'namespace': MONITORING_NAMESPACE},
         'data': {'receiver.py': RECEIVER}},
        {'apiVersion': 'apps/v1', 'kind': 'Deployment', 'metadata': {'name': 'ptah-receiver', 'namespace': MONITORING_NAMESPACE},
         'spec': {'replicas': 1, 'selector': {'matchLabels': labels},
                  'template': {'metadata': {'labels': labels}, 'spec': {
                      'containers': [{'name': 'receiver', 'image': RECEIVER_IMAGE, 'command': ['python3', '-u', '/app/receiver.py'],
                                      'ports': [{'containerPort': 8080}], 'readinessProbe': {'tcpSocket': {'port': 8080}, 'periodSeconds': 2},
                                      'volumeMounts': [{'name': 'app', 'mountPath': '/app'}]}],
                      'volumes': [{'name': 'app', 'configMap': {'name': 'ptah-receiver'}}]}}}},
        {'apiVersion': 'v1', 'kind': 'Service', 'metadata': {'name': 'ptah-receiver', 'namespace': MONITORING_NAMESPACE},
         'spec': {'selector': labels, 'ports': [{'port': 8080, 'targetPort': 8080}]}},
    ]


def chart_scrape_pool(service_monitors, release):
    """The scrape pool of the one ServiceMonitor the release rendered.

    The release namespace has to hold that ServiceMonitor and no other: a
    second one there is how a deployer would work around a chart that scrapes
    nothing, and a target it yielded would read as the chart's.
    """
    names = sorted(m['metadata']['name'] for m in service_monitors)
    owned = [m for m in service_monitors
             if m['metadata'].get('labels', {}).get('app.kubernetes.io/instance') == release
             and m['metadata'].get('labels', {}).get('app.kubernetes.io/managed-by') == 'Helm']
    if len(service_monitors) != 1 or len(owned) != 1:
        raise RuntimeError(f'expected the release\'s ServiceMonitor alone in {RELEASE_NAMESPACE}, found {names}')
    return f'serviceMonitor/{RELEASE_NAMESPACE}/{owned[0]["metadata"]["name"]}/0'


def scrape_findings(active_targets, manager_pods, pool):
    """What keeps Prometheus from scraping every manager through the chart.

    Every running manager Pod needs an up target in the chart's scrape pool,
    and no target in the release namespace may come from any other pool. No
    manager Pod at all is a finding rather than a pass.
    """
    findings = []
    in_release = [t for t in active_targets if t.get('labels', {}).get('namespace') == RELEASE_NAMESPACE]
    foreign = sorted({t.get('scrapePool') for t in in_release} - {pool})
    if foreign:
        findings.append(f'targets in {RELEASE_NAMESPACE} come from {foreign}')
    up = {t['labels'].get('pod') for t in in_release if t.get('scrapePool') == pool and t.get('health') == 'up'}
    if not manager_pods:
        findings.append('no manager Pod is running')
    for pod in sorted(manager_pods):
        if pod not in up:
            findings.append(f'manager Pod {pod} has no up target in {pool}')
    return findings


def delivered(notifications, status, started_after, received_after):
    """The first notification that reports the lost-view alert in status, for
    an incident that started after started_after, received after received_after.

    Instants are compared as instants, never as strings: Alertmanager writes
    milliseconds and a Z, the receiver microseconds and an offset.
    """
    for notification in notifications:
        if notification.get('status') != status or timestamp(notification['receivedAt']) <= received_after:
            continue
        for alert in notification.get('alerts', []):
            if (alert.get('labels', {}).get('alertname') == ALERT and alert.get('status') == status
                    and timestamp(alert['startsAt']) > started_after):
                return notification
    return None


def notifications_from(log):
    """The receiver's log as notifications; a line that is not one is skipped."""
    found = []
    for line in log.splitlines():
        try:
            document = json.loads(line)
        except ValueError:
            continue
        if isinstance(document, dict) and 'receivedAt' in document:
            found.append(document)
    return found


def now():
    return datetime.datetime.now(datetime.timezone.utc)


class MonitoringCheck:
    def __init__(self, args):
        self.guide = Walkthrough(argparse.Namespace(tag=args.tag, upgrade_from=None, engine=None,
                                                    work=args.work, evidence=args.evidence))
        self.guide.values = [PROFILE_VALUES]
        self.transcript = self.guide.transcript
        self.evidence = self.transcript.directory
        self.release = 'ptah-operator'
        self.prometheus = None

    def kubectl(self, *args, check=True, stdin=None):
        return self.guide.kubectl(*args, check=check, stdin=stdin)

    def keep(self, name, text):
        (self.evidence / name).write_text(text)

    def install_stack(self):
        if not re.fullmatch(r'[^@\s]+@sha256:[0-9a-f]{64}', RECEIVER_IMAGE):
            raise ValueError('the receiver image must be pinned by digest')
        self.transcript.run('install-monitoring-stack',
                            f'helm install kps {STACK_CHART} --namespace {MONITORING_NAMESPACE} --create-namespace '
                            f'--values {STACK_VALUES} --wait --timeout 10m\n',
                            self.guide.work, self.guide.env, source='fixture: the deployer\'s Prometheus Operator stack')
        self.kubectl('apply', '-f', '-', stdin='\n---\n'.join(json.dumps(m) for m in receiver_manifests()))
        self.kubectl('-n', MONITORING_NAMESPACE, 'rollout', 'status', 'deployment/ptah-receiver', '--timeout=300s')
        self.transcript.record(step='fixture', note='A Prometheus Operator stack and an alert receiver, as the deployer brings them.',
                               chart=STACK_CHART, receiver=RECEIVER_IMAGE)

    def prometheus_api(self, path):
        if self.prometheus is None:
            pods = json.loads(self.kubectl('-n', MONITORING_NAMESPACE, 'get', 'pods', '-l', 'app.kubernetes.io/name=prometheus',
                                           '-o', 'json').stdout)['items']
            if len(pods) != 1:
                raise RuntimeError(f'expected one Prometheus Pod, found {len(pods)}')
            self.prometheus = pods[0]['metadata']['name']
            self.kubectl('-n', MONITORING_NAMESPACE, 'wait', '--for=condition=Ready', f'pod/{self.prometheus}', '--timeout=300s')
        raw = self.kubectl('get', '--raw', f'/api/v1/namespaces/{MONITORING_NAMESPACE}/pods/{self.prometheus}:9090/proxy/api/v1/{path}').stdout
        return json.loads(raw)['data']

    def manager_deployment(self):
        deployments = json.loads(self.kubectl('-n', RELEASE_NAMESPACE, 'get', 'deployments', '-l',
                                              'app.kubernetes.io/component=controller', '-o', 'json').stdout)['items']
        if len(deployments) != 1:
            raise RuntimeError(f'expected one manager Deployment, found {len(deployments)}')
        return deployments[0]

    def manager_pods(self):
        pods = json.loads(self.kubectl('-n', RELEASE_NAMESPACE, 'get', 'pods', '-l', 'app.kubernetes.io/component=controller',
                                       '-o', 'json').stdout)['items']
        return [p for p in pods if p['status'].get('phase') == 'Running' and not p['metadata'].get('deletionTimestamp')]

    def scraped(self, timeout=300):
        """Wait for Prometheus to scrape every manager through the chart's
        ServiceMonitor, see the view synchronized, and hold the rule idle."""
        monitors = json.loads(self.kubectl('-n', RELEASE_NAMESPACE, 'get', 'servicemonitors', '-o', 'json').stdout)['items']
        self.keep('servicemonitors.json', json.dumps(monitors, indent=2))
        pool = chart_scrape_pool(monitors, self.release)
        deadline = time.monotonic() + timeout
        while True:
            pods = sorted(p['metadata']['name'] for p in self.manager_pods())
            targets = self.prometheus_api('targets')['activeTargets']
            findings = scrape_findings(targets, pods, pool)
            synced = self.prometheus_api('query?query=' + quote('max(ptah_operator_unresolved_view_synced)'))['result']
            if not synced or synced[0]['value'][1] != '1':
                findings.append(f'the view reads {synced}, not synchronized')
            rules = [r for g in self.prometheus_api('rules')['groups'] for r in g['rules'] if r['name'] == ALERT]
            if len(rules) != 1 or rules[0].get('health') != 'ok' or rules[0].get('state') != 'inactive':
                findings.append(f'{ALERT} reads {[(r.get("health"), r.get("state")) for r in rules]}, not one idle healthy rule')
            if not findings:
                break
            if time.monotonic() > deadline:
                self.keep('prometheus-targets.json', json.dumps(targets, indent=2))
                raise RuntimeError('Prometheus does not scrape the managers through the chart: ' + '; '.join(findings))
            time.sleep(5)
        self.keep('prometheus-targets.json', json.dumps(targets, indent=2))
        self.keep('prometheus-rules.json', json.dumps(rules, indent=2))
        self.transcript.record(step='scraped-through-the-chart', scrapePool=pool, managerPods=pods,
                               targets=sorted(t['scrapeUrl'] for t in targets if t.get('scrapePool') == pool))

    def notifications(self):
        return notifications_from(self.kubectl('-n', MONITORING_NAMESPACE, 'logs', 'deployment/ptah-receiver').stdout)

    def wait_delivery(self, status, started_after, received_after, timeout=600):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            notification = delivered(self.notifications(), status, started_after, received_after)
            if notification:
                return notification
            time.sleep(5)
        raise RuntimeError(f'{ALERT} never reached the receiver {status}')

    def lose_and_restore_managers(self):
        deployment = self.manager_deployment()
        name, replicas = deployment['metadata']['name'], deployment['spec']['replicas']
        removed = now()
        self.kubectl('-n', RELEASE_NAMESPACE, 'scale', f'deployment/{name}', '--replicas=0')
        self.kubectl('-n', RELEASE_NAMESPACE, 'wait', '--for=delete', 'pod', '-l', 'app.kubernetes.io/component=controller',
                     '--timeout=180s', check=False)
        if self.manager_pods():
            raise RuntimeError('a manager Pod is still running after the scale to zero')
        gone = now()
        self.transcript.record(step='managers-removed', deployment=name, replicas=replicas, removedAt=removed.isoformat(),
                               goneAt=gone.isoformat())
        firing = self.wait_delivery('firing', removed, removed)
        restored = now()
        self.kubectl('-n', RELEASE_NAMESPACE, 'scale', f'deployment/{name}', f'--replicas={replicas}')
        self.kubectl('-n', RELEASE_NAMESPACE, 'rollout', 'status', f'deployment/{name}', '--timeout=300s')
        self.transcript.record(step='managers-restored', restoredAt=restored.isoformat())
        resolved = self.wait_delivery('resolved', removed, restored)
        self.keep('receiver.jsonl', '\n'.join(json.dumps(n) for n in self.notifications()) + '\n')
        return {
            'alert': ALERT,
            'managersRemovedAt': removed.isoformat(),
            'managersGoneAt': gone.isoformat(),
            'firingReceivedAt': firing['receivedAt'],
            'firingSecondsAfterGone': round((timestamp(firing['receivedAt']) - gone).total_seconds(), 3),
            'managersRestoredAt': restored.isoformat(),
            'resolvedReceivedAt': resolved['receivedAt'],
            'resolvedSecondsAfterRestore': round((timestamp(resolved['receivedAt']) - restored).total_seconds(), 3),
        }

    def diagnose(self):
        for name, args in (('diagnostic-ptah-system.txt', ['-n', RELEASE_NAMESPACE, 'get', 'all,servicemonitors,prometheusrules,endpointslices', '-o', 'wide']),
                           ('diagnostic-monitoring.txt', ['-n', MONITORING_NAMESPACE, 'get', 'pods', '-o', 'wide']),
                           ('diagnostic-receiver.log', ['-n', MONITORING_NAMESPACE, 'logs', 'deployment/ptah-receiver']),
                           ('diagnostic-alertmanager.log', ['-n', MONITORING_NAMESPACE, 'logs', '-l', 'app.kubernetes.io/name=alertmanager',
                                                            '-c', 'alertmanager', '--tail=200'])):
            result = self.kubectl(*args, check=False)
            self.keep(name, result.stdout + result.stderr)

    def run(self):
        try:
            self.install_stack()
            identities = self.guide.verify_and_install()
            self.guide.confirm_installed()
            self.guide.runtime_on(identities['image'])
            self.scraped()
            result = self.lose_and_restore_managers()
        except Exception:
            self.diagnose()
            raise
        result.update(verdict='PASS', tag=self.guide.tag, releaseSource=self.guide.source, chart=STACK_CHART, **identities)
        self.keep('result.json', json.dumps(result, indent=2) + '\n')
        self.transcript.record(step='complete', **result)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--tag', required=True)
    parser.add_argument('--work', required=True)
    parser.add_argument('--evidence', required=True)
    args = parser.parse_args()
    os.umask(0o077)
    MonitoringCheck(args).run()
    return 0


if __name__ == '__main__':
    sys.exit(main())
