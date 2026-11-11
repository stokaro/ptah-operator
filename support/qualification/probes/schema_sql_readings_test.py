import contextlib
import io
import itertools
import json
import os
import re
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest import mock

import schema_sql_readings
from schema_sql_readings import (LINE_WITNESSES, ROOT, CommandTimeout, Lab, added_postgresql_lines, contract, contract_readings,
                                 received)

READINGS = ROOT / 'testdata/e2e/readings'
CONTRACT = ROOT / 'test/e2e/schema-sql-contract.json'


class SchemaSQLReadingsTest(unittest.TestCase):
    def test_the_contract_is_exactly_what_the_checked_in_readings_witness(self):
        checked_in = json.loads(CONTRACT.read_text())
        self.assertEqual(contract(READINGS, checked_in['ptahCommit'], checked_in['scope']), checked_in)

    def test_every_added_postgresql_line_witnesses_every_operation(self):
        lines = added_postgresql_lines()
        self.assertTrue(lines, 'the catalog adds no PostgreSQL line, so this checks nothing')
        readings = contract_readings('postgresql')
        for major, _ in lines:
            witnessed = {operation for name, operation in readings if name.startswith(f'postgresql-{major}-')}
            self.assertEqual(witnessed, {operation for _, operation in LINE_WITNESSES})
            for name, _ in LINE_WITNESSES:
                self.assertTrue((READINGS / f'postgresql-{major}-schema-{name}.jsonl').is_file())

    def test_statements_are_read_the_way_the_audit_reads_them(self):
        with tempfile.TemporaryDirectory() as directory:
            postgres = Path(directory) / 'postgresql.jsonl'
            postgres.write_text('\n'.join(json.dumps(row) for row in [
                {'dbname': 'ptah_audit_schema', 'remote_host': '172.19.0.3', 'message': 'statement: -- ping'},
                {'dbname': 'ptah_audit_schema', 'remote_host': '172.19.0.3', 'message': 'execute stmtcache_1: SELECT $1',
                 'detail': "Parameters: $1 = 'public'"},
                {'dbname': 'ptah_audit_schema', 'remote_host': '172.19.0.3', 'message': 'connection authorized'},
            ]) + '\n')
            self.assertEqual(list(received('postgresql', postgres)),
                             [('query', '-- ping', ''), ('query', 'SELECT $1', "Parameters: $1 = 'public'")])
            mysql = Path(directory) / 'mysql.jsonl'
            mysql.write_text('\n'.join(json.dumps({'time': '2026-10-10T00:00:00.000000', 'type': kind, 'client': 'x', 'thread': 1,
                                                   'argumentHex': text.encode().hex().upper()}) for kind, text in [
                ('Connect', 'ptah_audit@172.19.0.3 on ptah_audit_schema using TCP/IP'),
                ('Prepare', 'SELECT ? FROM t WHERE s = ?'),
                ('Execute', "SELECT 1 FROM t WHERE s = 'ptah_audit_schema'"),
                ('Quit', ''),
            ]) + '\n')
            self.assertEqual(list(received('mysql', mysql)),
                             [('Prepare', 'SELECT ? FROM t WHERE s = ?', ''),
                              ('Execute', "SELECT 1 FROM t WHERE s = '{{database}}'", '')])


class JournalFilterTest(unittest.TestCase):
    """The MySQL journal keeps every client a checked-in reading holds, and none of the lab's own."""

    def admits(self):
        sent = []
        def docker(argv, data=None, check=True, timeout=None):
            sent.append(data.decode())
            return SimpleNamespace(returncode=0, stdout=b'', stderr=b'')
        lab = Lab('ctx', 'mysql', 'executor')
        lab.server = lab.name + '-server'
        with mock.patch.object(schema_sql_readings, 'run', docker):
            self.assertEqual(lab.journal('1970-01-01'), [])
        clauses = re.findall(r"user_host (NOT )?LIKE '([^']*)'", sent[-1])
        self.assertTrue(clauses, sent[-1])
        def like(pattern):
            return re.compile(''.join('.*' if c == '%' else '.' if c == '_' else re.escape(c) for c in pattern), re.S)
        rules = [(bool(negated), like(pattern)) for negated, pattern in clauses]
        return lambda client: all(bool(rule.fullmatch(client)) != negated for negated, rule in rules)

    def test_it_keeps_the_executor_and_leaves_out_the_lab(self):
        admits = self.admits()
        files = sorted(READINGS.glob('mysql-schema-*.jsonl')) + [READINGS / 'mysql-migration-history-audit.jsonl',
                                                                   READINGS / 'mysql-restored-history-refusal.jsonl']
        clients = {json.loads(line)['client'] for path in files for line in path.read_text().splitlines() if line.strip()}
        self.assertGreaterEqual(len(clients), 2, files)
        for client in sorted(clients):
            self.assertTrue(admits(client), client)
        # The readiness probe over TCP, as the image's skip-name-resolve
        # writes it, and the lab's own socket statements.
        for client in ('root[root] @  [127.0.0.1]', 'root[root] @ localhost []'):
            self.assertFalse(admits(client), client)


