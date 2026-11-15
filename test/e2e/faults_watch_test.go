package e2e

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const watchTestNamespace = "e2e-watch"

func watchTestJob(name, resourceVersion string) *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: watchTestNamespace, Name: name, UID: types.UID("uid-" + name), ResourceVersion: resourceVersion,
	}}
}

// scriptedAPIServer answers a recorder's watches with segments, in order, and
// its lists with what at says the collection held at the revision asked for.
// It serves no list but an exact one of the watched namespace, so a list the
// recorder asks for any other way fails the recorder.
type scriptedAPIServer struct {
	segments [][]watch.Event
	at       func(revision uint64) ([]*batchv1.Job, string, error)
	recorder *watchRecorder[*batchv1.Job]

	mu      sync.Mutex
	watched []string
	listed  []string
}

func (s *scriptedAPIServer) client() client.WithWatch {
	return interceptor.NewClient(fake.NewClientBuilder().Build(), interceptor.Funcs{
		Watch: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, opts ...client.ListOption) (watch.Interface, error) {
			options := (&client.ListOptions{}).ApplyOptions(opts)
			s.mu.Lock()
			defer s.mu.Unlock()
			s.watched = append(s.watched, options.Raw.ResourceVersion)
			var events []watch.Event
			if len(s.segments) == 0 {
				s.recorder.requestStop()
			} else {
				events, s.segments = s.segments[0], s.segments[1:]
			}
			stream := watch.NewFakeWithChanSize(len(events), false)
			for _, event := range events {
				stream.Action(event.Type, event.Object)
			}
			stream.Stop()
			return stream, nil
		},
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			options := (&client.ListOptions{}).ApplyOptions(opts)
			if options.Namespace != watchTestNamespace || options.Raw == nil ||
				options.Raw.ResourceVersionMatch != metav1.ResourceVersionMatchExact {
				return fmt.Errorf("the scripted API server serves only exact lists of %s, and was asked for %+v", watchTestNamespace, options)
			}
			s.mu.Lock()
			s.listed = append(s.listed, options.Raw.ResourceVersion)
			s.mu.Unlock()
			revision, err := strconv.ParseUint(options.Raw.ResourceVersion, 10, 64)
			if err != nil {
				return err
			}
			items, answered, err := s.at(revision)
			if err != nil {
				return err
			}
			jobs := list.(*batchv1.JobList)
			for _, job := range items {
				jobs.Items = append(jobs.Items, *job)
			}
			jobs.ResourceVersion = answered
			return nil
		},
	})
}

// run records the script to its end, and is what the recorder failed with.
func (s *scriptedAPIServer) run(t *testing.T, initial *batchv1.JobList) error {
	t.Helper()
	r, err := newWatchRecorder[*batchv1.Job]("jobs", watchTestNamespace, false, s.client(),
		func() client.ObjectList { return &batchv1.JobList{} }, initial)
	if err != nil {
		t.Fatalf("start the recorder: %v", err)
	}
	s.recorder = r
	go r.run(initial.ResourceVersion)
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		r.abort()
		t.Fatalf("the recorder did not end with its script; it watched from %v and listed %v", s.watched, s.listed)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failure
}

func revisionsBetween(from, to uint64) []string {
	var revisions []string
	for revision := from + 1; revision <= to; revision++ {
		revisions = append(revisions, strconv.FormatUint(revision, 10))
	}
	return revisions
}

