"""Deterministic inputs for the frozen capacity workload, not run evidence."""

import hashlib
from pathlib import Path


ROWS = 10_000
HISTORIES = (2, 32, 128)
BANDS = {
    'small': (3 * 1024, 4 * 1024, 1),
    'medium': (240 * 1024, 256 * 1024, 4),
    'large': (960 * 1024, 1024 * 1024, 16),
}
ROW_TABLE = 'CREATE TABLE capacity_rows (id BIGINT NOT NULL PRIMARY KEY, payload VARCHAR(64) NOT NULL);\n'


def row_payload(slot, row):
    if type(slot) is not int or not 0 <= slot < 20 or type(row) is not int or not 1 <= row <= ROWS:
        raise ValueError('row identity must belong to a frozen workload slot')
    return hashlib.sha256(f'ptah-0.2.0-capacity/{slot}/{row}'.encode()).hexdigest()


def row_inventory(slot):
    return ''.join(f'{row}\t{row_payload(slot, row)}\n' for row in range(1, ROWS + 1)).encode()


def seed_sql(slot):
    # Batches bound parser/packet size on both supported engines. No random,
    # engine-generated, or collation-dependent values enter the row digest.
    lines = []
    for start in range(1, ROWS + 1, 1000):
        values = ',\n'.join(f"({row}, '{row_payload(slot, row)}')" for row in range(start, start + 1000))
        lines.append('INSERT INTO capacity_rows (id, payload) VALUES\n' + values + ';\n')
    return ''.join(lines).encode()


def schema_sql(engine, band, repeated, round_number=None, previous=()):
    if engine not in ('postgresql', 'mysql') or band not in BANDS:
        raise ValueError('unknown engine or plan band')
    if type(repeated) is not int or repeated < 0:
        raise ValueError('the repeated literal length must be nonnegative')
    if round_number is not None and (type(round_number) is not int or not 0 <= round_number <= 9):
        raise ValueError('the frozen workload has an initial artifact and nine change rounds')
    tables = BANDS[band][2]
    if engine == 'mysql':
        if len(previous) != (round_number or 0):
            raise ValueError('MySQL additions must preserve every previous round')
        counts = (*previous, repeated) if round_number is not None else ()
        if any(type(n) is not int or n < 0 or (n + tables - 1) // tables + 3 > 16_000 for n in counts):
            raise ValueError('a fixture default exceeds the MySQL VARCHAR bound')
        parts = [ROW_TABLE]
        # MODIFY COLUMN is conservatively destructive in the operator even
        # when Ptah labels a default change safe. Add tables and retain every
        # prior definition so the workload needs no destructive permission.
        for version, count in enumerate(counts):
            for table in range(tables):
                value = '<' * (count // tables + (table < count % tables)) + f'r{version:02d}'
                name = payload_table(engine, table, version)
                parts.append(f"CREATE TABLE {name} (id BIGINT NOT NULL PRIMARY KEY, payload VARCHAR(16000) DEFAULT '{value}');\n")
        return ''.join(parts).encode()
    # These are executable column defaults, not comments or padded plan JSON.
    # The verifier measures the saved native plan before accepting a band.
    if (repeated + tables - 1) // tables + 3 > 16_000:
        raise ValueError('a fixture default exceeds the MySQL VARCHAR bound')
    parts = [ROW_TABLE]
    for table in range(tables):
        count = repeated // tables + (table < repeated % tables)
        default = '' if round_number is None else '<' * count + f'r{round_number:02d}'
        column_type = 'TEXT' if engine == 'postgresql' else 'VARCHAR(16000)'
        parts.append(f"CREATE TABLE capacity_payload_{table:03d} (id BIGINT NOT NULL PRIMARY KEY, payload {column_type} DEFAULT '{default}');\n")
    return ''.join(parts).encode()


def payload_table(engine, table, version):
    suffix = f'_r{version:02d}' if engine == 'mysql' else ''
    return f'capacity_payload_{table:03d}{suffix}'


def migration_sql(version):
    if type(version) is not int or not 1 <= version <= 128:
        raise ValueError('migration version exceeds the frozen history bound')
    if version == 1:
        return ROW_TABLE.encode(), b'DROP TABLE capacity_rows;\n'
    name = f'history_{version:03d}'
    return (f'ALTER TABLE capacity_rows ADD COLUMN {name} INTEGER NOT NULL DEFAULT {version};\n'.encode(),
            f'ALTER TABLE capacity_rows DROP COLUMN {name};\n'.encode())


def write_migrations(directory, length):
    if type(length) is not int or not 2 <= length <= 128:
        raise ValueError('migration fixture must contain 2 through 128 versions')
    directory = Path(directory)
    directory.mkdir(mode=0o700)
    for version in range(1, length + 1):
        up, down = migration_sql(version)
        for direction, sql in [('up', up), ('down', down)]:
            with (directory / f'{version:010d}_capacity_{version:03d}.{direction}.sql').open('xb') as output:
                output.write(sql)


def verify_row_inventory(raw, slot):
    expected = row_inventory(slot)
    if raw != expected:
        raise ValueError('database rows differ from the complete deterministic slot inventory')
    return {'rows': ROWS, 'slot': slot, 'sha256': hashlib.sha256(raw).hexdigest()}
