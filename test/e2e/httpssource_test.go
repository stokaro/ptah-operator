package e2e

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/sourcebinding"
	"github.com/stokaro/ptah-operator/test/e2e/httpsfixture"
)

const (
	testHTTPSHost  = "e2e-https.ptah-test.svc"
	testHTTPSOther = "e2e-https-other.ptah-test.svc"
	testHTTPSURL   = "https://" + testHTTPSHost + "/basic/" + httpsfixture.SchemaFile
	testDownload   = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	// testHTTPSCaptures holds what the pinned Ptah image printed for this
	// phase's downloads; test/runnerharness captures it.
	testHTTPSCaptures = "../../internal/dataplane/testdata/https-pull"
)

func httpsCapture(t *testing.T, name, file string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(testHTTPSCaptures, name, file))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The rows that read Ptah's refusals in a condition message match what the
// pinned image printed for the same downloads, and the download refusal names
// both digests the way the republished row asks of it.
func TestTheHTTPSRefusalRowsMatchWhatPtahPrinted(t *testing.T) {
	t.Parallel()
	_, digests, err := httpsDownloads()
	if err != nil {
		t.Fatal(err)
	}
	for capture, want := range map[string]string{
		"refused-download-digest":       httpsDownloadRefusal,
		"refused-redirect-other-origin": httpsRedirectRefusal,
		"refused-dotdot-entry":          httpsDotDotRefusal,
		"refused-symlink":               httpsSymlinkRefusal,
	} {
		t.Run(capture, func(t *testing.T) {
			t.Parallel()
			if stderr := httpsCapture(t, capture, "stderr"); !strings.Contains(stderr, want) {
				t.Fatalf("the capture printed %q, and the row matches %q", stderr, want)
			}
		})
	}
	// The capture pinned the republished archive and was served the first, the
	// reverse of the row; the message names both either way.
	stderr := httpsCapture(t, "refused-download-digest", "stderr")
	if err := downloadRefused(stderr, digests[httpsfixture.SchemaArchive], digests[httpsfixture.SchemaArchiveRepublished]); err != nil {
		t.Fatal(err)
	}
	if downloadRefused(stderr, digests[httpsfixture.SchemaArchive], digests[httpsfixture.SchemaFile]) == nil {
		t.Fatal("downloadRefused accepted a message that does not name the pinned digest")
	}
}

// Every download the phase reads or is refused, by access and file, is one the
// runner harness captured: the rows send what was measured, not something
// next to it.
func TestThePhasesDownloadsWereCaptured(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(testHTTPSCaptures)
	if err != nil {
		t.Fatal(err)
	}
	captured := map[string]bool{}
	for _, entry := range entries {
		args := strings.Split(strings.TrimSpace(httpsCapture(t, entry.Name(), "args")), "\n")
		if len(args) < 3 {
			t.Fatalf("%s ran %q", entry.Name(), args)
		}
		parsed, err := url.Parse(args[2])
		if err != nil {
			t.Fatal(err)
		}
		captured[parsed.Path] = true
	}
	for _, access := range []string{
		"/same/basic/" + httpsfixture.SchemaFile,
		"/bearer/" + httpsfixture.MigrationArchive,
		"/public/" + httpsfixture.SchemaArchive,
		"/away/basic/" + httpsfixture.SchemaFile,
		"/bearer/" + httpsfixture.SymlinkArchive,
		"/bearer/" + httpsfixture.DotDotArchive,
	} {
		if !captured[access] {
			t.Errorf("the phase reads %s, which the runner harness did not capture", access)
		}
	}
}

