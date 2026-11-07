package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/types"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
	"github.com/stokaro/ptah-operator/test/e2e/gitfixture"
	"github.com/stokaro/ptah-operator/test/e2e/gitrefusal"
	"github.com/stokaro/ptah-operator/test/e2e/gitrepo"
)

// The Git source phase's fixtures. It runs on the data plane's PostgreSQL
// server, on databases no other phase touches, and serves its repository from
// the fixture image inside the test namespace.
const (
	gitSchemaDatabase    = "ptah_e2e_git_schema"
	gitMigrationDatabase = "ptah_e2e_git_migrations"
	gitSchema            = "e2e-git-schema"
	gitRefusedSchema     = "e2e-git-schema-refused"
	gitMigration         = "e2e-git-migration"
	gitSchemaSecret      = "e2e-git-schema-database"
	gitMigrationSecret   = "e2e-git-migration-database"

	// gitPinnedSchema reads one commit by its id, into a database of its own.
	gitPinnedSchema   = "e2e-git-pinned"
	gitPinnedDatabase = "ptah_e2e_git_pinned"
	gitPinnedSecret   = "e2e-git-pinned-database"
	// The sources Ptah refuses to read: a submodule, an LFS pointer, and a
	// server whose host key is not the one the known hosts name.
	gitSubmoduleSchema = "e2e-git-submodule"
	gitLFSSchema       = "e2e-git-lfs"
	gitHostKeySchema   = "e2e-git-host-key"
	// gitRefusedKey is the realm every refused schema claims, apart from the
	// schema in sync on the phase's database. Each is refused at its Resolve,
	// before anything reads a database.
	gitRefusedKey = "ptah_e2e_git_refused"

	// gitServer is the fixture Deployment and Service; gitServerSecret holds
	// its TLS key, token and SSH keys, and gitSpecConfigMap the repository.
	gitServer        = "e2e-git"
	gitServerSecret  = "e2e-git-server"
	gitSpecConfigMap = "e2e-git-spec"
	gitRepository    = gitfixture.Repository

	// The credential Secrets and trust ConfigMaps the resources select.
	gitTokenCredential     = "e2e-git-token"
	gitElsewhereCredential = "e2e-git-token-elsewhere"
	gitDeployKeyCredential = "e2e-git-deploy-key"
	gitCAConfigMap         = "e2e-git-ca"
	// gitOpenPolicy allows the server's host, and gitClosedPolicy another.
	gitOpenPolicy          = "e2e-git-source-policy"
	gitClosedPolicy        = "e2e-git-source-policy-closed"
	gitKnownHostsConfigMap = "e2e-git-known-hosts"
	// gitOtherHostsConfigMap pins a host key the server does not hold, and
	// gitOtherHostsCredential grants the deploy key under it.
	gitOtherHostsConfigMap  = "e2e-git-known-hosts-other"
	gitOtherHostsCredential = "e2e-git-deploy-key-other-hosts"

	gitSchemaPath    = gitfixture.SchemaPath
	gitMigrationPath = gitfixture.MigrationPath
	gitFetchInit     = "fetch-git-source"
	// gitInterval is long enough that no periodic refresh lands inside the
	// phase: every Resolve the phase sees is one it caused.
	gitInterval = "1h"
)

// What only the refused sources select: a submodule and a Git LFS pointer file.
const (
	gitSubmodulePath = gitfixture.SubmodulePath
	gitLFSPath       = gitfixture.LFSPath
	// What the refused sources' rows match in a condition message.
	gitSubmoduleRefusal = gitrefusal.Submodule + gitSubmodulePath
	gitLFSRefusal       = gitrefusal.LFSPointer + gitLFSPath
)

// The schema file and the migration directory Ptah reads and cannot load, each in
// a database of its own: the command connects before it reads the files.
const (
	gitUnreadableSchema   = "e2e-git-unreadable"
	gitUnreadableDatabase = "ptah_e2e_git_unreadable"
	gitUnreadableSecret   = "e2e-git-unreadable-database"
	gitUnreadablePath     = "db/unreadable/" + unreadableSchemaFile

	gitUnreadableMigration         = "e2e-git-unreadable-migration"
	gitUnreadableMigrationDatabase = "ptah_e2e_git_unreadable_migrations"
	gitUnreadableMigrationSecret   = "e2e-git-unreadable-migration-database"
	gitUnreadableMigrationPath     = "db/unreadable-migrations"
)

