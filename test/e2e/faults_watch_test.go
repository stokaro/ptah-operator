package e2e

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
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

// watchTestJob is the Job name as the revision writtenAt wrote it, at
// resourceVersion. The label survives a delete, which keeps the content of
// the object deleted and moves only its resourceVersion.
func watchTestJob(name, resourceVersion string) *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: watchTestNamespace, Name: name, UID: types.UID("uid-" + name), ResourceVersion: resourceVersion,
		Labels: map[string]string{"written-at": resourceVersion},
	}}
}

// scriptedAPIServer answers a recorder's watches with segments, in order, and
// its lists with what at says the collection held at the revision asked for.
// It serves no list but an exact one of the watched namespace, so a list the
// recorder asks for any other way fails the recorder.
type scriptedAPIServer struct {
	segments [][]watch.Event
	// refusalAfter is how long the second watch request, the one the script
	// refuses, takes to be answered.
	refusalAfter time.Duration
	at           func(revision uint64) ([]*batchv1.Job, string, error)
	// listTakes is how long the list of a revision takes to be answered,
	// unless the request's context ends first.
	listTakes func(revision uint64) time.Duration
	recorder  *watchRecorder[*batchv1.Job]

	mu      sync.Mutex
	watched []string
	listed  []string
}

func (s *scriptedAPIServer) client() client.WithWatch {
	return interceptor.NewClient(fake.NewClientBuilder().Build(), interceptor.Funcs{
		Watch: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, opts ...client.ListOption) (watch.Interface, error) {
			options := (&client.ListOptions{}).ApplyOptions(opts)
			s.mu.Lock()
			s.watched = append(s.watched, options.Raw.ResourceVersion)
			second := len(s.watched) == 2
			var events []watch.Event
			if len(s.segments) == 0 {
				s.recorder.requestStop()
			} else {
				events, s.segments = s.segments[0], s.segments[1:]
			}
			s.mu.Unlock()
			if second {
				time.Sleep(s.refusalAfter)
			}
			stream := watch.NewFakeWithChanSize(len(events), false)
			for _, event := range events {
				stream.Action(event.Type, event.Object)
			}
			stream.Stop()
			return stream, nil
		},
		List: func(ctx context.Context, _ client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
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
			if s.listTakes != nil {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(s.listTakes(revision)):
				}
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

// run records the script to its end inside the synctest bubble it is called
// in, and is how long that took and what the recorder failed with. ack, when
// given, is a write the heartbeat had acknowledged as the script started.
func (s *scriptedAPIServer) run(t *testing.T, initial *batchv1.JobList, ack *watchAck) (time.Duration, error) {
	t.Helper()
	started := time.Now()
	r := newWatchRecorder[*batchv1.Job]("jobs", watchTestNamespace, false, s.client(),
		func() client.ObjectList { return &batchv1.JobList{} })
	if ack != nil {
		owed := *ack
		owed.At = started
		r.follow(&heartbeat{acks: map[string][]watchAck{"jobs": {owed}}})
	}
	s.recorder = r
	go r.run(initial)
	select {
	case <-r.done:
	case <-time.After(time.Hour):
		r.abort()
		t.Fatalf("the recorder did not end with its script; it watched from %v and listed %v", s.watched, s.listed)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Since(started), r.failure
}

func revisionsFrom(from, to uint64) []string {
	var revisions []string
	for revision := from; revision <= to; revision++ {
		revisions = append(revisions, strconv.FormatUint(revision, 10))
	}
	return revisions
}

// replayedEvent is an event as a row expects it: its type, the object's name
// and resourceVersion, and the revision that last wrote the object's content.
type replayedEvent struct {
	Type                             watch.EventType
	Name, ResourceVersion, WrittenAt string
}

func replayedEvents(events []watchEvent[*batchv1.Job]) []replayedEvent {
	described := make([]replayedEvent, 0, len(events))
	for _, event := range events {
		described = append(described, replayedEvent{
			Type: event.Type, Name: event.Object.GetName(), ResourceVersion: event.Object.GetResourceVersion(),
			WrittenAt: event.Object.GetLabels()["written-at"],
		})
	}
	return described
}

// TestWatchRecorderReplaysTheGapItsListsRead runs a watch history, a stretch
// that ends in a bookmark at resourceVersion 20, and then the refusal an API
// server whose watch cache no longer holds resourceVersion 20 gives a watch
// from there. The recorder lists the collection exactly at 20 and at every
// revision after it up to where the API server's history starts, records the
// one change each revision made as the event the watch would have delivered,
// and resumes there; a list that is not the history it continues, or not one
// write's change, fails it naming the revision and the object.
func TestWatchRecorderReplaysTheGapItsListsRead(t *testing.T) {
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
	written := func(revision uint64) string { return strconv.FormatUint(revision, 10) }
	for _, test := range []struct {
		name    string
		refusal *metav1.Status
		// at changes what the collection holds at a revision; nil holds
		// what the history recorded, answered at that revision.
		at        func(revision uint64, jobs []*batchv1.Job) ([]*batchv1.Job, string, error)
		listTakes func(revision uint64) time.Duration
		// ack is a heartbeat write acknowledged as the script started, and
		// owed once the refused watch request was answered.
		ack *watchAck
		// resumes is where the recorder must resume, having replayed
		// replayed; zero means it must fail with every phrase in want.
		resumes  uint64
		replayed []replayedEvent
		want     []string
		withheld bool
		// took, when set, is how long the recorder must have run to fail.
		took   time.Duration
		listed []string
	}{
		{
			name: "a gap every list holds unchanged", refusal: window(25), resumes: 25, listed: revisionsFrom(20, 25),
		},
		{
			name: "a gap as wide as the limit", refusal: window(20 + watchGapRevisionLimit),
			resumes: 20 + watchGapRevisionLimit, listed: revisionsFrom(20, 20+watchGapRevisionLimit),
		},
		{
			name: "an object changed inside the gap", refusal: window(25),
			at: func(revision uint64, jobs []*batchv1.Job) ([]*batchv1.Job, string, error) {
				if revision >= 23 {
					jobs[1] = watchTestJob("b", "23")
				}
				return jobs, written(revision), nil
			},
			resumes: 25, replayed: []replayedEvent{{watch.Modified, "b", "23", "23"}}, listed: revisionsFrom(20, 25),
		},
		{
			name: "an object deleted inside the gap", refusal: window(25),
			at: func(revision uint64, jobs []*batchv1.Job) ([]*batchv1.Job, string, error) {
				if revision >= 22 {
					jobs = jobs[:2]
				}
				return jobs, written(revision), nil
			},
			resumes: 25, replayed: []replayedEvent{{watch.Deleted, "d", "22", "15"}}, listed: revisionsFrom(20, 25),
		},
		{
			name: "an object created inside the gap", refusal: window(25),
			at: func(revision uint64, jobs []*batchv1.Job) ([]*batchv1.Job, string, error) {
				if revision >= 24 {
					jobs = append(jobs, watchTestJob("e", "24"))
				}
				return jobs, written(revision), nil
			},
			resumes: 25, replayed: []replayedEvent{{watch.Added, "e", "24", "24"}}, listed: revisionsFrom(20, 25),
		},
		{
			// A list at the end of the gap alone holds what the history
			// recorded.
			name: "an object created and deleted inside the gap", refusal: window(25),
			at: func(revision uint64, jobs []*batchv1.Job) ([]*batchv1.Job, string, error) {
				if revision == 22 {
					jobs = append(jobs, watchTestJob("e", "22"))
				}
				return jobs, written(revision), nil
			},
			resumes: 25, replayed: []replayedEvent{{watch.Added, "e", "22", "22"}, {watch.Deleted, "e", "23", "22"}},
			listed: revisionsFrom(20, 25),
		},
		{
			name: "a heartbeat write inside the gap answers its acknowledgment", refusal: window(25),
			at: func(revision uint64, jobs []*batchv1.Job) ([]*batchv1.Job, string, error) {
				if revision >= 23 {
					jobs[1] = watchTestJob("b", "23")
				}
				return jobs, written(revision), nil
			},
			ack:     &watchAck{UID: "uid-b", ResourceVersion: "23"},
			resumes: 25, replayed: []replayedEvent{{watch.Modified, "b", "23", "23"}}, listed: revisionsFrom(20, 25),
		},
		{
			// The replay is held to the heartbeat where it ends, as a
			// segment is, and not one segment later.
			name: "a heartbeat write the gap does not hold", refusal: window(25),
			ack: &watchAck{UID: "uid-b", ResourceVersion: "23"},
			want: []string{
				"has not delivered the heartbeat write it acknowledged 31s ago at resourceVersion 23",
				"left the watch at resourceVersion 25",
			},
			withheld: true, listed: revisionsFrom(20, 25),
		},
		{
			name: "two changes in one revision", refusal: window(25),
			at: func(revision uint64, jobs []*batchv1.Job) ([]*batchv1.Job, string, error) {
				if revision >= 23 {
					jobs[1] = watchTestJob("b", "23")
					jobs = append(jobs, watchTestJob("e", "23"))
				}
				return jobs, written(revision), nil
			},
			want: []string{
				"at resourceVersion 23 the collection changed 2 objects, which one revision cannot write",
				"ADDED e2e-watch/e (uid uid-e) at resourceVersion 23; MODIFIED e2e-watch/b (uid uid-b) at resourceVersion 23",
			},
			listed: revisionsFrom(20, 23),
		},
		{
			name: "an object changed at another revision than the list's", refusal: window(25),
			at: func(revision uint64, jobs []*batchv1.Job) ([]*batchv1.Job, string, error) {
				if revision >= 23 {
					jobs[1] = watchTestJob("b", "22")
				}
				return jobs, written(revision), nil
			},
			want: []string{
				"at resourceVersion 23 the collection holds e2e-watch/b (uid uid-b) at resourceVersion 22, modified at another revision than the one that wrote it",
			},
			listed: revisionsFrom(20, 23),
		},
		{
			name: "an object created at another revision than the list's", refusal: window(25),
			at: func(revision uint64, jobs []*batchv1.Job) ([]*batchv1.Job, string, error) {
				if revision >= 24 {
					jobs = append(jobs, watchTestJob("e", "21"))
				}
				return jobs, written(revision), nil
			},
			want: []string{
				"at resourceVersion 24 the collection holds e2e-watch/e (uid uid-e) at resourceVersion 21, added at another revision than the one that wrote it",
			},
			listed: revisionsFrom(20, 24),
		},
		{
			name: "a list where the history stopped that the history does not hold", refusal: window(25),
			at: func(revision uint64, jobs []*batchv1.Job) ([]*batchv1.Job, string, error) {
				return append(jobs, watchTestJob("c", "9")), written(revision), nil
			},
			want: []string{
				"at resourceVersion 20, where the history stopped, the collection is not what the history holds: e2e-watch/c (uid uid-c) at resourceVersion 9 was never recorded",
			},
			listed: revisionsFrom(20, 20),
		},
		{
			name: "a list answered at another revision", refusal: window(25),
			at: func(_ uint64, jobs []*batchv1.Job) ([]*batchv1.Job, string, error) {
				return jobs, "25", nil
			},
			want:   []string{`the list asked for at resourceVersion 20 answered at "25"`},
			listed: revisionsFrom(20, 20),
		},
		{
			name: "history compacted inside the gap", refusal: window(25),
			at: func(uint64, []*batchv1.Job) ([]*batchv1.Job, string, error) {
				return nil, "", apierrors.NewResourceExpired("The resourceVersion for the provided list is too old.")
			},
			want: []string{
				"the jobs watch cannot resume at resourceVersion 20, since the API server's history starts after 25",
				"could not be listed at resourceVersion 20: The resourceVersion for the provided list is too old.",
			},
			listed: revisionsFrom(20, 20),
		},
		{
			name: "a list the API server failed", refusal: window(25),
			at: func(revision uint64, jobs []*batchv1.Job) ([]*batchv1.Job, string, error) {
				if revision == 23 {
					return nil, "", apierrors.NewInternalError(errors.New("etcdserver: request timed out"))
				}
				return jobs, written(revision), nil
			},
			want:   []string{"could not be listed at resourceVersion 23", "etcdserver: request timed out"},
			listed: revisionsFrom(20, 23),
		},
		{
			name: "a list that is never answered", refusal: window(25),
			listTakes: func(revision uint64) time.Duration {
				if revision == 22 {
					return 24 * time.Hour
				}
				return 0
			},
			want: []string{"could not be listed at resourceVersion 22: context deadline exceeded"},
			took: watchGapListTimeout, listed: revisionsFrom(20, 22),
		},
		{
			name: "a replay that outlasts its bound", refusal: window(25),
			listTakes: func(uint64) time.Duration { return watchGapListTimeout - time.Second },
			want:      []string{"could not be listed at resourceVersion 23: context deadline exceeded"},
			took:      watchGapReplayBound, listed: revisionsFrom(20, 23),
		},
		{
			name: "a gap wider than the limit", refusal: window(21 + watchGapRevisionLimit),
			want: []string{fmt.Sprintf("the %d revisions in between are more than the %d", watchGapRevisionLimit+1, watchGapRevisionLimit)},
		},
		{
			name: "a window that starts before the resume point", refusal: window(18),
			want: []string{"since the API server's history starts after 18, which leaves no gap to replay"},
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
			synctest.Test(t, func(t *testing.T) {
				after := uint64(30)
				if test.resumes != 0 {
					after = test.resumes + 5
				}
				server := &scriptedAPIServer{
					segments: [][]watch.Event{
						history,
						{{Type: watch.Error, Object: test.refusal}},
						{{Type: watch.Modified, Object: watchTestJob("a", written(after))}},
					},
					at: func(revision uint64) ([]*batchv1.Job, string, error) {
						if test.at == nil {
							return recorded(), written(revision), nil
						}
						return test.at(revision, recorded())
					},
					listTakes: test.listTakes,
				}
				if test.ack != nil {
					server.refusalAfter = watchDeliveryBound + time.Second
				}
				took, failure := server.run(t, initial, test.ack)
				if !slices.Equal(server.listed, test.listed) {
					t.Errorf("listed the collection at %v, want %v", server.listed, test.listed)
				}
				gaps := server.recorder.gaps()
				events := server.recorder.snapshot()
				if test.resumes == 0 {
					if failure == nil {
						t.Fatalf("the recorder resumed from %v and replayed the gaps %+v; want a failure naming %q", server.watched, gaps, test.want)
					}
					for _, phrase := range test.want {
						if !strings.Contains(failure.Error(), phrase) {
							t.Errorf("the failure %q does not say %q", failure, phrase)
						}
					}
					var withheld *withheldWriteError
					if errors.As(failure, &withheld) != test.withheld {
						t.Errorf("the failure %q is a withheld write: %t, want %t", failure, !test.withheld, test.withheld)
					}
					if test.took != 0 && took != test.took {
						t.Errorf("the recorder failed after %s, want %s", took, test.took)
					}
					if want := []string{"10", "20"}; !slices.Equal(server.watched, want) {
						t.Errorf("watched from %v, want %v and nothing after the refusal", server.watched, want)
					}
					if test.withheld {
						return
					}
					if len(gaps) != 0 || len(events) != len(history) {
						t.Errorf("recorded the gaps %+v and %d events after a refused gap, want none and the %d before it", gaps, len(events), len(history))
					}
					return
				}
				if failure != nil {
					t.Fatalf("the recorder failed: %v", failure)
				}
				resumes := written(test.resumes)
				if want := []string{"10", "20", resumes, written(after)}; !slices.Equal(server.watched, want) {
					t.Errorf("watched from %v, want %v", server.watched, want)
				}
				if want := []watchGap{{From: "20", To: resumes, Revisions: int(test.resumes - 20), Events: len(test.replayed)}}; !slices.Equal(gaps, want) {
					t.Errorf("recorded the gaps %+v, want %+v", gaps, want)
				}
				want := slices.Concat(replayedEvents(watchEventsOf(history)), test.replayed,
					[]replayedEvent{{watch.Modified, "a", written(after), written(after)}})
				if got := replayedEvents(events); !slices.Equal(got, want) {
					t.Errorf("recorded the events %+v, want %+v", got, want)
				}
				// The events the watch delivered are what it delivered: a
				// replayed delete moves a copy to its revision.
				if got, want := replayedEvents(events[:len(history)]), replayedEvents(watchEventsOf(history)); !slices.Equal(got, want) {
					t.Errorf("the delivered events now read %+v, want %+v", got, want)
				}
			})
		})
	}
}

func watchEventsOf(events []watch.Event) []watchEvent[*batchv1.Job] {
	recorded := make([]watchEvent[*batchv1.Job], 0, len(events))
	for _, event := range events {
		recorded = append(recorded, watchEvent[*batchv1.Job]{Type: event.Type, Object: event.Object.(*batchv1.Job)})
	}
	return recorded
}

// TestWatchRecorderReportsAReplayPastItsBound holds alive to a replay whose
// list never returns, whatever its context says: past the replay's bound and
// the list in flight, alive names the replay, and before that it does not.
func TestWatchRecorderReportsAReplayPastItsBound(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		watches := 0
		watcher := interceptor.NewClient(fake.NewClientBuilder().Build(), interceptor.Funcs{
			Watch: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) (watch.Interface, error) {
				watches++
				stream := watch.NewFakeWithChanSize(1, false)
				if watches == 1 {
					stream.Action(watch.Error, &metav1.Status{
						Status: metav1.StatusFailure, Code: http.StatusGone, Reason: metav1.StatusReasonExpired,
						Message: "too old resource version: 10 (12)",
					})
				}
				stream.Stop()
				return stream, nil
			},
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				<-release
				return errors.New("released")
			},
		})
		r := newWatchRecorder[*batchv1.Job]("jobs", watchTestNamespace, false, watcher,
			func() client.ObjectList { return &batchv1.JobList{} })
		go r.run(&batchv1.JobList{ListMeta: metav1.ListMeta{ResourceVersion: "10"}})
		defer func() {
			close(release)
			<-r.done
		}()
		synctest.Wait()
		time.Sleep(watchGapReplayOverrun)
		if err := r.alive(); err != nil {
			t.Fatalf("alive reported %v at the replay's overrun, before it was past it", err)
		}
		time.Sleep(time.Second)
		err := r.alive()
		if want := "the jobs watch has replayed a gap from exact lists for 21s, past the 15s a replay is bounded to"; err == nil || err.Error() != want {
			t.Fatalf("alive reported %v past the replay's overrun, want %q", err, want)
		}
	})
}