func TestHTTPSResolvedToNeedsTheDownloadTheContentAndTheSource(t *testing.T) {
	t.Parallel()
	status := &ptahv1alpha1.HTTPSSourceRevision{
		URL: testHTTPSURL, Format: ptahv1alpha1.HTTPSFormatFile, Path: httpsfixture.SchemaFile, Digest: testDownload, ContentDigest: testContentDigest,
	}
	if err := httpsResolvedTo(status, testHTTPSURL, ptahv1alpha1.HTTPSFormatFile, httpsfixture.SchemaFile, testDownload, testContentDigest); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*ptahv1alpha1.HTTPSSourceRevision){
		"another download": func(s *ptahv1alpha1.HTTPSSourceRevision) { s.Digest = "sha256:" + strings.Repeat("3", 64) },
		"other content":    func(s *ptahv1alpha1.HTTPSSourceRevision) { s.ContentDigest = "sha256:" + strings.Repeat("4", 64) },
		"another format":   func(s *ptahv1alpha1.HTTPSSourceRevision) { s.Format = ptahv1alpha1.HTTPSFormatZip },
		"another path":     func(s *ptahv1alpha1.HTTPSSourceRevision) { s.Path = httpsfixture.SchemaInArchive },
		"another URL":      func(s *ptahv1alpha1.HTTPSSourceRevision) { s.URL = "https://" + testHTTPSOther + "/basic/schema.sql" },
	} {
		moved := *status
		edit(&moved)
		if httpsResolvedTo(&moved, testHTTPSURL, ptahv1alpha1.HTTPSFormatFile, httpsfixture.SchemaFile, testDownload, testContentDigest) == nil {
			t.Errorf("httpsResolvedTo accepted %s", name)
		}
	}
	if httpsResolvedTo(nil, testHTTPSURL, ptahv1alpha1.HTTPSFormatFile, httpsfixture.SchemaFile, testDownload, testContentDigest) == nil {
		t.Error("httpsResolvedTo accepted no resolution")
	}
}

func TestHTTPSPlanBoundNeedsTheURLPathAndContent(t *testing.T) {
	t.Parallel()
	source := &ptahv1alpha1.PlanSourceBinding{
		Kind: ptahv1alpha1.SourceKindHTTPS, Revision: sourcebinding.HTTPSRevision(testHTTPSURL, httpsfixture.SchemaFile), ContentDigest: testContentDigest,
	}
	if err := httpsPlanBound(2, "", "", source, testHTTPSURL, httpsfixture.SchemaFile, testContentDigest); err != nil {
		t.Fatal(err)
	}
	otherPath := *source
	otherPath.Revision = sourcebinding.HTTPSRevision(testHTTPSURL, httpsfixture.SchemaInArchive)
	git := *source
	git.Kind = ptahv1alpha1.SourceKindGit
	otherURL := *source
	otherURL.Revision = sourcebinding.HTTPSRevision("https://"+testHTTPSOther+"/basic/"+httpsfixture.SchemaFile, httpsfixture.SchemaFile)
	otherContent := *source
	otherContent.ContentDigest = testDownload
	for name, row := range map[string]struct {
		contract      int32
		artifact, uid string
		source        *ptahv1alpha1.PlanSourceBinding
	}{
		"contract version 1":    {1, "", "", source},
		"an artifact digest":    {2, testContentDigest, "", source},
		"a verification policy": {2, "", "policy-uid", source},
		"another path":          {2, "", "", &otherPath},
		"a Git source":          {2, "", "", &git},
		"another URL":           {2, "", "", &otherURL},
		"other content":         {2, "", "", &otherContent},
		"no source":             {2, "", "", nil},
	} {
		if httpsPlanBound(row.contract, row.artifact, row.uid, row.source, testHTTPSURL, httpsfixture.SchemaFile, testContentDigest) == nil {
			t.Errorf("httpsPlanBound accepted %s", name)
		}
	}
}

