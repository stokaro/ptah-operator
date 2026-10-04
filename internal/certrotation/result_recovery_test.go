package certrotation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
)

// Stop at a durable journal boundary, before or after its projection write.
func pendingResultRotation(t *testing.T, phase string, projected bool) *resultRotationFixture {
	t.Helper()
	f := newResultRotationFixture(t)
	project := func() {
		t.Helper()
		if !projected {
			return
		}
		f.r.probe = func(context.Context, map[string][]byte, []certificateMaterial) error {
			return errors.New("endpoint has not loaded the projection")
		}
		if _, err := f.r.step(t.Context()); err == nil {
			t.Fatal("phase completed before endpoint convergence")
		}
		f.bind()
	}
	if phase == "bootstrap" {
		f.step(t)
		project()
		return f
	}
	f.bootstrap(t)
	if phase == "leaf" {
		f.clock = f.clock.Add(21 * 24 * time.Hour)
		f.step(t)
		project()
		return f
	}
	f.beginCA(t)
	for attempts := 0; f.state(t).Phase != phase; attempts++ {
		if attempts == 12 {
			t.Fatalf("rotation did not reach %s", phase)
		}
		result := f.step(t)
		if st := f.state(t); st.FencedAt != nil && result.RequeueAfter > time.Second {
			f.clock = f.clock.Add(result.RequeueAfter)
		}
	}
	if projected && (phase == "prepare" || phase == "switch") {
		f.step(t) // Persist the enrollment fence.
		f.step(t) // Publish the overlap projection and wait.
	} else {
		project()
	}
	return f
}

func finishResultRotation(t *testing.T, f *resultRotationFixture) {
	t.Helper()
	for range 30 {
		f.restart(t)
		result := f.step(t)
		st := f.state(t)
		if st.Phase == "stable" && !result.Pending && result.RequeueAfter == 0 {
			return
		}
		if result.RequeueAfter > time.Second {
			f.clock = f.clock.Add(result.RequeueAfter)
		}
	}
	t.Fatalf("rotation did not recover: %s", f.state(t).Phase)
}

func TestResultRotationRecoversPendingExpiration(t *testing.T) {
	for _, phase := range []string{"bootstrap", "leaf", "prepare", "switch", "retire"} {
		for _, expiration := range []string{"leaf", "authorities"} {
			for _, projected := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/projected=%t", phase, expiration, projected), func(t *testing.T) {
					f := pendingResultRotation(t, phase, projected)
					st := f.state(t)
					keys := st.Current
					if st.Next != nil {
						keys = *st.Next
					}
					server, _, err := f.r.inspectKeys(keys)
					if err != nil {
						t.Fatal(err)
					}
					f.clock = server.leaf.NotAfter.Add(time.Second)
					if expiration == "authorities" {
						f.clock = server.ca.NotAfter.Add(maximumCertificatePolicyClockSkew + time.Second)
					}
					finishResultRotation(t, f)
				})
			}
		}
	}
}