// The repository and the keys are shared with the runner harness, which serves
// the same fixture from a container.
var (
	gitSchemaRevisions  = gitfixture.SchemaRevisions
	gitRepositorySpec   = gitfixture.RepositorySpec
	gitRewrittenSpec    = gitfixture.RewrittenSpec
	gitSpecDocument     = gitfixture.SpecDocument
	gitSchemaContent    = gitfixture.SchemaContent
	gitMigrationContent = gitfixture.MigrationContent
	newGitServerKeys    = gitfixture.NewKeys
)

// gitServerKeys is everything the phase generates for its Git server.
type gitServerKeys = gitfixture.Keys

// gitUnreadableSpec is the repository after the force-push with one more commit
// on main, which adds a schema file Ptah cannot parse and a migration directory it
// cannot load. Only the phase's last
// scenario serves it: the Git captures name the commits before it, which this
// leaves as they were.
func gitUnreadableSpec() gitrepo.Spec {
	spec := gitRewrittenSpec()
	last := spec.Commits[len(spec.Commits)-1]
	files := maps.Clone(last.Files)
	files[gitUnreadablePath] = unreadableInheritsSchema
	for _, file := range unreadableMigrationFiles {
		files[gitUnreadableMigrationPath+"/"+file.path] = file.body
	}
	spec.Commits = append(spec.Commits, gitrepo.CommitSpec{Message: "Add files Ptah cannot read", Files: files, Submodules: last.Submodules})
	spec.Branches = map[string]int{"main": len(spec.Commits) - 1}
	return spec
}

// gitRepairedSpec is gitUnreadableSpec with one more commit, which repairs the
// schema file and leaves the migration directory alone, as an author pushes a
// fix to the branch a failing read is on.
func gitRepairedSpec() gitrepo.Spec {
	spec := gitUnreadableSpec()
	last := spec.Commits[len(spec.Commits)-1]
	files := maps.Clone(last.Files)
	files[gitUnreadablePath] = gitSchemaRevisions[0]
	spec.Commits = append(spec.Commits, gitrepo.CommitSpec{Message: "Repair the schema file", Files: files, Submodules: last.Submodules})
	spec.Branches = map[string]int{"main": len(spec.Commits) - 1}
	return spec
}

// gitResolvedTo holds a schema's Git status to the commit and content the
// phase expects, and to the repository, ref and path it asked for.
func gitResolvedTo(status *ptahv1alpha1.GitSourceRevision, url, path string, ref ptahv1alpha1.GitRef, commit, content string) error {
	switch {
	case status == nil:
		return errors.New("no Git resolution is recorded")
	case status.URL != url || status.Path != path || status.Ref != ref:
		return fmt.Errorf("the resolution answered %s %s %+v, not %s %s %+v", status.URL, status.Path, status.Ref, url, path, ref)
	case status.Commit != commit:
		return fmt.Errorf("the resolution read commit %s, not %s", status.Commit, commit)
	case status.ContentDigest != content:
		return fmt.Errorf("the resolution read content %s, not %s", status.ContentDigest, content)
	}
	return nil
}

// gitPlanBound holds a plan to the Git source it was computed from: contract
// version 2, the repository and path as the revision, and the content, and
// nothing of an OCI artifact or a verification policy.
func gitPlanBound(contract int32, artifactDigest, policyUID string, source *ptahv1alpha1.PlanSourceBinding, url, path, content string) error {
	switch {
	case contract != 2:
		return fmt.Errorf("contract version %d, want 2", contract)
	case artifactDigest != "" || policyUID != "":
		return errors.New("the plan binds an artifact or a verification policy")
	case source == nil || source.Kind != ptahv1alpha1.SourceKindGit:
		return fmt.Errorf("the plan binds the source %+v, not a Git one", source)
	case source.Revision != sourcebinding.GitRevision(url, path):
		return errors.New("the plan's revision is not its repository and path")
	case source.ContentDigest != content:
		return fmt.Errorf("the plan binds content %s, not %s", source.ContentDigest, content)
	}
	return nil
}

