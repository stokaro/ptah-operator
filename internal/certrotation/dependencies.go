package certrotation

import "reflect"

// nilDependency recognizes both a nil interface and an interface containing a
// typed nil. Constructors use it at the dependency boundary so an invalid
// implementation cannot turn into a delayed reconciliation panic.
func nilDependency(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
