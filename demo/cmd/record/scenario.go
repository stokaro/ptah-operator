package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// scenario is one demonstration, as demo/scenarios/<id>.yaml declares it.
//
// The file is the only place a scenario exists. The shell a reader sees on the
// page is the shell this ran, the narration is this file's, and the green
// result is the expectations below having held against a live cluster.
type scenario struct {
	ID      string   `yaml:"id"`
	Title   string   `yaml:"title"`
	Tagline string   `yaml:"tagline"`
	Learn   string   `yaml:"learn"`
	Tags    []string `yaml:"tags"`
	// Reset returns the lab to this scenario's starting point. It runs before
	// the steps and is not recorded: a reader is watching the scenario, not the
	// preparation.
	Reset []string `yaml:"reset"`
	Steps []step   `yaml:"steps"`
}

// step is one thing a reader watches happen.
type step struct {
	// Note is the demonstration's own narration, and is rendered differently
	// from anything a command wrote.
	Note string `yaml:"note"`
	// Await holds the step until the cluster reaches a state, before the
	// command runs. It publishes nothing: it is the reader's own patience, and
	// it lands the command at the moment a reader's second attempt would. What
	// it waited for is recorded as a check.
	Await *observation `yaml:"await"`
	// Run is the shell the step executes. It is shown as typed.
	Run string `yaml:"run"`
	// Retry re-runs the command until its expectation holds, for at most this
	// long. It is for a step that watches something settle, and only for a
	// command that changes nothing: what is published is the run that held,
	// which is what a reader gets by looking again.
	Retry time.Duration `yaml:"retry"`
	// Sync moves the phase pill in the terminal bar. It is how the operator's
	// state machine reaches a reader who is watching a terminal.
	Sync string `yaml:"sync"`
	// Show says which streams reach the transcript. The default is stdout; a
	// step that demonstrates a refusal names stderr, because the refusal is
	// what the reader came to see.
	Show []string `yaml:"show"`
	// Expect is what has to hold once the command has run. A step without it is
	// refused.
	Expect *expectation `yaml:"expect"`
}

// expectation is the condition under which this step's output may be published.
type expectation struct {
	Exit           *int         `yaml:"exit"`
	StdoutContains []string     `yaml:"stdout_contains"`
	StderrContains []string     `yaml:"stderr_contains"`
	StdoutAbsent   []string     `yaml:"stdout_absent"`
	Resource       *observation `yaml:"resource"`
}

// observation is a claim about one object in the cluster, read as fields.
//
// The recorder fetches the object itself rather than parsing the step's output:
// a transcript is trimmed for a reader, and a check that read the trimmed text
// would be checking the trimming.
type observation struct {
	Kind      string        `yaml:"kind"`
	Name      string        `yaml:"name"`
	Namespace string        `yaml:"namespace"`
	Timeout   time.Duration `yaml:"timeout"`
	Condition *condition    `yaml:"condition"`
	Fields    []field       `yaml:"fields"`
	// Generation set to "current" requires the condition to have observed the
	// object as it is now. Without it, a wait for a condition that was already
	// true returns before anything has been reconciled, and the step publishes
	// the state it was trying to watch change.
	Generation string `yaml:"generation"`
	// Absent expects the object not to exist. It is how a scenario shows that a
	// policy stopped something: nothing was created, and the way to say that is
	// to look for it.
	Absent bool `yaml:"absent"`
}

// field reads one value out of the object by its path in the JSON.
type field struct {
	Path     string `yaml:"path"`
	Equals   string `yaml:"equals"`
	NotEmpty bool   `yaml:"notEmpty"`
}

// condition reads a Kubernetes condition as fields.
//
// Never as a phrase in a message: a message is prose that changes, and a
// demonstration that watched prose would go green on the wrong state. The
// operator's contract distinguishes a finished Job, a pending verification and
// a proved convergence, and those are three different reasons on one type.
type condition struct {
	Type               string `yaml:"type"`
	Status             string `yaml:"status"`
	Reason             string `yaml:"reason"`
	ObservedGeneration *int64 `yaml:"observedGeneration"`
}

