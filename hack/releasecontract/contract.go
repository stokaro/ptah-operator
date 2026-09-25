// Package releasecontract holds what the release workflow is pinned to.
//
// Two programs audit that workflow -- hack/releaseverify reads its steps and
// hack/verify-kubernetes-support.go reads its support-evidence bindings -- and
// both refuse a file whose digest is not the audited one. The digest lived in
// each of them, so changing the workflow meant remembering to update two
// constants, and forgetting the second turned a deliberate change into a red
// build nobody could read. It is declared once, here.
package releasecontract

// WorkflowSHA256 is the digest of .github/workflows/release.yml as audited.
//
// Changing the release workflow means changing this line in the same commit.
// That is the point: the digest is how a reviewer sees that the release
// contract moved, rather than only the diff of the file that moved it.
const WorkflowSHA256 = "81b660ef440b25a0c8bc5c13f52e64c51d0d44126d45268ed2343e82c50950b2"
