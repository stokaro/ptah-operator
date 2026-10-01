package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func fixtureState() State {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	digest := "sha256:" + strings.Repeat("a", 64)
	crds := map[string]string{}
	for _, name := range []string{crdupgrade.PtahSchemaCRDName, crdupgrade.PtahSchemaPlanCRDName, crdupgrade.PtahSchemaPlanChunkCRDName, crdupgrade.PtahSchemaApprovalCRDName, crdupgrade.PtahMigrationCRDName, crdupgrade.PtahMigrationPlanCRDName, crdupgrade.PtahMigrationApprovalCRDName, crdupgrade.PtahMigrationRunAcknowledgmentCRDName, crdupgrade.PtahRealmCRDName} {
		crds[name] = digest
	}
	return State{Version: 1, Intent: Intent{Namespace: "operator", Release: "ptah", HookJob: "ptah-crd-manager", Manager: "ptah", Rotator: "ptah-cert-rotator", Image: "example.invalid/operator@" + digest, HookArgs: []string{"reconcile", "--schema-version=1"}, ChartDigest: digest, ValuesDigest: digest, CRDDigests: crds, Probes: []Probe{{Kind: "PtahSchema", Namespace: "workloads", Name: "probe", UID: "probe-uid", Generation: 1}}}, StartedAt: start, Deadline: start.Add(upgradeDeadline), BaselineJobUID: "previous-release", ResourceVersion: "100"}
}
func fixtureJob(s State, uid string, offset time.Duration) *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: s.Intent.Namespace, Name: s.Intent.HookJob, UID: types.UID(uid), ResourceVersion: "101", CreationTimestamp: metav1.NewTime(s.StartedAt.Add(offset)), Annotations: map[string]string{"helm.sh/hook": "pre-install,pre-upgrade,pre-rollback"}, Labels: map[string]string{"app.kubernetes.io/instance": s.Intent.Release, "app.kubernetes.io/managed-by": "Helm", "app.kubernetes.io/component": "crd-manager"}}, Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "crd-manager", Image: s.Intent.Image, Command: []string{"/ptah-crd-manager"}, Args: append([]string(nil), s.Intent.HookArgs...)}}}}}}
}
func terminateJob(job *batchv1.Job, kind batchv1.JobConditionType, at time.Time) {
	job.Status.Conditions = []batchv1.JobCondition{{Type: kind, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(at)}}
	if kind == batchv1.JobComplete {
		v := metav1.NewTime(at)
		job.Status.CompletionTime = &v
	}
}

func TestFailureSurvivesDeletionPersistenceAndRetry(t *testing.T) {
	s := fixtureState()
	failed := fixtureJob(s, "failed", time.Second)
	now := s.StartedAt.Add(time.Minute)
	terminateJob(failed, batchv1.JobFailed, now)
	if err := s.observe(failed, now); err != nil {
		t.Fatal(err)
	}
	// A deletion carries the final Job object. Seeing it again must preserve
	// the outcome even though the object is absent from later lists.
	failed.DeletionTimestamp = &metav1.Time{Time: now}
	if err := s.observe(failed, now); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := saveState(path, s); err != nil {
		t.Fatal(err)
	}
	restored, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	retry := fixtureJob(s, "retry", 2*time.Minute)
	if err := restored.observe(retry, s.StartedAt.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := restored.incidentAt(s.StartedAt.Add(3 * time.Minute)); got == nil || !got.Equal(now) {
		t.Fatal("retry erased the first failure")
	}
	if !restored.Deadline.Equal(s.Deadline) {
		t.Fatal("retry reset the original deadline")
	}
	terminateJob(retry, batchv1.JobComplete, s.StartedAt.Add(4*time.Minute))
	if err := restored.observe(retry, s.StartedAt.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if restored.incidentAt(s.StartedAt.Add(5*time.Minute)) == nil {
		t.Fatal("hook success alone resolved the installation")
	}
	recovered := s.StartedAt.Add(6 * time.Minute)
	restored.RecoveredAt = &recovered
	restored.RecoveryJobUID = "retry"
	if err := saveState(path, restored); err != nil {
		t.Fatal(err)
	}
	restored, err = loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if restored.incidentAt(recovered) != nil {
		t.Fatal("verified recovery remains firing")
	}
}

func TestDeadlineIsOriginalAndIndependentOfJobExistence(t *testing.T) {
	s := fixtureState()
	if s.incidentAt(s.Deadline.Add(-time.Nanosecond)) != nil {
		t.Fatal("early deadline")
	}
	if got := s.incidentAt(s.Deadline); got == nil || !got.Equal(s.Deadline) {
		t.Fatal("no incident at the original bound")
	}
	late := fixtureJob(s, "late", 16*time.Minute)
	now := s.StartedAt.Add(17 * time.Minute)
	terminateJob(late, batchv1.JobFailed, now)
	if err := s.observe(late, now); err != nil {
		t.Fatal(err)
	}
	if got := s.incidentAt(now); got == nil || !got.Equal(s.Deadline) {
		t.Fatal("late failure extended the deadline")
	}
}

func TestHookRefusesWrongIdentityAndContradictoryEvidence(t *testing.T) {
	for name, mutate := range map[string]func(*batchv1.Job){
		"namespace":          func(j *batchv1.Job) { j.Namespace = "other" },
		"name":               func(j *batchv1.Job) { j.Name = "other" },
		"uid":                func(j *batchv1.Job) { j.UID = "" },
		"cursor":             func(j *batchv1.Job) { j.ResourceVersion = "" },
		"release":            func(j *batchv1.Job) { j.Labels["app.kubernetes.io/instance"] = "other" },
		"manager":            func(j *batchv1.Job) { j.Labels["app.kubernetes.io/managed-by"] = "another" },
		"component":          func(j *batchv1.Job) { j.Labels["app.kubernetes.io/component"] = "manager" },
		"hook":               func(j *batchv1.Job) { j.Annotations["helm.sh/hook"] = "post-install" },
		"image":              func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].Image = "other:latest" },
		"arguments":          func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].Args = nil },
		"command":            func(j *batchv1.Job) { j.Spec.Template.Spec.Containers[0].Command = nil },
		"no time":            func(j *batchv1.Job) { j.CreationTimestamp = metav1.Time{} },
		"old job":            func(j *batchv1.Job) { j.CreationTimestamp.Time = j.CreationTimestamp.Add(-time.Hour) },
		"future job":         func(j *batchv1.Job) { j.CreationTimestamp.Time = j.CreationTimestamp.Add(time.Hour) },
		"empty failure time": func(j *batchv1.Job) { j.Status.Conditions[0].LastTransitionTime = metav1.Time{} },
		"future failure": func(j *batchv1.Job) {
			j.Status.Conditions[0].LastTransitionTime.Time = j.CreationTimestamp.Add(time.Hour)
		},
		"conflicting outcomes": func(j *batchv1.Job) {
			v := j.Status.Conditions[0]
			v.Type = batchv1.JobComplete
			j.Status.Conditions = append(j.Status.Conditions, v)
			j.Status.CompletionTime = &v.LastTransitionTime
		},
		"duplicate outcome": func(j *batchv1.Job) { j.Status.Conditions = append(j.Status.Conditions, j.Status.Conditions[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			s := fixtureState()
			j := fixtureJob(s, "current", time.Second)
			now := s.StartedAt.Add(time.Minute)
			terminateJob(j, batchv1.JobFailed, now)
			mutate(j)
			if err := s.observe(j, now); err == nil {
				t.Fatal("invalid hook accepted")
			}
		})
	}
	s := fixtureState()
	j := fixtureJob(s, "current", time.Second)
	now := s.StartedAt.Add(time.Minute)
	terminateJob(j, batchv1.JobFailed, now)
	if err := s.observe(j, now); err != nil {
		t.Fatal(err)
	}
	j.Status.Conditions = nil
	if err := s.observe(j, now); err == nil {
		t.Fatal("lost failure accepted")
	}
}

