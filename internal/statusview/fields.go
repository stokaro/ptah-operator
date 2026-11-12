package statusview

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WriteFields writes every field a status record holds, one line each,
// under indent, labeled with its API name in words. A details view prints a
// record whole this way, so a field the API gains is printed without this
// code or its caller changing, and the label is the name the API reference
// lists it under.
//
// A field the record leaves at its zero value is not printed when its JSON
// tag says omitempty, because the stored object does not hold it. A nested
// record is a label line with its fields indented under it, and a list of
// records is one indented block per item, each opened by "-". Every value
// passes through Clean.
func WriteFields(text *strings.Builder, indent string, record any) {
	writeRecord(text, indent, reflect.ValueOf(record))
}

// WriteScalars writes the fields of record that are printed on one line,
// the way WriteFields does, and leaves out its nested records, lists and
// maps, which a details view prints as records of their own.
func WriteScalars(text *strings.Builder, indent string, record any) {
	writeRecordOf(text, indent, reflect.ValueOf(record), true)
}

func writeRecord(text *strings.Builder, indent string, record reflect.Value) {
	writeRecordOf(text, indent, record, false)
}

func writeRecordOf(text *strings.Builder, indent string, record reflect.Value, scalarsOnly bool) {
	for record.Kind() == reflect.Pointer || record.Kind() == reflect.Interface {
		if record.IsNil() {
			return
		}
		record = record.Elem()
	}
	if record.Kind() != reflect.Struct {
		return
	}
	for index := range record.NumField() {
		field := record.Type().Field(index)
		if !field.IsExported() {
			continue
		}
		name, omitEmpty, inline := jsonField(field)
		if name == "-" {
			continue
		}
		if inline {
			writeRecordOf(text, indent, record.Field(index), scalarsOnly)
			continue
		}
		if scalarsOnly && !scalarType(record.Field(index).Type()) {
			continue
		}
		writeField(text, indent, Label(name), record.Field(index), omitEmpty)
	}
}

func writeField(text *strings.Builder, indent, label string, value reflect.Value, omitEmpty bool) {
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return
		}
		// A pointer that is set is a value the record holds, zero or not.
		value, omitEmpty = value.Elem(), false
	}
	line := func(rendered string) { fmt.Fprintf(text, "%s%s: %s\n", indent, label, rendered) }

	if rendered, ok := scalar(value); ok {
		if rendered == "" || (omitEmpty && value.IsZero()) {
			return
		}
		line(rendered)
		return
	}
	switch value.Kind() {
	case reflect.Slice, reflect.Array:
		if value.Len() == 0 {
			return
		}
		if scalarType(value.Type().Elem()) {
			items := make([]string, 0, value.Len())
			for index := range value.Len() {
				rendered, _ := scalar(value.Index(index))
				items = append(items, rendered)
			}
			line(strings.Join(items, ", "))
			return
		}
		fmt.Fprintf(text, "%s%s:\n", indent, label)
		for index := range value.Len() {
			var item strings.Builder
			writeRecord(&item, indent+"    ", value.Index(index))
			if item.Len() == 0 {
				// An item whose fields are all unset is still an item.
				fmt.Fprintf(text, "%s  - (empty)\n", indent)
				continue
			}
			fmt.Fprintf(text, "%s  - %s", indent, strings.TrimPrefix(item.String(), indent+"    "))
		}
	case reflect.Map:
		if value.Len() == 0 {
			return
		}
		pairs := make([]string, 0, value.Len())
		for _, key := range value.MapKeys() {
			name, _ := scalar(key)
			rendered, _ := scalar(value.MapIndex(key))
			pairs = append(pairs, name+"="+rendered)
		}
		sort.Strings(pairs)
		line(strings.Join(pairs, ", "))
	case reflect.Struct:
		if omitEmpty && value.IsZero() {
			return
		}
		var nested strings.Builder
		writeRecord(&nested, indent+"  ", value)
		if nested.Len() == 0 {
			return
		}
		fmt.Fprintf(text, "%s%s:\n%s", indent, label, nested.String())
	}
}

