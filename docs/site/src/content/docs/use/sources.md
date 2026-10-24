---
title: Deliver from a ConfigMap or Git
description: Keep a schema or a migration sequence in a ConfigMap or a Git repository, render it with Kustomize or Helm, reach a private repository, and bound what a source may name.
---

A `PtahSchema` reads its desired schema, and a `PtahMigration` its migration
directory, from one of three sources: an OCI artifact, a ConfigMap in the
resource's namespace, or a Git repository. This page covers the last two.
Neither needs a registry or a verification policy, and both work for both
resources.

Choose by what the files are trusted by. A ConfigMap is trusted by Kubernetes
RBAC: whoever may write it may change the desired state, exactly as whoever
may write the resource can. It fits a team whose manifests already carry the
files, through Kustomize, Helm or a GitOps tool. A Git source is trusted by the
repository: whoever may push to the branch or move the tag decides what the
operator plans next. It fits a team that wants the operator to follow a
branch or a tag itself.

Whatever the source, the operator plans first and applies only what was
approved or what `spec.policy` lets it apply, and every operation reads the
exact files the plan was computed from.
[Reconciliation](../../reference/reconciliation/#a-configmap-source) says how
each source is resolved and what each operation checks.

## From a ConfigMap

`configMap` names a ConfigMap and the keys that become files:

```yaml
spec:
  desired:
    configMap:
      name: inventory-schema
      items:
        - key: schema.sql
          path: schema.sql
```

A schema selects exactly one file, and its extension tells Ptah the format. A
migration directory selects up to 1,000 files, named as Ptah names migrations,
such as `0000000001_create_orders.up.sql`. A key cannot hold `/`, so a nested
path is spelled out in `path`. A key the list does not name is not part of the
source.

The manager copies the selected files into an immutable snapshot ConfigMap
that the resource owns, and every operation reads the snapshot. The selected
files and their manifest have to fit in one ConfigMap: 1 MiB together. An
operation that is already running reads its snapshot, so replacing or deleting
the source ConfigMap does not disturb it.

An edit to a selected key is read at once, and the plan it retires is replaced
by a new one. An edit to any other key changes nothing. A ConfigMap deleted and
created again under the same name is a new source too: a plan binds the
ConfigMap's UID with the content.

### With Kustomize

`examples/configmap-kustomize` delivers a schema and a migration sequence from
ConfigMaps that Kustomize generates from files beside it:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: application
resources:
  - ptahschema.yaml
  - ptahmigration.yaml
configurations:
  - ptah-references.yaml
generatorOptions:
  immutable: true
configMapGenerator:
  - name: inventory-schema
    files:
      - schema.sql=db/schema.sql
  - name: orders-migrations
    files:
      - db/migrations/0000000001_create_orders.up.sql
      - db/migrations/0000000001_create_orders.down.sql
      - db/migrations/0000000002_add_order_total.up.sql
      - db/migrations/0000000002_add_order_total.down.sql
```

A generator appends a hash of the content to the ConfigMap's name, so every
change of content is a new ConfigMap, and `immutable: true` costs nothing.
Kustomize rewrites a reference to the new name only in the fields it knows
name a ConfigMap, and it does not know the Ptah resources. The `configurations`
entry tells it:

```yaml
nameReference:
  - kind: ConfigMap
    version: v1
    fieldSpecs:
      - group: operator.ptah.run
        kind: PtahSchema
        path: spec/desired/configMap/name
      - group: operator.ptah.run
        kind: PtahMigration
        path: spec/artifact/configMap/name
```

Without it the build still succeeds, and the resources name
`inventory-schema` while the cluster holds `inventory-schema-<hash>`: the
source never resolves. Add a field spec of the same shape for any other
ConfigMap you generate and a resource names, such as `sourcePolicyFrom`.

Render the example to see the names it produces:

```sh
kubectl kustomize examples/configmap-kustomize
```

The generator keys a file by its name, so in the `PtahMigration` each item's
key and path are the file name. A new migration is a line in the generator and
an item in the resource. ConfigMaps from earlier builds stay in the cluster
until something prunes them; the operator does not need them once the new
name has resolved.

### With Helm

`examples/configmap-helm` is a chart that renders the same two resources.
Files under `files/` become ConfigMap keys through `.Files.Glob`, which keeps
their bytes exactly, and the ConfigMap is named after a hash of its data:

```yaml
{{- $files := .Files.Glob "files/migrations/*.sql" -}}
{{- $data := $files.AsConfig -}}
{{- $name := printf "%s-migrations-%s" .Release.Name ($data | sha256sum | trunc 10) -}}
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ $name }}
immutable: true
data:
{{ $data | indent 2 }}
---
apiVersion: operator.ptah.run/v1alpha1
kind: PtahMigration
metadata:
  name: {{ .Release.Name }}-orders
spec:
  target:
    engine: PostgreSQL
    coordinationKey: {{ .Values.migrations.coordinationKey }}
    urlFrom:
      name: {{ .Values.migrations.databaseSecret }}
      key: url
  artifact:
    configMap:
      name: {{ $name }}
      items:
      {{- range $path, $_ := $files }}
        - key: {{ base $path }}
          path: {{ base $path }}
      {{- end }}