func TestStateRefusesIncompleteContractsAndCorruption(t *testing.T) {
	for name, mutate := range map[string]func(*State){
		"unknown version": func(s *State) { s.Version++ },
		"new deadline":    func(s *State) { s.Deadline = s.Deadline.Add(time.Minute) },
		"missing cursor":  func(s *State) { s.ResourceVersion = "" },
		"arbitrary nine CRDs": func(s *State) {
			delete(s.Intent.CRDDigests, crdupgrade.PtahSchemaCRDName)
			s.Intent.CRDDigests["other.example.test"] = s.Intent.ChartDigest
		},
		"invalid namespace":  func(s *State) { s.Intent.Namespace = "not.a.namespace" },
		"empty probes":       func(s *State) { s.Intent.Probes = nil },
		"zero generation":    func(s *State) { s.Intent.Probes[0].Generation = 0 },
		"mutable image":      func(s *State) { s.Intent.Image = "operator:latest" },
		"identical runtimes": func(s *State) { s.Intent.Rotator = s.Intent.Manager },
		"duplicate probe":    func(s *State) { s.Intent.Probes = append(s.Intent.Probes, s.Intent.Probes[0]) },
		"invented recovery":  func(s *State) { s.RecoveredAt = &s.Deadline; s.RecoveryJobUID = "unknown" },
	} {
		t.Run(name, func(t *testing.T) {
			s := fixtureState()
			mutate(&s)
			if err := s.validate(); err == nil {
				t.Fatal("invalid state accepted")
			}
		})
	}
	s := fixtureState()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{"partial": raw[:len(raw)-1], "trailing": append(append([]byte(nil), raw...), []byte(" {}")...), "unknown": []byte(strings.Replace(string(raw), `"version":1`, `"version":1,"extra":true`, 1)), "oversized": []byte(strings.Repeat(" ", 1<<20+1))} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadState(path); err == nil {
				t.Fatal("corrupt state accepted")
			}
		})
	}
}

func TestStateHasOneWriterAndPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	lock, err := lockState(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if other, err := lockState(path); err == nil {
		other.Close()
		t.Fatal("second writer acquired state")
	}
	if err := saveState(path, fixtureState()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal("state is not private")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := lockState(path)
	if err != nil {
		t.Fatal("a terminated writer kept its lock", err)
	}
	next.Close()
}

func TestMissingUIDWithoutPreviousHookIsRefused(t *testing.T) {
	s := fixtureState()
	s.BaselineJobUID = ""
	j := fixtureJob(s, "", time.Second)
	if err := s.observe(j, s.StartedAt.Add(time.Minute)); err == nil {
		t.Fatal("empty UID was mistaken for a previous hook")
	}
}

func TestInspectReadsWhileTheObserverOwnsTheState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	lock, err := lockState(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := saveState(path, fixtureState()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := inspectState(path, &b); err != nil {
		t.Fatal(err)
	}
	var state State
	if err := json.Unmarshal(b.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.ResourceVersion != "100" || !state.Deadline.Equal(fixtureState().Deadline) {
		t.Fatal("inspection lost the original boundary")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("inspection changed evidence")
	}
}