func TestDownloadVerifiedNeedsTheReasonAndBothDigests(t *testing.T) {
	t.Parallel()
	condition := func(status metav1.ConditionStatus, reason ptahv1alpha1.ConditionReason, message string) []metav1.Condition {
		return []metav1.Condition{{Type: ptahv1alpha1.ConditionArtifactResolved, Status: status, Reason: string(reason), Message: message}}
	}
	said := "The download " + testDownload + " matched its digest; the selected files give " + testContentDigest
	if err := downloadVerified(condition(metav1.ConditionTrue, ptahv1alpha1.ReasonDownloadVerified, said), testDownload, testContentDigest); err != nil {
		t.Fatal(err)
	}
	for name, conditions := range map[string][]metav1.Condition{
		"no condition":       nil,
		"not resolved":       condition(metav1.ConditionFalse, ptahv1alpha1.ReasonDownloadVerified, said),
		"a commit resolved":  condition(metav1.ConditionTrue, ptahv1alpha1.ReasonCommitResolved, said),
		"no download digest": condition(metav1.ConditionTrue, ptahv1alpha1.ReasonDownloadVerified, "the selected files give "+testContentDigest),
		"no content digest":  condition(metav1.ConditionTrue, ptahv1alpha1.ReasonDownloadVerified, "The download "+testDownload+" matched its digest"),
	} {
		if downloadVerified(conditions, testDownload, testContentDigest) == nil {
			t.Errorf("downloadVerified accepted %s", name)
		}
	}
}

func httpsJobFixture(initContainers []string, credentialed bool, mainEnv []corev1.EnvVar, mounts []corev1.VolumeMount) *batchv1.Job {
	containers := make([]corev1.Container, 0, len(initContainers))
	for _, name := range initContainers {
		container := corev1.Container{Name: name}
		if name == httpsFetchInit {
			container.Env = []corev1.EnvVar{
				{Name: "PTAH_OPERATOR_HTTPS_URL", Value: testHTTPSURL},
				{Name: "PTAH_OPERATOR_HTTPS_DIGEST", Value: testDownload},
				{Name: "PTAH_OPERATOR_EXPECTED_CONTENT_DIGEST", Value: testContentDigest},
			}
			if credentialed {
				container.Env = append(container.Env,
					corev1.EnvVar{Name: "PTAH_OPERATOR_HTTPS_HOST_GRANT", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: httpsBasicCredential}, Key: "host",
					}}},
					corev1.EnvVar{Name: "PTAH_OPERATOR_HTTPS_CA_SHA256_GRANT", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: httpsBasicCredential}, Key: "caSHA256",
					}}},
					corev1.EnvVar{Name: "PTAH_OPERATOR_HTTPS_PASSWORD_FILE", Value: "/credentials/https/password"},
				)
				container.VolumeMounts = []corev1.VolumeMount{{Name: httpsCredentialsVolume, MountPath: "/credentials/https", ReadOnly: true}}
			}
		}
		containers = append(containers, container)
	}
	return &batchv1.Job{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		InitContainers: containers,
		Containers:     []corev1.Container{{Name: "ptah", Env: mainEnv, VolumeMounts: mounts}},
	}}}}
}

