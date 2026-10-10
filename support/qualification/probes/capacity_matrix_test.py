from pathlib import Path
import unittest

from capacity_matrix import ROOT, STEPS, WORKLOADS, cell_name, cells, step_argv


class CapacityMatrixTest(unittest.TestCase):
    def test_every_supported_minor_runs_both_engines_and_all_three_workloads(self):
        selected = list(cells(['1.35.1', '1.36.2', '1.37.0']))
        self.assertEqual(len(selected), 18)
        self.assertEqual(len({cell_name(c) for c in selected}), 18)
        for cell in selected:
            self.assertTrue((ROOT / cell['file']).is_file(), cell['file'])
            self.assertEqual(cell['file'].endswith('-mysql.json'), cell['engine'] == 'MySQL')

    def test_each_workload_file_names_its_engine(self):
        import json
        for (engine, _), name in WORKLOADS.items():
            workload = json.loads((ROOT / 'support/capacity' / name).read_text())
            self.assertEqual(workload['engine'], engine, name)

    def test_the_lab_steps_run_in_the_profile_order_with_their_inputs(self):
        self.assertEqual(STEPS, ('bootstrap', 'install', 'kindnet', 'fixtures', 'prepare', 'network', 'measure'))
        cell = next(cells(['1.37.0'], ('MySQL',), ('overload',)))
        bootstrap = step_argv('bootstrap', Path('/lab'), cell, 'diabolocom', Path('/manifest'))
        self.assertEqual(bootstrap[bootstrap.index('--kubernetes') + 1], '1.37.0')
        self.assertEqual(bootstrap[bootstrap.index('--context') + 1], 'diabolocom')
        self.assertEqual(bootstrap[bootstrap.index('--release-manifest') + 1], '/manifest')
        prepare = step_argv('prepare', Path('/lab'), cell, 'diabolocom', Path('/manifest'))
        self.assertEqual(prepare[prepare.index('--workload') + 1], str(ROOT / 'support/capacity/overload-mysql.json'))


if __name__ == '__main__':
    unittest.main()
