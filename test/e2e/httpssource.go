package e2e

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
	"github.com/stokaro/ptah-operator/test/e2e/httpsfixture"
)

// The HTTPS source phase's fixtures. It runs on the data plane's PostgreSQL
// server, on databases no other phase touches, and serves its downloads from
// the fixture image inside the test namespace, under two Service names: the
// second is the other origin a redirect may not carry a credential to.
const (
	httpsServer       = "e2e-https"
	httpsOtherServer  = "e2e-https-other"
	httpsServerSecret = "e2e-https-server"
	httpsFiles        = "e2e-https-files"
	httpsCAConfigMap  = "e2e-https-ca"
	httpsUser         = "deploy"

	// The schemas and the migration sequence that apply, each into a database
	// of its own.
	httpsSchema            = "e2e-https-schema"
	httpsSchemaDatabase    = "ptah_e2e_https_schema"
	httpsSchemaSecret      = "e2e-https-schema-database"
	httpsZipSchema         = "e2e-https-zip"
	httpsZipDatabase       = "ptah_e2e_https_zip"
	httpsZipSecret         = "e2e-https-zip-database"
	httpsMigration         = "e2e-https-migration"
	httpsMigrationDatabase = "ptah_e2e_https_migrations"
	httpsMigrationSecret   = "e2e-https-migration-database"

	// The sources refused before anything reads a database. They take one
	// realm of their own, one at a time, so none blocks a schema in sync.
	httpsRefusedKey        = "ptah_e2e_https_refused"
	httpsRedirectSchema    = "e2e-https-redirect"
	httpsElsewhereSchema   = "e2e-https-elsewhere"
	httpsSymlinkMigration  = "e2e-https-symlink"
	httpsDotDotMigration   = "e2e-https-dotdot"
	httpsHostPolicySchema  = "e2e-https-policy-host"
	httpsSizePolicySchema  = "e2e-https-policy-size"
	httpsHostPolicy        = "e2e-https-source-policy-host"
	httpsSizePolicy        = "e2e-https-source-policy-size"
	httpsPolicyMaxBytes    = 16
	httpsElsewhereHost     = "artifacts.example.com"
	httpsBasicCredential   = "e2e-https-basic"
	httpsBearerCredential  = "e2e-https-bearer"
	httpsElsewhereGrant    = "e2e-https-elsewhere"
	httpsFetchInit         = "fetch-https-source"
	httpsCredentialsVolume = "https-credentials"
	httpsCAVolume          = "https-ca"
	// httpsInterval is long enough that no periodic refresh lands inside the
	// phase: every Resolve the phase sees is one it caused.
	httpsInterval = "1h"

	// httpsRedirectControl is the redirect row's control: the same redirect
	// to the other origin with no credential set, which Ptah follows. It
	// takes the refused sources' realm, and is removed once it resolved.
	httpsRedirectControl = "e2e-https-redirect-public"
)

// What the refused sources' rows match in a condition message: Ptah's words
// for each refusal, as the pinned image printed them for the runner harness,
// and the runner's for a credential granted for another host.
const (
	httpsDownloadRefusal  = "download digest differs"
	httpsRedirectRefusal  = "redirect refused"
	httpsDotDotRefusal    = "archive entry name refused"
	httpsSymlinkRefusal   = "is a symbolic link"
	httpsElsewhereRefusal = "the HTTPS credential is not granted for the source's host"
)

// httpsCredentialVariables are the variables Ptah reads an HTTPS credential
// from. No container of the operator's Jobs carries one: the runner reads the
// credential from files into the environment of the pull it runs.
var httpsCredentialVariables = []string{"PTAH_HTTPS_USERNAME", "PTAH_HTTPS_PASSWORD", "PTAH_HTTPS_TOKEN"}