func TestHTTPSFetchJobNeedsTheFetchToHoldTheCredential(t *testing.T) {
	t.Parallel()
	readOnly := []corev1.VolumeMount{{Name: "schema-source", MountPath: "/source", ReadOnly: true}}
	database := []corev1.EnvVar{{Name: "PTAH_OPERATOR_DB_URL_FILE", Value: "/credentials/database/url"}}
	fetch := []string{"install-runner", httpsFetchInit}
	for _, credentialed := range []bool{true, false} {
		if err := httpsFetchJob(httpsJobFixture(fetch, credentialed, database, readOnly), credentialed, testDownload); err != nil {
			t.Fatalf("credentialed=%v: %v", credentialed, err)
		}
	}
	for name, job := range map[string]*batchv1.Job{
		"no fetch":                 httpsJobFixture([]string{"install-runner"}, true, database, readOnly),
		"a Git fetch":              httpsJobFixture([]string{"install-runner", gitFetchInit}, true, database, readOnly),
		"a credential in the main": httpsJobFixture(fetch, true, append(database, corev1.EnvVar{Name: "PTAH_HTTPS_TOKEN"}), readOnly),
		"the download in the main": httpsJobFixture(fetch, true, append(database, corev1.EnvVar{Name: "PTAH_OPERATOR_HTTPS_URL", Value: testHTTPSURL}), readOnly),
		"a writable source":        httpsJobFixture(fetch, true, database, []corev1.VolumeMount{{Name: "schema-source", MountPath: "/source"}}),
		"no source":                httpsJobFixture(fetch, true, database, nil),
		"the trust in the main":    httpsJobFixture(fetch, true, database, append(readOnly, corev1.VolumeMount{Name: httpsCAVolume, ReadOnly: true})),
		"the credential files in the main": httpsJobFixture(fetch, true, database,
			append(readOnly, corev1.VolumeMount{Name: httpsCredentialsVolume, ReadOnly: true})),
		"the database URL in the main's environment": httpsJobFixture(fetch, true,
			[]corev1.EnvVar{{Name: "PTAH_DB_URL", Value: "postgres://from-a-secret"}}, readOnly),
		"no grant": func() *batchv1.Job {
			job := httpsJobFixture(fetch, true, database, readOnly)
			job.Spec.Template.Spec.InitContainers[1].Env = job.Spec.Template.Spec.InitContainers[1].Env[:3]
			return job
		}(),
		"no content digest": func() *batchv1.Job {
			job := httpsJobFixture(fetch, true, database, readOnly)
			job.Spec.Template.Spec.InitContainers[1].Env[2].Value = ""
			return job
		}(),
		"the token in the fetch's environment": func() *batchv1.Job {
			job := httpsJobFixture(fetch, true, database, readOnly)
			container := &job.Spec.Template.Spec.InitContainers[1]
			container.Env = append(container.Env, corev1.EnvVar{Name: "PTAH_HTTPS_TOKEN", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: httpsBearerCredential}, Key: "token"},
			}})
			return job
		}(),
	} {
		if httpsFetchJob(job, true, testDownload) == nil {
			t.Errorf("httpsFetchJob accepted %s", name)
		}
	}
	if httpsFetchJob(httpsJobFixture(fetch, true, database, readOnly), true, "sha256:"+strings.Repeat("5", 64)) == nil {
		t.Error("httpsFetchJob accepted a fetch of another download")
	}
	if httpsFetchJob(httpsJobFixture(fetch, false, database, readOnly), true, testDownload) == nil {
		t.Error("httpsFetchJob accepted a credentialed source's fetch without the credential")
	}
	if httpsFetchJob(httpsJobFixture(fetch, true, database, readOnly), false, testDownload) == nil {
		t.Error("httpsFetchJob accepted a public source's fetch with a credential")
	}
}

func TestHTTPSResolveJobNeedsTheDownloadAndNoDatabase(t *testing.T) {
	t.Parallel()
	download := []corev1.EnvVar{{Name: "PTAH_OPERATOR_HTTPS_URL", Value: testHTTPSURL}}
	if err := httpsResolveJob(httpsJobFixture([]string{"install-runner"}, true, download, nil)); err != nil {
		t.Fatal(err)
	}
	for name, job := range map[string]*batchv1.Job{
		"a fetch first":    httpsJobFixture([]string{"install-runner", httpsFetchInit}, true, download, nil),
		"no download":      httpsJobFixture([]string{"install-runner"}, true, nil, nil),
		"a Git repository": httpsJobFixture([]string{"install-runner"}, true, []corev1.EnvVar{{Name: "PTAH_OPERATOR_GIT_URL"}}, nil),
		"a database URL":   httpsJobFixture([]string{"install-runner"}, true, append(download, corev1.EnvVar{Name: "PTAH_DB_URL"}), nil),
		"a database URL file": httpsJobFixture([]string{"install-runner"}, true,
			append(download, corev1.EnvVar{Name: "PTAH_OPERATOR_DB_URL_FILE", Value: "/credentials/database/url"}), nil),
		"a database URL mount": httpsJobFixture([]string{"install-runner"}, true, download,
			[]corev1.VolumeMount{{Name: "database-url", MountPath: "/credentials/database", ReadOnly: true}}),
	} {
		if httpsResolveJob(job) == nil {
			t.Errorf("httpsResolveJob accepted %s", name)
		}
	}
}