class LabCleanupTest(unittest.TestCase):
    """What a Lab leaves on the daemon, read from the commands it sends one."""

    def setUp(self):
        self.calls = []
        self.refuse = lambda verb: False
        patcher = mock.patch.object(schema_sql_readings, 'run', self.docker)
        patcher.start()
        self.addCleanup(patcher.stop)
        # Each reading of the clock is a minute later, so a wait ends at once.
        clock = mock.patch.object(schema_sql_readings, 'time')
        self.time = clock.start()
        self.addCleanup(clock.stop)
        self.time.time.side_effect = itertools.count(step=60)

    def docker(self, argv, data=None, check=True, timeout=None):
        verb = argv[3:]
        self.calls.append(verb)
        refused = self.refuse(verb)
        if refused and check:
            raise RuntimeError(f'{verb[0]} refused')
        return SimpleNamespace(returncode=1 if refused else 0, stdout=b'', stderr=b'')

    def called(self, *verb):
        return [call for call in self.calls if call[:len(verb)] == list(verb)]

    def server(self):
        """The container the Lab asked the daemon to run, by the name it sent."""
        started = self.called('run', '-d')
        self.assertEqual(len(started), 1, self.calls)
        return started[0][started[0].index('--name') + 1]

    def assert_removed_everything(self, lab):
        self.assertEqual(self.calls[-2:], [['rm', '-f', '-v', self.server()], ['network', 'rm', '-f', lab.name]])

    def test_a_start_that_fails_removes_what_it_created(self):
        probe = lambda verb: verb[0] == 'exec' and '-i' not in verb
        truncate = lambda verb: verb[:2] == ['exec', '-i']
        run_detached = lambda verb: verb[:2] == ['run', '-d']
        for name, engine, refuse, error in [
            ('postgresql never answers', 'postgresql', probe, 'did not start'),
            ('mysql never answers', 'mysql', probe, 'did not start'),
            ('mysql answers and refuses the first statement', 'mysql', truncate, 'exec refused'),
            ('a docker run that fails after creating the container', 'postgresql', run_detached, 'run refused'),
        ]:
            with self.subTest(name):
                self.calls.clear()
                self.refuse = refuse
                lab = Lab('ctx', engine, 'executor')
                with self.assertRaisesRegex(RuntimeError, error):
                    with lab:
                        self.fail('the block ran after a failed start')
                self.assert_removed_everything(lab)

    def test_an_interrupted_wait_removes_what_it_created(self):
        self.refuse = lambda verb: verb[0] == 'exec'
        self.time.sleep.side_effect = KeyboardInterrupt
        lab = Lab('ctx', 'postgresql', 'executor')
        with self.assertRaises(KeyboardInterrupt):
            with lab:
                self.fail('the block ran after a failed start')
        self.assert_removed_everything(lab)

    def test_a_network_that_failed_leaves_no_container_to_remove(self):
        self.refuse = lambda verb: verb[:2] == ['network', 'create']
        lab = Lab('ctx', 'postgresql', 'executor')
        with self.assertRaisesRegex(RuntimeError, 'network refused'):
            with lab:
                self.fail('the block ran after a failed start')
        self.assertEqual(self.called('run'), [])
        self.assertEqual(self.called('rm'), [])

    def test_a_start_that_works_removes_nothing_until_the_block_ends(self):
        lab = Lab('ctx', 'postgresql', 'executor')
        with lab:
            self.assertEqual(self.called('rm'), [])
            self.assertEqual(self.called('network', 'rm'), [])
        self.assert_removed_everything(lab)


