import copy
import json
from pathlib import Path
import unittest
from result_backup_restore import verify_report


class SnapshotRestoreEvidenceTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        path = Path(__file__).resolve().parent / 'testdata/results/result-backup-restore-2026-10-02/report.json'
        cls.reading = json.loads(path.read_text())

    def test_replays_actual_encrypted_restore(self):
        self.assertEqual(verify_report(self.reading), {'objects': 74, 'resultRecords': 54, 'cohorts': 4, 'receipts': 11})

    def test_refuses_incomplete_or_nonisolated_readback(self):
        for fault in ('same cluster', 'same member', 'wrong revision', 'host network', 'missing engine',
                      'changed bytes', 'missing record', 'missing trust', 'missing receipt', 'missing resource',
                      'wrong key accepted', 'cleanup failed'):
            with self.subTest(fault=fault):
                r = copy.deepcopy(self.reading)
                if fault == 'same cluster': r['restoredEtcd']['header']['cluster_id'] = r['sourceEtcd']['header']['cluster_id']
                if fault == 'same member': r['restoredEtcd']['header']['member_id'] = r['sourceEtcd']['header']['member_id']
                if fault == 'wrong revision': r['restoredEtcd']['header']['revision'] += 1
                if fault == 'host network': r['restoredNetworkNamespace'] = [{'type': 'network', 'path': '/proc/1/ns/net'}]
                if fault == 'missing engine': r['cohorts'].pop()
                if fault == 'changed bytes': r['storageRows'][0]['restoredSHA256'] = '0' * 64
                if fault == 'missing record': r['storageRows'] = [x for x in r['storageRows'] if x['kind'] != 'PtahResultRecord']
                if fault == 'missing trust': r['storageRows'] = [x for x in r['storageRows'] if not x['path'].endswith('-result-trust')]
                if fault == 'missing receipt': r['cohorts'][0]['restoredReceipts'] = []
                if fault == 'missing resource': r['storageRows'] = [x for x in r['storageRows'] if x['uid'] != r['cohorts'][0]['resourceUID']]
                if fault == 'wrong key accepted': r['wrongKeyRefused'] = False
                if fault == 'cleanup failed': r['cleanupExitCodes'][0] = 1
                with self.assertRaises(ValueError):
                    verify_report(r)
