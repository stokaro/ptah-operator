from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace

from restore_matrix import Matrix, cell_name, cells, command, digest_tree, shares_lab


class MatrixTest(unittest.TestCase):
    def test_the_matrix_is_the_declared_thirty_six_cells(self):
        all_cells = list(cells())
        self.assertEqual(len(all_cells), 36)
        self.assertEqual(len({cell_name(c) for c in all_cells}), 36)
        for dimension, values in (('engine', {'postgresql', 'mysql'}), ('family', {'schema', 'migration'}),
                                  ('loss', {'database', 'operator', 'combined'}),
                                  ('timing', {'idle', 'during-apply', 'operator-lag'})):
            self.assertEqual({c[dimension] for c in all_cells}, values)

    def test_only_database_loss_at_idle_or_during_apply_shares_a_lab(self):
        shared = [cell_name(c) for c in cells() if shares_lab(c)]
        self.assertEqual(len(shared), 8)
        self.assertTrue(all('-database-' in name and not name.endswith('operator-lag') for name in shared))

    def test_each_cell_runs_the_procedure_the_runbook_names(self):
        release = {'assets': '/assets', 'preparedManifestSHA256': 'a' * 64}
        for cell in cells():
            argv = command(cell, Path('/env'), Path('/out'), release)
            procedure = Path(argv[1]).name
            self.assertEqual(argv[2:6], [cell['engine'], cell['family'], '/env', '/out'])
            self.assertEqual(argv[argv.index('--loss') + 1], cell['loss'])
            self.assertEqual(argv[argv.index('--timing') + 1], cell['timing'])
            self.assertEqual(argv[argv.index('--release-assets') + 1], '/assets')
            self.assertEqual(argv[argv.index('--prepared-manifest-sha256') + 1], 'a' * 64)
            if shares_lab(cell):
                self.assertEqual(procedure, 'operator_restore.py')
                self.assertNotIn('--result-delivery', argv)
            else:
                self.assertEqual(procedure, 'cluster_restore.py')
                self.assertIn('--result-delivery', argv)

    def test_a_published_release_names_no_draft_digest(self):
        cell = next(cells())
        argv = command(cell, Path('/env'), Path('/out'), {'assets': '/assets'})
        self.assertNotIn('--prepared-manifest-sha256', argv)

    def test_an_unknown_cell_is_refused_before_any_work(self):
        with tempfile.TemporaryDirectory() as directory:
            args = SimpleNamespace(out=str(Path(directory) / 'run'), context='diabolocom', kubernetes='1.37.0',
                                   release_assets=directory, prepared_manifest_sha256=None,
                                   cell=['postgresql-schema-database-idle', 'oracle-schema-database-idle'])
            with self.assertRaises(ValueError):
                Matrix(args)
            self.assertFalse((Path(directory) / 'run').exists())

    def test_retained_files_are_hashed_by_path(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'nested').mkdir()
            (root / 'nested' / 'result.json').write_text('{}')
            (root / 'link').symlink_to(root / 'nested' / 'result.json')
            self.assertEqual(digest_tree(root), {
                'nested/result.json': '44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a'})


if __name__ == '__main__':
    unittest.main()