class CaptureTest(unittest.TestCase):
    """What a whole capture writes, against a daemon that answers every command."""

    def setUp(self):
        self.scratch = temporary_directory(self)
        # Where the script's own temporary files go, apart from what it writes.
        self.temporary = temporary_directory(self)
        for patcher in (mock.patch.object(schema_sql_readings, 'run', self.docker),
                        *temporary_files_in(self.temporary)):
            patcher.start()
            self.addCleanup(patcher.stop)
        # Each reading of the clock is a minute later, so a wait ends at once.
        clock = mock.patch.object(schema_sql_readings, 'time')
        clock.start().time.side_effect = itertools.count(step=60)
        self.addCleanup(clock.stop)

    def docker(self, argv, data=None, check=True, timeout=None):
        verb = argv[3:]
        # A saved plan copied out of the executor's container.
        if verb[0] == 'cp' and verb[1].endswith(':/tmp/plan.json'):
            Path(verb[2]).write_text('{}')
        return SimpleNamespace(returncode=0, stdout=b'', stderr=b'')

    def test_a_capture_writes_every_reading_the_contract_reads(self):
        for engine in ('postgresql', 'mysql'):
            with self.subTest(engine):
                out = self.scratch / engine
                out.mkdir()
                results = schema_sql_readings.capture('ctx', 'executor', engine, out)
                witnesses = {name for name, _ in contract_readings(engine) if name.startswith(f'{engine}-schema-')}
                self.assertTrue(witnesses)
                expected = witnesses | {f'{engine}-schema-drift-plan-before.jsonl', f'{engine}-schema-drift-apply-current.jsonl'}
                self.assertEqual(set(results), expected)
                self.assertEqual({path.name for path in out.glob('*.jsonl')}, expected)
                for scope in ('wide', 'narrow'):
                    self.assertTrue((out / f'{engine}-schema-exclusion-{scope}-plan.json').is_file())
                self.assertEqual(list(self.temporary.iterdir()), [], 'a capture left temporary files behind')


def temporary_files_in(directory):
    """Send every temporary file the script makes to directory, whichever way it names the place."""
    return (mock.patch.object(tempfile, 'tempdir', str(directory)),
            mock.patch.dict('os.environ', {'TMPDIR': str(directory)}))


def temporary_directory(test):
    directory = tempfile.TemporaryDirectory()
    test.addCleanup(directory.cleanup)
    return Path(directory.name)


