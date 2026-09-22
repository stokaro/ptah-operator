package crdupgrade

import (
	"strconv"

	"github.com/stokaro/ptah-operator/internal/controllerstate"
)

// The suite names controller-state versions relative to the compiled constant
// rather than as numbers. Two senses were written as literals before, `1` for
// "the version this binary understands" and `2` for "a version it must refuse",
// and both stopped meaning what they said the first time the constant moved --
// sixty-odd call sites to re-read, in a suite whose whole subject is a fence.
//
// Naming them costs one line per bump instead.
var (
	// ourStateVersion is what this binary compiled and therefore accepts.
	ourStateVersion = controllerstate.CurrentVersion
	// newerStateVersion is state written by a manager this one predates, which
	// the downgrade fence has to refuse.
	newerStateVersion = controllerstate.CurrentVersion + 1
)

func ourStateVersionString() string {
	return strconv.FormatInt(int64(ourStateVersion), 10)
}

func newerStateVersionString() string {
	return strconv.FormatInt(int64(newerStateVersion), 10)
}
