package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// recorder runs scenarios against the lab and collects what happened.
type recorder struct {
	lab         lab
	root        string
	environment []string
	// stepTimeout bounds one command. A scenario step that hangs would
	// otherwise hold the whole recording, and the run that produced no
	// recording is the one nobody can read.
	stepTimeout time.Duration
	// pollInterval is how often an await re-reads the object.
	pollInterval time.Duration
	// normalizer prepares captured text for publication, and refuses text
	// carrying a lab credential.
	normalizer normalizer
	// log receives progress, so a run that takes minutes says what it is on.
	log func(format string, arguments ...any)
}

// outcome is what one command did.
type outcome struct {
	stdout string
	stderr string
	exit   int
}

// check is one claim the recording carries, and whether it held.
//
// Every check is written down, passed or failed. A recording publishes only
// when all of them passed, and the list is what the page shows to say what
// "verified" means here.
type check struct {
	Scenario string `json:"scenario"`
	Step     int    `json:"step"`
	Claim    string `json:"claim"`
	Passed   bool   `json:"passed"`
	Detail   string `json:"detail,omitempty"`
}

// recording is one scenario as it ran.
type recording struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Tagline  string   `json:"tagline"`
	Learn    string   `json:"learn"`
	Tags     []string `json:"tags"`
	Commands int      `json:"commands"`
	Lines    int      `json:"lines"`
	Events   []event  `json:"events"`
	Checks   []check  `json:"checks"`
}

// run executes one scenario and returns what a reader will see.
//
// A failed expectation ends the scenario. Recording the rest of it would
// publish a session that continued past the point where the demonstration
// stopped being true.
func (r *recorder) run(ctx context.Context, current scenario) (recording, error) {
	r.log("scenario %s: resetting", current.ID)
	for _, command := range current.Reset {
		result, err := r.shell(ctx, command)
		if err != nil {
			return recording{}, fmt.Errorf("%s: reset %q: %w", current.ID, command, err)
		}
		if result.exit != 0 {
			return recording{}, fmt.Errorf(
				"%s: reset %q exited %d\n%s", current.ID, command, result.exit, result.stderr)
		}
	}

	recorded := recording{
		ID:      current.ID,
		Title:   current.Title,
		Tagline: current.Tagline,
		Learn:   current.Learn,
		Tags:    current.Tags,
	}
	for index, one := range current.Steps {
		events, checks, err := r.step(ctx, current.ID, index, one)
		recorded.Checks = append(recorded.Checks, checks...)
		if err != nil {
			return recorded, fmt.Errorf("%s step %d: %w", current.ID, index+1, err)
		}
		recorded.Events = append(recorded.Events, events...)
		recorded.Commands++
	}
	for _, one := range recorded.Events {
		if one[0] != kindSync {
			recorded.Lines++
		}
	}
	return recorded, nil
}

// step runs one step and returns its events and its checks.
func (r *recorder) step(ctx context.Context, scenarioID string, index int, current step) ([]event, []check, error) {
	var events []event
	var checks []check
	number := index + 1

	if current.Sync != "" {
		events = append(events, event{kindSync, current.Sync})
	}
	if note := strings.TrimSpace(current.Note); note != "" {
		events = append(events, event{kindNote, "# " + note})
	}

	if current.Await != nil {
		r.log("scenario %s step %d: waiting for %s", scenarioID, number, current.Await.describe())
		err := r.await(ctx, *current.Await)
		checks = append(checks, check{
			Scenario: scenarioID,
			Step:     number,
			Claim:    current.Await.describe(),
			Passed:   err == nil,
			Detail:   detail(err),
		})
		if err != nil {
			return events, checks, err
		}
	}

	r.log("scenario %s step %d: %s", scenarioID, number, firstLine(current.Run))
	expected := *current.Expect
	result, failures, err := r.attempt(ctx, current, expected)
	if err != nil {
		return events, checks, err
	}

	checks = append(checks, check{
		Scenario: scenarioID,
		Step:     number,
		Claim:    expected.describe(),
		Passed:   failures == nil,
		Detail:   detail(failures),
	})
	if failures != nil {
		return events, checks, fmt.Errorf("%w\n\nstdout:\n%s\nstderr:\n%s",
			failures, indent(result.stdout), indent(result.stderr))
	}

	if expected.Resource != nil {
		err := r.observe(ctx, *expected.Resource)
		checks = append(checks, check{
			Scenario: scenarioID,
			Step:     number,
			Claim:    expected.Resource.describe(),
			Passed:   err == nil,
			Detail:   detail(err),
		})
		if err != nil {
			return events, checks, err
		}
	}

	published, err := r.published(fmt.Sprintf("%s step %d", scenarioID, number), current, result)
	if err != nil {
		return events, checks, err
	}
	events = append(events, commandEvents(current.Run)...)
	events = append(events, published...)
	return events, checks, nil
}

