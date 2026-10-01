package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

const upgradeDeadline = 15 * time.Minute

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var imagePattern = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)

// Intent is prepared before Helm runs. The observer and its durable state
// live outside the release being upgraded. Retrying does not replace Intent.
type Intent struct {
	Namespace    string            `json:"namespace"`
	Release      string            `json:"release"`
	HookJob      string            `json:"hookJob"`
	Manager      string            `json:"manager"`
	Rotator      string            `json:"rotator"`
	Image        string            `json:"image"`
	HookArgs     []string          `json:"hookArgs"`
	ChartDigest  string            `json:"chartDigest"`
	ValuesDigest string            `json:"valuesDigest"`
	Probes       []Probe           `json:"probes"`
	CRDDigests   map[string]string `json:"crdDigests"`
}

type Probe struct {
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation"`
}

type Attempt struct {
	UID         string     `json:"uid"`
	CreatedAt   time.Time  `json:"createdAt"`
	FailedAt    *time.Time `json:"failedAt,omitempty"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}

type State struct {
	Version         int        `json:"version"`
	Intent          Intent     `json:"intent"`
	StartedAt       time.Time  `json:"startedAt"`
	Deadline        time.Time  `json:"deadline"`
	BaselineJobUID  string     `json:"baselineJobUID,omitempty"`
	ResourceVersion string     `json:"resourceVersion"`
	HistoryLost     bool       `json:"historyLost"`
	Attempts        []Attempt  `json:"attempts"`
	FailedAt        *time.Time `json:"failedAt,omitempty"`
	RecoveredAt     *time.Time `json:"recoveredAt,omitempty"`
	RecoveryJobUID  string     `json:"recoveryJobUID,omitempty"`
}

func (i Intent) validate() error {
	for _, name := range []string{i.Namespace, i.Release, i.HookJob, i.Manager, i.Rotator} {
		if name == "" || len(validation.IsDNS1123Subdomain(name)) != 0 {
			return errors.New("upgrade intent has an invalid resource name")
		}
	}
	if !imagePattern.MatchString(i.Image) || !digestPattern.MatchString(i.ChartDigest) || !digestPattern.MatchString(i.ValuesDigest) {
		return errors.New("upgrade intent requires exact image, chart and values digests")
	}
	if len(validation.IsDNS1123Label(i.Namespace)) != 0 || len(i.HookArgs) == 0 {
		return errors.New("upgrade intent requires a namespace and exact hook arguments")
	}
	if i.Manager == i.Rotator || len(i.Probes) == 0 || len(i.Probes) > 20 || len(i.CRDDigests) != 9 {
		return errors.New("upgrade intent requires distinct runtimes, probe inventory and all nine CRD identities")
	}
	seen := map[string]bool{}
	for _, p := range i.Probes {
		key := p.Kind + "/" + p.Namespace + "/" + p.Name
		if (p.Kind != "PtahSchema" && p.Kind != "PtahMigration") || p.UID == "" || p.Generation < 1 || seen[key] || p.Namespace == "" || p.Name == "" || len(validation.IsDNS1123Label(p.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(p.Name)) != 0 {
			return errors.New("upgrade intent has an incomplete or duplicate workload probe")
		}
		seen[key] = true
	}
	for _, name := range []string{crdupgrade.PtahSchemaCRDName, crdupgrade.PtahSchemaPlanCRDName, crdupgrade.PtahSchemaPlanChunkCRDName, crdupgrade.PtahSchemaApprovalCRDName, crdupgrade.PtahMigrationCRDName, crdupgrade.PtahMigrationPlanCRDName, crdupgrade.PtahMigrationApprovalCRDName, crdupgrade.PtahMigrationRunAcknowledgmentCRDName, crdupgrade.PtahRealmCRDName} {
		if i.CRDDigests[name] == "" {
			return errors.New("upgrade intent omitted an operator CRD")
		}
	}
	for name, digest := range i.CRDDigests {
		if name == "" || len(validation.IsDNS1123Subdomain(name)) != 0 || !digestPattern.MatchString(digest) {
			return errors.New("upgrade intent has an invalid CRD identity")
		}
	}
	return nil
}

func (s State) validate() error {
	if err := s.Intent.validate(); err != nil {
		return err
	}
	if s.Version != 1 || s.StartedAt.IsZero() || !s.Deadline.Equal(s.StartedAt.Add(upgradeDeadline)) || s.ResourceVersion == "" || len(s.Attempts) > 128 {
		return errors.New("upgrade state lost its original deadline or watch boundary")
	}
	seen := map[string]bool{}
	var first *time.Time
	for _, a := range s.Attempts {
		if a.UID == "" || a.UID == s.BaselineJobUID || seen[a.UID] || a.CreatedAt.IsZero() || a.CreatedAt.Before(s.StartedAt.Truncate(time.Second)) {
			return errors.New("upgrade state has an invalid hook identity")
		}
		seen[a.UID] = true
		if a.FailedAt != nil && a.CompletedAt != nil {
			return errors.New("upgrade hook has conflicting terminal outcomes")
		}
		for _, at := range []*time.Time{a.FailedAt, a.CompletedAt} {
			if at != nil && (at.IsZero() || at.Before(a.CreatedAt)) {
				return errors.New("upgrade hook has an invalid terminal timestamp")
			}
		}
		if a.FailedAt != nil && (first == nil || a.FailedAt.Before(*first)) {
			t := *a.FailedAt
			first = &t
		}
	}
	if (first == nil) != (s.FailedAt == nil) || first != nil && !first.Equal(*s.FailedAt) {
		return errors.New("upgrade state lost its first failure")
	}
	if s.RecoveredAt != nil {
		if s.HistoryLost {
			return errors.New("upgrade recovery has a gap in hook history")
		}
		if len(s.Attempts) == 0 {
			return errors.New("upgrade recovery has no completed hook")
		}
		a := s.Attempts[len(s.Attempts)-1]
		if a.UID != s.RecoveryJobUID || a.CompletedAt == nil || !s.RecoveredAt.After(*a.CompletedAt) {
			return errors.New("upgrade recovery predates its exact completed hook")
		}
	} else if s.RecoveryJobUID != "" {
		return errors.New("upgrade recovery has no completion time")
	}
	return nil
}

// Observe preserves terminal evidence after Helm deletes a failed hook. Old
// list contents and the pre-intent Job cannot become the new transaction.
func (s *State) observe(job *batchv1.Job, now time.Time) error {
	if job == nil {
		return errors.New("upgrade watch returned no Job")
	}
	if job.Namespace != s.Intent.Namespace || job.Name != s.Intent.HookJob {
		return errors.New("upgrade watch returned an unrelated Job")
	}
	if s.BaselineJobUID != "" && string(job.UID) == s.BaselineJobUID {
		return nil
	}
	if job.UID == "" || job.ResourceVersion == "" || job.CreationTimestamp.IsZero() || job.CreationTimestamp.After(now) || job.CreationTimestamp.Time.Before(s.StartedAt.Truncate(time.Second)) {
		return errors.New("upgrade hook has no current API identity")
	}
	hooks := strings.Split(job.Annotations["helm.sh/hook"], ",")
	if !slices.Contains(hooks, "pre-upgrade") || job.Labels["app.kubernetes.io/instance"] != s.Intent.Release || job.Labels["app.kubernetes.io/managed-by"] != "Helm" || job.Labels["app.kubernetes.io/component"] != "crd-manager" {
		return errors.New("upgrade hook is not owned by the declared release")
	}
	containers := job.Spec.Template.Spec.Containers
	if len(containers) != 1 || containers[0].Name != "crd-manager" || containers[0].Image != s.Intent.Image || !slices.Equal(containers[0].Command, []string{"/ptah-crd-manager"}) || !slices.Equal(containers[0].Args, s.Intent.HookArgs) {
		return errors.New("upgrade hook does not run the declared candidate")
	}
	terminalSeen := map[batchv1.JobConditionType]bool{}
	next := Attempt{UID: string(job.UID), CreatedAt: job.CreationTimestamp.Time}
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		if c.Type != batchv1.JobFailed && c.Type != batchv1.JobComplete {
			continue
		}
		if terminalSeen[c.Type] {
			return errors.New("upgrade hook repeats a terminal condition")
		}
		terminalSeen[c.Type] = true
		at := c.LastTransitionTime.Time
		if at.IsZero() || at.Before(next.CreatedAt) || at.After(now) {
			return errors.New("upgrade hook has no authoritative terminal time")
		}
		if c.Type == batchv1.JobFailed {
			next.FailedAt = &at
		} else {
			if job.Status.CompletionTime == nil || job.Status.CompletionTime.IsZero() || job.Status.CompletionTime.Before(&job.CreationTimestamp) || job.Status.CompletionTime.After(now) {
				return errors.New("completed upgrade hook has no completion time")
			}
			at = job.Status.CompletionTime.Time
			next.CompletedAt = &at
		}
	}
	if next.FailedAt != nil && next.CompletedAt != nil {
		return errors.New("upgrade hook reports both success and failure")
	}
	index := slices.IndexFunc(s.Attempts, func(a Attempt) bool { return a.UID == next.UID })
	if index < 0 {
		if s.RecoveredAt != nil || len(s.Attempts) >= 128 {
			return errors.New("upgrade transaction cannot accept another hook")
		}
		if len(s.Attempts) > 0 && next.CreatedAt.Before(s.Attempts[len(s.Attempts)-1].CreatedAt) {
			return errors.New("upgrade hooks arrived out of order")
		}
		s.Attempts = append(s.Attempts, next)
	} else {
		old := s.Attempts[index]
		if !old.CreatedAt.Equal(next.CreatedAt) || old.FailedAt != nil && (next.FailedAt == nil || !old.FailedAt.Equal(*next.FailedAt)) || old.CompletedAt != nil && (next.CompletedAt == nil || !old.CompletedAt.Equal(*next.CompletedAt)) {
			return errors.New("upgrade hook lost or changed its retained outcome")
		}
		s.Attempts[index] = next
	}
	if next.FailedAt != nil && (s.FailedAt == nil || next.FailedAt.Before(*s.FailedAt)) {
		at := *next.FailedAt
		s.FailedAt = &at
	}
	return nil
}

func (s State) incidentAt(now time.Time) *time.Time {
	if s.RecoveredAt != nil {
		return nil
	}
	if s.FailedAt != nil && !s.FailedAt.After(s.Deadline) {
		t := *s.FailedAt
		return &t
	}
	if !now.Before(s.Deadline) {
		t := s.Deadline
		return &t
	}
	return nil
}

// Replacing a file cannot erase the previous durable state on a partial
// write. The observer is the only writer after preparation.
func saveState(path string, s State) error {
	if err := s.validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".upgrade-state-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(append(raw, '\n'))
	}
	if err == nil {
		err = file.Sync()
	}
	closed := file.Close()
	if err == nil {
		err = closed
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func fileDigest(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(b)), nil
}

// Refuse partial, concatenated, unknown-field or structurally invalid state.
func loadState(path string) (State, error) {
	var s State
	f, err := os.Open(path)
	if err != nil {
		return s, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return s, err
	}
	if len(raw) > 1<<20 {
		return s, errors.New("upgrade state exceeds its bound")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(&s); err != nil {
		return s, errors.New("invalid upgrade state JSON")
	}
	if err = d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return s, errors.New("upgrade state has trailing content")
	}
	return s, s.validate()
}
