# Schema revisions

The desired state, as the SQL an artifact carries. Each file is one published
revision, and the scenarios move between them.

| File | What it declares |
| --- | --- |
| `v1.sql` | `customers`, the starting point. |
| `v2.sql` | A column on `customers`, and an `orders` table that references it. |
| `v3.sql` | `v2` with the `email` column removed, which is the destructive change a policy has to decide about. |

A revision is published to the lab's registry as an OCI artifact, and the
operator is pointed at the digest the push returned. Editing a file here
changes nothing until it is published, which is the point: the desired state is
an artifact with an identity, not a file somebody edited.
