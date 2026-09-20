package crdupgrade

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestStoredControllerStateScanCoversEveryCRDThatStoresIt derives the answer
// from the shipped CRDs instead of restating the preflight's own table. A kind
// that stores a controller-state version and is never listed reads exactly like
// a cluster with nothing to refuse, so the refusal has to be measured against
// the schemas rather than against itself.
func TestStoredControllerStateScanCoversEveryCRDThatStoresIt(t *testing.T) {
	durable, err := ControllerStateBearingResources()
	if err != nil {
		t.Fatal(err)
	}
	if len(durable) == 0 {
		t.Fatal("no embedded CRD carries a controllerStateVersion; the walk found nothing to cover")
	}
	wantPaths := make(map[string][]string, len(durable))
	for _, resource := range durable {
		wantPaths[resource.Kind] = resource.Paths
	}

	gotPaths := make(map[string][]string, len(storedControllerStateKinds))
	for _, scanned := range storedControllerStateKinds {
		if _, duplicate := gotPaths[scanned.kind]; duplicate {
			t.Fatalf("stored controller-state scan lists %s twice", scanned.kind)
		}
		paths := make([]string, 0, len(scanned.locations))
		for _, location := range scanned.locations {
			paths = append(paths, strings.Join(location.path, "."))
		}
		sort.Strings(paths)
		gotPaths[scanned.kind] = paths
	}

	var uncovered, unknown []string
	for kind := range wantPaths {
		if _, found := gotPaths[kind]; !found {
			uncovered = append(uncovered, kind)
		}
	}
	for kind := range gotPaths {
		if _, found := wantPaths[kind]; !found {
			unknown = append(unknown, kind)
		}
	}
	sort.Strings(uncovered)
	sort.Strings(unknown)
	if len(uncovered) != 0 || len(unknown) != 0 {
		t.Fatalf(
			"stored controller-state scan covers %v; the shipped CRDs store controller state in %v: unscanned %v, scanned without storing any %v",
			sortedKindNames(gotPaths), sortedKindNames(wantPaths), uncovered, unknown,
		)
	}
	for _, kind := range sortedKindNames(wantPaths) {
		if !reflect.DeepEqual(gotPaths[kind], wantPaths[kind]) {
			t.Errorf("%s scan reads %v, but its schema stores a controller-state version at %v", kind, gotPaths[kind], wantPaths[kind])
		}
	}
}

func sortedKindNames(byKind map[string][]string) []string {
	names := make([]string, 0, len(byKind))
	for kind := range byKind {
		names = append(names, kind)
	}
	sort.Strings(names)
	return names
}