// httpsResolvedTo holds a resource's HTTPS status to the download the phase
// pinned and the content its selected files give.
func httpsResolvedTo(status *ptahv1alpha1.HTTPSSourceRevision, url string, format ptahv1alpha1.HTTPSFormat, path, digest, content string) error {
	switch {
	case status == nil:
		return errors.New("no HTTPS resolution is recorded")
	case status.URL != url || status.Format != format || status.Path != path:
		return fmt.Errorf("the resolution answered %s %s %s, not %s %s %s", status.URL, status.Format, status.Path, url, format, path)
	case status.Digest != digest:
		return fmt.Errorf("the resolution read the download %s, not %s", status.Digest, digest)
	case status.ContentDigest != content:
		return fmt.Errorf("the resolution read content %s, not %s", status.ContentDigest, content)
	}
	return nil
}

// httpsPlanBound holds a plan to the HTTPS source it was computed from:
// contract version 2, the URL and path as the revision, and the content, and
// nothing of an OCI artifact or a verification policy. The download digest is
// not bound, so a plan survives a republished archive whose selected files
// did not change.
func httpsPlanBound(contract int32, artifactDigest, policyUID string, source *ptahv1alpha1.PlanSourceBinding, url, path, content string) error {
	switch {
	case contract != 2:
		return fmt.Errorf("contract version %d, want 2", contract)
	case artifactDigest != "" || policyUID != "":
		return errors.New("the plan binds an artifact or a verification policy")
	case source == nil || source.Kind != ptahv1alpha1.SourceKindHTTPS:
		return fmt.Errorf("the plan binds the source %+v, not an HTTPS one", source)
	case source.Revision != sourcebinding.HTTPSRevision(url, path):
		return errors.New("the plan's revision is not its URL and path")
	case source.ContentDigest != content:
		return fmt.Errorf("the plan binds content %s, not %s", source.ContentDigest, content)
	}
	return nil
}

// downloadVerified holds ArtifactResolved to the reason an HTTPS resolution
// writes, with a message that names the download and the content digest.
func downloadVerified(conditions []metav1.Condition, download, content string) error {
	resolved := meta.FindStatusCondition(conditions, ptahv1alpha1.ConditionArtifactResolved)
	switch {
	case resolved == nil || resolved.Status != metav1.ConditionTrue:
		return fmt.Errorf("ArtifactResolved is %+v", resolved)
	case resolved.Reason != string(ptahv1alpha1.ReasonDownloadVerified):
		return fmt.Errorf("ArtifactResolved has the reason %s, want DownloadVerified", resolved.Reason)
	case !strings.Contains(resolved.Message, download) || !strings.Contains(resolved.Message, content):
		return fmt.Errorf("ArtifactResolved says %q, which does not name %s and %s", resolved.Message, download, content)
	}
	return nil
}

// downloadRefused holds a refusal message to Ptah's words for a download
// whose bytes are not the pinned ones, naming what the server served and the
// digest the spec pins.
func downloadRefused(message, served, pinned string) error {
	if !strings.Contains(message, httpsDownloadRefusal) || !strings.Contains(message, served) || !strings.Contains(message, pinned) {
		return fmt.Errorf("the refusal %q does not say %q naming %s and %s", message, httpsDownloadRefusal, served, pinned)
	}
	return nil
}

