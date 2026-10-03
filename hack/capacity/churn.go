package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/planstore"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type churnProof struct {
	ObjectsBeforeDeletion int       `json:"objectsBeforeDeletion"`
	Round                 int       `json:"round"`
	Family                string    `json:"family"`
	Namespace             string    `json:"namespace"`
	Name                  string    `json:"name"`
	OldUID                string    `json:"oldUID"`
	NewUID                string    `json:"newUID"`
	DeleteSubmittedAt     time.Time `json:"deleteSubmittedAt"`
	CreatedAt             time.Time `json:"createdAt"`
	ExportPath            string    `json:"exportPath"`
	ExportSHA256          string    `json:"exportSHA256"`
	GarbageCollected      bool      `json:"garbageCollected"`
}

type retainedObject struct {
	Resource schema.GroupVersionResource `json:"resource"`
	Object   *unstructured.Unstructured  `json:"object"`
}

func churnReplacement(original *unstructured.Unstructured, family, workloadName string) (*unstructured.Unstructured, error) {
	reading, err := readCycle(family, "GET", original, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if !cycleReady(family, reading) || original.GetResourceVersion() == "" || original.GetDeletionTimestamp() != nil || original.GetLabels()[capacityLabel] != workloadName {
		return nil, fmt.Errorf("churn requires an idle, converged workload resource")
	}
	if value, exists, _ := unstructured.NestedFieldNoCopy(original.Object, "status", "pendingBindingRetirement"); exists && value != nil {
		return nil, fmt.Errorf("churn refuses pending binding retirement")
	}
	if _, exists := original.GetAnnotations()[operatorv1alpha1.UnresolvedRunAnnotation]; exists {
		return nil, fmt.Errorf("churn refuses unresolved-run evidence")
	}
	spec, found, err := unstructured.NestedMap(original.Object, "spec")
	if err != nil || !found {
		return nil, fmt.Errorf("churn resource has no valid spec")
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": original.GetAPIVersion(), "kind": original.GetKind(),
		"metadata": map[string]any{"name": original.GetName(), "namespace": original.GetNamespace(), "labels": map[string]any{capacityLabel: workloadName}},
		"spec":     spec,
	}}, nil
}

// Export before deleting the owner: owner garbage collection otherwise removes
// the only retained SQL. Loading through the plan store verifies every bound
// chunk and the reconstructed digest before any DELETE can be submitted.
func (s *scenarios) exportChurn(ctx context.Context, original *unstructured.Unstructured, family string, round, attempt int) (string, string, []retainedObject, error) {
	return s.exportOwnedPlans(ctx, original, family, filepath.Join(s.evidenceDir, "churn"), fmt.Sprintf("round-%02d-%s-%s-attempt-%d.json", round, family, original.GetName(), attempt))
}

