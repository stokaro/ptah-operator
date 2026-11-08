package runnerharness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// A change of shape is any difference left between a committed capture and a
// fresh one once the volatile values are masked. That covers a field added or
// removed, a field that changed type (null is a type of its own), an element
// added to or removed from a list, and every value not declared volatile: a
// contract version, an outcome, a finding's category, an exit status, a line of
// standard error, an argument.
//
// What is volatile is the value of a key in volatileKeys, wherever the key
// appears, and the parts of a string that volatilePatterns match, in a document,
// in standard error and in an argument. Nothing else is. A volatile key's type
// still counts, so a time that becomes a number is a change of shape.
//
// Declare what a second run on the same pin actually moved and no more. A digest
// is volatile only where it names an artifact, whose manifest records when it was
// pushed: a plan's digest, a fingerprint and a Git content digest stay held,
// because a release that changes one changed what it means. A rule nothing matched
// in a complete run fails it (see Masker), so the list cannot outlive the values
// it was written for.
var (
	volatileKeys = []string{
		// When a migration ran and how long it took, in a history or a run.
		"applied_at", "execution_time", "execution_time_ms",
		// An artifact's digest in a report of it, and in the result frame.
		"digest", "resolvedDigest",
	}
	volatilePatterns = []volatilePattern{
		{
			name:        "address",
			pattern:     regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}(:\d+)?\b`),
			replacement: "<address>",
		},
		{
			// The private files and directories the runner makes under TMPDIR.
			name:        "temporary name",
			pattern:     regexp.MustCompile(`\b(ptah-[a-z-]+)-\d{5,}\b`),
			replacement: "$1-<random>",
		},
		{
			name:        "time",
			pattern:     regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`),
			replacement: "<time>",
		},
		{
			name:        "artifact digest in a reference",
			pattern:     regexp.MustCompile(`(oci://[^@\s"]+@)sha256:[0-9a-f]{64}`),
			replacement: "$1<digest>",
		},
		{
			// The line an OCI pull prints about what it read.
			name:        "artifact digest in a pull",
			pattern:     regexp.MustCompile(`(?m)^Digest: sha256:[0-9a-f]{64}$`),
			replacement: "Digest: <digest>",
		},
		{
			// Ptah resolves a row file named through /proc/self to the path of
			// its own process, whose number depends on what the container
			// started before it.
			name:        "process id",
			pattern:     regexp.MustCompile(`(/proc/)\d+/`),
			replacement: "$1<pid>/",
		},
		{
			// What Ptah's log lines on standard error are tagged with per run.
			name:        "correlation id",
			pattern:     regexp.MustCompile(`\bcorrelation_id=[0-9a-f]+\b`),
			replacement: "correlation_id=<random>",
		},
	}
)

type volatilePattern struct {
	name        string
	pattern     *regexp.Regexp
	replacement string
}

// Masker masks the volatile values and counts what each rule matched, so a run
// can refuse a rule that masked nothing. It is safe for concurrent use: the rows
// of a run share one.
type Masker struct {
	keys     map[string]bool
	patterns []volatilePattern

	mu   sync.Mutex
	hits map[string]int
}

// NewMasker is a Masker holding the declared rules.
func NewMasker() *Masker {
	keys := make(map[string]bool, len(volatileKeys))
	for _, key := range volatileKeys {
		keys[key] = true
	}
	return &Masker{keys: keys, patterns: volatilePatterns, hits: map[string]int{}}
}

func (m *Masker) hit(rule string, times int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hits[rule] += times
}

// Text masks the volatile parts of a string.
func (m *Masker) Text(text string) string {
	for _, rule := range m.patterns {
		if found := rule.pattern.FindAllStringIndex(text, -1); len(found) > 0 {
			m.hit(rule.name, len(found))
			text = rule.pattern.ReplaceAllString(text, rule.replacement)
		}
	}
	return text
}

// Value masks a decoded JSON value. A volatile key keeps its value's type and
// loses the value.
func (m *Masker) Value(value any) any {
	switch typed := value.(type) {
	case string:
		return m.Text(typed)
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = m.Value(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			if m.keys[key] {
				m.hit("key "+key, 1)
				out[key] = maskedByType(item)
				continue
			}
			out[key] = m.Value(item)
		}
		return out
	default:
		return value
	}
}

func maskedByType(value any) any {
	switch value.(type) {
	case string:
		return "<volatile>"
	case json.Number:
		return json.Number("0")
	default:
		return value
	}
}

// UnusedRules names the declared rules that masked nothing yet.
func (m *Masker) UnusedRules() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var unused []string
	for _, rule := range m.patterns {
		if m.hits[rule.name] == 0 {
			unused = append(unused, rule.name)
		}
	}
	for key := range m.keys {
		if m.hits["key "+key] == 0 {
			unused = append(unused, "key "+key)
		}
	}
	slices.Sort(unused)
	return unused
}

// Difference is one change of shape, at a path inside a capture.
type Difference struct {
	Path string
	// Kind is added, removed, retyped or changed.
	Kind      string
	Committed string
	Fresh     string
}

func (d Difference) String() string {
	switch d.Kind {
	case "added":
		return fmt.Sprintf("%s: added %s", d.Path, d.Fresh)
	case "removed":
		return fmt.Sprintf("%s: removed, was %s", d.Path, d.Committed)
	case "retyped":
		return fmt.Sprintf("%s: was %s and is %s", d.Path, d.Committed, d.Fresh)
	default:
		return fmt.Sprintf("%s: was %s and is %s", d.Path, d.Committed, d.Fresh)
	}
}

