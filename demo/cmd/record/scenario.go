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
//
// A field tagged digest:"-" is how the scenario is described rather than what
// it runs, and definitionDigest leaves it out. Every other field is in the
// digest without being listed there.
type scenario struct {
	ID      string `yaml:"id"`
	Title   string `yaml:"title" digest:"-"`
	Tagline string `yaml:"tagline" digest:"-"`
	Learn   string `yaml:"learn" digest:"-"`
	// Start says where the scenario begins and how a reader gets there: an
	// empty namespace, or the state another scenario ends in. The reset is
	// what reaches it here, and it is not published, so this sentence is what
	// stands in for it on the page.
	Start string   `yaml:"start" digest:"-"`
	Tags  []string `yaml:"tags" digest:"-"`
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
	Note string `yaml:"note" digest:"-"`
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
	// Answers are what a person types at the terminal the command runs on,
	// each one after the command has printed its prompt. A step with answers
	// runs on a pseudo-terminal rather than on pipes: a command that asks for
	// a decision asks only a terminal, and refuses an answer piped into it.
	// The terminal echoes what was typed, so the transcript shows the answer
	// where the person typed it, and stdout and stderr arrive as one stream.
	Answers []answer `yaml:"answers"`
	// Expect is what has to hold once the command has run. A step without it is
	// refused.
	Expect *expectation `yaml:"expect"`
}

// answer is one reply typed at a terminal.
type answer struct {
	// Prompt is text the command prints when it stops to ask. The answer is
	// typed once the terminal has shown it, and not before: typed earlier, the
	// terminal would echo it above the question.
	Prompt string `yaml:"prompt"`
	// Type is the line typed in reply, without its newline.
	Type string `yaml:"type"`
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
	// Keep records the value, which has to be set, under a name a later step
	// compares with SameAs. It is how a scenario proves that something did not
	// happen between two steps -- the same Job's UID before and after another
	// pass -- without printing a comparison a reader would have to repeat.
	Keep string `yaml:"keep"`
	// SameAs requires the value an earlier step kept under this name.
	SameAs string `yaml:"sameAs"`
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
		{"start", s.Start},
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
	problems = append(problems, s.validateRepeatedWaits()...)
	problems = append(problems, s.validateKeptValues()...)
	return errors.Join(problems...)
}

// validateKeptValues refuses a comparison with a value no earlier step keeps,
// and a name kept twice. A sameAs that named nothing would compare the field
// with nothing, and the step would publish a claim it never checked.
func (s scenario) validateKeptValues() []error {
	var problems []error
	kept := map[string]int{}
	for index, current := range s.Steps {
		if current.Expect == nil || current.Expect.Resource == nil {
			continue
		}
		where := fmt.Sprintf("%s step %d", s.ID, index+1)
		for _, read := range current.Expect.Resource.Fields {
			if read.SameAs != "" {
				if _, ok := kept[read.SameAs]; !ok {
					problems = append(problems, fmt.Errorf(
						"%s compares %s with the value kept as %q, which no earlier step keeps",
						where, read.Path, read.SameAs))
				}
			}
		}
		for _, read := range current.Expect.Resource.Fields {
			if read.Keep == "" {
				continue
			}
			if earlier, ok := kept[read.Keep]; ok {
				problems = append(problems, fmt.Errorf(
					"%s keeps a value as %q, which step %d already keeps", where, read.Keep, earlier))
				continue
			}
			kept[read.Keep] = index + 1
		}
	}
	return problems
}

