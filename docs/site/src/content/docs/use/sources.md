---
title: Deliver from a ConfigMap, Git or HTTPS
description: Keep a schema or a migration sequence in a ConfigMap, a Git repository or a download pinned by digest, render it with Kustomize or Helm, reach a private server, and bound what a source may name.
---

A `PtahSchema` reads its desired schema, and a `PtahMigration` its migration
directory, from one of four sources: an OCI artifact, a ConfigMap in the
resource's namespace, a Git repository, or an HTTPS download pinned by its
digest. This page covers the last three. None needs a registry or a
verification policy, and each works for both resources.

Choose by what the files are trusted by. A ConfigMap is trusted by Kubernetes
RBAC: whoever may write it may change the desired state, exactly as whoever
may write the resource can. It fits a team whose manifests already carry the
files, through Kustomize, Helm or a GitOps tool. A Git source is trusted by the
repository: whoever may push to the branch or move the tag decides what the
operator plans next. It fits a team that wants the operator to follow a
branch or a tag itself. An HTTPS source is trusted by the digest in the spec:
whoever may write the resource decides which bytes are read, and the server
can only serve them or fail. It fits a team that already publishes release
archives to an artifact server and wants no registry.

Whatever the source, the operator plans first and applies only what was
approved or what `spec.policy` lets it apply, and every operation reads the
exact files the plan was computed from.
[Reconciliation](../../reference/reconciliation/#a-configmap-source) says how
each source is resolved and what each operation checks.

Moving a resource from one kind of source to another is an edit of its spec,
in its manifest or with `kubectl edit`. `kubectl ptah set-source` changes the
reference of an OCI source and refuses a ConfigMap, Git or HTTPS one: an
artifact also takes a verification policy, which those sources refuse, and
only the person making the edit can name it.
[Moving to a new artifact](../operations/#set-source) has the OCI case.

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

A schema selects exactly one file, and its extension tells Ptah the format.
Only that file reaches the operation, so a `data` block in it that names a row
file is refused; a schema with row files is published with `ptah schema push`,
whose artifact carries the rows ([Manage reference data](../../use/reference-data/)). A
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
by a new one. That includes a read that keeps failing on the file you are
repairing: it gives way to the edit. An Apply and the read that proves it keep
the snapshot they started from. An edit to any other key changes nothing. A
ConfigMap deleted and created again under the same name is a new source too: a
plan binds the ConfigMap's UID with the content.

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
with a reason, rather than read as SQL. A schema reads only the file `path`
selects, so a `data` block in it cannot name a row file, as for a ConfigMap.

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

## From HTTPS

`https` names a download and the SHA-256 of the exact bytes it serves:

```yaml
spec:
  desired:
    https:
      url: https://artifacts.example.com/inventory/schema-1.4.0.hcl
      digest: sha256:<SHA-256 of the bytes the URL serves>
      format: File
      path: schema.hcl
```

The digest is the whole trust decision. A Resolve Job downloads the URL and
refuses bytes that do not give it, then records the content digest of the
selected files; every later operation downloads the URL again, held to both
digests, and nothing is stored in the cluster. A new version of the files is a
new digest in the spec, so a source that moves is an edit of the resource,
never something the server decides.

`url` takes `https://` only, with no user name, password, query or fragment,
in at most 2,048 bytes. `format` says what the download is:

- `File`: one file, which `path` names. Its extension tells a schema's format,
  as a ConfigMap item's does. A migration directory cannot be one file.
- `TarGzip`: a gzip-compressed tar archive. `path` selects the schema file, or
  the migration directory, inside it.
- `Zip`: a zip archive, selected the same way.

An archive entry outside the archive, such as one named `../x`, or a symbolic
link inside the selection is refused at Resolve with Ptah's reason, rather
than read as SQL. A migration sequence in an archive:

```yaml
spec:
  artifact:
    https:
      url: https://artifacts.example.com/orders/orders-1.4.0.tar.gz
      digest: sha256:<SHA-256 of the archive>
      format: TarGzip
      path: orders-1.4.0/db/migrations
```

A plan is bound to the URL, the path and the content of the selected files,
not to the download digest. An archive built again holds new bytes even when
its files are the same, since tar and zip record times; once the spec names
its digest, Resolve records the same content digest and the plan stands.
Another URL or another path retires the plan.

### Publish from CI

Publish the file or the archive under a name that carries its version, take
the digest of the bytes you uploaded, and write that digest into the manifest
in the same change:

```sh
tar -czf orders-1.4.0.tar.gz orders-1.4.0/db/migrations
digest="sha256:$(shasum -a 256 orders-1.4.0.tar.gz | cut -d' ' -f1)"
curl --fail --upload-file orders-1.4.0.tar.gz \
  https://artifacts.example.com/orders/orders-1.4.0.tar.gz
yq -i ".spec.artifact.https.digest = \"$digest\"" deploy/orders-migrations.yaml
```

Take the digest of the file you uploaded, not of a copy built again: a rebuilt
archive has another digest. Keep every published version on the server for as
long as a resource names it.

### When the download changes or disappears

The server keeps no promise the operator can hold it to. When the bytes at a
URL are replaced, the next Resolve refuses them, naming the digest it was
served and the one the spec pins, and when they are deleted the download
fails. Either way nothing is planned or applied, and what was resolved and
applied before stays in the status. Recover in one of two ways:

- Put the original bytes back at the URL. The digest in the spec holds again
  and the next attempt reads them.
- Pin the new bytes: write their digest into the spec. When the selected files
  are the same the plan stands, and when they differ the operator plans
  again.

### The cost of the interval

A resource reads its source again at every interval, and an HTTPS source has
no cache: each Resolve downloads the whole file or archive, and so does every
operation that reads the files, including the read that follows each Resolve.
A resource that is in sync downloads the source at least twice per interval.
With a large archive and a short `spec.interval`, that is real bandwidth, on
both the cluster's egress and the server, for a source whose pinned digest
means nothing new can arrive. Keep archives small, select only what the
resource reads, and choose an interval measured in hours rather than minutes.

### A private download

`authFrom` names a Secret in the resource's namespace with fixed keys:
`username` and `password` for Basic authentication, or `token` for a Bearer
token, never both, and `host`, the only `host[:port]` the credential is sent
to. The fetch refuses a URL whose host is not that one, and Ptah refuses a
redirect to another origin while a credential is set, so a resource's author
cannot point the credential at another server.

```sh
kubectl -n application create secret generic orders-artifacts \
  --from-literal=host=artifacts.example.com \
  --from-literal=token="$ARTIFACTS_TOKEN"
```

For a private CA, `transport.caFrom` names the ConfigMap key that holds the
bundle, and a credentialed source's Secret holds `caSHA256`, the SHA-256 of
those exact bytes, as for Git:

```sh
kubectl -n application create configmap artifacts-ca --from-file=ca.pem
kubectl -n application create secret generic orders-artifacts \
  --from-literal=host=artifacts.example.com \
  --from-literal=username=deploy \
  --from-literal=password="$ARTIFACTS_PASSWORD" \
  --from-literal=caSHA256="sha256:$(shasum -a 256 ca.pem | cut -d' ' -f1)"
```

```yaml
spec:
  artifact:
    https:
      url: https://artifacts.example.com/orders/orders-1.4.0.tar.gz
      digest: sha256:<SHA-256 of the archive>
      format: TarGzip
      path: orders-1.4.0/db/migrations
      authFrom:
        name: orders-artifacts
      transport:
        caFrom:
          name: artifacts-ca
          key: ca.pem
```

The Job mounts the Secret into the container that downloads and into no
other; the manager never reads it.

## What resolved

`ArtifactResolved` says `SnapshotTaken` for a ConfigMap source,
`CommitResolved` for a Git one and `DownloadVerified` for an HTTPS one, whose
message names the download digest and the content digest. `kubectl ptah schema`
and `kubectl ptah migration` print the source, the commit, snapshot or
download, and the content digest; the status records the same:

```sh
kubectl -n application get ptahschema inventory \
  -o jsonpath='{.status.source.snapshot.name}{"\n"}'
kubectl -n application get ptahmigration orders \
  -o jsonpath='{.status.artifact.git.commit} {.status.artifact.git.contentDigest}{"\n"}'
```

A plan computed from any of these sources is bound to that content and to
the source's identity, the ConfigMap's UID, or the URL and path of a
repository or a download, in place of an artifact digest. [Plans and approvals](../../reference/plans-and-approvals/)
says what an approval binds.
