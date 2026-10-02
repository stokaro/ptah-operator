---
title: PtahResultRecord
description: Every field of the PtahResultRecord resource, generated from the API types.
---

`PtahResultRecord` is a namespaced resource in `operator.ptah.run`, served as `v1alpha1`.

This page is generated from the API types by `make docs-reference`. The shipped CRDs carry no descriptions, so this is where the field documentation lives.

The result transport writes these immutable records. Grant read access only to
identities allowed to inspect operation SQL and delivery credentials; ordinary
Secret read permission does not grant access to them. Include this resource in
the cluster's encryption-at-rest and restricted backup configuration.

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

## spec

| Field | Type | What it does |
| --- | --- | --- |
| `spec.data` | `string`, required | Data is the record's exact bytes, base64-encoded on the wire. Readers must validate its protocol and digest before using it. Treat every role as confidential: payloads may contain SQL and credentials contain private keys. MaxLength is the base64 length of the 512 KiB record ceiling. |
| `spec.type` | `string`, required, one of `credential`, `intent`, `chunk`, `complete` | Type identifies the record's role in credential or result publication. Credential records contain delivery key material; intent, chunk, and completion records contain authenticated operation evidence. |

## status

This resource declares no `status`.

