import copy
import datetime
import json
from pathlib import Path
import unittest

from monitoring_check import (ALERT, RELEASE_NAMESPACE, chart_scrape_pool, delivered, notifications_from,
                              receiver_manifests, scrape_findings)

TESTDATA = Path(__file__).resolve().parent / 'testdata/monitoring-check'
POOL = f'serviceMonitor/{RELEASE_NAMESPACE}/ptah-operator/0'


def instant(value):
    return datetime.datetime.fromisoformat(value)


class DeliveryTest(unittest.TestCase):
    """The v0.2.4 check's receiver log. The lost-view alert fired once while
    Prometheus was starting, before any manager was scraped, and resolved
    after the managers were removed at 11:13:17; that removal's own incident
    fired at 11:14:41, the managers came back at 11:14:48, and it resolved at
    11:15:11. An etcd alert arrived in between."""

    removed = instant('2026-10-19T11:13:17+00:00')
    restored = instant('2026-10-19T11:14:48+00:00')

    def setUp(self):
        self.notifications = notifications_from((TESTDATA / 'receiver-v0.2.4.jsonl').read_text())

    def test_the_log_reads_as_every_notification(self):
        self.assertEqual(len(self.notifications), 5)

    def test_firing_counts_only_for_an_incident_that_started_after_the_removal(self):
        found = delivered(self.notifications, 'firing', self.removed, self.removed)
        self.assertEqual(found['receivedAt'], '2026-10-19T11:14:41.930050+00:00')

    def test_a_repeat_of_the_startup_incident_does_not_count_as_firing(self):
        # Alertmanager repeats a firing group; a repeat that arrives after the
        # removal still belongs to the incident that began before it. Derived
        # from the startup notification by moving its arrival.
        repeat = copy.deepcopy(self.notifications[0])
        repeat['receivedAt'] = '2026-10-19T11:13:41.930020+00:00'
        self.assertIsNone(delivered([repeat], 'firing', self.removed, self.removed))
        self.assertIsNotNone(delivered([repeat], 'firing', self.removed - datetime.timedelta(minutes=5), self.removed))

    def test_resolution_counts_only_after_the_restore_for_the_removal_incident(self):
        found = delivered(self.notifications, 'resolved', self.removed, self.restored)
        self.assertEqual(found['receivedAt'], '2026-10-19T11:15:11.932117+00:00')
        # The startup incident resolved at 11:13:21, after the removal but
        # for an incident that began before it, and before the restore.
        self.assertIsNone(delivered(self.notifications[:3], 'resolved', self.removed, self.removed))

    def test_a_resolution_received_before_the_restore_does_not_count(self):
        # Had the managers come back at 11:15:20, the resolution received at
        # 11:15:11 would predate them and could not be theirs.
        self.assertIsNone(delivered(self.notifications, 'resolved', self.removed, instant('2026-10-19T11:15:20+00:00')))

    def test_another_alert_does_not_count(self):
        etcd = [n for n in self.notifications if n['groupLabels']['alertname'] != ALERT]
        self.assertEqual(len(etcd), 1)
        self.assertIsNone(delivered(etcd, 'firing', self.removed, self.removed))

    def test_a_line_that_is_not_a_notification_is_skipped(self):
        log = 'Traceback (most recent call last):\n' + json.dumps(self.notifications[3]) + '\n[]\n'
        self.assertEqual(notifications_from(log), [self.notifications[3]])


class FirstAttemptTest(unittest.TestCase):
    """The first v0.2.4 run, which passed and was wrong. It removed the
    managers at 11:08:49 and restored them in the same second, and took the
    startup incident's resolution, received at 11:08:50, as the resolution of
    an incident that never started."""

    def test_a_resolution_of_the_startup_incident_does_not_count(self):
        notifications = notifications_from((TESTDATA / 'receiver-v0.2.4-first-attempt.jsonl').read_text())
        removed = restored = instant('2026-10-19T11:08:49+00:00')
        self.assertEqual(len(notifications), 2)
        self.assertIsNone(delivered(notifications, 'resolved', removed, restored))
        self.assertIsNone(delivered(notifications, 'firing', removed, removed))
        # The control: the same resolution counts for the incident it belongs to.
        self.assertIsNotNone(delivered(notifications, 'resolved', instant('2026-10-19T11:08:00+00:00'), restored))