class DaemonTest(unittest.TestCase):
    """Whole runs of the script against a fake docker client, below run(), so its bounds are what is measured."""

    def setUp(self):
        self.calls = []
        # The verbs that hang, and how many of each to let through first.
        self.hang = {}
        # The verbs the daemon refuses, and what it says.
        self.refused = {}
        # The bound each command was sent with.
        self.bounds = []
        self.temporary = temporary_directory(self)
        self.out = temporary_directory(self)
        for patcher in (mock.patch.object(schema_sql_readings.subprocess, 'run', self.docker),
                        *temporary_files_in(self.temporary)):
            patcher.start()
            self.addCleanup(patcher.stop)
        # Each reading of the clock is a minute later, so a wait ends at once.
        clock = mock.patch.object(schema_sql_readings, 'time')
        clock.start().time.side_effect = itertools.count(step=60)
        self.addCleanup(clock.stop)
        # main() routes the signals that end a run to cleanup; this process keeps its own.
        for number in (signal.SIGTERM, signal.SIGHUP):
            self.addCleanup(signal.signal, number, signal.getsignal(number))

    def docker(self, argv, input=None, capture_output=False, timeout=None):
        verb = argv[3:]
        self.calls.append(verb)
        self.bounds.append((verb, timeout))
        self.assertIsNotNone(timeout, f'{verb[:2]} was sent with no bound')
        for prefix, stderr in self.refused.items():
            if verb[:len(prefix)] == list(prefix):
                return subprocess.CompletedProcess(argv, 1, b'', stderr)
        for (prefix, allowed) in list(self.hang.items()):
            if verb[:len(prefix)] == list(prefix):
                if allowed == 0:
                    raise subprocess.TimeoutExpired(argv, timeout)
                self.hang[prefix] = allowed - 1
        # A saved plan copied out of the executor's container.
        if verb[0] == 'cp' and verb[1].endswith(':/tmp/plan.json'):
            Path(verb[2]).write_text('{}')
        return subprocess.CompletedProcess(argv, 0, b'', b'')

    def main(self):
        argv = ['schema_sql_readings.py', '--context', 'ctx', '--executor-image', 'executor', '--out', str(self.out)]
        with mock.patch.object(schema_sql_readings.sys, 'argv', argv), contextlib.redirect_stdout(io.StringIO()):
            schema_sql_readings.main()

    def made(self):
        """Every container and network the run asked the daemon to make, by name."""
        containers = [verb[verb.index('--name') + 1] for verb in self.calls if verb[:2] == ['run', '-d'] or verb[0] == 'create']
        networks = [verb[2] for verb in self.calls if verb[:2] == ['network', 'create']]
        return containers, networks

    def assert_removed_what_it_made(self):
        containers, networks = self.made()
        self.assertTrue(containers and networks, 'the run made nothing, so this checks nothing')
        for name in containers:
            self.assertIn(['rm', '-f', '-v', name], self.calls)
        for name in networks:
            self.assertIn(['network', 'rm', '-f', name], self.calls)
        self.assertEqual(list(self.temporary.iterdir()), [], 'the run left temporary files behind')

    def test_a_run_that_works_leaves_nothing_behind(self):
        with self.assertRaises(SystemExit) as exit:
            self.main()
        self.assertEqual(exit.exception.code, 0)
        self.assertTrue((self.out / 'capture.json').is_file())
        self.assert_removed_what_it_made()

    def test_an_executor_that_hangs_partway_ends_the_run_and_removes_what_it_made(self):
        # The fifth executor run hangs, after earlier ones saved plans.
        self.hang = {('start', '-a'): 4}
        with self.assertRaisesRegex(CommandTimeout, 'docker --context ctx start did not finish within 600 seconds'):
            self.main()
        self.assert_removed_what_it_made()

    def test_a_statement_that_hangs_ends_the_run_and_removes_what_it_made(self):
        self.hang = {('exec', '-i'): 0}
        with self.assertRaisesRegex(CommandTimeout, 'exec did not finish'):
            self.main()
        self.assert_removed_what_it_made()

    def test_a_probe_that_hangs_is_a_server_that_did_not_answer(self):
        self.hang = {('exec',): 0}
        with self.assertRaisesRegex(RuntimeError, 'postgresql did not start'):
            with Lab('ctx', 'postgresql', 'executor'):
                self.fail('the block ran after a failed start')
        probes = [verb for verb in self.calls if verb[0] == 'exec']
        self.assertTrue(probes, 'nothing probed the server')
        self.assert_removed_what_it_made()

    def test_a_removal_that_hangs_is_reported_and_the_rest_still_go(self):
        self.hang = {('rm',): 0}
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            with Lab('ctx', 'postgresql', 'executor') as lab:
                pass
        server = lab.name + '-server'
        self.assertIn(['network', 'rm', '-f', lab.name], self.calls)
        self.assertIn(f'remove the container {server} by hand', stderr.getvalue())
        self.assertEqual(list(self.temporary.iterdir()), [])

    def test_a_removal_the_daemon_refuses_is_reported(self):
        # What a daemon answers for a container it could not kill, and for the
        # network that container is still attached to.
        self.refused = {('rm',): b'Error response from daemon: tried to kill container, but did not receive an exit event',
                        ('network', 'rm'): b'Error response from daemon: error while removing network: has active endpoints'}
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            with Lab('ctx', 'postgresql', 'executor') as lab:
                pass
        self.assertIn(f'remove the container {lab.name}-server by hand', stderr.getvalue())
        self.assertIn('did not receive an exit event', stderr.getvalue())
        self.assertIn(f'remove the network {lab.name} by hand', stderr.getvalue())
        self.assertEqual(list(self.temporary.iterdir()), [])

    def test_a_hung_container_removal_names_the_network_it_strands(self):
        self.hang = {('rm',): 0}
        self.refused = {('network', 'rm'): b'Error response from daemon: error while removing network: has active endpoints'}
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            with Lab('ctx', 'postgresql', 'executor') as lab:
                pass
        self.assertIn(f'remove the container {lab.name}-server by hand', stderr.getvalue())
        self.assertIn(f'remove the network {lab.name} by hand', stderr.getvalue())

    def test_a_probe_is_sent_with_the_probe_bound(self):
        with Lab('ctx', 'postgresql', 'executor'):
            pass
        probes = [bound for verb, bound in self.bounds if verb[0] == 'exec' and '-i' not in verb]
        self.assertTrue(probes, 'nothing probed the server')
        self.assertEqual(set(probes), {schema_sql_readings.PROBE_TIMEOUT})
        # A probe ends well inside the start's own deadline, so the deadline decides.
        self.assertLess(schema_sql_readings.PROBE_TIMEOUT * 10, 180)

    def test_a_removal_that_hangs_after_a_hung_executor_keeps_the_first_error(self):
        self.hang = {('start', '-a'): 0, ('rm',): 0}
        with contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaisesRegex(CommandTimeout, 'start did not finish'):
                with Lab('ctx', 'postgresql', 'executor') as lab:
                    lab.ptah(schema_sql_readings.OBSERVE, desired='postgresql-v1.sql')

    def test_a_create_that_hangs_still_removes_its_container(self):
        # The daemon may have made the container before the client stopped waiting.
        self.hang = {('create',): 0}
        with self.assertRaisesRegex(CommandTimeout, 'create did not finish'):
            self.main()
        self.assert_removed_what_it_made()