func (s *scenarios) exportOwnedPlans(ctx context.Context, original *unstructured.Unstructured, family, directory, filename string) (string, string, []retainedObject, error) {
	if s.inputReader == nil {
		return "", "", nil, fmt.Errorf("churn requires a plan-store reader")
	}
	planGVR, ownerField := schemaPlanResource, "schemaRef"
	if family == "migration" {
		planGVR, ownerField = migrationPlanResource, "migrationRef"
	}
	plans, err := s.dynamic.Resource(planGVR).Namespace(original.GetNamespace()).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", "", nil, err
	}
	objects := []retainedObject{}
	payloads := map[string][]byte{}
	planCount := 0
	for _, plan := range plans.Items {
		owner, _, err := unstructured.NestedString(plan.Object, "spec", ownerField, "uid")
		if err != nil {
			return "", "", nil, err
		}
		if owner != string(original.GetUID()) {
			continue
		}
		ownerName, _, err := unstructured.NestedString(plan.Object, "spec", ownerField, "name")
		if err != nil || plan.GetUID() == "" || plan.GetNamespace() != original.GetNamespace() || ownerName != original.GetName() {
			return "", "", nil, fmt.Errorf("plan export lacks its owner identity")
		}

		planCount++
		objects = append(objects, retainedObject{planGVR, plan.DeepCopy()})
		if family == "migration" {
			continue
		}
		var typed operatorv1alpha1.PtahSchemaPlan
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(plan.Object, &typed); err != nil {
			return "", "", nil, err
		}
		snapshot := exportedChunks{}
		for _, ref := range typed.Spec.Chunks {
			chunk, err := s.dynamic.Resource(planChunkResource).Namespace(plan.GetNamespace()).Get(ctx, ref.Name, metav1.GetOptions{})
			if err != nil {
				return "", "", nil, err
			}
			var captured operatorv1alpha1.PtahSchemaPlanChunk
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(chunk.Object, &captured); err != nil {
				return "", "", nil, err
			}
			snapshot[client.ObjectKeyFromObject(&captured)] = &captured
			objects = append(objects, retainedObject{planChunkResource, chunk})
			projection := &corev1.ConfigMap{}
			err = s.inputReader.Get(ctx, client.ObjectKey{Namespace: plan.GetNamespace(), Name: ref.Name}, projection)
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return "", "", nil, err
			}
			if err := planstore.VerifyProjection(&typed, ref, projection); err != nil {
				return "", "", nil, err
			}
			projection.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}
			value, err := runtime.DefaultUnstructuredConverter.ToUnstructured(projection)
			if err != nil {
				return "", "", nil, err
			}
			objects = append(objects, retainedObject{schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, &unstructured.Unstructured{Object: value}})
		}
		raw, err := (planstore.Store{Reader: snapshot}).Load(ctx, &typed)
		if err != nil {
			return "", "", nil, fmt.Errorf("export %s: %w", plan.GetName(), err)
		}
		payloads[string(plan.GetUID())] = raw
	}
	// A replacement migration already at its desired history may own no
	// plan. Prove that absence explicitly and still require every live pin.
	for _, fields := range [][]string{{"status", "plan"}, {"status", "applied", "planRef"}} {
		pin, exists, err := unstructured.NestedMap(original.Object, fields...)
		if err != nil {
			return "", "", nil, err
		}
		if !exists {
			continue
		}
		found := false
		for _, object := range objects {
			if object.Resource == planGVR && object.Object.GetName() == pin["name"] && string(object.Object.GetUID()) == pin["uid"] {
				found = true
			}
		}
		if !found {
			return "", "", nil, fmt.Errorf("churn export is missing a pinned plan")
		}
	}
	raw, err := json.MarshalIndent(map[string]any{"resource": original, "objects": objects, "verifiedPayloads": payloads, "planCount": planCount, "objectCount": len(objects)}, "", "  ")
	if err != nil {
		return "", "", nil, err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", "", nil, err
	}
	path := filepath.Join(directory, filename)
	output, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", "", nil, err
	}
	_, writeErr := output.Write(raw)
	closeErr := output.Close()
	if writeErr != nil {
		return "", "", nil, writeErr
	}
	if closeErr != nil {
		return "", "", nil, closeErr
	}
	digest := sha256.Sum256(raw)
	return path, hex.EncodeToString(digest[:]), objects, nil
}

func (s *scenarios) churn(ctx context.Context, round int) (err error) {
	start := time.Now().UTC()
	ctx, cancel := context.WithTimeout(ctx, s.load.Settle.Duration)
	defer cancel()
	var targets []batchTarget
	defer func() {
		outcome := map[string]string{"round": fmt.Sprint(round), "replaced": fmt.Sprint(len(targets))}
		if err == nil {
			outcome["converged"] = time.Since(start).String()
		} else {
			outcome["error"] = err.Error()
		}
		s.mark(fmt.Sprintf("soak churn %02d", round), start, outcome)
	}()
	for _, family := range []string{"schema", "migration"} {
		// These unchanged slots keep their actual current artifact. Their new UIDs
		// cannot inherit an old object's status or completed-cycle credit.
		for i := 10 - s.load.Soak.ChurnPerFamily; i < 10; i++ {
			target, err := s.churnOne(ctx, family, i, round)
			if err != nil {
				return err
			}
			targets = append(targets, target)
		}
	}
	return s.waitBatch(ctx, targets, start)
}