// TestWatchRecorderBridgesOnlyAGapItsListsProveEmpty runs a watch history, a
// quiet stretch that ends in a bookmark at resourceVersion 20, and then the
// refusal an API server whose watch cache restarted gives a watch from there.
// The recorder may resume where the API server's history starts only when the
// collection listed at every revision in between holds exactly what the
// history recorded, and must otherwise fail naming the revision and the
// object.
func TestWatchRecorderBridgesOnlyAGapItsListsProveEmpty(t *testing.T) {
	t.Parallel()
	initial := &batchv1.JobList{
		ListMeta: metav1.ListMeta{ResourceVersion: "10"},
		Items:    []batchv1.Job{*watchTestJob("a", "5"), *watchTestJob("b", "8"), *watchTestJob("c", "9")},
	}
	history := []watch.Event{
		{Type: watch.Modified, Object: watchTestJob("b", "12")},
		{Type: watch.Deleted, Object: watchTestJob("c", "14")},
		{Type: watch.Added, Object: watchTestJob("d", "15")},
		{Type: watch.Bookmark, Object: &batchv1.Job{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "20"}}},
	}
	// recorded is what the history holds from resourceVersion 15 on: a never
	// changed since the list the recorder started from, b changed, c deleted
	// and d added.
	recorded := func() []*batchv1.Job {
		return []*batchv1.Job{watchTestJob("a", "5"), watchTestJob("b", "12"), watchTestJob("d", "15")}
	}
	expired := func(message string) *metav1.Status {
		return &metav1.Status{Status: metav1.StatusFailure, Code: http.StatusGone, Reason: metav1.StatusReasonExpired, Message: message}
	}
	window := func(start uint64) *metav1.Status {
		return expired(fmt.Sprintf("too old resource version: 20 (%d)", start))
	}
	for _, test := range []struct {
		name    string
		refusal *metav1.Status
		// at changes what the collection holds at a revision; nil holds
		// what the history recorded, answered at that revision.
		at func(revision uint64, jobs []*batchv1.Job) ([]*batchv1.Job, string, error)
		// resumes is where the recorder must resume; zero means it must fail
		// with every phrase in want.
		resumes uint64
		want    []string
		listed  []string
	}{
		{
			name: "a gap every list holds unchanged", refusal: window(25), resumes: 25, listed: revisionsBetween(20, 25),
		},
		{
			name: "a gap as wide as the limit", refusal: window(20 + watchGapRevisionLimit),
			resumes: 20 + watchGapRevisionLimit, listed: revisionsBetween(20, 20+watchGapRevisionLimit),
		},
		{
			name: "an object changed inside the gap", refusal: window(25),
			at: func(revision uint64, jobs []*batchv1.Job) ([]*batchv1.Job, string, error) {
				if revision >= 23 {
					jobs[1] = watchTestJob("b", "23")
				}
				return jobs, strconv.FormatUint(revision, 10), nil
			},
			want:   []string{"at resourceVersion 23 the collection holds", "e2e-watch/b (uid uid-b) is at resourceVersion 23, recorded at 12"},
			listed: revisionsBetween(20, 23),
		},
		{
			name: "an object deleted inside the gap", refusal: window(25),
			at: func(revision uint64, jobs []*batchv1.Job) ([]*batchv1.Job, string, error) {
				if revision >= 22 {
					jobs = jobs[:2]
				}
				return jobs, strconv.FormatUint(revision, 10), nil
			},
			want:   []string{"at resourceVersion 22 the collection holds", "e2e-watch/d (uid uid-d), recorded at resourceVersion 15, is gone"},
			listed: revisionsBetween(20, 22),
		},
		{
			name: "an object created inside the gap", refusal: window(25),
			at: func(revision uint64, jobs []*batchv1.Job) ([]*batchv1.Job, string, error) {
				if revision >= 24 {
					jobs = append(jobs, watchTestJob("e", "24"))
				}
				return jobs, strconv.FormatUint(revision, 10), nil
			},
			want:   []string{"at resourceVersion 24 the collection holds", "e2e-watch/e (uid uid-e) at resourceVersion 24 was never recorded"},
			listed: revisionsBetween(20, 24),
		},
		{
			// A list at the end of the gap alone holds what the history
			// recorded.
			name: "an object created and deleted inside the gap", refusal: window(25),
			at: func(revision uint64, jobs []*batchv1.Job) ([]*batchv1.Job, string, error) {
				if revision == 22 {
					jobs = append(jobs, watchTestJob("e", "22"))
				}
				return jobs, strconv.FormatUint(revision, 10), nil
			},
			want:   []string{"at resourceVersion 22 the collection holds", "e2e-watch/e (uid uid-e) at resourceVersion 22 was never recorded"},
			listed: revisionsBetween(20, 22),
		},
		{
			name: "a list answered at another revision", refusal: window(25),
			at: func(_ uint64, jobs []*batchv1.Job) ([]*batchv1.Job, string, error) {
				return jobs, "25", nil
			},
			want:   []string{`the list asked for at resourceVersion 21 answered at "25"`},
			listed: revisionsBetween(20, 21),
		},
		{
			name: "history compacted inside the gap", refusal: window(25),
			at: func(uint64, []*batchv1.Job) ([]*batchv1.Job, string, error) {
				return nil, "", apierrors.NewResourceExpired("The resourceVersion for the provided list is too old.")
			},
			want: []string{
				"the jobs watch cannot resume at resourceVersion 20, since the API server's history starts after 25",
				"could not be listed at resourceVersion 21: The resourceVersion for the provided list is too old.",
			},
			listed: revisionsBetween(20, 21),
		},
		{
			name: "a list the API server failed", refusal: window(25),
			at: func(revision uint64, jobs []*batchv1.Job) ([]*batchv1.Job, string, error) {
				if revision == 23 {
					return nil, "", apierrors.NewInternalError(errors.New("etcdserver: request timed out"))
				}
				return jobs, strconv.FormatUint(revision, 10), nil
			},
			want:   []string{"could not be listed at resourceVersion 23", "etcdserver: request timed out"},
			listed: revisionsBetween(20, 23),
		},
		{
			name: "a gap wider than the limit", refusal: window(21 + watchGapRevisionLimit),
			want: []string{fmt.Sprintf("the %d revisions in between are more than the %d", watchGapRevisionLimit+1, watchGapRevisionLimit)},
		},
		{
			name: "a window that starts before the resume point", refusal: window(18),
			want: []string{"since the API server's history starts after 18, which leaves no gap to bridge"},
		},
		{
			name: "a refusal that names no window", refusal: expired("The resourceVersion for the provided watch is too old."),
			want: []string{"the jobs watch segment emitted an error event: The resourceVersion for the provided watch is too old."},
		},
		{
			name: "a window for another resourceVersion", refusal: expired("too old resource version: 19 (25)"),
			want: []string{"the jobs watch segment emitted an error event: too old resource version: 19 (25)"},
		},
		{
			name: "the window's words in a refusal of another reason",
			refusal: &metav1.Status{
				Status: metav1.StatusFailure, Code: http.StatusInternalServerError, Reason: metav1.StatusReasonInternalError,
				Message: "too old resource version: 20 (25)",
			},
			want: []string{"the jobs watch segment emitted an error event: too old resource version: 20 (25)"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := &scriptedAPIServer{
				segments: [][]watch.Event{
					history,
					{{Type: watch.Error, Object: test.refusal}},
					{{Type: watch.Modified, Object: watchTestJob("a", "30")}},
				},
				at: func(revision uint64) ([]*batchv1.Job, string, error) {
					if test.at == nil {
						return recorded(), strconv.FormatUint(revision, 10), nil
					}
					return test.at(revision, recorded())
				},
			}
			failure := server.run(t, initial)
			if !slices.Equal(server.listed, test.listed) {
				t.Errorf("listed the collection at %v, want %v", server.listed, test.listed)
			}
			gaps := server.recorder.gaps()
			if test.resumes == 0 {
				if failure == nil {
					t.Fatalf("the recorder resumed from %v and recorded the gaps %+v; want a failure naming %q", server.watched, gaps, test.want)
				}
				for _, phrase := range test.want {
					if !strings.Contains(failure.Error(), phrase) {
						t.Errorf("the failure %q does not say %q", failure, phrase)
					}
				}
				if want := []string{"10", "20"}; !slices.Equal(server.watched, want) {
					t.Errorf("watched from %v, want %v and nothing after the refusal", server.watched, want)
				}
				if len(gaps) != 0 {
					t.Errorf("recorded the gaps %+v after a refused gap", gaps)
				}
				return
			}
			if failure != nil {
				t.Fatalf("the recorder failed: %v", failure)
			}
			resumes := strconv.FormatUint(test.resumes, 10)
			if want := []string{"10", "20", resumes, "30"}; !slices.Equal(server.watched, want) {
				t.Errorf("watched from %v, want %v", server.watched, want)
			}
			if want := []watchGap{{From: "20", To: resumes, Revisions: int(test.resumes - 20), Objects: 3}}; !slices.Equal(gaps, want) {
				t.Errorf("recorded the gaps %+v, want %+v", gaps, want)
			}
			events := server.recorder.snapshot()
			if len(events) != len(history)+1 || events[len(events)-1].Object.GetResourceVersion() != "30" {
				t.Errorf("recorded %d events, the last at resourceVersion %s; want the %d before the gap and the change at 30 after it",
					len(events), events[len(events)-1].Object.GetResourceVersion(), len(history))
			}
		})
	}
}