// validate refuses a scenario that could publish an unchecked claim.
func (s scenario) validate() error {
	var problems []error
	if !identifier.MatchString(s.ID) {
		problems = append(problems, fmt.Errorf("id %q is not a lowercase kebab-case identifier", s.ID))
	}
	for _, declared := range []struct{ name, value string }{
		{"title", s.Title},
		{"tagline", s.Tagline},
		{"learn", s.Learn},
	} {
		if strings.TrimSpace(declared.value) == "" {
			problems = append(problems, fmt.Errorf("%s: %s is empty", s.ID, declared.name))
		}
	}
	if len(s.Tags) == 0 {
		problems = append(problems, fmt.Errorf("%s: carries no tag, so the catalog cannot group it", s.ID))
	}
	if len(s.Steps) == 0 {
		problems = append(problems, fmt.Errorf("%s: has no steps", s.ID))
	}
	for index, current := range s.Steps {
		problems = append(problems, current.validate(s.ID, index)...)
	}
	return errors.Join(problems...)
}

func (s step) validate(scenarioID string, index int) []error {
	var problems []error
	where := fmt.Sprintf("%s step %d", scenarioID, index+1)

	if strings.TrimSpace(s.Run) == "" {
		problems = append(problems, fmt.Errorf("%s runs nothing", where))
	}
	// A note is one line in a terminal frame. Two sentences of narration
	// between two commands is a paragraph the reader scrolls past, and the
	// scenario is what has to be shortened, not the frame.
	note := strings.TrimSpace(s.Note)
	if strings.Contains(note, "\n") {
		problems = append(problems, fmt.Errorf("%s has a note on more than one line", where))
	}
	if len(note) > maximumNote {
		problems = append(problems, fmt.Errorf(
			"%s has a note of %d characters; a note is one line of a terminal, so keep it under %d",
			where, len(note), maximumNote))
	}
	for _, name := range s.Show {
		if name != "stdout" && name != "stderr" {
			problems = append(problems, fmt.Errorf("%s shows %q, which is neither stdout nor stderr", where, name))
		}
	}
	if s.Await != nil {
		problems = append(problems, s.Await.validate(where+" await")...)
	}
	if s.Expect == nil {
		problems = append(problems, fmt.Errorf(
			"%s states no expectation; a recording is a claim that something happened, and a "+
				"step nobody checked is the part of the claim nobody made", where))
		return problems
	}
	if s.Expect.Exit == nil {
		problems = append(problems, fmt.Errorf("%s does not say which exit status it expects", where))
	}
	if s.Expect.Resource != nil {
		problems = append(problems, s.Expect.Resource.validate(where+" resource")...)
	}
	return problems
}

func (o observation) validate(where string) []error {
	var problems []error
	if strings.TrimSpace(o.Kind) == "" {
		problems = append(problems, fmt.Errorf("%s names no kind", where))
	}
	if strings.TrimSpace(o.Name) == "" {
		problems = append(problems, fmt.Errorf("%s names no object", where))
	}
	if o.Absent {
		if o.Condition != nil || len(o.Fields) > 0 {
			problems = append(problems, fmt.Errorf(
				"%s expects the object to be absent and also reads fields out of it", where))
		}
		return problems
	}
	if o.Condition == nil && len(o.Fields) == 0 {
		problems = append(problems, fmt.Errorf(
			"%s reads the object and asks nothing of it", where))
	}
	if o.Generation != "" && o.Generation != "current" {
		problems = append(problems, fmt.Errorf(
			"%s reads generation %q; the only value is \"current\", which means the condition "+
				"has to have observed the object as it is now", where, o.Generation))
	}
	if o.Generation != "" && o.Condition == nil {
		problems = append(problems, fmt.Errorf(
			"%s asks for the current generation without a condition to read it from", where))
	}
	if o.Condition != nil {
		problems = append(problems, o.Condition.validate(where)...)
	}
	for _, read := range o.Fields {
		problems = append(problems, read.validate(where)...)
	}
	return problems
}

