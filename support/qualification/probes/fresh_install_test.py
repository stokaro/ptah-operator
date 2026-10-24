import copy
import datetime
import json
from pathlib import Path
import unittest

from fresh_install import SERVERS, blocks, converged_after, fixture_deployment, runtime_findings, substitute, upgrade_command

DOCS = Path(__file__).resolve().parents[3] / 'docs/site/src/content/docs'


class BlockExtractionTest(unittest.TestCase):
    def test_a_section_ends_at_a_heading_of_its_level_and_keeps_its_subsections(self):
        page = '\n'.join([
            '## First', '```sh', 'one', '```', '### Inside', '```sh', 'two', '```',
            '```yaml', 'not: shell', '```', '## Second {#second}', '```sh', 'three', '```',
        ])
        self.assertEqual(blocks(page, 'First'), ['one\n', 'two\n'])
        self.assertEqual(blocks(page, 'Second'), ['three\n'])
        with self.assertRaises(ValueError):
            blocks(page, 'Missing')

    def test_an_anchor_picks_one_of_several_headings_with_the_same_title(self):
        page = '\n'.join(['## Install', '### Run it {#install-run}', '```sh', 'install', '```',
                          '## Upgrade', '### Run it {#upgrade-run}', '```sh', 'upgrade', '```'])
        self.assertEqual(blocks(page, '#upgrade-run'), ['upgrade\n'])
        self.assertEqual(blocks(page, 'Run it'), ['install\n', 'upgrade\n'])
        with self.assertRaises(ValueError):
            blocks(page, '#rollback-run')

    def test_a_heading_inside_a_fence_is_not_a_heading(self):
        page = '\n'.join(['## Real', '```sh', '## not a heading', 'echo kept', '```'])
        self.assertEqual(blocks(page, 'Real'), ['## not a heading\necho kept\n'])

    def test_substitution_refuses_a_placeholder_the_guide_lost_or_repeats(self):
        self.assertEqual(substitute('a=<x>\n', [('<x>', '1')]), 'a=1\n')
        with self.assertRaises(ValueError):
            substitute('a=1\n', [('<x>', '1')])
        with self.assertRaises(ValueError):
            substitute('<x> <x>\n', [('<x>', '1')])


class FixtureTest(unittest.TestCase):
    def test_a_database_keeps_its_data_on_an_emptydir_and_is_ready_only_on_its_port(self):
        for engine, server in SERVERS.items():
            deployment, service = fixture_deployment('ns', server['name'], 'image@sha256:' + '0' * 64, server['port'], [],
                                                     data=server['data'])
            spec = deployment['spec']['template']['spec']
            container = spec['containers'][0]
            self.assertEqual(container['volumeMounts'], [{'name': 'data', 'mountPath': server['data']}], engine)
            self.assertEqual(spec['volumes'], [{'name': 'data', 'emptyDir': {}}], engine)
            self.assertEqual(container['readinessProbe']['tcpSocket'], {'port': server['port']}, engine)
            self.assertEqual(service['spec']['ports'], [{'port': server['port'], 'targetPort': server['port']}], engine)

    def test_a_server_without_data_mounts_nothing(self):
        deployment, _ = fixture_deployment('ns', 'registry', 'image@sha256:' + '0' * 64, 5000, [])
        spec = deployment['spec']['template']['spec']
        self.assertNotIn('volumes', spec)
        self.assertNotIn('volumeMounts', spec['containers'][0])
        self.assertEqual(spec['containers'][0]['readinessProbe']['tcpSocket'], {'port': 5000})


CANDIDATE = 'sha256:' + '1' * 64
PREDECESSOR = 'sha256:' + '0' * 64


def deployment(name, component, image=CANDIDATE, init=CANDIDATE, rolled_out=True):
    return {
        'metadata': {'name': name, 'generation': 3},
        'spec': {'replicas': 2, 'selector': {'matchLabels': {'component': component}},
                 'template': {'spec': {'initContainers': [{'name': 'init', 'image': f'ghcr.io/stokaro/ptah-operator@{init}'}],
                                       'containers': [{'name': 'run', 'image': f'ghcr.io/stokaro/ptah-operator@{image}'}]}}},
        'status': {'observedGeneration': 3, 'replicas': 2, 'updatedReplicas': 2 if rolled_out else 1, 'availableReplicas': 2},
    }


def pod(name, component, digest=CANDIDATE, init=CANDIDATE):
    return {'metadata': {'name': name, 'labels': {'component': component}},
            'status': {'initContainerStatuses': [{'name': 'init', 'imageID': f'ghcr.io/stokaro/ptah-operator@{init}'}],
                       'containerStatuses': [{'name': 'run', 'imageID': f'ghcr.io/stokaro/ptah-operator@{digest}'}]}}


