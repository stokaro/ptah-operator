# Source content digest cases

`cases.json` holds the cases every implementation of the source content digest
is held to: `internal/sourcecontent` here, and any other program that computes
it, such as Ptah's Git pull (stokaro/ptah#4827). Copy the file byte for byte
and name the commit it came from.

The digest is defined in `internal/sourcecontent`. The manifest has one line
per file, sorted by the path's bytes:

    <lowercase hex SHA-256 of the file> <size in bytes, decimal> <path>\n

and the digest is `sha256:` followed by the lowercase hex SHA-256 of the
manifest. A path is relative and slash-separated, valid UTF-8, with no empty,
`.` or `..` element, no control character, no backslash, and no element longer
than 255 bytes. A source selects between 1 and 1,000 files, no path twice, and
no path that is both a file and a directory.

- `accepted`: the files, base64-encoded, with the manifest and the digest they
  must give. The files are listed out of order on purpose.
- `generated`: a case too large to list. `generate` expands to `count` files,
  the i-th at `pathFormat % i` for i from 0, each holding `data`.
- `refused`: files the rules refuse, and the rule: `empty`, `too-many-files`,
  `invalid-path`, `duplicate` or `path-conflict`. A path that is not valid
  UTF-8 is given as `pathBase64`.

The expected manifests and digests were computed by an implementation written
apart from the Go package, with Python's `hashlib`, so the Go tests compare two
implementations rather than restating one.
