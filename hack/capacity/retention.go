package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var configMapResource = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

type retentionDeletion struct {
	Plan             retainedPlanID `json:"plan"`
	ResourceVersion  string         `json:"resourceVersion"`
	Children         int            `json:"children"`
	SubmittedAt      time.Time      `json:"submittedAt"`
	GarbageCollected bool           `json:"garbageCollected"`
}

type retentionProof struct {
	Round           int                 `json:"round"`
	StartedAt       time.Time           `json:"startedAt"`
	QuietAt         time.Time           `json:"quietAt"`
	FinishedAt      time.Time           `json:"finishedAt"`
	Archives        []retentionArchive  `json:"archives"`
	PinsBefore      []retentionPin      `json:"pinsBefore"`
	PinsAfter       []retentionPin      `json:"pinsAfter"`
	Deleted         []retentionDeletion `json:"deleted"`
	RetainedPlans   int                 `json:"retainedPlans"`
	ChunkBytes      int64               `json:"chunkBytes"`
	ProjectionBytes int64               `json:"projectionBytes"`
	Metrics         *sample             `json:"metrics,omitempty"`
	Resumed         bool                `json:"resumed"`
	Error           string              `json:"error,omitempty"`
}

func writeRetentionEvidence(root, relative string, value any) (retentionArchive, error) {
	if !filepath.IsLocal(relative) {
		return retentionArchive{}, fmt.Errorf("retention evidence escaped its directory")
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return retentionArchive{}, err
	}
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return retentionArchive{}, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return retentionArchive{}, err
	}
	_, writeErr := file.Write(raw)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return retentionArchive{}, err
	}
	digest := sha256.Sum256(raw)
	return retentionArchive{relative, hex.EncodeToString(digest[:])}, nil
}

func verifyRetentionArchives(root string, archives []retentionArchive) error {
	if len(archives) == 0 {
		return fmt.Errorf("maintenance exported no evidence")
	}
	for _, a := range archives {
		if !filepath.IsLocal(a.Path) || !capacityHexDigest.MatchString(a.SHA256) {
			return fmt.Errorf("invalid retention archive identity")
		}
		raw, err := os.ReadFile(filepath.Join(root, a.Path))
		if err != nil {
			return err
		}
		digest := sha256.Sum256(raw)
		if hex.EncodeToString(digest[:]) != a.SHA256 {
			return fmt.Errorf("retention archive changed: %s", a.Path)
		}
	}
	return nil
}

func ownedPlan(plan *unstructured.Unstructured, family string, paused []pausedResource) bool {
	ownerField := "schemaRef"
	kind := "PtahSchema"
	if family == "migration" {
		ownerField = "migrationRef"
		kind = "PtahMigration"
	}
	name, _, _ := unstructured.NestedString(plan.Object, "spec", ownerField, "name")
	uid, _, _ := unstructured.NestedString(plan.Object, "spec", ownerField, "uid")
	for _, p := range paused {
		if p.target.family != family || p.target.namespace != plan.GetNamespace() || p.target.name != name || string(p.target.uid) != uid {
			continue
		}
		for _, owner := range plan.GetOwnerReferences() {
			if owner.UID == p.target.uid && owner.Name == name && owner.Kind == kind && owner.APIVersion == "operator.ptah.run/v1alpha1" && owner.Controller != nil && *owner.Controller {
				return true
			}
		}
	}
	return false
}

func ownedChildren(inventory retentionInventory, id retainedPlanID) []retainedObject {
	var result []retainedObject
	for _, entry := range inventory.Objects {
		if entry.Object.GetNamespace() != id.Namespace || entry.Resource != planChunkResource && entry.Resource != configMapResource {
			continue
		}
		for _, owner := range entry.Object.GetOwnerReferences() {
			if string(owner.UID) == id.UID {
				result = append(result, entry)
				break
			}
		}
	}
	return result
}