func saveResultState(t *testing.T, f *resultRotationFixture, st resultJournal) {
	t.Helper()
	journal, err := f.api.CoreV1().Secrets("system").Get(t.Context(), "result-journal", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.r.saveJournal(t.Context(), journal, st); err != nil {
		t.Fatal(err)
	}
}

func shortenResultLeaf(t *testing.T, f *resultRotationFixture, keys *resultKeys, validity time.Duration) {
	t.Helper()
	server, _, err := f.r.inspectKeys(*keys)
	if err != nil {
		t.Fatal(err)
	}
	material, err := generateServingMaterialForService(f.r.random, f.clock, validity, "results", "system", server)
	if err != nil {
		t.Fatal(err)
	}
	keys.ServerCertificate, keys.ServerCertificateKey = material.certPEM, material.keyPEM
}

func TestResultRotationLeafRepairPreservesFencesAndAuthorities(t *testing.T) {
	for _, phase := range []string{"prepare", "switch"} {
		t.Run(phase, func(t *testing.T) {
			f := pendingResultRotation(t, phase, false)
			st := f.state(t)
			shortenResultLeaf(t, f, &st.Current, 2*time.Hour)
			shortenResultLeaf(t, f, st.Next, 2*time.Hour)
			// Seed the prior projection that a completed leaf renewal would leave.
			_, prior := st.projections()
			projection, _ := f.api.CoreV1().Secrets("system").Get(t.Context(), "results", metav1.GetOptions{})
			if err := f.r.writeProjection(t.Context(), projection, prior); err != nil {
				t.Fatal(err)
			}
			saveResultState(t, f, st)
			f.step(t)
			f.step(t)
			before := f.state(t)
			deadline := f.r.retirementDeadline(before)
			f.clock = f.clock.Add(2*time.Hour + time.Second)
			for range 3 {
				f.restart(t)
				f.step(t)
			}
			after := f.state(t)
			if after.Phase != phase || after.PreviousLeaves != nil || !after.FencedAt.Equal(*before.FencedAt) || !f.r.retirementDeadline(after).Equal(deadline) {
				t.Fatal("leaf repair advanced or rewrote the persisted retirement wait")
			}
			for i, pair := range [][2]resultKeys{{before.Current, after.Current}, {*before.Next, *after.Next}} {
				if !bytes.Equal(pair[0].ServerCA, pair[1].ServerCA) || !bytes.Equal(pair[0].ClientCA, pair[1].ClientCA) || !bytes.Equal(pair[0].ServerKey, pair[1].ServerKey) || !bytes.Equal(pair[0].ClientKey, pair[1].ClientKey) {
					t.Fatalf("repair changed authority %d", i)
				}
			}
			f.clock = deadline.Add(-time.Nanosecond)
			if got := f.step(t); got.RequeueAfter != time.Nanosecond || f.state(t).Phase != phase {
				t.Fatal("repair shortened the remaining wait")
			}
			f.clock = deadline
			finishResultRotation(t, f)
		})
	}
}

func TestResultRotationRecoveryResumesEveryUncertainWrite(t *testing.T) {
	for _, mode := range []string{"leaf", "authorities"} {
		for _, fault := range []struct {
			name string
			nth  int
		}{{"result-journal", 1}, {"result-journal", 2}, {"results", 1}, {"result-enrollment", 1}} {
			if mode == "leaf" && fault.name == "result-enrollment" {
				continue // Leaf replacement never changes enrollment.
			}
			t.Run(fmt.Sprintf("%s/%s/%d", mode, fault.name, fault.nth), func(t *testing.T) {
				f := pendingResultRotation(t, "switch", true)
				before := f.state(t)
				server, _, _ := f.r.inspectKeys(*before.Next)
				f.clock = server.leaf.NotAfter.Add(time.Second)
				if mode == "authorities" {
					f.clock = server.ca.NotAfter.Add(maximumCertificatePolicyClockSkew + time.Second)
				}
				writes, failures, lost, denyRead := 0, 0, false, false
				f.api.PrependReactor("update", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
					obj := action.(ktesting.UpdateAction).GetObject()
					if obj.(metav1.Object).GetName() != fault.name {
						return false, nil, nil
					}
					writes++
					if writes != fault.nth {
						return false, nil, nil
					}
					lost, denyRead = true, true
					if err := f.api.Tracker().Update(action.GetResource(), obj, action.GetNamespace()); err != nil {
						t.Fatal(err)
					}
					return true, nil, errors.New("lost recovery write response")
				})
				f.api.PrependReactor("get", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
					if denyRead && action.(ktesting.GetAction).GetName() == fault.name {
						denyRead = false
						return true, nil, errors.New("recovery readback unavailable")
					}
					return false, nil, nil
				})
				converged := false
				for range 30 {
					f.restart(t)
					result, err := f.r.step(t.Context())
					if err != nil {
						failures++
						if failures != 1 || !lost {
							t.Fatal(err)
						}
						continue
					}
					if st := f.state(t); st.Phase == "stable" && !result.Pending && result.RequeueAfter == 0 {
						converged = true
						break
					}
					if result.RequeueAfter > time.Second {
						f.clock = f.clock.Add(result.RequeueAfter)
					}
				}
				if !lost || failures != 1 || !converged {
					t.Fatal("write failure was not exercised")
				}
				after := f.state(t)
				if after.Phase != "stable" || after.PreviousLeaves != nil || after.DiscardExpiredCandidate {
					t.Fatal("recovery did not finish")
				}
				if mode == "leaf" && (!bytes.Equal(before.Next.ServerCA, after.Current.ServerCA) || !bytes.Equal(before.Next.ClientCA, after.Current.ClientCA)) {
					t.Fatal("uncertain leaf repair regenerated its candidate authorities")
				}
			})
		}
	}
}

func TestResultRotationRecoveryCanItselfExpire(t *testing.T) {
	for _, mode := range []string{"leaf", "authorities"} {
		t.Run(mode, func(t *testing.T) {
			f := pendingResultRotation(t, "bootstrap", false)
			server, _, _ := f.r.inspectKeys(f.state(t).Current)
			f.clock = server.leaf.NotAfter.Add(time.Second)
			f.step(t) // Repair persisted, projection not yet written.
			st := f.state(t)
			if st.PreviousLeaves == nil {
				t.Fatal("leaf repair was not staged")
			}
			server, _, _ = f.r.inspectKeys(st.Current)
			f.clock = server.leaf.NotAfter.Add(time.Second)
			if mode == "authorities" {
				f.clock = server.ca.NotAfter.Add(maximumCertificatePolicyClockSkew + time.Second)
			}
			finishResultRotation(t, f)
		})
	}
}