func (f field) validate(where string) []error {
	var problems []error
	if strings.TrimSpace(f.Path) == "" {
		problems = append(problems, fmt.Errorf("%s reads a field with no path", where))
	}
	if f.Equals == "" && !f.NotEmpty {
		problems = append(problems, fmt.Errorf(
			"%s reads %s and asks nothing of it; say equals or notEmpty", where, f.Path))
	}
	if f.Equals != "" && f.NotEmpty {
		problems = append(problems, fmt.Errorf(
			"%s reads %s with both equals and notEmpty; notEmpty is what you say when you "+
				"cannot name the value", where, f.Path))
	}
	return problems
}

func (c condition) validate(where string) []error {
	var problems []error
	if strings.TrimSpace(c.Type) == "" {
		problems = append(problems, fmt.Errorf("%s reads a condition with no type", where))
	}
	if c.Status != "True" && c.Status != "False" && c.Status != "Unknown" {
		problems = append(problems, fmt.Errorf(
			"%s expects condition status %q; Kubernetes writes True, False or Unknown", where, c.Status))
	}
	if strings.TrimSpace(c.Reason) == "" {
		problems = append(problems, fmt.Errorf(
			"%s reads condition %s without a reason; the operator distinguishes a finished Job, a "+
				"pending verification and a proved convergence by the reason on one type", where, c.Type))
	}
	return problems
}

// maximumNote is how much narration fits on one line of the frame the player
// draws, at the width a phone gives it.
const maximumNote = 96

var identifier = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// conditionsOf reads the conditions out of an object's JSON.
func conditionsOf(object []byte) ([]map[string]any, error) {
	var read struct {
		Status struct {
			Conditions []map[string]any `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(object, &read); err != nil {
		return nil, fmt.Errorf("read conditions: %w", err)
	}
	return read.Status.Conditions, nil
}

// matches reports whether one of the object's conditions is the one expected,
// and says what it found when none is.
func (c condition) matches(conditions []map[string]any) error {
	for _, found := range conditions {
		if fmt.Sprint(found["type"]) != c.Type {
			continue
		}
		if got := fmt.Sprint(found["status"]); got != c.Status {
			return fmt.Errorf("condition %s is %s, expected %s", c.Type, got, c.Status)
		}
		if got := fmt.Sprint(found["reason"]); got != c.Reason {
			return fmt.Errorf("condition %s has reason %s, expected %s", c.Type, got, c.Reason)
		}
		if c.ObservedGeneration != nil {
			got, ok := found["observedGeneration"].(float64)
			if !ok || int64(got) != *c.ObservedGeneration {
				return fmt.Errorf("condition %s observed generation %v, expected %d",
					c.Type, found["observedGeneration"], *c.ObservedGeneration)
			}
		}
		return nil
	}
	return fmt.Errorf("the object carries no %s condition", c.Type)
}

// read returns the value at a dot-separated path, and whether it was there.
//
// Only objects: a demonstration reads a named field out of a resource, and an
// index into a list is a position that moves between runs.
func read(object map[string]any, path string) (string, bool) {
	var current any = object
	for _, segment := range strings.Split(path, ".") {
		holder, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		current, ok = holder[segment]
		if !ok {
			return "", false
		}
	}
	if current == nil {
		return "", false
	}
	if number, ok := current.(float64); ok && number == float64(int64(number)) {
		return fmt.Sprintf("%d", int64(number)), true
	}
	return fmt.Sprint(current), true
}

// matches reports whether the object carries the value this field expects.
func (f field) matches(object map[string]any) error {
	value, present := read(object, f.Path)
	if !present {
		return fmt.Errorf("%s is not set", f.Path)
	}
	if f.NotEmpty {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is empty", f.Path)
		}
		return nil
	}
	if value != f.Equals {
		return fmt.Errorf("%s is %q, expected %q", f.Path, value, f.Equals)
	}
	return nil
}
