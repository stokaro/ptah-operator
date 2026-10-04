# PostgreSQL lifecycle barrier reading

The JSON file projects one actual server log row from the native lifecycle run
`020-lifecycle-sql`, Kubernetes 1.37.0 on Linux amd64, using operator source
`7a9072948ed104bd491b45f3cd915eceb8affdc8` and the catalog-pinned Ptah source
`f6e562c5b0986cd29a53a5cc01938827336b780a`.

The original Job was `db1dc4d9-a4b1-4a22-958a-32f66a8e80a8` and its Pod was
`63ec0574-ae2f-4039-a55b-904653d5ede3`. The executed audit identified PostgreSQL
backend 190, session start `2026-09-30 16:50:19 UTC`, in `ptah_external`.
The server saw `172.18.0.5`, the kind node address after masquerading, rather
than the Pod address. It received the exact advisory-lock statement shown in
the JSON while that backend was blocked by the fixture's barrier.

Both the late upgrade failure and same-candidate retry preserved that backend
session and produced zero additional remote SQL records in their measured
windows. The harness also required the original Job/Pod ownership and running
state. This single row tests the parser's received-statement control; the full
runtime result and surrounding journal are separate evidence. It does not
qualify MySQL, migration operations, SQL after the barrier was released, or
later-source rollback/uninstall audits.

Only the timestamp, database, process, client, session start and controlled
statement were copied. Other raw SQL remains private. The retained private
source snapshot has SHA-256 `c0651feeb94301df7ea82a713d7225f48197277a503d668d45dff2378208d9ce`.
