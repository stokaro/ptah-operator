<p align="center"><img src="docs/site/src/assets/logo.svg" alt="The Ptah mark: an amber capstone above two sky-blue courses on a dark rounded square" width="72" height="72"></p>

<h1 align="center">Ptah Operator</h1>

<p align="center">A Kubernetes control plane that converges PostgreSQL and MySQL schemas from immutable OCI artifacts.</p>

<p align="center"><strong>English</strong> · <a href="README.ja.md">日本語</a> · <a href="README.de.md">Deutsch</a> · <a href="README.fr.md">Français</a></p>

<p align="center">
  <a href="https://github.com/stokaro/ptah-operator/actions/workflows/ci.yml?query=branch%3Amaster"><img src="https://img.shields.io/github/actions/workflow/status/stokaro/ptah-operator/ci.yml?branch=master&label=ci&logo=github" alt="Status of the CI workflow on the master branch, which verifies the source, runs the race detector and drives the full Kubernetes lifecycle"></a>
  <a href="https://github.com/stokaro/ptah-operator/actions/workflows/demo.yml?query=branch%3Amaster"><img src="https://img.shields.io/github/actions/workflow/status/stokaro/ptah-operator/demo.yml?branch=master&label=demonstration&logo=github" alt="Status of the weekly demonstration workflow on the master branch, which re-records every scenario against a real cluster"></a>
  <a href="https://github.com/stokaro/ptah-operator/actions/workflows/docs.yml?query=branch%3Amaster"><img src="https://img.shields.io/github/actions/workflow/status/stokaro/ptah-operator/docs.yml?branch=master&label=docs&logo=github" alt="Status of the documentation workflow on the master branch"></a>
  <a href="https://github.com/stokaro/ptah-operator/blob/master/LICENSE"><img src="https://img.shields.io/github/license/stokaro/ptah-operator?label=license&color=blue" alt="The license badge, reading MIT"></a>
  <a href="https://github.com/stokaro/ptah-operator/blob/master/go.mod"><img src="https://img.shields.io/github/go-mod/go-version/stokaro/ptah-operator?label=go%20%E2%89%A5&logo=go&logoColor=white" alt="The lowest Go version this module compiles against, declared in go.mod"></a>
</p>

<p align="center"><a href="https://operator.ptah.run/edge/start/install/">Install</a> · <a href="https://operator.ptah.run/edge/start/first-schema/">First schema</a> · <a href="https://operator.ptah.run/demo/">Recorded runs</a> · <a href="https://operator.ptah.run/">Documentation</a> · <a href="https://operator.ptah.run/support/ptah/">Ptah compatibility</a></p>

Ptah Operator is a Kubernetes-native control plane for continuously converging
PostgreSQL and MySQL schemas from immutable OCI artifacts. Database work runs
in short-lived, hardened Jobs; the controller itself has no permission to read
database Secrets.

`PtahSchema` converges the database structure and the reference rows a
declaration names. `PtahMigration` executes a prepared migration sequence and
holds the recorded history to the artifact it selected, including a checkpoint
bootstrap. Both run on PostgreSQL and MySQL.

The API is currently `v1alpha1`. The end-to-end matrix is green -- every
supported Kubernetes minor, both engines, both artifact formats -- so what is
still missing before this stops being an implementation preview is a published
release. What a run measured, and against which Ptah build, is recorded in
[`support/ptah.json`](support/ptah.json).

## Reconciliation model

```text
resolve tag to digest -> verify artifact -> observe database -> publish plan
          ^                                                          |
          |                 approval (when required) <---------------+
          |                                                          |
          +--- verify convergence <- apply exact approved plan <-----+
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

## Reading what it applied

The SQL lives in immutable ConfigMaps a plan binds by index, size and digest,
never in a status field or the manager's log. `kubectl ptah` reads it back the
way the operator does:

```sh
kubectl ptah plan storefront --applied -n application -o sql
```

It is a read-only client, published with each release as a `kubectl` plugin for
the supported client platforms. [Read a
plan](https://operator.ptah.run/edge/use/read-a-plan/#install) carries the
installation and the namespace-scoped RBAC it needs.

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
`docs/site/src/content/docs`. One of them is written for somebody changing this
code rather than for somebody running it:

- [Architecture](https://operator.ptah.run/edge/reference/architecture/) -- the
  pieces, where they live, and which invariant each one holds.

Ptah itself -- the schema formats, the OCI artifact layout, the CLI -- is
documented at [docs.ptah.run](https://docs.ptah.run/edge/). Which Ptah builds
have been verified with this operator is published in the user guide, as the
[compatibility matrix](https://operator.ptah.run/support/ptah/).

`PtahMigration` is deliberately not folded into `PtahSchema`. A declared row
set describes desired rows and a migration sequence describes a transition
between states, so the two keep their own APIs and their own state machines
while sharing the OCI transport, the credential isolation, the execution
protocol and the target coordination.

## License and help

Ptah Operator is published under the [MIT license](LICENSE).

For questions and bug reports, open an issue in
[stokaro/ptah-operator](https://github.com/stokaro/ptah-operator/issues).
Anything the Ptah CLI does on its own belongs in
[stokaro/ptah](https://github.com/stokaro/ptah/issues) instead.
[CONTRIBUTING.md](CONTRIBUTING.md) covers what makes a report actionable and
what a change has to pass, and participation is covered by the
[Code of Conduct](CODE_OF_CONDUCT.md). Commercial enquiries go to
`ask@stokaro.com`.
