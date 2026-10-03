//go:build e2e

package e2e

import (
	"os"
	"testing"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// TestMain runs the one phase -e2e.phase names, under that phase's bound, and
// fails a run in which the phase's test did not reach its end.
func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.UseDevMode(false), zap.WriteTo(os.Stderr)))
	os.Exit(harness.Main(m))
}
