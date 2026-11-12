// Package statusviewtest fills a status record for the tests that hold a
// details view to printing all of it.
package statusviewtest

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stokaro/ptah-operator/internal/statusview"
)

// Line is one field a filled record holds and the line a details view has to
// print for it.
type Line struct {
	// Path is the field's place in the record, as Go names it.
	Path string
	// Text is "Label: value", the label and value as statusview.WriteFields
	// prints them.
	Text string
}

// Fill sets every field the record pointed to holds, at every depth, to a
// value no other field has: a list gets one item and a map one entry. It
// returns the line each field has to be printed as. Fields named in skip, by
// their Go name at any depth, are left as they are.
//
// A field the API gains is filled too, so a test over Fill fails until a
// details view prints it.
func Fill(record any, skip ...string) []Line {
	filler := &filler{skip: map[string]bool{}}
	for _, name := range skip {
		filler.skip[name] = true
	}
	filler.record(reflect.ValueOf(record).Elem(), "")
	return filler.lines
}

type filler struct {
	skip  map[string]bool
	next  int
	lines []Line
}

var (
	timeType     = reflect.TypeFor[metav1.Time]()
	durationType = reflect.TypeFor[metav1.Duration]()
	quantityType = reflect.TypeFor[resource.Quantity]()
	day0         = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
)

func (f *filler) record(value reflect.Value, path string) {
	for index := range value.NumField() {
		field := value.Type().Field(index)
		if !field.IsExported() || f.skip[field.Name] {
			continue
		}
		name, inline := jsonName(field)
		if name == "-" {
			continue
		}
		if inline {
			f.record(value.Field(index), path+"."+field.Name)
			continue
		}
		f.field(value.Field(index), path+"."+field.Name, statusview.Label(name))
	}
}

func (f *filler) field(value reflect.Value, path, label string) {
	f.next++
	switch value.Type() {
	case timeType:
		at := metav1.NewTime(day0.Add(time.Duration(f.next) * time.Minute))
		value.Set(reflect.ValueOf(at))
		f.add(path, label, statusview.Time(at))
		return
	case durationType:
		// Whole minutes, which statusview.Duration prints as "5m" where
		// time.Duration prints "5m0s".
		duration := metav1.Duration{Duration: time.Duration(f.next) * time.Minute}
		value.Set(reflect.ValueOf(duration))
		f.add(path, label, statusview.Duration(duration.Duration))
		return
	case quantityType:
		quantity := resource.MustParse(fmt.Sprintf("%dMi", 1000+f.next))
		value.Set(reflect.ValueOf(quantity))
		f.add(path, label, quantity.String())
		return
	}
	switch value.Kind() {
	case reflect.Pointer:
		value.Set(reflect.New(value.Type().Elem()))
		f.field(value.Elem(), path, label)
	case reflect.String:
		text := fmt.Sprintf("f%04dz", f.next)
		value.SetString(text)
		f.add(path, label, text)
	case reflect.Bool:
		value.SetBool(true)
		f.add(path, label, "true")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		number := int64(70000 + f.next)
		value.SetInt(number)
		f.add(path, label, fmt.Sprint(number))
	case reflect.Slice:
		value.Set(reflect.MakeSlice(value.Type(), 1, 1))
		f.field(value.Index(0), path+"[0]", label)
	case reflect.Map:
		key := reflect.New(value.Type().Key()).Elem()
		key.SetString(fmt.Sprintf("k%04dz", f.next))
		entry := reflect.New(value.Type().Elem()).Elem()
		f.next++
		var rendered string
		switch entry.Type() {
		case quantityType:
			quantity := resource.MustParse(fmt.Sprintf("%dMi", 1000+f.next))
			entry.Set(reflect.ValueOf(quantity))
			rendered = quantity.String()
		default:
			rendered = fmt.Sprintf("m%04dz", f.next)
			entry.SetString(rendered)
		}
		value.Set(reflect.MakeMap(value.Type()))
		value.SetMapIndex(key, entry)
		f.add(path, label, key.String()+"="+rendered)
	case reflect.Struct:
		f.record(value, path)
	default:
		panic(fmt.Sprintf("statusviewtest: %s is a %s, which Fill does not fill", path, value.Kind()))
	}
}

func (f *filler) add(path, label, rendered string) {
	f.lines = append(f.lines, Line{Path: strings.TrimPrefix(path, "."), Text: label + ": " + rendered})
}

func jsonName(field reflect.StructField) (name string, inline bool) {
	tag, ok := field.Tag.Lookup("json")
	if !ok {
		return field.Name, field.Anonymous
	}
	parts := strings.Split(tag, ",")
	for _, option := range parts[1:] {
		if option == "inline" {
			inline = true
		}
	}
	if parts[0] == "" {
		return field.Name, inline || field.Anonymous
	}
	return parts[0], inline
}