func (s *scenarios) waitRetainedObjectsGone(ctx context.Context, objects []retainedObject) error {
	for _, entry := range objects {
		for {
			current, err := s.dynamic.Resource(entry.Resource).Namespace(entry.Object.GetNamespace()).Get(ctx, entry.Object.GetName(), metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				break
			}
			if err != nil {
				return err
			}
			if current.GetUID() != entry.Object.GetUID() {
				return fmt.Errorf("retention found a replacement of %s/%s", entry.Resource.Resource, entry.Object.GetName())
			}
			if err := waitCapacityPoll(ctx); err != nil {
				return fmt.Errorf("garbage collection left %s/%s: %w", entry.Resource.Resource, entry.Object.GetName(), err)
			}
		}
	}
	return nil
}

// The whole fleet remains suspended throughout. Re-list every documented pin
// immediately before each DELETE, and bind the DELETE to both UID and RV.
func (s *scenarios) pruneRetention(ctx context.Context, paused []pausedResource, before retentionInventory, proof *retentionProof) error {
	plans := before.plans()
	ids := make([]retainedPlanID, 0, len(plans))
	for id, plan := range plans {
		if ownedPlan(plan, id.Family, paused) && !before.pinned(id) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := ids[i], ids[j]
		return a.Family+"/"+a.Namespace+"/"+a.Name < b.Family+"/"+b.Namespace+"/"+b.Name
	})
	for _, id := range ids {
		quiet, err := s.maintenanceQuiet(ctx, paused)
		if err != nil {
			return err
		}
		if !quiet {
			return fmt.Errorf("maintenance lost quiescence before deletion")
		}
		current, err := s.retentionInventory(ctx)
		if err != nil {
			return err
		}
		if err := s.validateMaintenanceInventory(current, paused); err != nil {
			return err
		}
		if current.pinned(id) {
			continue
		}
		plan := current.plans()[id]
		if plan == nil || plan.GetDeletionTimestamp() != nil || !ownedPlan(plan, id.Family, paused) || !reflect.DeepEqual(plan.Object["spec"], plans[id].Object["spec"]) {
			return fmt.Errorf("obsolete plan changed after export")
		}
		children := ownedChildren(current, id)
		if !unchangedRetainedChildren(before, current, id) {
			return fmt.Errorf("plan children changed after export")
		}
		if err := verifyRetentionArchives(s.evidenceDir, proof.Archives); err != nil {
			return err
		}
		uid, rv := plan.GetUID(), plan.GetResourceVersion()
		deletion := retentionDeletion{Plan: id, ResourceVersion: rv, Children: len(children), SubmittedAt: time.Now().UTC()}
		if err := s.dynamic.Resource(planResource(id.Family)).Namespace(id.Namespace).Delete(ctx, id.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}); err != nil {
			return err
		}
		proof.Deleted = append(proof.Deleted, deletion)
		gone := append([]retainedObject{{planResource(id.Family), plan}}, children...)
		if err := s.waitRetainedObjectsGone(ctx, gone); err != nil {
			return err
		}
		proof.Deleted[len(proof.Deleted)-1].GarbageCollected = true
	}
	return nil
}

func retainedPayload(inventory retentionInventory, paused []pausedResource) (int, int64, int64, error) {
	count := 0
	var chunks, projections int64
	for id, plan := range inventory.plans() {
		if !ownedPlan(plan, id.Family, paused) {
			continue
		}
		count++
		for _, child := range ownedChildren(inventory, id) {
			path := []string{"spec", "data"}
			if child.Resource == configMapResource {
				path = []string{"binaryData", "chunk"}
			}
			encoded, found, err := unstructured.NestedString(child.Object.Object, path...)
			if err != nil || !found {
				return 0, 0, 0, fmt.Errorf("retained plan child lacks its payload")
			}
			raw, err := base64.StdEncoding.Strict().DecodeString(encoded)
			if err != nil {
				return 0, 0, 0, err
			}
			if child.Resource == configMapResource {
				projections += int64(len(raw))
			} else {
				chunks += int64(len(raw))
			}
		}
	}
	return count, chunks, projections, nil
}