// published turns the streams a step shows into transcript events.
func (r *recorder) published(where string, current step, result outcome) ([]event, error) {
	shown := current.Show
	if len(shown) == 0 {
		shown = []string{"stdout"}
	}
	var events []event
	for _, stream := range shown {
		text := result.stdout
		kind := kindOutput
		if stream == "stderr" {
			text = result.stderr
			// A step that shows stderr is showing a refusal or a diagnostic,
			// which is the half of the contract a demonstration exists to make
			// visible.
			kind = kindError
		}
		if err := r.normalizer.audit(where, text); err != nil {
			return nil, err
		}
		events = append(events, outputEvents(r.normalizer.apply(text), kind)...)
	}
	return events, nil
}

// shell runs one command the way a reader would, and captures both streams.
func (r *recorder) shell(ctx context.Context, command string) (outcome, error) {
	ctx, cancel := context.WithTimeout(ctx, r.stepTimeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	// sh, not bash: every command a scenario publishes has to work in the shell
	// the reader's cluster tooling already assumes.
	process := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	process.Dir = r.root
	process.Env = r.environment
	process.Stdout = &stdout
	process.Stderr = &stderr

	err := process.Run()
	result := outcome{stdout: stdout.String(), stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case err == nil:
		return result, nil
	case errors.As(err, &exit):
		result.exit = exit.ExitCode()
		return result, nil
	case ctx.Err() != nil:
		return result, fmt.Errorf("%q did not finish within %s", firstLine(command), r.stepTimeout)
	default:
		return result, fmt.Errorf("run %q: %w", firstLine(command), err)
	}
}

// evaluate reports every way the outcome failed the expectation.
//
// Every way, not the first: a step that exited wrong and also printed the wrong
// thing is one repair, and reporting half of it costs a second run of a
// scenario that takes minutes.
func (e expectation) evaluate(result outcome) error {
	var problems []error
	if e.Exit != nil && result.exit != *e.Exit {
		problems = append(problems, fmt.Errorf("exited %d, expected %d", result.exit, *e.Exit))
	}
	for _, want := range e.StdoutContains {
		if !strings.Contains(result.stdout, want) {
			problems = append(problems, fmt.Errorf("stdout does not contain %q", want))
		}
	}
	for _, want := range e.StderrContains {
		if !strings.Contains(result.stderr, want) {
			problems = append(problems, fmt.Errorf("stderr does not contain %q", want))
		}
	}
	for _, unwanted := range e.StdoutAbsent {
		if strings.Contains(result.stdout, unwanted) {
			problems = append(problems, fmt.Errorf("stdout contains %q, which it must not", unwanted))
		}
	}
	return errors.Join(problems...)
}

// describe says what an expectation claims, for the recorded check.
func (e expectation) describe() string {
	claims := []string{fmt.Sprintf("exits %d", derefOr(e.Exit, 0))}
	for _, want := range e.StdoutContains {
		claims = append(claims, fmt.Sprintf("prints %q", want))
	}
	for _, want := range e.StderrContains {
		claims = append(claims, fmt.Sprintf("reports %q", want))
	}
	for _, unwanted := range e.StdoutAbsent {
		claims = append(claims, fmt.Sprintf("never prints %q", unwanted))
	}
	return strings.Join(claims, ", ")
}

// describe says what an observation claims.
func (o observation) describe() string {
	subject := o.Kind + "/" + o.Name
	if o.Absent {
		return subject + " does not exist"
	}
	claims := make([]string, 0, len(o.Fields)+1)
	if o.Condition != nil {
		claims = append(claims, fmt.Sprintf("%s=%s (%s)", o.Condition.Type, o.Condition.Status, o.Condition.Reason))
	}
	for _, read := range o.Fields {
		if read.NotEmpty {
			claims = append(claims, read.Path+" is set")
			continue
		}
		claims = append(claims, fmt.Sprintf("%s=%s", read.Path, read.Equals))
	}
	return subject + ": " + strings.Join(claims, ", ")
}

// await holds until the observation holds, or until its deadline.
func (r *recorder) await(ctx context.Context, expected observation) error {
	timeout := expected.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()

	last := r.observe(ctx, expected)
	for last != nil {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s did not hold within %s: %w", expected.describe(), timeout, last)
		case <-ticker.C:
			last = r.observe(ctx, expected)
		}
	}
	return nil
}

