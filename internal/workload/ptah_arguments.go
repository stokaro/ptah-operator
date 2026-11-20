package workload

import (
	"regexp"
	"strings"

	"golang.org/x/mod/semver"
)

// ptahArgument is an argument of a Ptah command that a Ptah release accepts
// from since on. A Job carries it only when the Ptah version it is built for
// has it, so the Job depends on the version alone: a Job rebuilt under a
// binding a claim recorded is the Job a manager configured with that binding
// built, whatever this manager is configured with now. That is what lets a
// rollout that moved the Ptah version adopt an Apply its predecessor
// dispatched, and it keeps a manager configured with an earlier Ptah from
// passing an argument its executor would refuse.
//
// Every argument whose presence depends on the Ptah version is declared here
// and nowhere else.
type ptahArgument struct {
	argument string
	since    string
}

// migrationPullArguments are the version-dependent arguments of the
// migrations pull an OCI History or Apply fetches its artifact with.
//
// --skip-validation (stokaro/ptah#6406, Ptah v0.34.0) leaves the directory
// unread as migrations in the fetch, so a file Ptah cannot load, or one the
// format does not read, is found by the operation container, which holds no
// registry credential and can name it, rather than by the fetch, whose words
// never reach status. The fetch still checks the manifest, the artifact type
// and format, the layer types, sizes, digests and names, and that the
// artifact holds a SQL file. Without it, the fetch reads the directory as
// migrations, as every Ptah before v0.34.0 does.
var migrationPullArguments = []ptahArgument{
	{argument: "--skip-validation", since: "v0.34.0"},
}

// ptahArguments returns the declared arguments that a Ptah of the given
// version accepts, in their declared order.
func ptahArguments(version string, declared []ptahArgument) []string {
	var arguments []string
	for _, each := range declared {
		if ptahHas(version, each.since) {
			arguments = append(arguments, each.argument)
		}
	}
	return arguments
}

// describedCommit is the suffix git describe gives a commit after a tag,
// v0.34.0-3-gabcdef1 for the third commit after v0.34.0, and the one it adds
// for a tree with changes, v0.34.0-dirty or v0.34.0-3-gabcdef1-dirty.
var describedCommit = regexp.MustCompile(`^(-[0-9]+-g[0-9a-f]{4,40})?(-dirty)?$`)

// ptahHas reports whether Ptah version has what release since introduced,
// by semantic version precedence. A version without the leading v, 0.34.0,
// reads as v0.34.0.
//
// A git describe version names a tree built from the tag it starts with or
// from a commit after it, so it has everything that tag has: v0.34.0-3-gabcdef1
// has what v0.34.0 has. Any other pre-release precedes the release it names,
// as semantic versioning orders it, so v0.34.0-rc.1 does not count as v0.34.0.
// Build metadata is ignored.
//
// A version that is not a semantic version cannot be ordered. The manager is
// installed with the Ptah version verified for its executor image, and the
// build this release verifies has every argument declared here, so such a
// version is taken to have them all, the arguments this build passed before
// they depended on the version. A recorded binding that names one rebuilds
// with them, so a Job a release that did not pass them dispatched under it
// differs from its rebuild and settles as outcome unknown, rather than being
// adopted as a Job it is not.
func ptahHas(version, since string) bool {
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	if !semver.IsValid(version) {
		return true
	}
	canonical := semver.Canonical(version)
	if prerelease := semver.Prerelease(canonical); prerelease != "" && describedCommit.MatchString(prerelease) {
		canonical = strings.TrimSuffix(canonical, prerelease)
	}
	return semver.Compare(canonical, since) >= 0
}
