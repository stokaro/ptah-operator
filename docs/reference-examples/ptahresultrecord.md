The result transport writes these immutable records. A result up to 256 KiB
commits atomically in its intent, which contains its bytes, digest, and operation
binding. Larger results use the chunk and completion records shown below.
Grant read access only to
identities allowed to inspect operation SQL and delivery credentials; ordinary
Secret read permission does not grant access to them. Include this resource in
the cluster's encryption-at-rest and restricted backup configuration.

A `retired` record binds a completed or abandoned attempt to the exact credential
or publication intent that proved its identity. Its API-assigned creation time
starts the retention window. It prevents issuing credentials or publishing more
results for that attempt; it does not erase an acknowledged result or override
the resource's recovery pins. These records are created by the manager.

A payload chunk has the following shape. Its publication intent supplies the
binding, index, byte length, and digest. Reading a chunk alone does not establish
that a complete result was accepted.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahResultRecord
metadata:
  name: example-result-chunk
  namespace: default
spec:
  type: chunk
  data: U0VMRUNUIDE7Cg==
```

A completion record contains the verified intent and chunk identities. This
example illustrates its envelope; its referenced intent and chunk must exist
and pass digest checks before a consumer accepts the result.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahResultRecord
metadata:
  name: example-result-complete
  namespace: default
spec:
  type: complete
  data: eyJtYW5pZmVzdFVJRCI6IjZmNGIyYTE4LThjM2UtNGQ1YS1iMWY3LTJlMGM5ZDhhN2I2NCIsIm1hbmlmZXN0RGlnZXN0Ijoic2hhMjU2OmFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWEiLCJjaHVua1VJRHMiOlsiN2M5ZTY2NzktNzQyNS00MGRlLTk0NGItZTA3ZmMxZjkwYWU3Il19
```
