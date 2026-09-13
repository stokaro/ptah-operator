package releasecontract_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stokaro/ptah-operator/hack/releasecontract"
)

// The digest is the release workflow's, and it is one declaration.
//
// It used to be a constant in each of the two programs that audit that
// workflow. Changing the workflow then meant changing both, and changing one
// left the other refusing the file for a reason that reads like a tampering
// alarm rather than a forgotten line.
func TestWorkflowDigestIsTheReleaseWorkflowsOwn(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", ".github", "workflows", "release.yml")
	document, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the release workflow: %v", err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(document))
	if digest != releasecontract.WorkflowSHA256 {
		t.Fatalf("the release workflow digest is %s and the audited contract says %s;\n"+
			"if the change to the workflow is intended, update hack/releasecontract",
			digest, releasecontract.WorkflowSHA256)
	}
}
