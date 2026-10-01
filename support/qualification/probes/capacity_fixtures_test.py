"""Input and refusal controls; native SQL execution is a separate probe."""

import hashlib
from pathlib import Path
import tempfile
import unittest

from capacity_fixtures import (BANDS, HISTORIES, ROWS, migration_sql, row_inventory,
                               schema_sql, seed_sql, verify_row_inventory, write_migrations)


class FixtureTests(unittest.TestCase):
    def test_complete_distinct_slot_inventories_and_corruption_refusals(self):
        digests = set()
        for slot in range(20):
            raw = row_inventory(slot)
            rows = raw.splitlines()
            self.assertEqual(len(rows), ROWS)
            self.assertEqual([int(row.split(b'\t')[0]) for row in rows], list(range(1, ROWS + 1)))
            self.assertEqual(len(set(row.split(b'\t')[1] for row in rows)), ROWS)
            digests.add(verify_row_inventory(raw, slot)['sha256'])
        self.assertEqual(len(digests), 20)
        raw = row_inventory(0)
        rows = raw.splitlines(keepends=True)
        for bad in [b'', raw[:-1], b''.join(rows[1:]), rows[0] + raw, b''.join(reversed(rows)),
                    raw.replace(rows[2], rows[3]), row_inventory(1)]:
            with self.assertRaises(ValueError):
                verify_row_inventory(bad, 0)

    def test_seed_is_bounded_and_contains_every_expected_value_once(self):
        sql = seed_sql(0)
        self.assertEqual(sql.count(b'INSERT INTO capacity_rows'), 10)
        self.assertEqual(sql.count(b';\n'), 10)
        values = row_inventory(0).splitlines()
        for row in values:
            number, payload = row.split(b'\t')
            self.assertEqual(sql.count(b'(' + number + b", '" + payload + b"')"), 1)

    def test_schema_rounds_change_real_defaults_without_changing_rows(self):
        for engine in ['postgresql', 'mysql']:
            for band, (_, _, tables) in BANDS.items():
                baseline = schema_sql(engine, band, 0)
                self.assertEqual(baseline.count(b"DEFAULT ''"), tables)
                variants = [schema_sql(engine, band, tables * 50 + 1, r) for r in range(10)]
                self.assertEqual(len({hashlib.sha256(v).digest() for v in variants}), 10)
                self.assertEqual(len({len(v) for v in variants}), 1)
                for r, content in enumerate(variants):
                    self.assertEqual(content.count(b'<'), tables * 50 + 1)
                    self.assertEqual(content.count(f'r{r:02d}'.encode()), tables)
                    self.assertEqual(content.count(b'CREATE TABLE capacity_rows'), 1)
                    self.assertNotIn(b'--', content)
                    self.assertNotIn(b'DROP', content)

    def test_history_lengths_include_real_up_and_down_ddl(self):
        with tempfile.TemporaryDirectory() as root:
            for length in HISTORIES:
                directory = Path(root) / str(length)
                write_migrations(directory, length)
                self.assertEqual(len(list(directory.iterdir())), 2 * length)
                for version in range(1, length + 1):
                    up, down = migration_sql(version)
                    self.assertEqual((directory / f'{version:010d}_capacity_{version:03d}.up.sql').read_bytes(), up)
                    self.assertEqual((directory / f'{version:010d}_capacity_{version:03d}.down.sql').read_bytes(), down)
                with self.assertRaises(FileExistsError):
                    write_migrations(directory, length)

    def test_invalid_dimensions_are_refused(self):
        for args in [('sqlite', 'small', 1, 0), ('mysql', 'unknown', 1, 0),
                     ('mysql', 'small', 16000, 0), ('postgresql', 'small', -1, 0),
                     ('postgresql', 'small', 1, 10), ('postgresql', 'small', True, 0)]:
            with self.assertRaises(ValueError):
                schema_sql(*args)
        for version in [0, 129, True, '2']:
            with self.assertRaises(ValueError):
                migration_sql(version)


if __name__ == '__main__':
    unittest.main()
