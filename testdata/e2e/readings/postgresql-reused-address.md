The JSON Lines file contains actual PostgreSQL 17 records from two native
Docker clients on a task-owned network. The first client read `postgres` and
was removed. The second received the same numeric IP, read the isolated
`ptah_lifetime_probe` database, then read `postgres`. The adjacent JSON retains
the distinct client IDs and the second client's actual Docker creation,
start and finish times. The server observed that same address for every record.

The matcher using only IP attributes the first client's earlier statement to
the second client and refuses it. `TestPostgresRefusalReadsNativeReusedAddressSessions`
requires the dated matcher to accept the isolated diagnostic control after
excluding that earlier use of the address. It must still refuse the second
client's real cross-database statement inside its own lifetime. Missing or
out-of-lifetime target SQL cannot supply a diagnostic control.

The native probe used the driver's pinned PostgreSQL 17 Alpine image, UTC JSON
logging and an isolated disposable network with no published port. Its clients,
database container, temporary storage and network were removed. Docker dates
are rounded to the same second precision the acceptance matcher handles for
Kubernetes API dates. This evidence measures server fields and address reuse;
it does not prove Kubernetes ownership, admission or production acceptance.

The final matcher also retains PostgreSQL's server-side `session_start`.
A statement after container termination cannot be excused if its connection
started during that container's lifetime. Regression controls require such a
late error to fail, along with missing session dates and overlapping timestamp
precision. The clock comparison is limited to kind nodes and database
containers sharing the Docker host's clock.