class RuntimeTest(unittest.TestCase):
    def test_a_namespace_on_the_candidate_passes_and_reports_what_it_examined(self):
        findings, examined = runtime_findings([deployment('manager', 'controller')],
                                              [pod('a', 'controller'), pod('b', 'controller'), pod('hook', 'other', PREDECESSOR)],
                                              CANDIDATE)
        self.assertEqual(findings, [])
        self.assertEqual([p['metadata']['name'] for p in examined], ['a', 'b'])

    def test_anything_left_on_the_predecessor_is_a_finding(self):
        cases = {
            'a predecessor Pod still terminating': ([deployment('manager', 'controller')],
                                                    [pod('a', 'controller'), pod('old', 'controller', PREDECESSOR)]),
            'an init container on the predecessor': ([deployment('manager', 'controller')],
                                                     [pod('a', 'controller', init=PREDECESSOR)]),
            'a template that names the predecessor': ([deployment('manager', 'controller', image=PREDECESSOR)],
                                                      [pod('a', 'controller')]),
            'a rollout still in progress': ([deployment('manager', 'controller', rolled_out=False)], [pod('a', 'controller')]),
            'a Deployment with no Pod': ([deployment('manager', 'controller'), deployment('rotator', 'rotation')],
                                         [pod('a', 'controller')]),
            'a Pod that reports no container': ([deployment('manager', 'controller')],
                                                [{'metadata': {'name': 'a', 'labels': {'component': 'controller'}}, 'status': {}}]),
        }
        for name, (deployments, pods) in cases.items():
            findings, _ = runtime_findings(deployments, pods, CANDIDATE)
            self.assertTrue(findings, name)


STARTED = datetime.datetime(2026, 10, 11, 12, 0, 0, tzinfo=datetime.timezone.utc)
APPLIED = {'planRef': {'name': 'application-1', 'uid': 'u1'}, 'completedAt': '2026-10-11T11:40:00Z'}
EPOCH = 'v1-' + 'a' * 32
EXECUTOR = 'ghcr.io/stokaro/ptah@sha256:' + 'e' * 64
OTHER_EXECUTOR = 'ghcr.io/stokaro/ptah@sha256:' + 'f' * 64
BINDING = {'controllerStateVersion': 1, 'executorImage': EXECUTOR, 'ptahVersion': 'v0.13.0', 'runnerProtocolVersion': 1,
           'epoch': EPOCH}
BEFORE = {'applied': APPLIED, 'executionBinding': BINDING}
MOVED = Path(__file__).parent / 'testdata/fresh-install/upgrade-moved-executor.json'


def schema(finished='2026-10-11T12:08:30Z', phase='InSync', applied=APPLIED, status='True', reason='ScopedConverged',
           binding=BINDING):
    return {'status': {'phase': phase, 'applied': copy.deepcopy(applied), 'lastSuccessfulReconciliation': finished,
                       'executionBinding': copy.deepcopy(binding),
                       'conditions': [{'type': 'Ready', 'status': 'True', 'reason': 'Converged'},
                                      {'type': 'InSync', 'status': status, 'reason': reason}]}}


def moved():
    """The v0.2.6 upgrade over v0.2.5, which moved the executor from Ptah
    v0.13.0 to v0.14.0, as the probe read it in run 39916361220."""
    reading = json.loads(MOVED.read_text())
    started = datetime.datetime.fromisoformat(reading['started'])
    return reading['after'], reading['before'], started, reading['candidate']


