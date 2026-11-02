# Scenario definitions

One file per scenario, named for its id. Each is the whole of what a scenario
is: the state it starts from, the steps a reader watches, the narration between
them, and the conditions that have to hold for the recording to be published.

There is no second copy of any of that. The shell a reader sees is the shell
the recorder ran, the narration is the file's, and the green result on the page
is the `expect` and `await` blocks below having held against a live cluster.

## Shape

```yaml
id: first-apply                 # the scenario's address, in URLs and file names
title: Apply a schema           # the tile's heading
tagline: One sentence on what this shows.
learn: One sentence on what a reader knows afterwards.
# Where the scenario begins and how a reader gets there: an empty namespace,
# or the state another scenario ends in. The reset reaches it here and is not
# published, so this is what stands in for it on the page.
start: An empty database and a namespace with the lab's Secrets.
tags: [Lifecycle]

# Commands that return the lab to this scenario's starting point. They run
# before the steps and are not recorded: a reader is watching the scenario,
# not the preparation.
reset:
  - demo/bin/lab reset

steps:
  - note: One line of narration, under 96 characters.
    run: |
      kubectl -n "$NAMESPACE" create configmap storefront-v1 --from-file=schema.sql=demo/schemas/v1.sql
      kubectl -n "$NAMESPACE" apply -f demo/manifests/manual-approval/storefront.yaml
    sync: applying                # the phase shown in the terminal's pill
    expect:
      exit: 0
      stdout_contains: [storefront created]

  - note: The operator plans and stops. Wait until it does.
    # The wait is a command the reader sees and repeats. kubectl ptah wait
    # counts only a reading of the database that began after it did, so a
    # condition left over from before the change cannot end it.
    run: kubectl ptah wait schema/storefront -n "$NAMESPACE" --for approval
    expect:
      exit: 0
      stdout_contains: [reached approval]
      # Read out of band, off the live object rather than out of the text the
      # step printed: a transcript is trimmed for a reader, and a check that
      # read the trimmed text would be checking the trimming.
      resource:
        kind: ptahschema
        name: storefront
        generation: current
        condition:
          type: ApprovalRequired
          status: "True"
          reason: Waiting

  - note: Approve it. It shows the exact plan, asks, and creates the approval bound to it.
    run: kubectl ptah approve schema/storefront -n "$NAMESPACE" --name storefront-v1 --wait
    # Typed at a terminal, after the command asks. See below.
    answers:
      - prompt: "Type yes to approve: "
        type: "yes"
    expect:
      exit: 0
      stdout_contains: ["Type yes to approve: yes", reached applied]
      resource:
        kind: ptahschemaapproval
        name: storefront-v1
        fields:
          - path: spec.approver.username
            notEmpty: true
```

## What the recorder refuses

**A step with no `expect`.** A recording is a claim that something happened,
and a step nobody checked is the part of the claim nobody made. A step also has
to say which exit status it expects, and a condition has to name a reason.

**A condition read as prose.** `condition` reads `type`, `status`, `reason` and
`observedGeneration` as fields. It never matches a substring of a message: a
message is prose and changes, and a demonstration that watched prose would go
green on the wrong state. The operator distinguishes a finished Job, a pending
verification and a proved convergence by the reason on one type, and that is
exactly the distinction a message match would lose.

**Narration that is not one line.** A note is one line of a terminal frame at
the width a phone gives it. Two sentences between two commands is a paragraph
the reader scrolls past, and the scenario is what has to be shorter.

## Changing a recorded scenario

The recording holds a digest of what this file says the scenario runs: the
`reset`, and each step's `run`, `await`, `retry`, `sync`, `show`, `answers`
and `expect`. Edit any of them and `make verify-demo-recording`, which
`make verify-source` runs, refuses the committed recording until the scenario
is recorded again. `title`, `tagline`, `learn`, `start`, `tags` and `note` are
left out, because rewording them does not change what ran, and a YAML comment
is not part of the definition at all.

## Waiting for something that has not happened yet

A wait for a condition that was already true returns at once and proves
nothing, and a wait the reader cannot see makes the commands they can see look
synchronous: copied, they read the old state. So a step that depends on the
operator having acted waits in a command of its own, where it is published:
`kubectl ptah wait`, or the `--wait` of the command that made the change.

Those waits count only what happened after they began. `--for in-sync` and
`--for approval` need a reading of the database that began after the wait
did, which is what makes them right after a change only the database saw --
drift, an incident, a hand-run statement -- where no generation moves and a
condition stays true throughout. `kubectl ptah refresh --wait` asks for such a
reading now instead of at the operator's interval.

Where no `kubectl ptah` target fits, the step uses `kubectl wait` on a
condition or a phase that cannot have held before the change, and says why.

The recorder's own checks stay, as independent evidence rather than as
waiting: an `expect.resource` reads the object once, after the command, and
holds it to the condition the command claims to have waited for.

`await` and `retry` still exist. `await` holds the step before its command
runs and `retry` repeats a read until it holds, and neither is published, so
a step uses them only where the command it shows does not depend on the
state they wait for. A second `await` on a condition an earlier step already
awaited has to name `generation: current`, or it would be answered by the
first step's residue.

## Typing at a terminal

`kubectl ptah approve` shows the plan and asks for a decision on a terminal,
and refuses one piped into it. A step that runs it lists what a person types
under `answers`: each `prompt` is text the command prints when it asks, and
`type` is the line typed after it appears. The step runs on a pseudo-terminal,
so the transcript is what the terminal showed, the echoed answer included, and
stdout and stderr are one stream: a step with answers expects text on stdout
and shows nothing else. It never retries, because typing an answer again is a
second decision rather than a second look.

## Comparing two steps

A field can keep the value it read for a later step to compare with:
`keep: <name>` records it, and `sameAs: <name>` requires the same value. It is
how a scenario proves that something did not happen in between -- the last run
of a migration is the same Job before and after another reading of its
history -- without printing a comparison a reader would have to repeat. The
transcript prints every UID the same way, so the comparison is the
recorder's.

## What a transcript may say

Output is shown as the command wrote it. Trimming a long block to its telling
lines is what a transcript does; reordering it, or writing a line the command
did not print, is not.

What is rewritten is named in `demo/cmd/record/normalize.go`, and it is only
identifiers a lab generates: a UID, a timestamp, and the names the bootstrap
builds from its run identifier. A name inside a padded column is padded back to
the width it replaced, so a table stays where kubectl put it.

Nothing else. Not the order of events, not the plan, not the fingerprint an
approval names, and not the reason on a condition.

## What a step may read

A step is published, and a reader repeats it, so a step reads only names a
reader can have: `KUBECONFIG`, `NAMESPACE`, `OPERATOR_NAMESPACE`, `CONTROLLER`,
`EXECUTOR_IMAGE`, `REGISTRY_IN_CLUSTER`, the three `PTAH_OCI_*` credentials,
`PATH` and `HOME`.
Anything else the step sets itself, in the same block, where a reader sees it
being set.

The recorder refuses a step that reads another name and says which one. The
`E2E_` names the bootstrap writes are the ones this keeps out: they are how
`demo/bin/lab` finds the lab's own pieces, and a command printing one would
resolve nowhere but here. [`../README.md`](../README.md) states what each
published name holds.

A `reset` block is not published and is not held to this: it is the
preparation, and it may use the lab's scripts freely.

The commands themselves are `kubectl`, `ptah` and `kubectl-ptah`, which the lab
puts on `PATH` before a step runs. A step never installs one; a reader installs
`kubectl-ptah` from a release, which
[Read a plan](https://operator.ptah.run/edge/use/read-a-plan/#install) carries.