func (s *scenarios) maintenance(ctx context.Context, round int) (err error) {
	proof := retentionProof{Round: round, StartedAt: time.Now().UTC()}
	var paused []pausedResource
	defer func() {
		if !proof.Resumed && len(paused) > 0 {
			recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.load.Settle.Duration)
			defer cancel()
			_, resumeErr := s.resumeMaintenance(recovery, paused)
			err = errors.Join(err, resumeErr)
		}
		proof.FinishedAt = time.Now().UTC()
		outcome := map[string]string{"round": fmt.Sprint(round), "deletedPlans": fmt.Sprint(len(proof.Deleted))}
		if err != nil {
			proof.Error = err.Error()
			outcome["error"] = err.Error()
		}
		s.retentionProofs = append(s.retentionProofs, proof)
		s.mark(fmt.Sprintf("soak maintenance %02d", round), proof.StartedAt, outcome)
	}()
	for _, family := range []string{"schema", "migration"} {
		for index := range 10 {
			resource, archive, e := s.exportAndSuspend(ctx, family, index, round)
			if resource.target.uid != "" {
				paused = append(paused, resource)
			}
			if archive.Path != "" {
				proof.Archives = append(proof.Archives, archive)
			}
			if e != nil {
				return e
			}
		}
	}
	if err := s.waitMaintenanceQuiet(ctx, paused); err != nil {
		return err
	}
	proof.QuietAt = time.Now().UTC()
	before, err := s.retentionInventory(ctx)
	if err != nil {
		return err
	}
	if err := s.validateMaintenanceInventory(before, paused); err != nil {
		return err
	}
	if err := verifyRetentionPayloads(ctx, before); err != nil {
		return err
	}
	proof.PinsBefore = before.Pins
	archive, err := writeRetentionEvidence(s.evidenceDir, fmt.Sprintf("retention/round-%02d/before-prune.json", round), before)
	if err != nil {
		return err
	}
	proof.Archives = append(proof.Archives, archive)
	if err := s.pruneRetention(ctx, paused, before, &proof); err != nil {
		return err
	}
	if len(proof.Deleted) == 0 {
		return fmt.Errorf("maintenance exercised no obsolete-plan deletion")
	}
	quiet, err := s.maintenanceQuiet(ctx, paused)
	if err != nil {
		return err
	}
	if !quiet {
		return fmt.Errorf("workload resumed during maintenance")
	}
	after, err := s.retentionInventory(ctx)
	if err != nil {
		return err
	}
	// Every pre-deletion pin must still resolve to the same immutable spec,
	// even if the controller has retired the field or an approval in the meantime.
	if err := s.validateMaintenanceInventory(after, paused); err != nil {
		return err
	}
	if err := verifyRetentionPayloads(ctx, after); err != nil {
		return err
	}
	remaining := after.plans()
	for _, pin := range before.Pins {
		if remaining[pin.Plan] == nil || !reflect.DeepEqual(before.plans()[pin.Plan].Object["spec"], remaining[pin.Plan].Object["spec"]) || !unchangedRetainedChildren(before, after, pin.Plan) {
			return fmt.Errorf("maintenance lost or changed a pinned plan")
		}
	}
	proof.PinsAfter = after.Pins
	proof.RetainedPlans, proof.ChunkBytes, proof.ProjectionBytes, err = retainedPayload(after, paused)
	if err != nil {
		return err
	}
	archive, err = writeRetentionEvidence(s.evidenceDir, fmt.Sprintf("retention/round-%02d/after-prune.json", round), after)
	if err != nil {
		return err
	}
	proof.Archives = append(proof.Archives, archive)
	if proof.ChunkBytes+proof.ProjectionBytes > 128*1024*1024 {
		return fmt.Errorf("retained payload exceeds 128 MiB")
	}
	// Compare the same processes at the same settled point after every cleanup.
	metrics, err := s.maintenanceMetrics(ctx, time.Now().UTC().Add(2*s.load.SampleEvery.Duration))
	if err != nil {
		return err
	}
	proof.Metrics = &metrics
	if err := s.resumeAndConverge(ctx, paused); err != nil {
		return err
	}
	proof.Resumed = true
	return nil
}