// httpsFetchJob holds an operation Job that reads an HTTPS source to its
// shape: the runner install and the HTTPS fetch before the operation
// container, the fetch holding one of the downloads the resource resolved to
// and, for a credentialed source, the credential grant and the credential
// files; no credential in any container's environment; and the operation
// container holding neither and mounting the fetched files read-only.
func httpsFetchJob(job *batchv1.Job, credentialed bool, downloads ...string) error {
	spec := job.Spec.Template.Spec
	names := make([]string, 0, len(spec.InitContainers))
	for _, container := range spec.InitContainers {
		names = append(names, container.Name)
	}
	if !slices.Equal(names, []string{"install-runner", httpsFetchInit}) {
		return fmt.Errorf("init containers %v, want the runner install and the HTTPS fetch", names)
	}
	fetch := spec.InitContainers[1]
	pinned, content, grant := false, false, false
	for _, variable := range fetch.Env {
		switch {
		case variable.Name == "PTAH_OPERATOR_HTTPS_DIGEST" && slices.Contains(downloads, variable.Value):
			pinned = true
		case variable.Name == "PTAH_OPERATOR_EXPECTED_CONTENT_DIGEST" && strings.HasPrefix(variable.Value, "sha256:"):
			content = true
		case variable.Name == "PTAH_OPERATOR_HTTPS_HOST_GRANT" && variable.ValueFrom != nil && variable.ValueFrom.SecretKeyRef != nil:
			grant = true
		}
	}
	files := slices.ContainsFunc(fetch.VolumeMounts, func(mount corev1.VolumeMount) bool {
		return mount.Name == httpsCredentialsVolume && mount.ReadOnly
	})
	if !pinned || !content || grant != credentialed || files != credentialed {
		return fmt.Errorf("the fetch carries a resolved download=%v, a content digest=%v, a grant=%v and the credential files=%v; credentialed=%v",
			pinned, content, grant, files, credentialed)
	}
	for _, container := range slices.Concat(spec.InitContainers, spec.Containers) {
		for _, variable := range container.Env {
			if slices.Contains(credentialVariables, variable.Name) || slices.Contains(httpsCredentialVariables, variable.Name) {
				return fmt.Errorf("container %s carries %s in its environment", container.Name, variable.Name)
			}
		}
	}
	if len(spec.Containers) != 1 {
		return fmt.Errorf("%d operation containers", len(spec.Containers))
	}
	for _, variable := range spec.Containers[0].Env {
		if strings.HasPrefix(variable.Name, "PTAH_HTTPS_") || strings.HasPrefix(variable.Name, "PTAH_OPERATOR_HTTPS_") {
			return fmt.Errorf("the operation container carries %s", variable.Name)
		}
	}
	source := false
	for _, mount := range spec.Containers[0].VolumeMounts {
		switch mount.Name {
		case httpsCredentialsVolume, httpsCAVolume, "fetch-work":
			return fmt.Errorf("the operation container mounts %s", mount.Name)
		case "schema-source":
			if !mount.ReadOnly {
				return errors.New("the operation container can write the fetched files")
			}
			source = true
		}
	}
	if !source {
		return errors.New("the operation container does not mount the fetched files")
	}
	for _, volume := range spec.Volumes {
		if volume.Name == "source-snapshot" || volume.Name == "registry-docker-config" {
			return fmt.Errorf("the Job carries the volume %s", volume.Name)
		}
	}
	return nil
}

// httpsResolveJob holds a Resolve Job of an HTTPS source to its shape: no
// init container but the runner install, and the download in its operation
// container, which holds no database credential.
func httpsResolveJob(job *batchv1.Job) error {
	spec := job.Spec.Template.Spec
	if len(spec.InitContainers) != 1 || spec.InitContainers[0].Name != "install-runner" {
		return errors.New("a Resolve Job fetches nothing before its operation")
	}
	if len(spec.Containers) != 1 {
		return fmt.Errorf("%d operation containers", len(spec.Containers))
	}
	download, database := false, false
	for _, variable := range spec.Containers[0].Env {
		download = download || variable.Name == "PTAH_OPERATOR_HTTPS_URL"
		database = database || slices.Contains([]string{
			"PTAH_DB_URL", "PTAH_DEV_URL", "PTAH_OPERATOR_DB_URL_FILE", "PTAH_OPERATOR_DEV_URL_FILE",
		}, variable.Name)
	}
	for _, mount := range spec.Containers[0].VolumeMounts {
		database = database || mount.Name == "database-url" || mount.Name == "development-database-url"
	}
	if !download || database {
		return fmt.Errorf("the Resolve Job names the download=%v and a database=%v", download, database)
	}
	return nil
}

// httpsServed is one request the download server logged.
type httpsServed struct {
	host, path, credential string
	status                 int
}

