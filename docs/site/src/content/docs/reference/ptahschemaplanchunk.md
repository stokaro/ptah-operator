---
title: PtahSchemaPlanChunk
description: Every field of the PtahSchemaPlanChunk resource, generated from the API types.
---

`PtahSchemaPlanChunk` is a namespaced resource in `operator.ptah.run`, served as `v1alpha1`.

This page is generated from the API types by `make docs-reference`. The shipped CRDs carry no descriptions, so this is where the field documentation lives.

## Examples

Nobody writes a `PtahSchemaPlanChunk`. The operator writes one per chunk of a
[`PtahSchemaPlan`](../ptahschemaplan/) when it publishes the plan, reads each
one back to verify it, and only then marks the plan `Ready`. The chunk is
immutable afterwards, owned by its plan, and deleted with it.

A plan's bytes are the concatenation of its chunks' `spec.data`, in the order
the plan's `spec.chunks` gives, and each chunk has to match the index, size and
digest recorded there. A chunk boundary falls wherever 512 KiB does, which can
be the middle of a statement or of a character, so read a plan with the plugin
rather than by decoding chunks:

```sh
kubectl ptah plan application -n application
```

Reading a plan takes one RBAC rule, `get` on `ptahschemaplanchunks` in the
namespace. The chart's approver ClusterRole carries it, and so does
`examples/approver-plan-reader-role.yaml` for a reviewer who is granted the
plans of one namespace. Neither reaches a ConfigMap.

The chunks are not what an Apply Pod reads. The Pod holds no Kubernetes
credential, and the kubelet mounts no custom resource, so just before it
creates the Apply Job the operator writes the plan into immutable ConfigMaps
of the same names, owned by the plan, and the Pod mounts those. The runner
checks the whole document against the plan's `spec.contentDigest` before it
runs anything.

### The one chunk of a small plan

The destructive plan on the `PtahSchemaPlan` page is 396 bytes, so it fits in
one chunk, `-000`. `spec.data` is those bytes, base64-encoded as the API
carries them.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahSchemaPlanChunk
metadata:
  name: ptah-plan-9c56cc51b374c3ba189210d5-000
  namespace: application
  labels:
    operator.ptah.run/plan: ptah-plan-9c56cc51b374c3ba189210d5
    operator.ptah.run/schema: application
  # The chunk belongs to its plan and is collected with it.
  ownerReferences:
    - apiVersion: operator.ptah.run/v1alpha1
      kind: PtahSchemaPlan
      name: ptah-plan-9c56cc51b374c3ba189210d5
      uid: 3b8e2f6a-1c4d-4e5f-9a7b-8c6d5e4f3a2b
      controller: true
      blockOwnerDeletion: true
spec:
  data: eyJmb3JtYXRfdmVyc2lvbiI6MSwibmFtZSI6ImFwcGxpY2F0aW9uIiwiZGlhbGVjdCI6InBvc3RncmVzIiwiZnJvbV9maW5nZXJwcmludCI6InNoYTI1Njo0YTQ0ZGMxNTM2NDIwNGE4MGZlODBlOTAzOTQ1NWNjMTYwODI4MTgyMGZlMmIyNGYxZTUyMzNhZGU2YWYxZGQ1IiwidG9fZmluZ2VycHJpbnQiOiJzaGEyNTY6NGUwNzQwODU2MmJlZGI4YjYwY2UwNWMxZGVjZmUzYWQxNmI3MjIzMDk2N2RlMDFmNjQwYjdlNDcyOWI0OWZjZSIsImRlc3RydWN0aXZlIjp0cnVlLCJzdGF0ZW1lbnRzIjpbeyJzcWwiOiJEUk9QIFRBQkxFIGxlZ2FjeV9zZXNzaW9uczsiLCJzZXZlcml0eSI6ImRlc3RydWN0aXZlIiwicmVhc29uIjoiZHJvcHMgdGFibGUgbGVnYWN5X3Nlc3Npb25zIGFuZCBpdHMgcm93cyJ9XX0K