// observe reads the object once and evaluates the claim.
func (r *recorder) observe(ctx context.Context, expected observation) error {
	object, found, err := r.fetch(ctx, expected)
	if err != nil {
		return err
	}
	if expected.Absent {
		if found {
			return fmt.Errorf("%s/%s exists, and this step expects it not to", expected.Kind, expected.Name)
		}
		return nil
	}
	if !found {
		return fmt.Errorf("%s/%s does not exist", expected.Kind, expected.Name)
	}

	var problems []error
	if expected.Condition != nil {
		conditions, err := conditionsOf(object)
		wanted := *expected.Condition
		switch {
		case err != nil:
			problems = append(problems, err)
		case expected.Generation == "current":
			generation, found, err := generationOf(object)
			if err != nil || !found {
				problems = append(problems, fmt.Errorf(
					"%s/%s carries no metadata.generation", expected.Kind, expected.Name))
				break
			}
			wanted.ObservedGeneration = &generation
			problems = append(problems, wanted.matches(conditions))
		default:
			problems = append(problems, wanted.matches(conditions))
		}
	}
	if len(expected.Fields) > 0 {
		var read map[string]any
		if err := json.Unmarshal(object, &read); err != nil {
			problems = append(problems, fmt.Errorf("read %s/%s: %w", expected.Kind, expected.Name, err))
		} else {
			for _, one := range expected.Fields {
				problems = append(problems, one.matches(read))
			}
		}
	}
	return errors.Join(problems...)
}

// fetch reads one object as JSON, and separates "not there" from "could not ask".
//
// The separation matters: an absent object is a result a scenario may expect,
// and an unreachable API server is a broken run wearing that result's clothes.
func (r *recorder) fetch(ctx context.Context, expected observation) ([]byte, bool, error) {
	namespace := expected.Namespace
	if namespace == "" {
		namespace = "$NAMESPACE"
	}
	command := fmt.Sprintf(
		"kubectl -n %s get %s %s -o json 2>&1", quote(namespace), quote(expected.Kind), quote(expected.Name))
	result, err := r.shell(ctx, command)
	if err != nil {
		return nil, false, err
	}
	if result.exit != 0 {
		if strings.Contains(result.stdout, "NotFound") || strings.Contains(result.stdout, "not found") {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf(
			"read %s/%s: kubectl exited %d: %s", expected.Kind, expected.Name, result.exit,
			strings.TrimSpace(result.stdout))
	}
	return []byte(result.stdout), true, nil
}

// quote makes one shell word out of a value, leaving a variable reference to
// the shell. A namespace arrives as $NAMESPACE by default and as a literal when
// a scenario names one, and both have to reach kubectl as one argument.
func quote(value string) string {
	if strings.HasPrefix(value, "$") {
		return `"` + value + `"`
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func detail(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func derefOr(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return line
}

func indent(text string) string {
	if strings.TrimSpace(text) == "" {
		return "  (empty)\n"
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for index, line := range lines {
		lines[index] = "  " + line
	}
	return strings.Join(lines, "\n") + "\n"
}

// generationOf reads metadata.generation out of an object's JSON.
func generationOf(object []byte) (int64, bool, error) {
	var read struct {
		Metadata struct {
			Generation *int64 `json:"generation"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(object, &read); err != nil {
		return 0, false, fmt.Errorf("read generation: %w", err)
	}
	if read.Metadata.Generation == nil {
		return 0, false, nil
	}
	return *read.Metadata.Generation, true, nil
}

// attempt runs a step's command, once or until its expectation holds.
//
// A step that declares no retry runs once: most commands change something, and
// running one twice is a second change. A step that declares one is watching
// something settle, and re-running is what a reader does. What is published is
// the run that held.
func (r *recorder) attempt(ctx context.Context, current step, expected expectation) (outcome, error, error) {
	deadline := time.Now().Add(current.Retry)
	for {
		result, err := r.shell(ctx, current.Run)
		if err != nil {
			return outcome{}, nil, err
		}
		failures := expected.evaluate(result)
		if failures == nil || !time.Now().Before(deadline) {
			return result, failures, nil
		}
		select {
		case <-ctx.Done():
			return result, failures, nil
		case <-time.After(r.pollInterval):
		}
	}
}
