<p align="center"><img src="docs/site/src/assets/logo.svg" alt="The Ptah mark: an amber capstone above two sky-blue courses on a dark rounded square" width="72" height="72"></p>

<h1 align="center">Ptah Operator</h1>

<p align="center">A Kubernetes control plane that converges PostgreSQL and MySQL schemas from immutable OCI artifacts.</p>

<p align="center">
  <a href="https://github.com/stokaro/ptah-operator/actions/workflows/ci.yml?query=branch%3Amaster"><img src="https://img.shields.io/github/actions/workflow/status/stokaro/ptah-operator/ci.yml?branch=master&label=ci&logo=github" alt="Status of the CI workflow on the master branch, which verifies the source, runs the race detector and drives the full Kubernetes lifecycle"></a>
  <a href="https://github.com/stokaro/ptah-operator/actions/workflows/demo.yml?query=branch%3Amaster"><img src="https://img.shields.io/github/actions/workflow/status/stokaro/ptah-operator/demo.yml?branch=master&label=demonstration&logo=github" alt="Status of the weekly demonstration workflow on the master branch, which re-records every scenario against a real cluster"></a>
  <a href="https://github.com/stokaro/ptah-operator/actions/workflows/docs.yml?query=branch%3Amaster"><img src="https://img.shields.io/github/actions/workflow/status/stokaro/ptah-operator/docs.yml?branch=master&label=docs&logo=github" alt="Status of the documentation workflow on the master branch"></a>
  <a href="https://github.com/stokaro/ptah-operator/blob/master/go.mod"><img src="https://img.shields.io/github/go-mod/go-version/stokaro/ptah-operator?label=go&logo=go&logoColor=white" alt="The Go version declared in go.mod"></a>
</p>

<p align="center"><a href="https://operator.ptah.run/edge/start/install/">Install</a> · <a href="https://operator.ptah.run/edge/start/first-schema/">First schema</a> · <a href="https://operator.ptah.run/demo/">Recorded runs</a> · <a href="https://operator.ptah.run/">Documentation</a> · <a href="https://docs.ptah.run/compatibility/operator/">Ptah compatibility</a></p>

Ptah Operator is a Kubernetes-native control plane for continuously converging
PostgreSQL and MySQL schemas from immutable OCI artifacts. Database work runs
in short-lived, hardened Jobs; the controller itself has no permission to read
database Secrets.

The API is currently `v1alpha1`. Treat it as an implementation preview until
the complete database end-to-end matrix is green and a release is published.

## Reconciliation model

```text
resolve tag to digest -> verify artifact -> observe database -> publish plan
       ^                                                        |
       |                         approval (when required) <------+
       |                                                        |
       +--- verify convergence <- apply exact approved plan <---+
```

The controller records a claim before creating each Job, permits at most one
active Job per `PtahSchema`, and resumes that claim after a restart. A process
exit is never treated as proof of convergence: every apply is followed by a
new read-only observation.

Key safety properties:

- OCI tags are resolved once and all later artifact access uses the digest.
- Plans bind exact bytes to artifact, target, observed state, policy, the
  digest-pinned manager image, manager revision and state semantics, Ptah
  version, executor image, runner image, and runner protocol.
- Approvals are separate immutable resources stamped with authenticated
  admission identity.
- Destructive plans are disabled by default and still require an exact-plan
  approval when enabled.
- Database and registry credentials are isolated from each other and are never
  placed in status, Events, plan resources, or command arguments.
- Deletion and suspension never execute cleanup SQL.
- An uncertain apply outcome always returns to observation instead of replay.

## Install and first schema

The guide lives on its own site: [operator.ptah.run](https://operator.ptah.run/).
It carries installation, the three values the chart refuses to guess, a first
worked schema, configuration, operations, the security model, the condition
reasons and the support windows.

```sh
cd docs/site && npm ci && npm run build
```

builds it from this checkout.

## See it run

[Recorded runs](https://operator.ptah.run/demo/) are terminal sessions captured
while the operator ran against a real cluster: applying a schema, changing it,
approving a plan, refusing a destructive change, closing drift, failing and
recovering. Each one is checked while it is recorded -- the conditions the
session claims are read off the live objects -- and published only if every
check held.

The `demonstration` badge above is the weekly re-recording on `master`, not the
sessions the site plays. Green means every scenario still holds against a cluster built from
`master`; what a reader watches is the recording committed in
`demo/recordings/runs.json`, which changes only when somebody records it. A red
badge is therefore a demonstration that has stopped being true, which is worth
knowing before somebody reads it as current.

[`demo/`](demo/README.md) is how it is built and how to run it yourself.

## Documentation

The user guide is published at
[operator.ptah.run](https://operator.ptah.run/) and its pages live in
`docs/site/src/content/docs`. One document is deliberately not part of it:

- [Architecture and state machine](docs/architecture.md), which is written for
  somebody changing this code rather than for somebody running it.

Ptah itself -- the schema formats, the OCI artifact layout, the CLI -- is
documented at [docs.ptah.run](https://docs.ptah.run/edge/), and which Ptah
builds have been verified with this operator is published as the
[compatibility matrix](https://docs.ptah.run/compatibility/operator/).

`PtahMigration` is deliberately not folded into `PtahSchema`. A future
versioned-migration controller can reuse the OCI transport, credential
isolation, execution protocol, and target coordination primitives while
retaining its own API and state machine.