// testHTTPSServerLog is what the download server prints, in the format
// test/e2e/httpsserver writes: its start, each file's digest, and one line
// per request, with no scheme for a request without a credential.
const testHTTPSServerLog = `2026/11/13 20:10:01 serving /files over https :8443
2026/11/13 20:10:01 file schema.zip is sha256:1111111111111111111111111111111111111111111111111111111111111111
2026/11/13 20:10:05 e2e-https.ptah-test.svc /same/basic/schema.sql Basic 302
2026/11/13 20:10:05 e2e-https.ptah-test.svc /basic/schema.sql Basic 200
2026/11/13 20:10:09 e2e-https.ptah-test.svc /public/schema.zip  200
2026/11/13 20:11:30 file schema.zip is sha256:2222222222222222222222222222222222222222222222222222222222222222
2026/11/13 20:12:00 e2e-https.ptah-test.svc /away/basic/schema.sql Basic 302
2026/11/13 20:12:20 e2e-https.ptah-test.svc /away/public/schema.sql  302
2026/11/13 20:12:20 e2e-https-other.ptah-test.svc /public/schema.sql  200
`

func TestHTTPSServerRequestsReadsEveryRequestAndNothingElse(t *testing.T) {
	t.Parallel()
	requests := httpsServerRequests(testHTTPSServerLog)
	want := []httpsServed{
		{testHTTPSHost, "/same/basic/schema.sql", "Basic", 302},
		{testHTTPSHost, "/basic/schema.sql", "Basic", 200},
		{testHTTPSHost, "/public/schema.zip", "", 200},
		{testHTTPSHost, "/away/basic/schema.sql", "Basic", 302},
		{testHTTPSHost, "/away/public/schema.sql", "", 302},
		{testHTTPSOther, "/public/schema.sql", "", 200},
	}
	if len(requests) != len(want) {
		t.Fatalf("read %+v, want %+v", requests, want)
	}
	for index := range want {
		if requests[index] != want[index] {
			t.Fatalf("request %d is %+v, want %+v", index, requests[index], want[index])
		}
	}
}

func TestHTTPSServingReadsTheLastDigestOfTheFile(t *testing.T) {
	t.Parallel()
	first, second := "sha256:"+strings.Repeat("1", 64), "sha256:"+strings.Repeat("2", 64)
	if !httpsServing(testHTTPSServerLog, "schema.zip", second) {
		t.Fatal("httpsServing refused the digest the server logged last")
	}
	for name, row := range map[string]struct{ file, digest string }{
		"the digest it replaced": {"schema.zip", first},
		"another file":           {"schema.sql", second},
		"a file's prefix":        {"schema", second},
	} {
		if httpsServing(testHTTPSServerLog, row.file, row.digest) {
			t.Errorf("httpsServing accepted %s", name)
		}
	}
}