```

Here a new migration is only a new file. `helm upgrade` creates the new
ConfigMap, points the resource at it and deletes the previous one. A chart can
also keep one mutable ConfigMap under a fixed name: an upgrade then edits it,
and the operator reads the edit at once.

```sh
helm template inventory examples/configmap-helm --namespace application
```

### A source policy and generated names

A [source policy](../security/#source-policies) can restrict which ConfigMaps
a source may name with `allowedConfigMaps`. It compares exact names, so a
generated name with a content hash never matches one. With generated names,
bound the source with `maxContentBytes` and leave `allowedConfigMaps` out, or
give the ConfigMap a fixed name: `generatorOptions.disableNameSuffixHash` in
Kustomize, a name without the hash in Helm.

## From Git

`git` names a repository, a ref and a path:

```yaml
spec:
  artifact:
    git:
      url: https://git.example.com/acme/orders.git
      ref:
        branch: main
      path: db/migrations
```

A Resolve Job fetches the commit the ref names and records it with the content
digest of the selected files, and every later operation fetches that commit
again and refuses other content. The manager itself never reads the repository
or its credential.

`ref` names exactly one of:

- `branch`, read at its tip, again at every interval. A commit that leaves the
  selected files as they were keeps the plan.
- `tag`, read at the commit it names, again at every interval.
- `commit`, by its full 40-character id. A server that does not say it gives
  any commit by id gives one only while a branch or a tag points at it, so a
  pinned commit can stop resolving once its branch moves on; to hold a
  resource at one commit, prefer a tag. [Reconciliation](../../reference/reconciliation/#a-git-source) says
  which servers allow what.

`path` is the schema file, or the migration directory, which may be `.`, the
root of the repository. A submodule or a Git LFS pointer at the selected path is refused
with a reason, rather than read as SQL.

A public repository needs nothing else. A private one needs a credential, and
the credential's owner decides where it may be sent.

### A private repository over HTTPS

`authFrom` names a Secret in the resource's namespace. Its keys are fixed:
`password` holds the token, `username` is optional, and `host` is the only
`host[:port]` the fetch sends the credential to. The fetch refuses a URL whose
host is not that one, so a resource's author cannot point the credential at
another server.

```sh
kubectl -n application create secret generic orders-git \
  --from-literal=host=git.example.com \
  --from-literal=password="$GIT_TOKEN"
```

A server whose certificate chains to a private CA names the CA bundle in
`transport.caFrom`, a ConfigMap key. With a credential, the Secret must also
hold `caSHA256`, the SHA-256 of those exact bytes, so the credential is sent
only under the trust its owner granted:

```sh
kubectl -n application create configmap git-example-ca --from-file=ca.pem
kubectl -n application create secret generic orders-git \
  --from-literal=host=git.example.com \
  --from-literal=password="$GIT_TOKEN" \
  --from-literal=caSHA256="sha256:$(shasum -a 256 ca.pem | cut -d' ' -f1)"
```

```yaml
spec:
  artifact:
    git:
      url: https://git.example.com/acme/orders.git
      ref:
        branch: main
      path: db/migrations
      authFrom:
        name: orders-git
      transport:
        caFrom:
          name: git-example-ca
          key: ca.pem
```

### A private repository over SSH

An `ssh://` URL needs `transport.knownHostsFrom`, the host keys the server may
present; a fetch never trusts a key on first use. Take the keys from the
fingerprints your Git provider publishes rather than from whatever the
network answers. The Secret holds `sshPrivateKey`, `host`, and
`knownHostsSHA256`, the SHA-256 of the known-hosts bytes:

```sh
kubectl -n application create configmap git-example-known-hosts --from-file=known_hosts
kubectl -n application create secret generic orders-deploy-key \
  --from-file=sshPrivateKey=./deploy_key \
  --from-literal=host=git.example.com \
  --from-literal=knownHostsSHA256="sha256:$(shasum -a 256 known_hosts | cut -d' ' -f1)"
```

```yaml
spec:
  artifact:
    git:
      url: ssh://git@git.example.com/acme/orders.git
      ref:
        tag: v1.4.0
      path: db/migrations
      authFrom:
        name: orders-deploy-key
      transport:
        knownHostsFrom:
          name: git-example-known-hosts
          key: known_hosts
```

A deploy key with read access to the one repository is the narrowest
credential that works.

## What resolved

`ArtifactResolved` says `SnapshotTaken` for a ConfigMap source and
`CommitResolved` for a Git one. `kubectl ptah schema` and
`kubectl ptah migration` print the source, the commit or snapshot, and the
content digest; the status records the same:

```sh
kubectl -n application get ptahschema inventory \
  -o jsonpath='{.status.source.snapshot.name}{"\n"}'
kubectl -n application get ptahmigration orders \
  -o jsonpath='{.status.artifact.git.commit} {.status.artifact.git.contentDigest}{"\n"}'
```

A plan computed from either source is bound to that content and to the
source's identity, the ConfigMap's UID or the repository URL and path, in
place of an artifact digest. [Plans and approvals](../../reference/plans-and-approvals/)
says what an approval binds.