class ScrapeTest(unittest.TestCase):
    """The v0.2.4 targets, read while that chart's ServiceMonitor was the only
    one in the release namespace: it selected no Service, so Prometheus held
    no target there."""

    def setUp(self):
        self.targets = json.loads((TESTDATA / 'targets-v0.2.4.json').read_text())
        self.managers = ['ptah-operator-7c9f8d7b5-a', 'ptah-operator-7c9f8d7b5-b']

    def scraped(self, pods, pool=POOL, health='up'):
        return self.targets + [{'scrapePool': pool, 'health': health, 'scrapeUrl': f'http://{pod}:8080/metrics',
                                'labels': {'namespace': RELEASE_NAMESPACE, 'pod': pod}} for pod in pods]

    def test_the_v024_chart_reaches_no_manager(self):
        self.assertEqual(scrape_findings(self.targets, self.managers, POOL), [
            f'manager Pod {pod} has no up target in {POOL}' for pod in self.managers])

    def test_every_manager_scraped_through_the_chart_passes(self):
        self.assertEqual(scrape_findings(self.scraped(self.managers), self.managers, POOL), [])

    def test_a_manager_left_out_or_down_is_a_finding(self):
        self.assertEqual(scrape_findings(self.scraped(self.managers[:1]), self.managers, POOL),
                         [f'manager Pod {self.managers[1]} has no up target in {POOL}'])
        self.assertEqual(len(scrape_findings(self.scraped(self.managers, health='down'), self.managers, POOL)), 2)

    def test_a_target_from_another_servicemonitor_is_a_finding(self):
        workaround = f'serviceMonitor/{RELEASE_NAMESPACE}/ptah-operator-metrics-deployer/0'
        findings = scrape_findings(self.scraped(self.managers) + self.scraped(self.managers, pool=workaround)[len(self.targets):],
                                   self.managers, POOL)
        self.assertEqual(findings, [f"targets in {RELEASE_NAMESPACE} come from ['{workaround}']"])

    def test_no_manager_running_is_a_finding_and_not_a_pass(self):
        self.assertEqual(scrape_findings(self.scraped([]), [], POOL), ['no manager Pod is running'])


class ServiceMonitorTest(unittest.TestCase):
    chart = {'metadata': {'name': 'ptah-operator', 'labels': {
        'app.kubernetes.io/instance': 'ptah-operator', 'app.kubernetes.io/managed-by': 'Helm'}}}

    def test_the_release_servicemonitor_names_the_pool(self):
        self.assertEqual(chart_scrape_pool([self.chart], 'ptah-operator'), POOL)

    def test_a_second_servicemonitor_in_the_namespace_is_refused(self):
        deployer = {'metadata': {'name': 'ptah-operator-metrics-deployer', 'labels': {}}}
        with self.assertRaises(RuntimeError):
            chart_scrape_pool([self.chart, deployer], 'ptah-operator')
        with self.assertRaises(RuntimeError):
            chart_scrape_pool([deployer], 'ptah-operator')

    def test_none_at_all_is_refused(self):
        with self.assertRaises(RuntimeError):
            chart_scrape_pool([], 'ptah-operator')

    def test_another_release_servicemonitor_is_refused(self):
        other = copy.deepcopy(self.chart)
        other['metadata']['labels']['app.kubernetes.io/instance'] = 'other'
        with self.assertRaises(RuntimeError):
            chart_scrape_pool([other], 'ptah-operator')


class ReceiverTest(unittest.TestCase):
    def test_the_receiver_runs_a_pinned_image_and_is_ready_only_on_its_port(self):
        deployment = next(m for m in receiver_manifests() if m['kind'] == 'Deployment')
        container = deployment['spec']['template']['spec']['containers'][0]
        self.assertRegex(container['image'], r'@sha256:[0-9a-f]{64}$')
        self.assertEqual(container['readinessProbe']['tcpSocket'], {'port': 8080})


if __name__ == '__main__':
    unittest.main()