// validateRepeatedWaits refuses a second await on a condition an earlier step
// already awaited, unless it binds the condition to the current generation.
//
// A condition is a latch, not an event. Once a step has waited for InSync=True,
// that condition stays true until something makes it false, so a later step
// awaiting the same thing is answered by the first step's residue and waits for
// nothing. The step then reads the cluster in the state it was trying to watch
// change, and whether it passes is a race with the reconciler.
//
// Where a spec changed, generation: current ties the condition to the object as
// it is now and the wait is real. Where nothing changed the spec -- a row edited
// in the database, a column dropped by hand -- no generation moves and no await
// can help: re-read the thing itself with retry, which is what drift.yaml does.
func (s scenario) validateRepeatedWaits() []error {
	type waited struct {
		kind, name, conditionType, status, reason string
	}
	first := make(map[waited]int, len(s.Steps))
	var problems []error
	for index, current := range s.Steps {
		if current.Await == nil || current.Await.Condition == nil {
			continue
		}
		key := waited{
			kind:          current.Await.Kind,
			name:          current.Await.Name,
			conditionType: current.Await.Condition.Type,
			status:        current.Await.Condition.Status,
			reason:        current.Await.Condition.Reason,
		}
		earlier, repeated := first[key]
		if !repeated {
			first[key] = index + 1
			continue
		}
		if current.Await.Generation == "current" {
			continue
		}
		problems = append(problems, fmt.Errorf(
			"%s step %d waits for %s=%s (%s) on %s/%s, which step %d already waited for; "+
				"the condition has been true since, so this step waits for nothing. Say "+
				"generation: current when a spec change is what it is waiting on, and retry "+
				"the command itself when nothing changed the spec",
			s.ID, index+1, key.conditionType, key.status, key.reason,
			key.kind, key.name, earlier))
	}
	return problems
}