func TestResultRotationExpiredCandidateKeepsUsableCurrentAuthorities(t *testing.T) {
	f := pendingResultRotation(t, "prepare", false)
	st := f.state(t)
	// A configuration change can make the candidate expire before Current.
	f.r.config.CACertificateValidity = 7 * 24 * time.Hour
	f.r.config.ServingCertificateValidity = 5 * 24 * time.Hour
	f.r.config.RenewalThreshold = 4 * 24 * time.Hour
	next, err := f.r.generateKeys()
	if err != nil {
		t.Fatal(err)
	}
	st.Next = &next
	saveResultState(t, f, st)
	f.step(t)
	f.step(t)
	server, _, _ := f.r.inspectKeys(next)
	f.clock = server.ca.NotAfter.Add(maximumCertificatePolicyClockSkew + time.Second)
	f.step(t)
	if !f.state(t).DiscardExpiredCandidate {
		t.Fatal("candidate recovery was not journaled")
	}
	if !f.step(t).Pending {
		t.Fatal("discard checkpoint claimed readiness without an endpoint verdict")
	}
	after := f.state(t)
	if after.Phase != "stable" || after.FencedAt != nil || after.Next != nil || !equalBytes(st.Current.projection(), after.Current.projection()) {
		t.Fatal("recovery discarded Current or reused the expired candidate's fence")
	}
	current, client, _ := f.r.inspectKeys(after.Current)
	if !certificateCurrentlyValid(current.ca, f.clock) || !certificateCurrentlyValid(client.ca, f.clock) {
		t.Fatal("fixture did not retain usable Current authorities")
	}
	finishResultRotation(t, f)
}

func TestResultRotationCannotDiscardUnexpiredAuthority(t *testing.T) {
	for _, unexpired := range []string{"server", "client", "skew"} {
		t.Run(unexpired, func(t *testing.T) {
			f := pendingResultRotation(t, "prepare", false)
			st := f.state(t)
			server, _, _ := f.r.inspectKeys(*st.Next)
			f.clock = server.ca.NotAfter.Add(maximumCertificatePolicyClockSkew)
			if unexpired == "skew" {
				f.clock = f.clock.Add(-time.Nanosecond)
			} else {
				fresh, err := f.r.generateKeys()
				if err != nil {
					t.Fatal(err)
				}
				if unexpired == "server" {
					fresh.ClientCA, fresh.ClientKey = st.Next.ClientCA, st.Next.ClientKey
					st.Next = &fresh
				} else {
					st.Next.ClientCA, st.Next.ClientKey = fresh.ClientCA, fresh.ClientKey
				}
			}
			if f.r.candidateExpired(st) {
				t.Fatal("candidate was declared expired prematurely")
			}
			st.DiscardExpiredCandidate = true
			saveResultState(t, f, st)
			f.api.ClearActions()
			if _, err := f.r.step(t.Context()); err == nil {
				t.Fatal("forged recovery bypassed authority expiration")
			}
			for _, action := range f.api.Actions() {
				if action.GetVerb() != "get" {
					t.Fatalf("refusal changed trust: %s", action.GetVerb())
				}
			}
		})
	}
}

func TestResultRotationRecoveryRefusesForeignState(t *testing.T) {
	for _, mode := range []string{"leaf", "authorities"} {
		for _, staged := range []bool{false, true} {
			for _, target := range []string{"projection", "policy"} {
				t.Run(fmt.Sprintf("%s/%s/staged=%t", mode, target, staged), func(t *testing.T) {
					f := pendingResultRotation(t, "switch", true)
					server, _, _ := f.r.inspectKeys(*f.state(t).Next)
					f.clock = server.leaf.NotAfter.Add(time.Second)
					if mode == "authorities" {
						f.clock = server.ca.NotAfter.Add(maximumCertificatePolicyClockSkew + time.Second)
					}
					if staged {
						f.step(t)
					}
					if target == "projection" {
						obj, _ := f.api.CoreV1().Secrets("system").Get(t.Context(), "results", metav1.GetOptions{})
						obj.Data["foreign"] = []byte("value")
						if _, err := f.api.CoreV1().Secrets("system").Update(t.Context(), obj, metav1.UpdateOptions{}); err != nil {
							t.Fatal(err)
						}
					} else {
						obj, _ := f.api.CoreV1().ConfigMaps("system").Get(t.Context(), "result-enrollment", metav1.GetOptions{})
						obj.Data["foreign"] = "value"
						if _, err := f.api.CoreV1().ConfigMaps("system").Update(t.Context(), obj, metav1.UpdateOptions{}); err != nil {
							t.Fatal(err)
						}
					}
					f.api.ClearActions()
					f.restart(t)
					if _, err := f.r.step(t.Context()); err == nil {
						t.Fatal("recovery accepted foreign state")
					}
					for _, action := range f.api.Actions() {
						if action.GetVerb() != "get" {
							t.Fatal("recovery overwrote foreign state")
						}
					}
				})
			}
		}
	}
}

func TestResultRotationRefusesMalformedLeafRepair(t *testing.T) {
	f := pendingResultRotation(t, "bootstrap", false)
	server, _, _ := f.r.inspectKeys(f.state(t).Current)
	f.clock = server.leaf.NotAfter.Add(time.Second)
	f.step(t)
	st := f.state(t)
	st.PreviousLeaves.Current.Key = []byte("foreign private key")
	saveResultState(t, f, st)
	journal, _ := f.api.CoreV1().Secrets("system").Get(t.Context(), "result-journal", metav1.GetOptions{})
	if _, err := f.r.decodeJournal(journal.Data); err == nil {
		t.Fatal("journal accepted an unauthenticated previous leaf")
	}
}