func TestHTTPSRedirectStoppedAtTheOriginNeedsTheRedirectAndNothingElsewhere(t *testing.T) {
	t.Parallel()
	refused := httpsRedirect{from: "/away/basic/schema.sql", to: "/basic/schema.sql"}
	control := httpsRedirect{from: "/away/public/schema.sql", to: "/public/schema.sql"}
	requests := httpsServerRequests(testHTTPSServerLog)
	if err := httpsRedirectStoppedAtTheOrigin(requests, testHTTPSHost, testHTTPSOther, refused, control); err != nil {
		t.Fatal(err)
	}
	// with is the reading with its request at index replaced.
	with := func(index int, request httpsServed) []httpsServed {
		changed := slices.Clone(requests)
		changed[index] = request
		return changed
	}
	controlRedirect, controlFile := requests[4], requests[5]
	for name, reading := range map[string][]httpsServed{
		"the refused path followed with the credential": append(slices.Clone(requests), httpsServed{testHTTPSOther, refused.to, "Basic", 200}),
		// Go drops Authorization on a redirect to another host, so a
		// followed redirect can arrive with no credential at all.
		"the refused path followed without it": append(slices.Clone(requests), httpsServed{testHTTPSOther, refused.to, "", 401}),
		"a credential at the other origin":     with(5, httpsServed{testHTTPSOther, control.to, "Basic", 200}),
		"no credentialed redirect asked for":   slices.Delete(slices.Clone(requests), 3, 4),
		"the redirect asked for without the credential": httpsServerRequests(strings.ReplaceAll(testHTTPSServerLog,
			"/away/basic/schema.sql Basic 302", "/away/basic/schema.sql  302")),
		// The reading the row accepted before it had a control: an other
		// origin that never answered reads exactly like one Ptah stayed away from.
		"no control":                               requests[:4],
		"the control redirected, not followed":     requests[:5],
		"the control followed on the first origin": with(5, httpsServed{testHTTPSHost, control.to, "", 200}),
		"the control's file before its redirect":   append(slices.Clone(requests[:4]), controlFile, controlRedirect),
		"the control's file refused":               with(5, httpsServed{testHTTPSOther, control.to, "", 404}),
		"nothing read":                             nil,
	} {
		if httpsRedirectStoppedAtTheOrigin(reading, testHTTPSHost, testHTTPSOther, refused, control) == nil {
			t.Errorf("httpsRedirectStoppedAtTheOrigin accepted %s", name)
		}
	}
}

func TestHTTPSRedirectFollowedNeedsTheRedirectThenTheFile(t *testing.T) {
	t.Parallel()
	requests := httpsServerRequests(testHTTPSServerLog)
	if err := httpsRedirectFollowed(requests, testHTTPSHost, "/same/basic/schema.sql", "/basic/schema.sql", "Basic"); err != nil {
		t.Fatal(err)
	}
	for name, requests := range map[string][]httpsServed{
		"the file alone":      requests[1:2],
		"the redirect alone":  requests[:1],
		"the file first":      {requests[1], requests[0]},
		"another host":        {{testHTTPSOther, "/same/basic/schema.sql", "Basic", 302}, {testHTTPSOther, "/basic/schema.sql", "Basic", 200}},
		"without the scheme":  {{testHTTPSHost, "/same/basic/schema.sql", "", 302}, {testHTTPSHost, "/basic/schema.sql", "", 200}},
		"a refused file":      {requests[0], {testHTTPSHost, "/basic/schema.sql", "Basic", 401}},
		"nothing read at all": nil,
	} {
		if httpsRedirectFollowed(requests, testHTTPSHost, "/same/basic/schema.sql", "/basic/schema.sql", "Basic") == nil {
			t.Errorf("httpsRedirectFollowed accepted %s", name)
		}
	}
}

func TestHTTPSNoRequestForNeedsSomeRequestsAndNoneForThePath(t *testing.T) {
	t.Parallel()
	requests := httpsServerRequests(testHTTPSServerLog)
	if err := httpsNoRequestFor(requests, testHTTPSHost, "/bearer/schema.sql"); err != nil {
		t.Fatal(err)
	}
	// The log holds /public/schema.sql on the other origin alone.
	if err := httpsNoRequestFor(requests, testHTTPSHost, "/public/schema.sql"); err != nil {
		t.Fatalf("httpsNoRequestFor refused a path only another host received: %v", err)
	}
	if httpsNoRequestFor(requests, testHTTPSHost, "/basic/schema.sql") == nil {
		t.Error("httpsNoRequestFor accepted a path the server received")
	}
	if httpsNoRequestFor(requests, testHTTPSOther, "/public/schema.sql") == nil {
		t.Error("httpsNoRequestFor accepted a path the other origin received")
	}
	if httpsNoRequestFor(nil, testHTTPSHost, "/bearer/schema.sql") == nil {
		t.Error("httpsNoRequestFor accepted a log with no request")
	}
}

// builtJob is a Job the operation Job builder wrote: the golden files
// internal/workload holds every builder change to, so a filter read against
// them reads what the operator dispatches rather than what a test assumed.
func builtJob(t *testing.T, name string) *batchv1.Job {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "internal", "workload", "testdata", "jobs", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	job := &batchv1.Job{}
	if err := json.Unmarshal(body, job); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return job
}

