package planread

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// A display read prints the name of a field it did not read. Ptah adds a few
// short identifiers, so a key of another shape or a long list of them is not a
// field added to a plan, and a name that passes is safe on a terminal.
const (
	maxUnreadFields    = 64
	maxUnreadNameBytes = 128
)

var unreadFieldName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// decodeForDisplay reads a document through the strict decoder, with every
// field this build does not declare taken out first. There is no second
// decoder: the format version, the dialect, the statements, the operator's own
// reading of their SQL and every refusal are DecodePlan's. A document that
// decodes as it is has nothing unread.
//
// This code sits in this package and not beside DecodePlan so that the manager
// and the runner, which do not import this package, do not link it.
func decodeForDisplay(document []byte, dialect string) (dataplane.PlanFile, []string, error) {
	decoded, err := dataplane.DecodePlan(document, dialect)
	if err == nil {
		return decoded, nil, nil
	}
	declared, unread, err := withoutUnreadFields(document)
	if err != nil {
		return dataplane.PlanFile{}, nil, err
	}
	decoded, err = dataplane.DecodePlan(declared, dialect)
	if err != nil {
		return dataplane.PlanFile{}, nil, err
	}
	return decoded, unread, nil
}

// withoutUnreadFields copies a plan document without the object keys
// dataplane.PlanFile does not declare, and names what it left out. Keys it keeps
// stay in their order, repeats included, so the strict decoder reads the fields
// it declares as it would read them in the stored document. It does not look
// inside a value it leaves out, and no error it returns quotes the document.
func withoutUnreadFields(document []byte) ([]byte, []string, error) {
	top := json.NewDecoder(bytes.NewReader(document))
	var value json.RawMessage
	if err := top.Decode(&value); err != nil {
		return nil, nil, fmt.Errorf("decode plan document: %w", err)
	}
	if _, err := top.Token(); !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("decode plan document: multiple JSON values")
	}
	unread := unreadFields{}
	declared, err := unread.value(value, reflect.TypeFor[dataplane.PlanFile](), "")
	if err != nil {
		return nil, nil, fmt.Errorf("decode plan document: %w", err)
	}
	names := make([]string, 0, len(unread))
	for name := range unread {
		names = append(names, name)
	}
	slices.Sort(names)
	return declared, names, nil
}

// unreadFields is the set of fields a walk over a document left out, by path.
type unreadFields map[string]bool

// value copies one value of a declared type. A value of the wrong shape passes
// through, and the strict decoder refuses it.
func (u unreadFields) value(raw json.RawMessage, declared reflect.Type, path string) (json.RawMessage, error) {
	switch declared.Kind() {
	case reflect.Pointer:
		return u.value(raw, declared.Elem(), path)
	case reflect.Struct:
		if !bytes.HasPrefix(raw, []byte("{")) {
			return raw, nil
		}
		return u.object(raw, declared, path)
	case reflect.Slice:
		if !bytes.HasPrefix(raw, []byte("[")) {
			return raw, nil
		}
		return u.list(raw, declared.Elem(), path+"[]")
	default:
		return raw, nil
	}
}

func (u unreadFields) object(raw json.RawMessage, declared reflect.Type, path string) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	kept := []byte("{")
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("an object key is not a string")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		name, fieldType, ok := declaredField(declared, key)
		if !ok {
			if err := u.add(path, key); err != nil {
				return nil, err
			}
			continue
		}
		copied, err := u.value(value, fieldType, joinField(path, name))
		if err != nil {
			return nil, err
		}
		encodedKey, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		if len(kept) > 1 {
			kept = append(kept, ',')
		}
		kept = append(append(append(kept, encodedKey...), ':'), copied...)
	}
	return append(kept, '}'), nil
}

func (u unreadFields) list(raw json.RawMessage, element reflect.Type, path string) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	kept := []byte("[")
	for decoder.More() {
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		copied, err := u.value(value, element, path)
		if err != nil {
			return nil, err
		}
		if len(kept) > 1 {
			kept = append(kept, ',')
		}
		kept = append(kept, copied...)
	}
	return append(kept, ']'), nil
}

func (u unreadFields) add(path, key string) error {
	if len(key) > maxUnreadNameBytes || !unreadFieldName.MatchString(key) {
		return errors.New("a field this build does not declare has a name that is not an identifier")
	}
	u[joinField(path, key)] = true
	if len(u) > maxUnreadFields {
		return fmt.Errorf("the document carries more than %d fields this build does not declare", maxUnreadFields)
	}
	return nil
}

func joinField(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// declaredField finds the field encoding/json would store a key in. It folds
// case the way encoding/json does, or a key the strict decoder accepts would be
// reported as unread and taken out.
func declaredField(declared reflect.Type, key string) (string, reflect.Type, bool) {
	for index := range declared.NumField() {
		field := declared.Field(index)
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		if strings.EqualFold(name, key) {
			return name, field.Type, true
		}
	}
	return "", nil, false
}