class ConvergedAfterTest(unittest.TestCase):
    def test_a_refresh_the_new_manager_finished_in_sync_for_the_same_record_passes(self):
        self.assertTrue(converged_after(schema(), BEFORE, STARTED, EXECUTOR, 'v0.13.0'))

    def test_an_upgrade_that_moves_the_executor_passes_under_a_new_epoch(self):
        after, before, started, candidate = moved()
        self.assertNotEqual(after['status']['executionBinding']['epoch'], before['executionBinding']['epoch'])
        self.assertTrue(converged_after(after, before, started, candidate['executor'], candidate['ptah_version']))

    def test_readings_that_do_not_show_it_are_refused(self):
        cases = {
            "the predecessor's last refresh": schema(finished='2026-10-11T11:58:00Z'),
            'a refresh finished the second the Pod started': schema(finished='2026-10-11T12:00:00Z'),
            'no completed refresh': schema(finished=None),
            'a plan waiting for approval': schema(phase='AwaitingApproval'),
            'another applied record': schema(applied={**APPLIED, 'planRef': {'name': 'application-2', 'uid': 'u2'}}),
            'a new execution epoch for the same executor': schema(binding={**BINDING, 'epoch': 'v1-' + 'b' * 32}),
            'another executor under the same epoch': schema(binding={**BINDING, 'executorImage': OTHER_EXECUTOR}),
            'no execution binding': schema(binding=None),
            'not in sync': schema(status='False', reason='PlanRequired'),
            'in sync for another reason': schema(reason='Converged'),
        }
        for name, reading in cases.items():
            self.assertFalse(converged_after(reading, BEFORE, STARTED, EXECUTOR, 'v0.13.0'), name)

    def test_a_moved_executor_is_refused_where_the_binding_does_not_follow_it(self):
        after, before, started, candidate = moved()
        status = after['status']
        cases = {
            "the predecessor's epoch": {**status['executionBinding'], 'epoch': before['executionBinding']['epoch']},
            "the predecessor's executor": {**before['executionBinding'], 'epoch': status['executionBinding']['epoch']},
            'another Ptah version': {**status['executionBinding'], 'ptahVersion': 'v0.13.0'},
        }
        for name, binding in cases.items():
            reading = copy.deepcopy(after)
            reading['status']['executionBinding'] = binding
            self.assertFalse(converged_after(reading, before, started, candidate['executor'], candidate['ptah_version']), name)
        self.assertFalse(converged_after(after, before, started, OTHER_EXECUTOR, candidate['ptah_version']),
                         'a candidate that installs another executor')


class PublishedGuideTest(unittest.TestCase):
    """The walkthrough reads these sections; a renamed heading or a reworded
    placeholder must fail here rather than on a cluster."""

    def read(self, page):
        return (DOCS / page).read_text()

    def test_the_release_guide_verifies_then_installs_in_one_shell(self):
        page = self.read('support/releases.md')
        verify = ''.join(blocks(page, 'Verify before installation'))
        install = ''.join(blocks(page, 'Install by digest'))
        self.assertIn('tag=v0.2.7\n', verify)
        for variable in ('image', 'executor', 'ptah_version', 'version'):
            self.assertIn(f'{variable}=', verify)
        self.assertIn('helm upgrade --install ptah-operator', install)
        self.assertIn('${image##*@}', install)

    def test_the_upgrade_adds_only_the_flag_the_operations_guide_names(self):
        install = ''.join(blocks(self.read('support/releases.md'), 'Install by digest'))
        upgraded = upgrade_command(install, self.read('use/operations.md'))
        self.assertEqual(upgraded.replace(' \\\n  --force-conflicts', ''), install)
        self.assertIn('--force-conflicts\n', upgraded)
        with self.assertRaises(ValueError):
            upgrade_command(install, self.read('use/operations.md').replace('--force-conflicts', '--force'))

    def test_the_install_guide_confirms_the_installation(self):
        confirm = ''.join(blocks(self.read('start/install.md'), 'Confirm it installed'))
        self.assertIn('wait --for=condition=Available deployment --all', confirm)
        self.assertIn('Established', confirm)

    def test_the_first_schema_guide_has_the_blocks_the_walkthrough_runs(self):
        page = self.read('start/first-schema.md')
        create = blocks(page, 'Create what the schema reads')
        self.assertEqual(len(create), 3)
        substitute(create[0], [('DATABASE_URL_FILE=/path/to/private/database-url\n', 'DATABASE_URL_FILE=x\n')])
        self.assertIn('kubectl apply -f examples/ptahschema.yaml\n', create[1])
        self.assertIn('kubectl apply -f examples/ptahschema-mysql.yaml\n', create[2])
        for block in create[1:]:
            self.assertIn('kubectl -n application get ptahschema application -w', block)
        stops = blocks(page, 'It stops, and waits for you')
        self.assertEqual(len(stops), 4)
        self.assertTrue(any('kubectl -n application create -f -' in block for block in stops))

    def test_the_plugin_install_has_the_placeholders_the_walkthrough_fills(self):
        block = ''.join(blocks(self.read('use/read-a-plan.md'), 'Install it'))
        substitute(block, [
            ('version=<release tag>\n', 'version=v0.2.0\n'),
            ('platform=darwin-arm64   # or linux-amd64, linux-arm64, darwin-amd64\n', 'platform=linux-amd64\n'),
            ('/usr/local/bin/kubectl-ptah', '/tmp/bin/kubectl-ptah'),
        ])

    def test_each_example_has_one_artifact_placeholder_and_one_policy_reference(self):
        examples = Path(__file__).resolve().parents[3] / 'examples'
        for name in ('ptahschema.yaml', 'ptahschema-mysql.yaml'):
            text = (examples / name).read_text()
            self.assertEqual(text.count('@sha256:<digest>'), 1, name)
            self.assertEqual(text.count('    verificationPolicyFrom:\n'), 1, name)


if __name__ == '__main__':
    unittest.main()