// CompareDocuments reports every change of shape between two documents a child
// printed. JSON is compared as a tree and anything else line by line.
func CompareDocuments(masker *Masker, committed, fresh []byte) []Difference {
	committedValue, committedJSON := decodeDocument(committed)
	freshValue, freshJSON := decodeDocument(fresh)
	switch {
	case committedJSON && freshJSON:
		return CompareValues(masker, committedValue, freshValue)
	case committedJSON != freshJSON:
		return []Difference{{
			Path: "$", Kind: "retyped",
			Committed: documentKind(committedJSON), Fresh: documentKind(freshJSON),
		}}
	default:
		return CompareLines(masker, string(committed), string(fresh))
	}
}

func documentKind(isJSON bool) string {
	if isJSON {
		return "a JSON document"
	}
	return "text"
}

// CompareValues reports every change of shape between two decoded JSON values.
func CompareValues(masker *Masker, committed, fresh any) []Difference {
	var differences []Difference
	compareTree("$", masker.Value(committed), masker.Value(fresh), &differences)
	return differences
}

// CompareLines reports every line of text that differs, by its number.
func CompareLines(masker *Masker, committed, fresh string) []Difference {
	committedLines := strings.Split(masker.Text(committed), "\n")
	freshLines := strings.Split(masker.Text(fresh), "\n")
	var differences []Difference
	for index := range max(len(committedLines), len(freshLines)) {
		path := "line " + strconv.Itoa(index+1)
		switch {
		case index >= len(committedLines):
			differences = append(differences, Difference{Path: path, Kind: "added", Fresh: strconv.Quote(freshLines[index])})
		case index >= len(freshLines):
			differences = append(differences, Difference{Path: path, Kind: "removed", Committed: strconv.Quote(committedLines[index])})
		case committedLines[index] != freshLines[index]:
			differences = append(differences, Difference{
				Path: path, Kind: "changed",
				Committed: strconv.Quote(committedLines[index]), Fresh: strconv.Quote(freshLines[index]),
			})
		}
	}
	return differences
}

func decodeDocument(data []byte) (any, bool) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false
	}
	return value, true
}

func compareTree(path string, committed, fresh any, differences *[]Difference) {
	committedType, freshType := typeName(committed), typeName(fresh)
	if committedType != freshType {
		*differences = append(*differences, Difference{Path: path, Kind: "retyped", Committed: committedType, Fresh: freshType})
		return
	}
	switch typed := committed.(type) {
	case map[string]any:
		other := fresh.(map[string]any) // the types were compared above
		keys := make([]string, 0, len(typed)+len(other))
		for key := range typed {
			keys = append(keys, key)
		}
		for key := range other {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range slices.Compact(keys) {
			before, inCommitted := typed[key]
			after, inFresh := other[key]
			child := path + "." + key
			switch {
			case !inCommitted:
				*differences = append(*differences, Difference{Path: child, Kind: "added", Fresh: render(after)})
			case !inFresh:
				*differences = append(*differences, Difference{Path: child, Kind: "removed", Committed: render(before)})
			default:
				compareTree(child, before, after, differences)
			}
		}
	case []any:
		other := fresh.([]any) // the types were compared above
		for index := range max(len(typed), len(other)) {
			child := path + "[" + strconv.Itoa(index) + "]"
			switch {
			case index >= len(typed):
				*differences = append(*differences, Difference{Path: child, Kind: "added", Fresh: render(other[index])})
			case index >= len(other):
				*differences = append(*differences, Difference{Path: child, Kind: "removed", Committed: render(typed[index])})
			default:
				compareTree(child, typed[index], other[index], differences)
			}
		}
	default:
		if render(committed) != render(fresh) {
			*differences = append(*differences, Difference{Path: path, Kind: "changed", Committed: render(committed), Fresh: render(fresh)})
		}
	}
}

func typeName(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case json.Number:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", value)
	}
}

func render(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(data)
}

// scrubbedFields are the top-level fields of a drift report that name the
// machine that produced it, and the second carries a connection string. They
// are removed before a report is stored and before it is compared, and are the
// only change a stored document has from what Ptah printed.
var scrubbedFields = []string{"sources", "database_url"}

// Scrub removes the scrubbed fields from a document Ptah printed with two spaces
// of indentation, keeping every other byte. A document without them is returned
// as it came, and one that still holds them afterwards is an error.
func Scrub(document []byte) ([]byte, error) {
	if !hasScrubbedField(document) {
		return document, nil
	}
	lines := strings.SplitAfter(string(document), "\n")
	var out []string
	for index := 0; index < len(lines); index++ {
		scrubbed := slices.ContainsFunc(scrubbedFields, func(field string) bool {
			return strings.HasPrefix(lines[index], `  "`+field+`":`)
		})
		if !scrubbed {
			out = append(out, lines[index])
			continue
		}
		// A field's value ends where the next top-level line starts.
		for index+1 < len(lines) && !strings.HasPrefix(lines[index+1], `  "`) && !strings.HasPrefix(lines[index+1], "}") {
			index++
		}
	}
	// The last field carries no comma, and the one removed may have been last.
	if closing := slices.IndexFunc(out, func(line string) bool { return strings.HasPrefix(line, "}") }); closing > 0 {
		out[closing-1] = strings.TrimSuffix(strings.TrimSuffix(out[closing-1], "\n"), ",") + "\n"
	}
	result := []byte(strings.Join(out, ""))
	if !json.Valid(result) || hasScrubbedField(result) {
		return nil, fmt.Errorf("the fields %q could not be removed from the document", scrubbedFields)
	}
	return result, nil
}

func hasScrubbedField(document []byte) bool {
	var top map[string]json.RawMessage
	if json.Unmarshal(document, &top) != nil {
		return false
	}
	for _, field := range scrubbedFields {
		if _, present := top[field]; present {
			return true
		}
	}
	return false
}
