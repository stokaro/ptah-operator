package runnerharness

import (
	"errors"
	"fmt"
	"slices"

	"github.com/stokaro/ptah-operator/internal/dataplane"
)

// DecodeDocument reads what a child printed with the decoder the runner uses for
// that command, so a release that moves a contract fails here as it would in a
// Pod. A command that prints no document under this runner's arguments, the OCI
// fetch and a refused Git pull, has nothing to read.
func DecodeDocument(args []string, exit int, stdout []byte) error {
	command := CommandWords(args)
	switch command {
	case "oci resolve":
		_, err := dataplane.DecodeResolve(stdout)
		return err
	case "oci verify":
		// A verification Ptah could not run stops before its report, and says
		// why on standard error. A verdict always has a report.
		if exit != 0 && len(stdout) == 0 {
			return nil
		}
		_, err := dataplane.DecodeVerify(stdout)
		return err
	case "oci inspect":
		_, err := dataplane.DecodeInspect(stdout)
		return err
	case "schema drift":
		if exit != 0 && exit != 1 {
			_, err := dataplane.DecodeDriftFailure(stdout)
			return err
		}
		_, err := dataplane.DecodeDrift(stdout, exit)
		return err
	case "schema plan":
		_, err := dataplane.DecodeSchemaPlan(stdout)
		return err
	case "schema apply", "schema apply --dry-run":
		_, err := dataplane.DecodeSchemaApply(stdout)
		return err
	case "migrations status":
		if exit != 0 {
			_, err := dataplane.DecodeMigrationStatusFailure(stdout)
			return err
		}
		_, err := dataplane.DecodeMigrationStatus(stdout)
		return err
	case "migrations up":
		_, err := dataplane.DecodeMigrationRun(stdout)
		return err
	case "schema validate":
		// Text for a person, which SchemaSourceProblem reads for one shape. A
		// release that changes it is a change of shape, not a refusal.
		return nil
	case "schema pull", "migrations pull":
		if !slices.Contains(args, "--json") {
			return nil
		}
		if exit != 0 {
			if len(stdout) != 0 {
				return errors.New("a refused pull printed a document")
			}
			return nil
		}
		reference, selected, err := pullAsked(args)
		if err != nil {
			return err
		}
		_, err = dataplane.DecodeGitPull(stdout, reference, selected)
		return err
	default:
		return fmt.Errorf("no decoder for `ptah %s`", command)
	}
}

// pullAsked reads the reference and the path a pull's arguments name.
func pullAsked(args []string) (reference, selected string, err error) {
	flag := slices.Index(args, "--path")
	if len(args) < 3 || flag < 0 || flag+1 >= len(args) {
		return "", "", fmt.Errorf("%q is not a pull of a path", args)
	}
	return args[2], args[flag+1], nil
}