// scalar renders a value that is printed on one line: a string, a number, a
// boolean, a time, a duration or a quantity. ok is false for a record, a
// list or a map.
func scalar(value reflect.Value) (rendered string, ok bool) {
	if value.CanInterface() {
		switch typed := value.Interface().(type) {
		case metav1.Time:
			if typed.IsZero() {
				return "", true
			}
			return Time(typed), true
		case metav1.Duration:
			return Duration(typed.Duration), true
		case resource.Quantity:
			return Clean(typed.String()), true
		}
	}
	switch value.Kind() {
	case reflect.String:
		return Clean(value.String()), true
	case reflect.Bool:
		return strconv.FormatBool(value.Bool()), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(value.Int(), 10), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(value.Uint(), 10), true
	case reflect.Pointer:
		if value.IsNil() {
			return "", true
		}
		return scalar(value.Elem())
	}
	return "", false
}

var (
	timeType     = reflect.TypeFor[metav1.Time]()
	durationType = reflect.TypeFor[metav1.Duration]()
	quantityType = reflect.TypeFor[resource.Quantity]()
)

// scalarType reports a type printed on one line, through a pointer or not.
func scalarType(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t {
	case timeType, durationType, quantityType:
		return true
	}
	switch t.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	}
	return false
}

func jsonField(field reflect.StructField) (name string, omitEmpty, inline bool) {
	tag, ok := field.Tag.Lookup("json")
	if !ok {
		return field.Name, false, field.Anonymous
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	for _, option := range parts[1:] {
		switch option {
		case "omitempty", "omitzero":
			omitEmpty = true
		case "inline":
			inline = true
		}
	}
	if name == "" {
		if field.Anonymous {
			inline = true
		}
		name = field.Name
	}
	return name, omitEmpty, inline
}

// acronyms are the words an API name spells in capitals, longest first so
// "UIDs" is not read as "UID" and a stray "s", nor "HTTPS" as "HTTP".
var acronyms = []string{
	"HTTPS", "HTTP", "JSON", "UIDs", "UID", "IDs", "ID", "SQL", "URL", "TTL", "OCI", "TLS", "API",
}

// lowerAcronyms are the same words where a name starts with them in lower
// case, as sqlState does.
var lowerAcronyms = map[string]string{
	"uid": "UID", "id": "ID", "sql": "SQL", "url": "URL", "ttl": "TTL", "oci": "OCI",
	"json": "JSON", "http": "HTTP", "https": "HTTPS",
}

// Label turns an API field name into the words a reader sees: jobUID is
// "Job UID", applyPodUIDs "Apply pod UIDs", dispatchNotAfter "Dispatch not
// after".
func Label(name string) string {
	var words []string
	for rest := name; rest != ""; {
		if word := leadingAcronym(rest); word != "" {
			words = append(words, word)
			rest = rest[len(word):]
			continue
		}
		end := 1
		for end < len(rest) && !unicode.IsUpper(rune(rest[end])) {
			end++
		}
		word := strings.ToLower(rest[:end])
		if upper, ok := lowerAcronyms[word]; ok {
			word = upper
		}
		words = append(words, word)
		rest = rest[end:]
	}
	if len(words) == 0 {
		return name
	}
	if first := words[0]; first == strings.ToLower(first) {
		words[0] = strings.ToUpper(first[:1]) + first[1:]
	}
	return strings.Join(words, " ")
}

func leadingAcronym(name string) string {
	for _, word := range acronyms {
		if !strings.HasPrefix(name, word) {
			continue
		}
		// An acronym is a word of its own only where the next one starts:
		// "IDs" ends a name or meets a capital, and "Identity" is not "ID".
		if next := name[len(word):]; next == "" || unicode.IsUpper(rune(next[0])) || unicode.IsDigit(rune(next[0])) {
			return word
		}
	}
	return ""
}
