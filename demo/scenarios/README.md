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
tags: [Lifecycle]               # the first tag groups the catalog

# Commands that return the lab to this scenario's starting point. They run
# before the steps and are not recorded: a reader is watching the scenario,
# not the preparation.
reset:
  - demo/bin/lab reset
  - demo/bin/lab publish v1 > demo/.lab/digest

steps:
  - note: One line of narration, under 96 characters.
    run: kubectl apply -f demo/.lab/storefront.yaml
    sync: applying                # the phase shown in the terminal's pill
    expect:
      exit: 0
      stdout_contains: [created]

  - note: The operator resolves, verifies, observes, plans and applies.
    # Hold until the cluster reaches a state, before the command runs. This
    # publishes nothing: it is the reader's own patience, and it lands the
    # command where a reader's second attempt would. What it waited for is
    # recorded as a check.
    await:
      kind: ptahschema
      name: storefront
      timeout: 6m
      generation: current
      condition:
        type: InSync
        status: "True"
        reason: ScopedConverged
    run: kubectl -n "$NAMESPACE" get ptahschema storefront
    show: [stdout]                # the default; a refusal names stderr
    expect:
      exit: 0
      stdout_contains: [InSync]
      # Read out of band, off the live object rather than out of the text the
      # step printed: a transcript is trimmed for a reader, and a check that
      # read the trimmed text would be checking the trimming.
      resource:
        kind: ptahschema
        name: storefront
        condition:
          type: Applying
          status: "False"
          reason: JobCompleted
        fields:
          - path: status.phase
            equals: InSync
          - path: status.applied.artifactDigest
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

## Waiting for something that has not happened yet

A wait for a condition that was already true returns at once and proves
nothing. There are two answers, and which one applies is decided by what the
step before it changed.

**The spec changed** -- an apply, a patch, a suspend. Add `generation: current`
to the `await`. The condition then has to have observed the object as it is
now, so a stale condition from before the change does not satisfy it.

**Only the database changed** -- drift, an incident, a hand-run statement.
There is no new generation to wait for, because nothing in Kubernetes changed.
Give the step `retry: 6m` and a command that reads: the recorder re-runs it
until the expectation holds, which is what a reader does by looking again.
`retry` is only for a command that changes nothing; what is published is the
run that held.

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
