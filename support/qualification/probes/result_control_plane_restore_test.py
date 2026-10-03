import copy
import unittest

from result_control_plane_restore import ready_replacements, retained_objects


class RestoreReadbackTests(unittest.TestCase):
    def runtime(self):
        before = [{'uid': str(i), 'containerID': 'old-' + str(i), 'imageID': 'same-image'} for i in range(3)]
        pods = [{'metadata': {'uid': str(i)}, 'status': {'conditions': [{'type': 'Ready', 'status': 'True'}], 'containerStatuses': [{'containerID': 'new-' + str(i), 'imageID': 'same-image', 'ready': True, 'state': {'running': {'startedAt': '2026-10-03T00:00:00Z'}}}]}} for i in range(3)]
        return before, pods

    def test_requires_new_ready_processes_on_original_pods(self):
        before, pods = self.runtime()
        self.assertTrue(ready_replacements(before, pods))
        for fault in ('old-process', 'missing-id', 'different-image', 'not-ready', 'not-running', 'replaced-pod', 'missing-pod'):
            with self.subTest(fault=fault):
                changed = copy.deepcopy(pods)
                status = changed[0]['status']['containerStatuses'][0]
                if fault == 'old-process':
                    status['containerID'] = before[0]['containerID']
                elif fault == 'missing-id':
                    status.pop('containerID')
                elif fault == 'different-image':
                    status['imageID'] = 'unqualified-image'
                elif fault == 'not-ready':
                    status['ready'] = False
                elif fault == 'not-running':
                    status['state'] = {'terminated': {}}
                elif fault == 'replaced-pod':
                    changed[0]['metadata']['uid'] = 'replacement'
                else:
                    changed.pop()
                self.assertFalse(ready_replacements(before, changed))
        with self.assertRaises(ValueError):
            ready_replacements([], [])

    def test_inventory_covers_bytes_and_identity(self):
        def obj(kind, name, data):
            return {'kind': kind, 'metadata': {'name': name, 'namespace': 'test', 'uid': name + '-uid'}, 'data': data}
        values = {'records': [], 'credentialProjections': [], 'trust': obj('Secret', 'trust', {'key': 'original'}), 'journal': obj('Secret', 'journal', {'rotation.json': 'original'}), 'enrollmentPolicy': obj('ConfigMap', 'policy', {'ca': 'original'})}
        baseline = retained_objects(values)
        for field in ('trust', 'journal', 'enrollmentPolicy'):
            changed = copy.deepcopy(values)
            changed[field]['data'] = {'changed': 'changed'}
            self.assertNotEqual(retained_objects(changed), baseline)
            changed = copy.deepcopy(values)
            changed[field]['metadata']['uid'] = 'replacement'
            self.assertNotEqual(retained_objects(changed), baseline)
        values['credentialProjections'].append(values['trust'])
        with self.assertRaises(ValueError):
            retained_objects(values)


if __name__ == '__main__':
    unittest.main()