// gitFetchJob holds an operation Job that reads a Git source to its shape:
// the runner install and the Git fetch before the operation container, the
// fetch holding the credential and one of the commits the resource resolved
// to, and the operation container holding neither and mounting the fetched
// files read-only.
func gitFetchJob(job *batchv1.Job, commits ...string) error {
	spec := job.Spec.Template.Spec
	names := make([]string, 0, len(spec.InitContainers))
	for _, container := range spec.InitContainers {
		names = append(names, container.Name)
	}
	if !slices.Equal(names, []string{"install-runner", gitFetchInit}) {
		return fmt.Errorf("init containers %v, want the runner install and the Git fetch", names)
	}
	fetch := spec.InitContainers[1]
	committed, credential := false, false
	for _, variable := range fetch.Env {
		switch {
		case variable.Name == "PTAH_OPERATOR_GIT_COMMIT" && slices.Contains(commits, variable.Value):
			committed = true
		case variable.Name == "PTAH_OPERATOR_GIT_HOST_GRANT" && variable.ValueFrom != nil && variable.ValueFrom.SecretKeyRef != nil:
			credential = true
		}
	}
	if !committed || !credential {
		return fmt.Errorf("the fetch carries commit=%v and a credential grant=%v", committed, credential)
	}
	if len(spec.Containers) != 1 {
		return fmt.Errorf("%d operation containers", len(spec.Containers))
	}
	for _, variable := range spec.Containers[0].Env {
		if strings.HasPrefix(variable.Name, "PTAH_GIT_") || strings.HasPrefix(variable.Name, "PTAH_OPERATOR_GIT_") {
			return fmt.Errorf("the operation container carries %s", variable.Name)
		}
	}
	for _, mount := range spec.Containers[0].VolumeMounts {
		switch mount.Name {
		case "git-credentials", "git-ca", "git-known-hosts", "fetch-work":
			return fmt.Errorf("the operation container mounts %s", mount.Name)
		case "schema-source":
			if !mount.ReadOnly {
				return errors.New("the operation container can write the fetched files")
			}
		}
	}
	for _, volume := range spec.Volumes {
		if volume.Name == "source-snapshot" || volume.Name == "registry-docker-config" {
			return fmt.Errorf("the Job carries the volume %s", volume.Name)
		}
	}
	return nil
}

// gitResolveJob holds a Resolve Job of a Git source to its shape: no init
// container but the runner install, and the repository in its operation
// container, which holds no database credential.
func gitResolveJob(job *batchv1.Job) error {
	spec := job.Spec.Template.Spec
	if len(spec.InitContainers) != 1 || spec.InitContainers[0].Name != "install-runner" {
		return errors.New("a Resolve Job fetches nothing before its operation")
	}
	repository, database := false, false
	for _, variable := range spec.Containers[0].Env {
		repository = repository || variable.Name == "PTAH_OPERATOR_GIT_URL"
		database = database || variable.Name == "PTAH_DB_URL"
	}
	if !repository || database {
		return fmt.Errorf("the Resolve Job names the repository=%v and a database=%v", repository, database)
	}
	return nil
}

// jobsOutside names the Jobs whose identity before does not hold. A finished
// Job is deleted a few minutes after its result is read, so two readings of a
// resource's Jobs are compared by identity, never by count: a Job deleted
// between them is not one that was never made, and one deleted as another
// was made would hide it.
func jobsOutside(before map[types.UID]bool, jobs []batchv1.Job) []string {
	var made []string
	for index := range jobs {
		if !before[jobs[index].UID] {
			made = append(made, jobs[index].Name)
		}
	}
	return made
}

// grantDigest is the "sha256:" digest a credential Secret grants trust by.
func grantDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
