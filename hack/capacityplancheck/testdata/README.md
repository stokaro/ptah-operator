# Native MySQL capacity plans

Both documents were emitted by Ptah commit
`f6e562c5b0986cd29a53a5cc01938827336b780a` on MySQL 8.4.

`mysql-modify-default.json` comes from the failed populated operator run on
operator commit `1fcdde7ea5534bbec0b5a4c48a7468b2ed6c6426`. Ptah marked a
`MODIFY COLUMN` default change safe. The operator correctly raised it to
destructive and refused to dispatch it under the workload policy.

`mysql-create-default.json` comes from the corrected small-band fixture. It
adds a table with an executable default and passes the same operator policy.
These plans contain fixture SQL and state fingerprints, with no credentials.