// watchRecorderConstructions names, as file:function, every place source
// builds a watchRecorder value: a composite literal, new, or a declared
// variable of the type itself. A pointer to one is not a construction.
func watchRecorderConstructions(filename string, source []byte) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), filename, source, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	isRecorder := func(expr ast.Expr) bool {
		switch typed := expr.(type) {
		case *ast.IndexExpr:
			expr = typed.X
		case *ast.IndexListExpr:
			expr = typed.X
		}
		ident, ok := expr.(*ast.Ident)
		return ok && ident.Name == "watchRecorder"
	}
	var sites []string
	scan := func(node ast.Node, function string) {
		ast.Inspect(node, func(node ast.Node) bool {
			built := false
			switch typed := node.(type) {
			case *ast.CompositeLit:
				built = isRecorder(typed.Type)
			case *ast.CallExpr:
				ident, ok := typed.Fun.(*ast.Ident)
				built = ok && ident.Name == "new" && len(typed.Args) == 1 && isRecorder(typed.Args[0])
			case *ast.ValueSpec:
				built = typed.Type != nil && isRecorder(typed.Type)
			}
			if built {
				sites = append(sites, filename+":"+function)
			}
			return true
		})
	}
	for _, declaration := range file.Decls {
		function := ""
		if decl, ok := declaration.(*ast.FuncDecl); ok {
			function = decl.Name.Name
		}
		scan(declaration, function)
	}
	return sites, nil
}