# A docker client that records each command and stays in `start` until killed.
FAKE_DOCKER = """#!/bin/sh
echo "$*" >> "$PTAH_FAKE_DOCKER_LOG"
case "$3" in
  start) exec sleep 60 ;;
esac
exit 0
"""


class SignalTest(unittest.TestCase):
    """The script run as a process, ended by the signals a terminal or a job runner sends."""

    def stop_with(self, number):
        root = temporary_directory(self)
        bin_dir, temporary, out = root / 'bin', root / 'tmp', root / 'out'
        for directory in (bin_dir, temporary, out):
            directory.mkdir()
        log = root / 'calls.log'
        log.write_text('')
        (bin_dir / 'docker').write_text(FAKE_DOCKER)
        (bin_dir / 'docker').chmod(0o755)
        environment = {**os.environ, 'PATH': f"{bin_dir}{os.pathsep}{os.environ['PATH']}", 'TMPDIR': str(temporary),
                       'PTAH_FAKE_DOCKER_LOG': str(log)}
        process = subprocess.Popen([sys.executable, schema_sql_readings.__file__, '--context', 'ctx', '--executor-image', 'executor',
                                    '--engine', 'postgresql', '--part', 'schema', '--out', str(out)],
                                   env=environment, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        self.addCleanup(process.kill)
        deadline = time.monotonic() + 30
        while not any(line.split()[2:3] == ['start'] for line in log.read_text().splitlines()):
            self.assertIsNone(process.poll(), process.stderr.read().decode()[-400:] if process.poll() is not None else '')
            self.assertLess(time.monotonic(), deadline, 'the run never started the executor')
            time.sleep(0.05)
        process.send_signal(number)
        process.communicate(timeout=30)
        calls = [line.split()[2:] for line in log.read_text().splitlines()]
        made = [call[call.index('--name') + 1] for call in calls if call[:2] == ['run', '-d'] or call[:1] == ['create']]
        networks = [call[2] for call in calls if call[:2] == ['network', 'create']]
        self.assertEqual(len(made), 2, calls)
        self.assertEqual(len(networks), 1, calls)
        for name in made:
            self.assertIn(['rm', '-f', '-v', name], calls)
        self.assertIn(['network', 'rm', '-f', networks[0]], calls)
        self.assertEqual(list(temporary.iterdir()), [], 'the run left temporary files behind')

    def test_sigterm_removes_what_it_made(self):
        self.stop_with(signal.SIGTERM)

    def test_sighup_removes_what_it_made(self):
        self.stop_with(signal.SIGHUP)


if __name__ == '__main__':
    unittest.main()
