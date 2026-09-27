// Package admissionpolicy_test installs the chart's ValidatingAdmissionPolicies
// and their bindings into a real kube-apiserver, in the state a completed
// install leaves them, and holds each policy to what it exists for: the
// requests it refuses, the writes it lets through, and the mutations of its
// match or binding that would let it fail open.
package admissionpolicy_test

import (
	"context"
	"os"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/stokaro/ptah-operator/test/envtest/internal/harness"
	"github.com/stokaro/ptah-operator/test/envtest/internal/policyenv"
)

var (
	plane = harness.New(&envtest.Environment{
		CRDDirectoryPaths: []string{harness.CRDDirectory()},
	})
	env *policyenv.Env
)

func TestMain(m *testing.M) {
	os.Exit(plane.Main(m, setup))
}

// setup installs the release and the tenant the rows write into. The API
// server compiles the policies asynchronously; TestAdmissionPolicies waits for
// that before it judges.
func setup() error {
	ctx := context.Background()
	chart, err := policyenv.Render(ctx, harness.DefaultRelease())
	if err != nil {
		return err
	}
	env, err = policyenv.Install(ctx, plane, chart)
	if err != nil {
		return err
	}
	return setupTenant(ctx)
}
