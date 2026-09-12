# The demonstration

Scenarios run against a real Kubernetes cluster with the operator installed from
its chart, recorded while they run, and replayed on
[operator.ptah.run](https://operator.ptah.run/demo/). One file in
`scenarios/` is one scenario; that directory is the census.

Nothing here is written for the page. A transcript is what the commands
printed, and a recording exists only because every condition its scenario
claims held on the live cluster.

## Three parts

**`scenarios/`** declares what a scenario is: the state it starts from, the
steps a reader watches, the narration between them, and the conditions that
have to hold. [`scenarios/README.md`](scenarios/README.md) is the format.

**`cmd/record`** executes them against the lab, checks the cluster reached the
state each scenario claims, and writes `recordings/runs.json`.

**The site** reads that file. `docs/site/src/pages/demo/` renders every run as
a transcript in the markup, and `docs/site/public/demo/player.js` replays the
same events into a terminal frame. Both read one file, so a printed session and
a played one cannot differ.

## The lab

The lab is the end-to-end harness stopped after its bootstrap:

```sh
make demo-up
```

That is `hack/e2e-kind.sh` with `E2E_STOP_AFTER=bootstrap`. It builds the
cluster, an isolated registry with credentials, an external PostgreSQL, and the
chart installed from a reproducible package with digest-pinned images; then it
releases the cleanup trap and writes what it built to `demo/.lab/environment`.

There is no second cluster and no second chart install. A demonstration on an
environment the suite does not prove would be a demonstration of something
else, and the first of the two to drift would do so silently.

`demo/bin/lab prepare` adds what a namespace needs on top of that: the two
Services that route to the registry and the database containers, the
credentials as Secrets, and the verification policy as an immutable ConfigMap.

## Recording

```sh
make demo-record                                  # every scenario
go run ./demo/cmd/record -root . -only drift      # one, merged into the file
go run ./demo/cmd/record -root . -check           # parse and validate, run nothing
```

A failed expectation ends the scenario and writes nothing. Recording the rest
of it would publish a session that continued past the point where the
demonstration stopped being true.

`-only` replaces one scenario in an existing recording and keeps the others. It
refuses when the file was recorded against a different lab: one recording
carries one set of versions, and two sets of transcripts under them would be a
claim nobody made.

## Reading it

```sh
make demo-serve       # the site, with the committed recording
```

## Removing it

```sh
make demo-down
```

The cluster and the containers the lab created, and nothing else. A lab left up
holds four kind nodes, a registry and a database on whichever Docker context it
was built against.

## What it needs

- Docker, reachable through the context `make` passes (`DOCKER_CONTEXT`,
  `remote-dev-container` by default). The bootstrap builds images, so a remote
  context with a few cores is much faster than a laptop.
- `kind`, `kubectl`, `helm`, `jq`, `git`, Go and Node.
- About 8 GB of memory and 20 GB of disk for the cluster, the registry and the
  mirrored images.
- A clean tree. The harness archives the exact commit it runs, and refuses a
  working copy that does not match it.

Measured on macOS against a remote Linux Docker context. The `demo` job in
`.github/workflows/ci.yml` runs the same targets on a Linux runner, which is
where Linux is exercised on every run rather than once.

Windows is not verified and is not expected to work: the harness and these
scripts are POSIX shell throughout.

## When something fails

The recorder prints every check of the run that stopped, passed and failed,
before the failure. The failing one names the object and what it read.

The lab stays up after a failure, which is the point of it being a lab:

```sh
set -a; . demo/.lab/environment; set +a
kubectl --kubeconfig "$E2E_KUBECONFIG" -n "$E2E_TEST_NAMESPACE" get ptahschema -o yaml
kubectl --kubeconfig "$E2E_KUBECONFIG" -n "$E2E_OPERATOR_NAMESPACE" logs -l app.kubernetes.io/component=controller
demo/bin/lab psql '\dt'
```

Two failures are worth naming because they look like the operator being wrong:

- **A wait that returns at once.** A condition that was already true proves
  nothing about what a step just did. A step that changes the spec waits with
  `generation: current`; a step that changes only the database uses `retry` and
  watches the database settle.
- **A publish that is refused.** A Ptah artifact is not byte-identical between
  two pushes of one file, and a tag is write-once. `demo/bin/lab publish` gives
  each push its own tag for that reason; what the operator is pointed at is the
  digest the push returned.

## What a recording may not carry

A transcript is committed and published. The recorder reads the lab's own
credential files and refuses to publish any text containing one of the values
in them.

It is a refusal and not a redaction. A transcript that reached a password
reached it through a command the scenario asked for, and the repair is the
scenario. The operator exists to keep a database password out of the places a
schema change would otherwise carry it; a demonstration of that which leaked
one would be the exact failure it claims to prevent.
