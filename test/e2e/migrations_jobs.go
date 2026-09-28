package e2e

import (
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// migrationJobIsolation is the
// credential boundary a migration Job has to keep, read from the Jobs the
// controller created.
//
// Resolve and Verify reach the registry from the container that runs Ptah,
// and that container is given no database URL. History and Apply hold the
// database URL, and the artifact reaches them as files a separate init
// container pair wrote: the process that runs SQL never holds a registry
// credential, never holds the registry CA, and is pointed at a local
// directory rather than at a reference it could fetch itself.
//
// The Jobs read have to cover the whole lifecycle, one of each of the four
// operations at least, and every one of them has to keep the boundary.
func migrationJobIsolation(jobs []batchv1.Job, databaseSecret, registrySecret, executorImage, runnerImage, serviceAccountName string) bool {
	if len(jobs) == 0 {
		return false
	}
	var operations []string
	for index := range jobs {
		// jq collected a missing label as null, and a null in the set is not
		// one of the four operations.
		operation, found := jobs[index].Labels[labelOperation]
		if !found {
			return false
		}
		operations = append(operations, operation)
	}
	slices.Sort(operations)
	if !slices.Equal(slices.Compact(operations), []string{"apply", "history", "resolve", "verify"}) {
		return false
	}
	boundary := migrationBoundary{
		databaseSecret: databaseSecret, registrySecret: registrySecret,
		executorImage: executorImage, runnerImage: runnerImage, serviceAccountName: serviceAccountName,
	}
	for index := range jobs {
		job := &jobs[index]
		if !boundary.safeJobContract(job) {
			return false
		}
		for _, container := range migrationJobContainers(job) {
			if !noEnvFrom(container) {
				return false
			}
		}
		switch job.Labels[labelOperation] {
		case "resolve", "verify":
			if !boundary.sourceOperation(job) {
				return false
			}
		default:
			if !boundary.databaseOperation(job) {
				return false
			}
		}
	}
	return true
}

// migrationBoundary is what the isolation reads besides the Jobs.
type migrationBoundary struct {
	databaseSecret, registrySecret, executorImage, runnerImage, serviceAccountName string
}

// migrationJobContainers is every container a Job's Pod declares: the main
// ones, the init ones and the ephemeral ones, in that order.
func migrationJobContainers(job *batchv1.Job) []containerView {
	spec := job.Spec.Template.Spec
	return slices.Concat(viewContainers(spec.Containers), viewContainers(spec.InitContainers),
		viewEphemeralContainers(spec.EphemeralContainers))
}

// noDatabase is a container that neither names the database URL nor reads the
// database Secret.
func (b migrationBoundary) noDatabase(c containerView) bool {
	return !c.hasEnvName("PTAH_DB_URL") && !c.hasEnvName("PTAH_DEV_URL") &&
		!slices.Contains(c.secretNames(), b.databaseSecret)
}

// noRegistry is a container with no registry variable, no read of the
// registry Secret, and none of the registry credential or CA mounts.
func (b migrationBoundary) noRegistry(c containerView) bool {
	return !c.envNameWith(registryEnvName) && !slices.Contains(c.secretNames(), b.registrySecret) &&
		len(c.mountsNamed("registry-docker-config")) == 0 && len(c.mountsNamed("registry-ca")) == 0 &&
		len(c.mountsNamed("registry-ca-snapshot")) == 0
}

// reachesRegistry is a container that reads the registry Secret and is told,
// by a literal alone, to speak plain HTTP to it.
func (b migrationBoundary) reachesRegistry(c containerView) bool {
	return slices.Contains(c.secretNames(), b.registrySecret) && exactLiteralEnv(c, "PTAH_PLAIN_HTTP", "true")
}

// holdsDatabase is a container with exactly one PTAH_DB_URL, with no literal
// value and a source that is the url key of the database Secret and nothing
// else.
func (b migrationBoundary) holdsDatabase(c containerView) bool {
	matches := c.envNamed("PTAH_DB_URL")
	if len(matches) != 1 || matches[0].Value != "" || matches[0].ValueFrom == nil ||
		!slices.Equal(renderedKeys(matches[0].ValueFrom), []string{"secretKeyRef"}) {
		return false
	}
	reference := matches[0].ValueFrom.SecretKeyRef
	return reference.Name == b.databaseSecret && reference.Key == "url"
}

// localMigrationsDir is a container pointed at a path the fetch container
// wrote, never a reference: Ptah would fetch a reference itself, from the
// process holding the database URL.
func localMigrationsDir(c containerView) bool {
	matches := c.envNamed("PTAH_MIGRATIONS_DIR")
	return len(matches) == 1 && matches[0].ValueFrom == nil && strings.HasPrefix(matches[0].Value, "/") &&
		!strings.Contains(matches[0].Value, "://")
}

// safeJobContract is the Job every migration operation runs as: one Pod that
// is never retried or replaced, no service account token, no service links,
// none of the host's namespaces, the service account given, the hardened Pod
// security context and no ephemeral container.
func (b migrationBoundary) safeJobContract(job *batchv1.Job) bool {
	spec := job.Spec.Template.Spec
	return int32PointerIs(job.Spec.BackoffLimit, 0) &&
		job.Spec.PodReplacementPolicy != nil && *job.Spec.PodReplacementPolicy == batchv1.Failed &&
		spec.RestartPolicy == corev1.RestartPolicyNever &&
		boolPointerIs(spec.AutomountServiceAccountToken, false) && boolPointerIs(spec.EnableServiceLinks, false) &&
		!spec.HostNetwork && !spec.HostPID && !spec.HostIPC && falseOrAbsent(spec.ShareProcessNamespace) &&
		spec.ServiceAccountName == b.serviceAccountName &&
		renderedEqual(spec.SecurityContext, hardenedSourcePod) &&
		len(spec.EphemeralContainers) == 0
}

// sourceOperation is a Resolve or Verify Job: the runner installed, then Ptah
// from the executor image, reaching the registry, and no container of the
// Pod near the database.
func (b migrationBoundary) sourceOperation(job *batchv1.Job) bool {
	spec := job.Spec.Template.Spec
	if !slices.Equal(containerNames(viewContainers(spec.Containers)), []string{"ptah"}) ||
		!slices.Equal(containerNames(viewContainers(spec.InitContainers)), []string{"install-runner"}) ||
		spec.Containers[0].Image != b.executorImage || spec.InitContainers[0].Image != b.runnerImage ||
		!b.reachesRegistry(viewContainers(spec.Containers)[0]) {
		return false
	}
	for _, container := range migrationJobContainers(job) {
		if !b.noDatabase(container) {
			return false
		}
	}
	return true
}

// databaseOperation is a History or Apply Job: the runner installed, the
// source authority validated, the migrations pulled by Ptah from the registry
// in a container that holds no database URL, and Ptah run against the
// database, from the pulled files, in a container that holds no registry
// credential and reads the files read-only.
func (b migrationBoundary) databaseOperation(job *batchv1.Job) bool {
	spec := job.Spec.Template.Spec
	if !slices.Equal(containerNames(viewContainers(spec.Containers)), []string{"ptah"}) ||
		!slices.Equal(containerNames(viewContainers(spec.InitContainers)),
			[]string{"install-runner", "validate-source-authority", "fetch-migrations"}) {
		return false
	}
	main, init := viewContainers(spec.Containers)[0], viewContainers(spec.InitContainers)
	fetch := &spec.InitContainers[2]
	args := fetch.Args
	if len(args) > 2 {
		args = args[:2]
	}
	sources := main.mountsNamed("schema-source")
	return spec.Containers[0].Image == b.executorImage && spec.InitContainers[0].Image == b.runnerImage &&
		fetch.Image == b.executorImage && slices.Equal(fetch.Command, []string{"/usr/local/bin/ptah"}) &&
		slices.Equal(args, []string{"migrations", "pull"}) &&
		b.holdsDatabase(main) && localMigrationsDir(main) && b.noRegistry(main) &&
		b.reachesRegistry(init[2]) && b.noDatabase(init[1]) && b.noDatabase(init[2]) &&
		len(sources) == 1 && sources[0].ReadOnly
}