// TestWatchRecorderIsBuiltOnlyByItsConstructor holds every file of the
// package, whatever its build tag, to building a watchRecorder through
// newWatchRecorder. A recorder built any other way has no list to start its
// history from, so the first event it records panics the test binary, and
// the files that would do it compile only under the e2e tag.
func TestWatchRecorderIsBuiltOnlyByItsConstructor(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var sites []string
	read := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		source, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		found, err := watchRecorderConstructions(entry.Name(), source)
		if err != nil {
			t.Fatal(err)
		}
		sites = append(sites, found...)
		read++
	}
	if want := []string{"faults_watch.go:newWatchRecorder"}; !slices.Equal(sites, want) {
		t.Fatalf("watchRecorder is built at %v across %d files; only %v may build it", sites, read, want)
	}

	// The scan refuses what it exists to refuse, in a file the e2e tag
	// hides from this build, and leaves a pointer alone.
	for _, row := range []struct {
		source string
		sites  int
	}{
		{"//go:build e2e\n\npackage e2e\nfunc f[T any]() { _ = &watchRecorder[T]{name: \"jobs\", quiet: true} }\n", 1},
		{"package e2e\nfunc f[T any]() { _ = new(watchRecorder[T]) }\n", 1},
		{"package e2e\nfunc f[T any]() { var r watchRecorder[T]; _ = r }\n", 1},
		{"package e2e\nvar r = watchRecorder[int]{}\n", 1},
		{"package e2e\nfunc f[T any](r *watchRecorder[T]) { var s *watchRecorder[T] = r; _ = s }\n", 0},
	} {
		found, err := watchRecorderConstructions("sample.go", []byte(row.source))
		if err != nil {
			t.Fatal(err)
		}
		if len(found) != row.sites {
			t.Errorf("the scan found %v in:\n%s\nwant %d sites", found, row.source, row.sites)
		}
	}
}