```

### The chunk of a plan that changes privileges

Same shape for the privilege-changing plan on the `PtahSchemaPlan` page. The
plan records the chunk's size, 653 bytes, and its digest; a chunk whose data
does not hash to that digest is refused by every reader, the operator's own
included, before any of it is shown or run.

```yaml
apiVersion: operator.ptah.run/v1alpha1
kind: PtahSchemaPlanChunk
metadata:
  name: ptah-plan-3263a9026c3e3f7e368860bd-000
  namespace: application
  labels:
    operator.ptah.run/plan: ptah-plan-3263a9026c3e3f7e368860bd
    operator.ptah.run/schema: application
  ownerReferences:
    - apiVersion: operator.ptah.run/v1alpha1
      kind: PtahSchemaPlan
      name: ptah-plan-3263a9026c3e3f7e368860bd
      uid: 6e1d9c4b-7a2f-4b8e-a3c5-0f9e8d7c6b5a
      controller: true
      blockOwnerDeletion: true
spec:
  data: eyJmb3JtYXRfdmVyc2lvbiI6MSwibmFtZSI6ImFwcGxpY2F0aW9uIiwiZGlhbGVjdCI6InBvc3RncmVzIiwiZnJvbV9maW5nZXJwcmludCI6InNoYTI1Njo0YTQ0ZGMxNTM2NDIwNGE4MGZlODBlOTAzOTQ1NWNjMTYwODI4MTgyMGZlMmIyNGYxZTUyMzNhZGU2YWYxZGQ1IiwidG9fZmluZ2VycHJpbnQiOiJzaGEyNTY6ZTlkZDhjZjYxYjJiZTBlMjc2YzllMTZlMGExNDdkYzAyZGY4YzRiMDY4MzU3MjFjYTQzYWRjOTljYmZhMDZhMyIsImRlc3RydWN0aXZlIjpmYWxzZSwic3RhdGVtZW50cyI6W3sic3FsIjoiR1JBTlQgU0VMRUNUIE9OIG9yZGVycyBUTyByZXBvcnRpbmc7Iiwic2V2ZXJpdHkiOiJzYWZlIiwicmVhc29uIjoiZ3JhbnRzIFNFTEVDVCBvbiBvcmRlcnMgdG8gcmVwb3J0aW5nIn0seyJzcWwiOiJDUkVBVEUgT1IgUkVQTEFDRSBGVU5DVElPTiBvcmRlcl90b3RhbChwX29yZGVyX2lkIGJpZ2ludCkgUkVUVVJOUyBudW1lcmljIExBTkdVQUdFIHNxbCBTRUNVUklUWSBERUZJTkVSIEFTICQkIFNFTEVDVCBzdW0oYW1vdW50KSBGUk9NIG9yZGVyX2xpbmVzIFdIRVJFIG9yZGVyX2lkID0gcF9vcmRlcl9pZCAkJDsiLCJzZXZlcml0eSI6InNhZmUiLCJyZWFzb24iOiJjcmVhdGVzIG9yIHJlcGxhY2VzIGZ1bmN0aW9uIG9yZGVyX3RvdGFsIn1dfQo=
```

A plan larger than 512 KiB has more than one chunk, `-000` up to at most
`-015`, and every chunk but the last is exactly 512 KiB. The data of each is
at most 699,052 base64 characters, which is what the schema bounds.

## spec

| Field | Type | What it does |
| --- | --- | --- |
| `spec.data` | `string`, required | Data is this chunk's bytes, base64-encoded on the wire. A plan is the concatenation of its chunks' data in the order its spec.chunks gives, and a chunk boundary falls wherever the byte count does, which may be inside a statement or a character. Read a whole plan with kubectl ptah plan rather than by decoding chunks. MaxLength is the base64 length of the 512 KiB chunk ceiling. |

## status

This resource declares no `status`.