func (s *scenarios) churnOne(ctx context.Context, family string, index, round int) (batchTarget, error) {
	resource, name, field, reference := schemaResource, s.schemaName(index), "desired", s.schemaReference(index, round)
	if family == "migration" {
		resource, name, field, reference = migrationResource, s.migrationName(index), "artifact", s.migrationReference(index, round)
	}
	target := batchTarget{family: family, resource: resource, namespace: s.in.namespaceFor(index), name: name, reference: reference}
	objects := s.dynamic.Resource(resource).Namespace(target.namespace)
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return target, err
		}
		original, err := objects.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return target, err
		}
		replacement, err := churnReplacement(original, family, s.load.Name)
		if err != nil {
			if waitErr := waitCapacityPoll(ctx); waitErr != nil {
				return target, fmt.Errorf("churn %s remains unsafe: %w (wait: %v)", name, err, waitErr)
			}
			continue
		}
		current, _, err := unstructured.NestedString(original.Object, "spec", field, "ociRef")
		if err != nil || current != reference {
			return target, fmt.Errorf("churn %s no longer has its declared current artifact", name)
		}
		path, digest, children, err := s.exportChurn(ctx, original, family, round, attempt)
		if err != nil {
			return target, err
		}
		relativePath, err := filepath.Rel(s.evidenceDir, path)
		if err != nil || !filepath.IsLocal(relativePath) {
			return target, fmt.Errorf("churn export escaped its evidence directory")
		}
		uid, rv := original.GetUID(), original.GetResourceVersion()
		proof := churnProof{ObjectsBeforeDeletion: len(children), Round: round, Family: family, Namespace: target.namespace, Name: name, OldUID: string(uid), DeleteSubmittedAt: time.Now().UTC(), ExportPath: relativePath, ExportSHA256: digest}
		err = objects.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}})
		if apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return target, err
		}
		s.churnProofs = append(s.churnProofs, proof)
		record := &s.churnProofs[len(s.churnProofs)-1]
		for {
			current, err := objects.Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				break
			}
			if err != nil {
				return target, err
			}
			if current.GetUID() != uid {
				return target, fmt.Errorf("churn found a foreign replacement of %s", name)
			}
			if err := waitCapacityPoll(ctx); err != nil {
				return target, err
			}
		}
		created, err := objects.Create(ctx, replacement, metav1.CreateOptions{})
		if err != nil {
			return target, err
		}
		if created.GetUID() == "" || created.GetUID() == uid || created.GetCreationTimestamp().Time.IsZero() || !reflect.DeepEqual(created.Object["spec"], replacement.Object["spec"]) {
			return target, fmt.Errorf("churn replacement lost identity or changed spec")
		}
		record.NewUID = string(created.GetUID())
		record.CreatedAt = created.GetCreationTimestamp().Time
		target.uid, target.generation = created.GetUID(), created.GetGeneration()
		for _, child := range children {
			for {
				current, err := s.dynamic.Resource(child.Resource).Namespace(child.Object.GetNamespace()).Get(ctx, child.Object.GetName(), metav1.GetOptions{})
				if apierrors.IsNotFound(err) {
					break
				}
				if err != nil {
					return target, err
				}
				if current.GetUID() != child.Object.GetUID() {
					break
				}
				if err := waitCapacityPoll(ctx); err != nil {
					return target, fmt.Errorf("owner garbage collection left %s/%s: %w", child.Resource.Resource, child.Object.GetName(), err)
				}
			}
		}
		record.GarbageCollected = true
		return target, nil
	}
}

