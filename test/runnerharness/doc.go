// Package runnerharness runs the runner against the Ptah image support/ptah.json
// pins, without a cluster, and holds what Ptah printed to the committed captures.
//
// The runner's contract with Ptah is the arguments it builds, the documents it
// decodes, the exit statuses it reads and the standard error it reads or
// ignores. The harness builds each operation's Job through internal/workload,
// runs that Job's containers in Docker in the shape the Job declares, with the
// runner from this commit calling a recording runner.Executor inside the
// executor image, against PostgreSQL, MySQL, an OCI registry and the end-to-end
// Git server. Every document is decoded with internal/dataplane, every result is
// framed and parsed back with the manager's parser, and everything is compared
// with the captures under internal/dataplane/testdata. A write mode regenerates
// those captures, so a repin is one command and a reviewed diff:
//
//	DOCKER_CONTEXT=<context> make runner-harness        # compare
//	DOCKER_CONTEXT=<context> make runner-harness-write  # regenerate
//
// CompareCaptures and Masker say what counts as a change of shape and which
// values are volatile. Declared is the set of operations and commands the run is
// counted against. Translate is the stand-in for the kubelet.
//
// It does not replace kind. Admission, RBAC, the Job and Pod lifecycle,
// NetworkPolicy and durable result delivery are proven there.
package runnerharness
