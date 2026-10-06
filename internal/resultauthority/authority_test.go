package resultauthority

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/resulttest"
	"github.com/stokaro/ptah-operator/internal/workload"
)

func TestAllOperationJobs(t *testing.T) {
	for _, name := range []string{"schema-resolve", "schema-verify-admitted", "schema-observe", "schema-plan-dev-fence-scheduling", "schema-apply-admitted-scheduling", "migration-resolve", "migration-verify-admitted", "migration-history", "migration-apply-admitted-scheduling"} {
		t.Run(name, func(t *testing.T) {
			f := resulttest.New(t, name)
			if err := (Authorizer{Reader: f.Client(t)}).Check(t.Context(), f.Identity); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRefusals(t *testing.T) {
	cases := map[string]func(*resulttest.Fixture){
		"subject UID":     func(f *resulttest.Fixture) { f.Subject.SetUID("replacement") },
		"generation":      func(f *resulttest.Fixture) { f.Subject.SetGeneration(3) },
		"retired claim":   func(f *resulttest.Fixture) { f.Subject.(*api.PtahSchema).Status.ActiveOperation = nil },
		"binding removed": func(f *resulttest.Fixture) { f.Subject.(*api.PtahSchema).Status.ExecutionBinding = nil },
		"binding rotated": func(f *resulttest.Fixture) {
			f.Subject.(*api.PtahSchema).Status.ExecutionBinding.Epoch = "v1-44444444444444444444444444444444"
		},
		"operation ID": func(f *resulttest.Fixture) { f.Subject.(*api.PtahSchema).Status.ActiveOperation.ID = "replacement" },
		"input fingerprint": func(f *resulttest.Fixture) {
			f.Subject.(*api.PtahSchema).Status.ActiveOperation.InputFingerprint = "sha256:" + strings.Repeat("a", 64)
		},
		"operation kind": func(f *resulttest.Fixture) {
			f.Subject.(*api.PtahSchema).Status.ActiveOperation.Type = api.OperationObserve
		},
		"claim Job UID": func(f *resulttest.Fixture) { f.Subject.(*api.PtahSchema).Status.ActiveOperation.JobUID = "replacement" },
		"claim Job name": func(f *resulttest.Fixture) {
			f.Subject.(*api.PtahSchema).Status.ActiveOperation.JobName = "replacement"
		},
		"engine": func(f *resulttest.Fixture) { f.Identity.Engine = "mysql" },
		"dispatch not recorded": func(f *resulttest.Fixture) {
			f.Subject.(*api.PtahSchema).Status.ActiveOperation.DispatchStarted = false
		},
		"lease continuity lost": func(f *resulttest.Fixture) {
			f.Subject.(*api.PtahSchema).Status.ActiveOperation.LeaseContinuityLost = true
		},
		"Job UID":             func(f *resulttest.Fixture) { f.Job.UID = "replacement" },
		"Job owner":           func(f *resulttest.Fixture) { f.Job.OwnerReferences[0].UID = "replacement" },
		"Job cleanup":         func(f *resulttest.Fixture) { f.Job.Spec.TTLSecondsAfterFinished = ptr.To(int32(300)) },
		"Job retries":         func(f *resulttest.Fixture) { f.Job.Spec.BackoffLimit = ptr.To(int32(1)) },
		"Job template":        func(f *resulttest.Fixture) { f.Job.Spec.Template.Spec.Containers[0].Image = "other" },
		"Pod UID":             func(f *resulttest.Fixture) { f.Pod.UID = "replacement" },
		"Pod owner":           func(f *resulttest.Fixture) { f.Pod.OwnerReferences[0].UID = "replacement" },
		"Pod name chain":      func(f *resulttest.Fixture) { f.Pod.GenerateName = "other-" },
		"Pod command":         func(f *resulttest.Fixture) { f.Pod.Spec.Containers[0].Command = []string{"other"} },
		"Pod service account": func(f *resulttest.Fixture) { f.Pod.Spec.ServiceAccountName = "other" },
		"Pod metadata":        func(f *resulttest.Fixture) { f.Pod.Annotations[workload.AnnotationOperationID] = "other" },
		"Pod snapshot": func(f *resulttest.Fixture) {
			f.Subject.(*api.PtahSchema).Status.ActiveOperation.AdmissionSnapshot.Digest = "invalid"
		},
		"certificate generation": func(f *resulttest.Fixture) { f.Identity.Binding.Generation = 0 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := resulttest.New(t, "schema-apply-admitted-scheduling")
			change(f)
			if err := (Authorizer{Reader: f.Client(t)}).Check(t.Context(), f.Identity); !errors.Is(err, resultdelivery.ErrAuthority) {
				t.Fatalf("got %v, want authority refusal", err)
			}
		})
	}
}

func TestAdoptionAndLateOutcome(t *testing.T) {
	for _, name := range []string{"schema-apply-admitted-scheduling", "migration-apply-admitted-scheduling"} {
		t.Run(name, func(t *testing.T) {
			f := resulttest.New(t, name)
			expired := metav1.NewTime(time.Now().Add(-time.Hour))
			switch subject := f.Subject.(type) {
			case *api.PtahSchema:
				subject.Status.ActiveOperation.ExecutionNotAfter = &expired
			case *api.PtahMigration:
				subject.Status.ActiveOperation.ExecutionNotAfter = &expired
			}
			f.Pod.DeletionTimestamp = &expired
			f.Pod.Finalizers = []string{"test.ptah.run/hold"}
			if err := (Authorizer{Reader: f.Client(t)}).Check(t.Context(), f.Identity); err != nil {
				t.Fatalf("late outcome from original terminating Pod: %v", err)
			}
			switch subject := f.Subject.(type) {
			case *api.PtahSchema:
				subject.Status.ActiveOperation.JobUID = ""
			case *api.PtahMigration:
				subject.Status.ActiveOperation.JobUID = ""
			}
			if err := (Authorizer{Reader: f.Client(t)}).Check(t.Context(), f.Identity); !errors.Is(err, ErrNotReady) {
				t.Fatalf("unadopted Job: %v", err)
			}
		})
	}
}

func TestSecondPodRefused(t *testing.T) {
	f := resulttest.New(t, "migration-history")
	other := f.Pod.DeepCopy()
	other.Name = f.Job.Name + "-fghij"
	other.UID = "other-pod"
	if err := (Authorizer{Reader: f.Client(t, other)}).Check(t.Context(), f.Identity); !errors.Is(err, resultdelivery.ErrAuthority) {
		t.Fatal(err)
	}
}

func TestAuthorityValidatesTheListedPod(t *testing.T) {
	for _, name := range []string{"schema-observe", "migration-history"} {
		for _, changed := range []bool{false, true} {
			t.Run(name+"/changed="+fmt.Sprint(changed), func(t *testing.T) {
				f := resulttest.New(t, name)
				podGets := 0
				reader := readerHook{Reader: f.Client(t), get: func(object client.Object) error {
					if _, ok := object.(*corev1.Pod); ok {
						podGets++
					}
					return nil
				}, list: func(pods *corev1.PodList) error {
					if changed {
						pods.Items[0].Spec.Containers[0].Command = []string{"other"}
					}
					return nil
				}}
				err := (Authorizer{Reader: reader}).Check(t.Context(), f.Identity)
				if changed && !errors.Is(err, resultdelivery.ErrAuthority) || !changed && err != nil {
					t.Fatalf("authority did not validate the listed Pod: %v", err)
				}
				if podGets != 0 {
					t.Fatalf("read the Pod %d times before listing that same Pod", podGets)
				}
			})
		}
	}
}

type readerHook struct {
	client.Reader
	get  func(client.Object) error
	list func(*corev1.PodList) error
}

func (r readerHook) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := r.Reader.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if r.get != nil {
		return r.get(obj)
	}
	return nil
}
func (r readerHook) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := r.Reader.List(ctx, list, opts...); err != nil {
		return err
	}
	if r.list != nil {
		return r.list(list.(*corev1.PodList))
	}
	return nil
}

func TestAuthorityReadRacesAndFailures(t *testing.T) {
	for _, stage := range []string{"subject", "job", "list", "second subject"} {
		t.Run("API failure "+stage, func(t *testing.T) {
			f := resulttest.New(t, "schema-observe")
			unavailable := errors.New("API unavailable")
			reads := 0
			reader := readerHook{Reader: f.Client(t), get: func(obj client.Object) error {
				reads++
				matches := stage == "subject" && reads == 1 || stage == "job" && reads == 2 || stage == "second subject" && reads == 3
				if matches {
					return unavailable
				}
				return nil
			}, list: func(*corev1.PodList) error {
				if stage == "list" {
					return unavailable
				}
				return nil
			}}
			if err := (Authorizer{Reader: reader}).Check(t.Context(), f.Identity); !errors.Is(err, unavailable) {
				t.Fatalf("want retryable API error, got %v", err)
			}
		})
	}
	t.Run("retirement during Pod reads", func(t *testing.T) {
		f := resulttest.New(t, "migration-history")
		reads := 0
		reader := readerHook{Reader: f.Client(t), get: func(obj client.Object) error {
			if m, ok := obj.(*api.PtahMigration); ok {
				reads++
				if reads == 2 {
					m.Status.ActiveOperation = nil
				}
			}
			return nil
		}}
		if err := (Authorizer{Reader: reader}).Check(t.Context(), f.Identity); !errors.Is(err, resultdelivery.ErrAuthority) {
			t.Fatal(err)
		}
	})
	t.Run("claim changes without retirement", func(t *testing.T) {
		f := resulttest.New(t, "schema-observe")
		reads := 0
		reader := readerHook{Reader: f.Client(t), get: func(obj client.Object) error {
			if s, ok := obj.(*api.PtahSchema); ok {
				reads++
				if reads == 2 {
					s.Status.ExecutionBinding.PtahVersion = "v9.0.0"
				}
			}
			return nil
		}}
		if err := (Authorizer{Reader: reader}).Check(t.Context(), f.Identity); !errors.Is(err, resultdelivery.ErrAuthority) {
			t.Fatal(err)
		}
	})
	for _, mode := range []string{"empty", "continued", "replaced"} {
		t.Run("Pod list "+mode, func(t *testing.T) {
			f := resulttest.New(t, "schema-observe")
			reader := readerHook{Reader: f.Client(t), list: func(list *corev1.PodList) error {
				switch mode {
				case "empty":
					list.Items = nil
				case "continued":
					list.Continue = "more"
				case "replaced":
					list.Items[0].UID = "replacement"
				}
				return nil
			}}
			if err := (Authorizer{Reader: reader}).Check(t.Context(), f.Identity); !errors.Is(err, resultdelivery.ErrAuthority) {
				t.Fatal(err)
			}
		})
	}
}

func TestMigrationClaimRefusals(t *testing.T) {
	changes := map[string]func(*api.PtahMigration){
		"retired":                func(m *api.PtahMigration) { m.Status.ActiveOperation = nil },
		"no binding":             func(m *api.PtahMigration) { m.Status.ExecutionBinding = nil },
		"epoch rotated":          func(m *api.PtahMigration) { m.Status.ExecutionBinding.Epoch = "v1-44444444444444444444444444444444" },
		"operation changed":      func(m *api.PtahMigration) { m.Status.ActiveOperation.Type = api.MigrationOperationHistory },
		"claim identity changed": func(m *api.PtahMigration) { m.Status.ActiveOperation.ID = "other" },
		"engine changed":         func(m *api.PtahMigration) { m.Status.ActiveOperation.Target.Engine = api.DatabaseEngine("MySQL") },
		"not dispatched":         func(m *api.PtahMigration) { m.Status.ActiveOperation.DispatchStarted = false },
		"continuity lost":        func(m *api.PtahMigration) { m.Status.ActiveOperation.LeaseContinuityLost = true },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			f := resulttest.New(t, "migration-apply-admitted-scheduling")
			change(f.Subject.(*api.PtahMigration))
			if err := (Authorizer{Reader: f.Client(t)}).Check(t.Context(), f.Identity); !errors.Is(err, resultdelivery.ErrAuthority) {
				t.Fatal(err)
			}
		})
	}
}

func TestMissingObjectsAreDefinitive(t *testing.T) {
	for _, kind := range []string{"subject", "job", "pod"} {
		t.Run(kind, func(t *testing.T) {
			f := resulttest.New(t, "schema-observe")
			reader := f.Client(t)
			var obj client.Object
			switch kind {
			case "subject":
				obj = f.Subject
			case "job":
				obj = f.Job
			case "pod":
				obj = f.Pod
			}
			if err := reader.Delete(t.Context(), obj); err != nil {
				t.Fatal(err)
			}
			if err := (Authorizer{Reader: reader}).Check(t.Context(), f.Identity); !errors.Is(err, resultdelivery.ErrAuthority) {
				t.Fatalf("want definitive refusal, got %v", err)
			}
		})
	}
}
