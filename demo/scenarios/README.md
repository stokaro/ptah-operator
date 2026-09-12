# Scenario definitions

One file per scenario. Each is the whole of what a scenario is: the state it
starts from, the steps a reader watches, the narration between them, and the
conditions that have to hold for the recording to be published.

There is no second copy of any of that. The shell a reader sees is the shell
the recorder ran, the narration is the file's, and the green result on the page
is the `expect` block below having passed — not a status written into a fixture.

## Shape

```yaml
id: first-apply                 # the scenario's address, in URLs and file names
title: Apply a schema           # the tile's heading
tagline: >                      # the tile's sentence
  One sentence on what this shows.
learn: >                        # what a reader takes away
  One sentence on what they will know afterwards.
tags: [lifecycle]

# Commands that return the lab to this scenario's starting point. They run
# before the steps and are not recorded: a reader is watching the scenario,
# not the reset.
reset:
  - kubectl delete ptahschema --all -n "$NAMESPACE" --ignore-not-found

steps:
  - note: Publish the desired schema as an OCI artifact.
    run: ptah schema push ...
    sync: artifact published      # the phase shown in the terminal's pill
    expect:
      exit: 0
      stdout_contains: [digest]

  - note: The operator resolves the tag once and works from the digest.
    run: kubectl -n "$NAMESPACE" get ptahschema demo -o yaml
    show: stdout
    expect:
      exit: 0
      # Conditions are read as fields, never as a phrase in a message.
      condition:
        type: Ready
        status: "True"
        reason: Converged
```

## Rules the recorder enforces

A step with no `expect` is refused. A recording is a claim that something
happened, and a step nobody checked is the part of the claim nobody made.

`condition` reads `type`, `status`, `reason` and `observedGeneration` from the
resource. It never matches a substring of a message: a message is prose and
changes, and a demonstration that watched prose would go green on the wrong
state.

Output is shown as the command wrote it. Trimming a long block to its telling
lines is what a transcript does; reordering it, or writing a line the command
did not print, is not. What may be rewritten is named in `normalize`, and only
technical identifiers that change between runs.