// The builder's fixtures pin these downloads: httpsFileSpec and
// httpsArchiveSpec in internal/workload.
var (
	builtFileDownload    = "sha256:" + strings.Repeat("d", 64)
	builtArchiveDownload = "sha256:" + strings.Repeat("e", 64)
)

func TestHTTPSJobFiltersAcceptWhatTheBuilderWrites(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, download string
		credentialed   bool
	}{
		{"schema-plan-https-file", builtFileDownload, true},
		{"migration-apply-https-archive", builtArchiveDownload, false},
		{"migration-apply-https-archive-token", builtArchiveDownload, true},
	} {
		if err := httpsFetchJob(builtJob(t, row.name), row.credentialed, row.download); err != nil {
			t.Errorf("httpsFetchJob refused the builder's %s: %v", row.name, err)
		}
	}
	for _, name := range []string{"schema-resolve-https-file", "migration-resolve-https-archive"} {
		if err := httpsResolveJob(builtJob(t, name)); err != nil {
			t.Errorf("httpsResolveJob refused the builder's %s: %v", name, err)
		}
	}
}

func TestHTTPSFetchJobRefusesTheBuilderJobChanged(t *testing.T) {
	t.Parallel()
	// changed is the builder's credentialed Plan Job with change applied to
	// its fetch container and its operation container.
	changed := func(change func(fetch, main *corev1.Container, spec *corev1.PodSpec)) *batchv1.Job {
		job := builtJob(t, "schema-plan-https-file")
		spec := &job.Spec.Template.Spec
		change(&spec.InitContainers[1], &spec.Containers[0], spec)
		return job
	}
	without := func(env []corev1.EnvVar, name string) []corev1.EnvVar {
		return slices.DeleteFunc(slices.Clone(env), func(variable corev1.EnvVar) bool { return variable.Name == name })
	}
	for name, job := range map[string]*batchv1.Job{
		"no host grant": changed(func(fetch, _ *corev1.Container, _ *corev1.PodSpec) {
			fetch.Env = without(fetch.Env, "PTAH_OPERATOR_HTTPS_HOST_GRANT")
		}),
		"no CA grant": changed(func(fetch, _ *corev1.Container, _ *corev1.PodSpec) {
			fetch.Env = without(fetch.Env, "PTAH_OPERATOR_HTTPS_CA_SHA256_GRANT")
		}),
		"a CA grant from another Secret": changed(func(fetch, _ *corev1.Container, _ *corev1.PodSpec) {
			for index := range fetch.Env {
				if fetch.Env[index].Name == "PTAH_OPERATOR_HTTPS_CA_SHA256_GRANT" {
					fetch.Env[index].ValueFrom.SecretKeyRef.Name = "another-owner"
				}
			}
		}),
		"a CA grant read from another key": changed(func(fetch, _ *corev1.Container, _ *corev1.PodSpec) {
			for index := range fetch.Env {
				if fetch.Env[index].Name == "PTAH_OPERATOR_HTTPS_CA_SHA256_GRANT" {
					fetch.Env[index].ValueFrom.SecretKeyRef.Key = "host"
				}
			}
		}),
		"a CA grant written as a value": changed(func(fetch, _ *corev1.Container, _ *corev1.PodSpec) {
			fetch.Env = append(without(fetch.Env, "PTAH_OPERATOR_HTTPS_CA_SHA256_GRANT"),
				corev1.EnvVar{Name: "PTAH_OPERATOR_HTTPS_CA_SHA256_GRANT", Value: "sha256:" + strings.Repeat("c", 64)})
		}),
		"no content digest": changed(func(fetch, _ *corev1.Container, _ *corev1.PodSpec) {
			fetch.Env = without(fetch.Env, "PTAH_OPERATOR_EXPECTED_CONTENT_DIGEST")
		}),
		"the credential files unmounted": changed(func(fetch, _ *corev1.Container, _ *corev1.PodSpec) {
			fetch.VolumeMounts = slices.DeleteFunc(fetch.VolumeMounts, func(mount corev1.VolumeMount) bool { return mount.Name == httpsCredentialsVolume })
		}),
		"the token in the fetch's environment": changed(func(fetch, _ *corev1.Container, _ *corev1.PodSpec) {
			fetch.Env = append(fetch.Env, corev1.EnvVar{Name: "PTAH_HTTPS_TOKEN", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "files-token"}, Key: "token"},
			}})
		}),
		"the fetch renamed": changed(func(fetch, _ *corev1.Container, _ *corev1.PodSpec) { fetch.Name = gitFetchInit }),
		"the CA in the operation container": changed(func(_, main *corev1.Container, _ *corev1.PodSpec) {
			main.VolumeMounts = append(main.VolumeMounts, corev1.VolumeMount{Name: httpsCAVolume, MountPath: "/credentials/https-ca", ReadOnly: true})
		}),
		"a grant in the operation container": changed(func(fetch, main *corev1.Container, _ *corev1.PodSpec) {
			for _, variable := range fetch.Env {
				if variable.Name == "PTAH_OPERATOR_HTTPS_HOST_GRANT" {
					main.Env = append(main.Env, variable)
				}
			}
		}),
		"the fetched files writable": changed(func(_, main *corev1.Container, _ *corev1.PodSpec) {
			for index := range main.VolumeMounts {
				if main.VolumeMounts[index].Name == "schema-source" {
					main.VolumeMounts[index].ReadOnly = false
				}
			}
		}),
		"a second operation container": changed(func(_, main *corev1.Container, spec *corev1.PodSpec) {
			spec.Containers = append(spec.Containers, *main.DeepCopy())
		}),
	} {
		if httpsFetchJob(job, true, builtFileDownload) == nil {
			t.Errorf("httpsFetchJob accepted the builder's Plan Job with %s", name)
		}
	}
	plan := builtJob(t, "schema-plan-https-file")
	for name, call := range map[string]func() error{
		"another download":                    func() error { return httpsFetchJob(plan, true, builtArchiveDownload) },
		"a credentialed fetch read as public": func() error { return httpsFetchJob(plan, false, builtFileDownload) },
		"a public fetch read as credentialed": func() error {
			return httpsFetchJob(builtJob(t, "migration-apply-https-archive"), true, builtArchiveDownload)
		},
		"a Resolve Job": func() error { return httpsFetchJob(builtJob(t, "schema-resolve-https-file"), true, builtFileDownload) },
	} {
		if call() == nil {
			t.Errorf("httpsFetchJob accepted %s", name)
		}
	}
}

func TestHTTPSResolveJobRefusesTheBuilderJobChanged(t *testing.T) {
	t.Parallel()
	for name, job := range map[string]*batchv1.Job{
		"a Plan Job": builtJob(t, "schema-plan-https-file"),
		"a database URL file": func() *batchv1.Job {
			job := builtJob(t, "schema-resolve-https-file")
			main := &job.Spec.Template.Spec.Containers[0]
			main.Env = append(main.Env, corev1.EnvVar{Name: "PTAH_OPERATOR_DB_URL_FILE", Value: "/credentials/database/url"})
			main.VolumeMounts = append(main.VolumeMounts, corev1.VolumeMount{Name: "database-url", MountPath: "/credentials/database", ReadOnly: true})
			return job
		}(),
		"no download": func() *batchv1.Job {
			job := builtJob(t, "migration-resolve-https-archive")
			main := &job.Spec.Template.Spec.Containers[0]
			main.Env = slices.DeleteFunc(main.Env, func(variable corev1.EnvVar) bool { return variable.Name == "PTAH_OPERATOR_HTTPS_URL" })
			return job
		}(),
	} {
		if httpsResolveJob(job) == nil {
			t.Errorf("httpsResolveJob accepted the builder's Job as %s", name)
		}
	}
}
