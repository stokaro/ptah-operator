import copy
import importlib.util
import json
from pathlib import Path
import unittest

ROOT = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('audit_verifier', ROOT / 'verify-audit.py')
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class AuditProof(unittest.TestCase):
    def test_retained_reading(self):
        result = module.verify(json.loads((ROOT / 'report.json').read_text()), module.read_audit(ROOT))
        self.assertEqual(len(result['records']), 8)
        self.assertEqual(sum(r['method'] == 'namespace-controller DeleteCollection' for r in result['records']), 2)

    def test_refusals(self):
        report = json.loads((ROOT / 'report.json').read_text())
        original = module.read_audit(ROOT)
        for fault in ('missing-post-deadline-read', 'early-collection', 'failed-collection', 'filtered-collection', 'foreign-actor', 'replaced-object'):
            with self.subTest(fault=fault):
                audit = copy.deepcopy(original)
                collection = next(e for e in audit if e['verb'] == 'deletecollection' and e.get('responseStatus', {}).get('code') == 200)
                namespace = collection['objectRef']['namespace']
                if fault == 'missing-post-deadline-read':
                    audit = [e for e in audit if e['verb'] != 'get']
                elif fault == 'early-collection':
                    collection['requestReceivedTimestamp'] = '2026-10-02T21:00:00Z'
                elif fault == 'failed-collection':
                    collection['responseStatus']['code'] = 403
                elif fault == 'filtered-collection':
                    collection['requestURI'] += '?labelSelector=wrong'
                elif fault == 'foreign-actor':
                    collection['user']['username'] = 'unrelated-user'
                else:
                    extra = copy.deepcopy(collection)
                    extra['verb'] = 'create'
                    audit.append(extra)
                with self.assertRaises(AssertionError):
                    module.verify(report, audit)


if __name__ == '__main__':
    unittest.main(verbosity=2)