func (s step) validate(scenarioID string, index int) []error {
	var problems []error
	where := fmt.Sprintf("%s step %d", scenarioID, index+1)

	if strings.TrimSpace(s.Run) == "" {
		problems = append(problems, fmt.Errorf("%s runs nothing", where))
	}
	for _, name := range unpublishedVariables(s.Run) {
		problems = append(problems, fmt.Errorf(
			"%s reads $%s, which the demonstration does not publish; a reader repeating this "+
				"command has no such variable, so either the step sets it or demo/README.md "+
				"states where the value comes from", where, name))
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
	problems = append(problems, s.validateAnswers(where)...)
	if s.Await != nil {
		problems = append(problems, s.Await.validate(where+" await")...)
		for _, read := range s.Await.Fields {
			if read.Keep != "" || read.SameAs != "" {
				problems = append(problems, fmt.Errorf(
					"%s await keeps or compares %s; a wait reads the object many times, so only an "+
						"expectation, which reads it once, may keep a value", where, read.Path))
			}
		}
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

// validateAnswers refuses answers a terminal could not deliver as written,
// and a step that would type them more than once.
func (s step) validateAnswers(where string) []error {
	if len(s.Answers) == 0 {
		return nil
	}
	var problems []error
	for index, one := range s.Answers {
		switch {
		case strings.TrimSpace(one.Prompt) == "":
			problems = append(problems, fmt.Errorf("%s answer %d waits for no prompt", where, index+1))
		case strings.ContainsAny(one.Prompt, "\r\n"):
			// A terminal writes a line break as CR LF, so a prompt that spans
			// two lines would never be found in what it showed.
			problems = append(problems, fmt.Errorf("%s answer %d waits for a prompt of more than one line", where, index+1))
		}
		if strings.TrimSpace(one.Type) == "" || strings.ContainsAny(one.Type, "\r\n") {
			problems = append(problems, fmt.Errorf("%s answer %d types %q; an answer is one line of text", where, index+1, one.Type))
		}
	}
	if s.Retry != 0 {
		problems = append(problems, fmt.Errorf(
			"%s answers a prompt and retries; typing the answer again is a second decision, not a second look", where))
	}
	for _, name := range s.Show {
		if name == "stderr" {
			problems = append(problems, fmt.Errorf(
				"%s shows stderr, and a terminal carries one stream; what the command wrote is in stdout", where))
		}
	}
	if s.Expect != nil && len(s.Expect.StderrContains) > 0 {
		problems = append(problems, fmt.Errorf(
			"%s expects text on stderr, and a terminal carries one stream; expect it in stdout", where))
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
	asked := 0
	for _, set := range []bool{f.Equals != "", f.NotEmpty, f.Keep != "", f.SameAs != ""} {
		if set {
			asked++
		}
	}
	switch {
	case asked == 0:
		problems = append(problems, fmt.Errorf(
			"%s reads %s and asks nothing of it; say equals, notEmpty, keep or sameAs", where, f.Path))
	case f.Equals != "" && f.NotEmpty:
		problems = append(problems, fmt.Errorf(
			"%s reads %s with both equals and notEmpty; notEmpty is what you say when you "+
				"cannot name the value", where, f.Path))
	case asked > 1:
		problems = append(problems, fmt.Errorf(
			"%s reads %s with more than one of equals, notEmpty, keep and sameAs; say one", where, f.Path))
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
// A field that keeps its value writes it into kept, and one that compares
// reads it from there.
func (f field) matches(object map[string]any, kept map[string]string) error {
	value, present := read(object, f.Path)
	if !present {
		return fmt.Errorf("%s is not set", f.Path)
	}
	if f.NotEmpty || f.Keep != "" {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is empty", f.Path)
		}
		if f.Keep != "" {
			kept[f.Keep] = value
		}
		return nil
	}
	if f.SameAs != "" {
		earlier, ok := kept[f.SameAs]
		switch {
		case !ok:
			return fmt.Errorf("no step kept a value as %q to compare %s with", f.SameAs, f.Path)
		case value != earlier:
			return fmt.Errorf("%s is %q, and the value kept as %q is %q", f.Path, value, f.SameAs, earlier)
		}
		return nil
	}
	if value != f.Equals {
		return fmt.Errorf("%s is %q, expected %q", f.Path, value, f.Equals)
	}
	return nil
}

// publishedVariables are the environment names a step may read.
//
// demo/README.md states what each one holds and where a reader's own value
// comes from, which is what makes a published command repeatable. The lab's
// own E2E_ names are deliberately absent: demo/bin/lab reads those to compose a
// manifest, and a step that read one would print a command that resolves
// nowhere but in this repository's lab.
var publishedVariables = map[string]bool{
	"KUBECONFIG":          true,
	"NAMESPACE":           true,
	"OPERATOR_NAMESPACE":  true,
	"CONTROLLER":          true,
	"EXECUTOR_IMAGE":      true,
	"REGISTRY_IN_CLUSTER": true,
	"PTAH_OCI_REGISTRY":   true,
	"PTAH_OCI_USERNAME":   true,
	"PTAH_OCI_PASSWORD":   true,
}

// shellVariables are the reader's shell's own, not the demonstration's. They
// are kept out of the list above because a page telling a reader what PATH
// holds would be telling them something they already have.
var shellVariables = map[string]bool{
	"PATH": true,
	"HOME": true,
}

// variableRead matches a shell expansion of an upper-case name.
//
// Upper-case only. That is the shell's convention for an environment variable,
// and it leaves jq's own alone: a scenario passes `--arg f` and reads `$f`
// inside the filter, where the shell never looks.
var variableRead = regexp.MustCompile(`\$\{?([A-Z][A-Z0-9_]*)\}?`)

// unpublishedVariables returns the names a reader could not resolve, in the
// order they are read, without repeating one.
//
// The shell is read in its own order. A name is resolvable at a read only if
// something earlier in the step set it and that value outlived the command it
// was written for: an assignment in front of a command belongs to that command
// alone, so `APPLY=Always cmd "$APPLY"` reads whatever the caller's environment
// happened to hold, and so does every later line.
func unpublishedVariables(run string) []string {
	known := map[string]bool{}
	seen := map[string]bool{}
	var unpublished []string
	for _, segment := range shellSegments(run) {
		for _, name := range shellReads(segment) {
			if publishedVariables[name] || shellVariables[name] || known[name] || seen[name] {
				continue
			}
			seen[name] = true
			unpublished = append(unpublished, name)
		}
		for _, name := range segmentSets(segment) {
			known[name] = true
		}
	}
	return unpublished
}

// segmentSets returns the names one segment leaves set behind it: a standalone
// assignment, a for loop's own variable, and what `read` was told to fill.
func segmentSets(segment string) []string {
	words := shellWords(segment)
	var assigned []string
	index := 0
	for ; index < len(words); index++ {
		name, _, found := strings.Cut(words[index], "=")
		if !found || !shellName.MatchString(name) {
			break
		}
		assigned = append(assigned, name)
	}
	// A command word after them means they were written for that command, and
	// the shell drops them when it returns.
	if index < len(words) {
		assigned = nil
	}

	for position, word := range words {
		switch word {
		case "for":
			if position+2 < len(words) && words[position+2] == "in" && shellName.MatchString(words[position+1]) {
				assigned = append(assigned, words[position+1])
			}
		case "read":
			for _, target := range words[position+1:] {
				if strings.HasPrefix(target, "-") {
					continue
				}
				if shellName.MatchString(target) {
					assigned = append(assigned, target)
				}
				break
			}
		}
	}
	return assigned
}

var shellName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// shellReads returns the names one segment expands.
//
// What sits in single quotes is not one of them: the shell hands it over as
// written, which is how a jq filter carries a `$` the shell never sees.
func shellReads(segment string) []string {
	var names []string
	for _, match := range variableRead.FindAllStringSubmatch(withoutLiterals(segment), -1) {
		names = append(names, match[1])
	}
	return names
}

// shellSegments splits a step's shell into the pieces the shell runs one after
// another. A separator inside quotes or inside a substitution is text, not a
// separator, so the scan carries both.
func shellSegments(run string) []string {
	var segments []string
	var current strings.Builder
	flush := func() {
		if strings.TrimSpace(current.String()) != "" {
			segments = append(segments, current.String())
		}
		current.Reset()
	}
	scan(run, func(r rune, single, double bool, depth int) {
		if !single && !double && depth == 0 && strings.ContainsRune(";\n&|", r) {
			flush()
			return
		}
		current.WriteRune(r)
	})
	flush()
	return segments
}

// shellWords splits one segment into the words the shell would pass along.
func shellWords(segment string) []string {
	var words []string
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			words = append(words, current.String())
		}
		current.Reset()
	}
	scan(segment, func(r rune, single, double bool, depth int) {
		if !single && !double && depth == 0 && (r == ' ' || r == '\t' || r == '\n') {
			flush()
			return
		}
		current.WriteRune(r)
	})
	flush()
	return words
}

// withoutLiterals blanks what single quotes protect, keeping every other
// character where it was.
func withoutLiterals(segment string) string {
	var out strings.Builder
	scan(segment, func(r rune, single, double bool, depth int) {
		if single {
			out.WriteRune(' ')
			return
		}
		out.WriteRune(r)
	})
	return out.String()
}

// scan walks shell text once and reports each character with what the shell
// would make of it: whether quotes protect it, and how deep inside a
// substitution it sits. A backslash escapes the character behind it, and a
// backslash before a newline is dropped the way the shell drops it.
func scan(text string, visit func(r rune, single, double bool, depth int)) {
	single, double, escaped := false, false, false
	depth := 0
	for _, r := range text {
		switch {
		case escaped:
			escaped = false
			if r != '\n' {
				visit(r, single, double, depth)
			}
			continue
		case single:
			if r != '\'' {
				visit(r, true, false, depth)
				continue
			}
			single = false
		case r == '\\':
			escaped = true
			continue
		case r == '\'':
			single = true
		case r == '"':
			double = !double
		case r == '(':
			depth++
		case r == ')':
			if depth > 0 {
				depth--
			}
		}
		visit(r, false, double, depth)
	}
}
