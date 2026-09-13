# Contributing to Ptah Operator

Thanks for taking the time. This page covers filing an issue, proposing a
change, and the checks a change has to pass.

[AGENTS.md](AGENTS.md) is the authority on how work is done in this repository.
It is written for coding agents, and it is equally the rulebook for people: what
a change to the API owes, what a change to a controller owes, and which gate
measures what. This page does not restate it, because a second copy drifts the
moment the first one moves.

## This is not the Ptah CLI

The operator is a separate project from [Ptah](https://github.com/stokaro/ptah),
with its own API, releases, and support matrix. It does not contain Ptah; it
runs one, as a Job whose image and version identity the installation binds.

A report about schema rendering, migration files, dialect support, or anything
the CLI does on its own belongs in the Ptah repository. A report about
reconciliation, conditions, approvals, Jobs, RBAC, or the chart belongs here. An
issue filed in the wrong place has to be moved before anyone can act on it.

## Before you open an issue

Read the condition first. Conditions are the operator's stable machine
interface, and
[Condition reasons](docs/site/src/content/docs/troubleshoot/condition-reasons.md)
says what each reason means and what to do about it. Many surprising states are
deliberate refusals that name themselves.

### A bug report that can be acted on

- the operator version, and the manager, executor, and runner image digests;
- the Kubernetes version and distribution;
- the database engine and server version;
- the full condition tuple, `type`, `status`, and `reason`, plus
  `status.observedGeneration`;
- the `PtahSchema` with credentials removed, and the plan reference it names;
- what you expected, and what happened.

Redact database URLs, registry credentials, and anything else the Secrets hold.
The operator keeps credentials out of status, Events, plan resources, and
command arguments; please keep them out of the issue too.

### A feature request

Say which part of the lifecycle it belongs to: resolve, verify, observe, plan,
approve, apply, or the convergence proof after it. The safety model is the
reason for most of the current shape, so a request that names the guarantee it
needs is easier to place than one that names a field.

### Give the issue a label

Issue forms apply a type label for you. When you open an issue another way, add
one type label -- `bug`, `enhancement`, `documentation`, or `question`.

## Proposing a change

Open an issue before a change that alters behavior, an API field, or a
condition reason. Work on a branch off `origin/master`.

### What a change owes

A change under `api/` regenerates: `make generate manifests`, with the generated
output committed. Review the generated CRD schema rather than only the Go type;
an `+optional` that does not reach the CRD or an enum that lost a value is
invisible in the Go diff and plain in the YAML one. Treat every stored object as
a user: a field that was required and is now absent is an object that stops
validating.

A change under a controller has two rules no gate catches, both in AGENTS.md: a
non-nil error requeues by itself, and envtest runs no built-in controllers, so a
green envtest is evidence about this controller and about nothing Kubernetes
would have done. Behavior that depends on Kubernetes doing its part belongs in
`test/e2e`, against kind.

### Building and testing

```bash
make build          # the four binaries
make test           # unit contour
make generate       # deepcopy
make manifests      # CRDs and RBAC from the markers
make verify         # the checks CI runs, including the CRD schema history
make e2e            # the suite against kind
```

`make verify` is the one that refuses a change the generators did not produce.
Run it after touching `api/` or any marker.

### What a pull request says

Match the existing message style; read `git log` before writing. An imperative
subject under about 72 characters, a blank line, then a body explaining why. The
diff already shows what changed, so spend the body on the reason, the trade-off,
or the risk.

American English, and plain international English: short sentences, concrete
nouns, active voice.

## Licensing of contributions

Ptah Operator is MIT licensed. By contributing you agree that your contribution
is licensed under the same terms.

## Conduct

Participation is covered by the [Code of Conduct](CODE_OF_CONDUCT.md).

## Commercial enquiries

Commercial questions go to `ask@stokaro.com`. Bug reports and feature requests
belong on the issue tracker, where they stay public and get labeled.