func (s *scenarios) validateChurnEvidence(evidence cycleEvidence) error {
	expected := s.load.Soak.Rounds * s.load.Soak.ChurnPerFamily * 2
	if len(s.churnProofs) != expected {
		return fmt.Errorf("soak retained %d replacements, need %d", len(s.churnProofs), expected)
	}
	lives := map[string]cycleLifetime{}
	for _, life := range evidence.Lifetimes {
		if life.CreatedAt.After(s.soakWindow.End) || life.DeletedSeenAt != nil && life.DeletedSeenAt.Before(s.soakWindow.Start) {
			continue
		}
		if _, duplicate := lives[life.UID]; duplicate {
			return fmt.Errorf("duplicate resource lifetime %s", life.UID)
		}
		lives[life.UID] = life
	}
	seen := map[string]bool{}
	replacementUIDs := map[string]bool{}
	for _, proof := range s.churnProofs {
		if proof.Round < 1 || proof.Round > s.load.Soak.Rounds || proof.Family != "schema" && proof.Family != "migration" ||
			proof.OldUID == "" || proof.NewUID == "" || proof.OldUID == proof.NewUID || !proof.GarbageCollected || !capacityHexDigest.MatchString(proof.ExportSHA256) || !filepath.IsLocal(proof.ExportPath) || proof.ObjectsBeforeDeletion < 0 {
			return fmt.Errorf("incomplete churn proof for %s/%s", proof.Namespace, proof.Name)
		}
		bucketStart := s.soakWindow.Start.Add(time.Duration(proof.Round-1) * s.load.Soak.Cadence.Duration)
		if proof.DeleteSubmittedAt.Before(bucketStart) || !proof.CreatedAt.Before(bucketStart.Add(s.load.Soak.Cadence.Duration)) {
			return fmt.Errorf("churn escaped its declared cadence")
		}
		expectedSlot := false
		for i := 10 - s.load.Soak.ChurnPerFamily; i < 10; i++ {
			name := s.schemaName(i)
			if proof.Family == "migration" {
				name = s.migrationName(i)
			}
			if name == proof.Name && s.in.namespaceFor(i) == proof.Namespace {
				expectedSlot = true
			}
		}
		key := fmt.Sprintf("%d/%s/%s/%s", proof.Round, proof.Family, proof.Namespace, proof.Name)
		if !expectedSlot || seen[key] || replacementUIDs[proof.NewUID] {
			return fmt.Errorf("churn repeated or replaced an undeclared slot: %s", key)
		}
		seen[key] = true
		replacementUIDs[proof.NewUID] = true
		old, oldOK := lives[proof.OldUID]
		next, nextOK := lives[proof.NewUID]
		if !oldOK || !nextOK || old.Family != proof.Family || next.Family != proof.Family || old.Name != proof.Name || next.Name != proof.Name || old.Namespace != proof.Namespace || next.Namespace != proof.Namespace ||
			old.DeletedSeenAt == nil || old.DeletedSeenAt.Before(proof.DeleteSubmittedAt) ||
			!next.CreatedAt.Equal(proof.CreatedAt) || next.CreatedAt.Before(proof.DeleteSubmittedAt.Truncate(time.Second)) {
			return fmt.Errorf("churn %s is not bound to observed old/new lifetimes", key)
		}
	}
	// There must be exactly one original incarnation per slot; every later UID
	// must have a corresponding replacement submitted by this workload.
	originals := map[string]bool{}
	for uid, life := range lives {
		if replacementUIDs[uid] {
			continue
		}
		key := life.Family + "/" + life.Namespace + "/" + life.Name
		if originals[key] || life.FirstSeenAt.After(s.soakWindow.Start) {
			return fmt.Errorf("undeclared resource replacement %s", uid)
		}
		originals[key] = true
	}
	if len(originals) != 20 {
		return fmt.Errorf("soak lacks the twenty original resource lifetimes")
	}
	return nil
}

// Verify the bytes being exported, rather than a separate API reading that
// could differ from the captured objects. Store.Load needs only exact GETs.
type exportedChunks map[types.NamespacedName]*operatorv1alpha1.PtahSchemaPlanChunk

func (s exportedChunks) Get(_ context.Context, key client.ObjectKey, object client.Object, _ ...client.GetOption) error {
	out, ok := object.(*operatorv1alpha1.PtahSchemaPlanChunk)
	if !ok {
		return fmt.Errorf("export reader only holds plan chunks")
	}
	chunk, ok := s[key]
	if !ok {
		return fmt.Errorf("export lacks plan chunk %s", key)
	}
	chunk.DeepCopyInto(out)
	return nil
}

func (s exportedChunks) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return fmt.Errorf("export reader requires exact chunk identities")
}