// httpsServerRequests reads the requests out of the download server's log.
// Each is logged as "<date> <time> <host> <path> <scheme> <status>", with no
// scheme for a request that carried no credential; every other line is
// something else the server said.
func httpsServerRequests(logs string) []httpsServed {
	var requests []httpsServed
	for line := range strings.SplitSeq(logs, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || len(fields) > 6 || !strings.HasPrefix(fields[3], "/") || strings.HasPrefix(fields[2], "/") {
			continue
		}
		status, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			continue
		}
		request := httpsServed{host: fields[2], path: fields[3], status: status}
		if len(fields) == 6 {
			request.credential = fields[4]
		}
		requests = append(requests, request)
	}
	return requests
}

// httpsServing reports whether the last digest the server logged for a file
// is digest: the server logs a file's digest when it starts and whenever the
// kubelet hands it new bytes.
func httpsServing(logs, file, digest string) bool {
	prefix := "file " + file + " is "
	last := ""
	for line := range strings.SplitSeq(logs, "\n") {
		if _, rest, ok := strings.Cut(line, prefix); ok {
			last = strings.TrimSpace(rest)
		}
	}
	return last == digest
}

// httpsRedirect is a path the server redirects to the other origin and the
// path it names there.
type httpsRedirect struct{ from, to string }

// httpsRedirectStoppedAtTheOrigin holds the server's requests to a redirect
// that Ptah refused to follow beside one it followed. The refused redirect
// was asked for with the credential and answered on host, and the other
// origin received neither a credential nor a request for the path it names.
// The control, the same redirect asked for with no credential, reached the
// other origin under otherHost: without it, an origin that was unreachable or
// logged under another name would read as one Ptah never contacted.
func httpsRedirectStoppedAtTheOrigin(requests []httpsServed, host, otherHost string, refused, control httpsRedirect) error {
	asked, redirected, followed := false, false, false
	for _, request := range requests {
		if request.host == otherHost && (request.credential != "" || request.path == refused.to) {
			return fmt.Errorf("the other origin received %q %s", request.credential, request.path)
		}
		switch {
		case request.host == host && request.path == refused.from && request.status == 302 && request.credential != "":
			asked = true
		case request.host == host && request.path == control.from && request.status == 302 && request.credential == "":
			redirected = true
		case request.host == otherHost && request.path == control.to && request.status == 200 && redirected:
			followed = true
		}
	}
	switch {
	case !asked:
		return fmt.Errorf("the server logged no credentialed request for %s that it redirected, in %d requests", refused.from, len(requests))
	case !followed:
		return fmt.Errorf("the server logged no redirect from %s on %s followed to %s on %s without a credential, in %d requests",
			control.from, host, control.to, otherHost, len(requests))
	}
	return nil
}

// httpsRedirectFollowed holds the server's requests to a same-origin redirect
// that was followed with the credential: the redirect from one path, and then
// the file at the other, both on host and both carrying the scheme.
func httpsRedirectFollowed(requests []httpsServed, host, from, to, scheme string) error {
	redirected := false
	for _, request := range requests {
		if request.host != host || request.credential != scheme {
			continue
		}
		switch {
		case request.path == from && request.status == 302:
			redirected = true
		case request.path == to && request.status == 200 && redirected:
			return nil
		}
	}
	return fmt.Errorf("the server logged no %s redirect from %s followed to %s on %s, in %d requests", scheme, from, to, host, len(requests))
}

// httpsNoRequestFor holds the server's requests to having none for path on
// host, and to having some: an empty reading cannot show that a path received
// nothing. The host is part of the URL a row names: the redirect row's control
// reads /public/ on the other origin, a URL no other row names.
func httpsNoRequestFor(requests []httpsServed, host, path string) error {
	if len(requests) == 0 {
		return errors.New("the server logged no request at all")
	}
	for _, request := range requests {
		if request.host == host && request.path == path {
			return fmt.Errorf("the server received %s %s from %s", request.credential, request.path, request.host)
		}
	}
	return nil
}

// httpsDownloads is every file the server serves, by the name it is served
// under, and the digest of each.
func httpsDownloads() (map[string][]byte, map[string]string, error) {
	files, err := httpsfixture.Files()
	if err != nil {
		return nil, nil, err
	}
	digests := make(map[string]string, len(files))
	for name, data := range files {
		digests[name] = httpsfixture.Digest(data)
	}
	return files, digests, nil
}
