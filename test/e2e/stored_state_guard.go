package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

const storedStateStatusDenial = "Ptah status is written only by the operator's manager; settle an unresolved migration run with a PtahMigrationRunAcknowledgment"

// The installed policy leaves reason unset. Its API-server denial is Invalid
// (422), even though the message retains the admission plugin's "forbidden"
// wording. Attribute the refusal to the exact policy, binding and resource;
// an authorization or schema error cannot substitute for this control.
func storedStateStatusRefused(err error, object client.Object, policy string) bool {
	var response apierrors.APIStatus
	_, resource, _ := storedStateFamily(object)
	if resource == "" || policy == "" || object.GetName() == "" || !apierrors.IsInvalid(err) || !errors.As(err, &response) {
		return false
	}
	status := response.Status()
	denial := fmt.Sprintf("ValidatingAdmissionPolicy '%s' with binding '%s' denied request: %s", policy, policy, storedStateStatusDenial)
	return status.Status == metav1.StatusFailure && status.Code == 422 && status.Reason == metav1.StatusReasonInvalid &&
		status.Details != nil && status.Details.Group == "operator.ptah.run" && status.Details.Kind == resource &&
		status.Details.Name == object.GetName() && strings.HasSuffix(status.Message, denial) &&
		len(status.Details.Causes) == 1 && status.Details.Causes[0].Message == denial
}

func storedStateVersion(object client.Object) (int32, error) {
	switch resource := object.(type) {
	case *ptahv1alpha1.PtahSchema:
		if resource != nil && resource.Status.ExecutionBinding != nil {
			return resource.Status.ExecutionBinding.ControllerStateVersion, nil
		}
	case *ptahv1alpha1.PtahMigration:
		if resource != nil && resource.Status.ExecutionBinding != nil {
			return resource.Status.ExecutionBinding.ControllerStateVersion, nil
		}
	}
	return 0, errors.New("the stored-state control has no execution binding")
}

func storedStateReady(object client.Object, supported int32) error {
	version, err := storedStateVersion(object)
	if err != nil || supported < 1 || version != supported || object.GetUID() == "" || object.GetGeneration() < 1 {
		return errors.New("the stored-state control lacks its exact supported identity")
	}
	switch resource := object.(type) {
	case *ptahv1alpha1.PtahSchema:
		if planAwaitingApproval(resource) && resource.Status.ActiveOperation == nil && resource.Status.PendingLockRelease == nil &&
			resource.Status.PendingObservation == nil && resource.Status.PendingBindingRetirement == nil && !resource.Spec.Suspend &&
			resource.Status.ObservedGeneration == resource.Generation && resource.Status.Plan.UID != "" &&
			resource.Status.Plan.ControllerStateVersion == supported {
			return nil
		}
	case *ptahv1alpha1.PtahMigration:
		if resource.Status.Phase == ptahv1alpha1.MigrationPhaseAwaitingApproval && resource.Status.Plan != nil &&
			resource.Status.Plan.Name != "" && resource.Status.Plan.UID != "" && resource.Status.ObservedGeneration == resource.Generation &&
			resource.Status.ActiveOperation == nil && resource.Status.PendingLockRelease == nil &&
			resource.Status.UnresolvedRun == nil && !resource.Spec.Suspend {
			return nil
		}
	}
	return errors.New("the stored-state control did not settle before dispatch")
}

// The injected field is the only permitted state change. The current manager
// must leave unsupported state uninterpreted, including claims and finalizers.
func storedStateChangedOnlyByVersion(before, current client.Object, version int32) error {
	if _, err := storedStateVersion(before); err != nil {
		return err
	}
	if _, err := storedStateVersion(current); err != nil {
		return err
	}
	if version < 1 || before.GetName() == "" || before.GetNamespace() == "" || before.GetUID() == "" ||
		before.GetName() != current.GetName() || before.GetNamespace() != current.GetNamespace() || before.GetUID() != current.GetUID() ||
		before.GetGeneration() != current.GetGeneration() || !equality.Semantic.DeepEqual(before.GetFinalizers(), current.GetFinalizers()) ||
		!equality.Semantic.DeepEqual(before.GetOwnerReferences(), current.GetOwnerReferences()) ||
		!equality.Semantic.DeepEqual(before.GetDeletionTimestamp(), current.GetDeletionTimestamp()) {
		return errors.New("the unsupported-state resource changed its identity, finalizers or deletion boundary")
	}
	switch original := before.(type) {
	case *ptahv1alpha1.PtahSchema:
		after, ok := current.(*ptahv1alpha1.PtahSchema)
		if ok {
			want := original.DeepCopy()
			want.Status.ExecutionBinding.ControllerStateVersion = version
			if equality.Semantic.DeepEqual(want.Spec, after.Spec) && equality.Semantic.DeepEqual(want.Status, after.Status) {
				return nil
			}
		}
	case *ptahv1alpha1.PtahMigration:
		after, ok := current.(*ptahv1alpha1.PtahMigration)
		if ok {
			want := original.DeepCopy()
			want.Status.ExecutionBinding.ControllerStateVersion = version
			if equality.Semantic.DeepEqual(want.Spec, after.Spec) && equality.Semantic.DeepEqual(want.Status, after.Status) {
				return nil
			}
		}
	}
	return errors.New("the unsupported-state manager interpreted or changed the held status or work")
}

func storedStatePatch(object client.Object, from, to int32) ([]byte, error) {
	version, err := storedStateVersion(object)
	if err != nil || version != from || from < 1 || to < 1 || from == to || object.GetResourceVersion() == "" {
		return nil, errors.New("the stored-state patch lacks its exact version and API boundary")
	}
	return json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/resourceVersion", "value": object.GetResourceVersion()},
		{"op": "test", "path": "/status/executionBinding/controllerStateVersion", "value": from},
		{"op": "replace", "path": "/status/executionBinding/controllerStateVersion", "value": to},
	})
}

func storedStateApprovalUnconsumed(before, current client.Object) error {
	unchanged := false
	switch original := before.(type) {
	case *ptahv1alpha1.PtahSchemaApproval:
		if after, ok := current.(*ptahv1alpha1.PtahSchemaApproval); ok && original != nil && after != nil && equality.Semantic.DeepEqual(original.Spec, after.Spec) &&
			!conditionStatus(after.Status.Conditions, "Consumed", metav1.ConditionTrue) {
			unchanged = true
		}
	case *ptahv1alpha1.PtahMigrationApproval:
		if after, ok := current.(*ptahv1alpha1.PtahMigrationApproval); ok && original != nil && after != nil && equality.Semantic.DeepEqual(original.Spec, after.Spec) &&
			!conditionStatus(after.Status.Conditions, "Consumed", metav1.ConditionTrue) {
			unchanged = true
		}
	}
	if !unchanged {
		return errors.New("the unsupported state changed or consumed its admitted approval")
	}
	if before.GetUID() == "" || before.GetUID() != current.GetUID() || before.GetName() != current.GetName() ||
		before.GetNamespace() != current.GetNamespace() || before.GetGeneration() != current.GetGeneration() {
		return errors.New("the unsupported-state control lost its exact admitted approval")
	}
	return nil
}

// An extra impersonated group gets one status PATCH; the real manager never
// receives that group. Installed status admission still checks its username.
func storedStateWriterObjects(namespace, name, resource, target, group string) (*rbacv1.Role, *rbacv1.RoleBinding, error) {
	if namespace == "" || name == "" || target == "" || group == "" ||
		!slices.Contains([]string{"ptahschemas", "ptahmigrations"}, resource) {
		return nil, nil, errors.New("the injection writer needs one exact resource and private group")
	}
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{"operator.ptah.run"}, Resources: []string{resource + "/status"},
		ResourceNames: []string{target}, Verbs: []string{"patch"},
	}}}
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
		Subjects: []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "Group", Name: group}}}
	return role, binding, nil
}

func storedStateGuardMessage(kind string, future, supported int32) string {
	return fmt.Sprintf("stored status.executionBinding controller state version %d exceeds supported version %d; refusing to interpret or write %s state", future, supported, kind)
}

// A console prefix is permitted; the context itself must identify the exact
// controller request and refusal. Distinct reconcile IDs prove repeated entry.
func storedStateGuardReconciliations(logs []byte, namespace, name, kind string, future, supported int32) (map[string]bool, error) {
	if namespace == "" || name == "" || !slices.Contains([]string{"PtahSchema", "PtahMigration"}, kind) || supported < 1 || future <= supported {
		return nil, errors.New("the guard log predicate has no exact unsupported-state request")
	}
	ids := map[string]bool{}
	for _, line := range bytes.Split(logs, []byte{'\n'}) {
		start := bytes.IndexByte(line, '{')
		if start < 0 {
			continue
		}
		var entry struct {
			Namespace, Name, ControllerKind, ControllerGroup, ReconcileID, Error string
		}
		if json.Unmarshal(line[start:], &entry) == nil && entry.Namespace == namespace && entry.Name == name &&
			entry.ControllerKind == kind && entry.ControllerGroup == "operator.ptah.run" && entry.ReconcileID != "" &&
			entry.Error == storedStateGuardMessage(kind, future, supported) {
			ids[entry.ReconcileID] = true
		}
	}
	return ids, nil
}

func storedStateHeldHistory[T client.Object](events []watchEvent[T], before client.Object, injected, end string, future int32) error {
	if injected == "" || end == "" || injected == end || before == nil || before.GetUID() == "" {
		return errors.New("the stored-state history has no distinct exact API boundaries")
	}
	started, ended := false, false
	for _, event := range events {
		switch resource := any(event.Object).(type) {
		case *ptahv1alpha1.PtahSchema:
			if resource == nil {
				return errors.New("the stored-state history has a nil schema event")
			}
		case *ptahv1alpha1.PtahMigration:
			if resource == nil {
				return errors.New("the stored-state history has a nil migration event")
			}
		default:
			return errors.New("the stored-state history has another resource kind")
		}
		if event.Object.GetUID() != before.GetUID() {
			continue
		}
		if event.Object.GetResourceVersion() == injected {
			started = true
		}
		if !started {
			continue
		}
		if event.Type != watch.Modified && event.Type != watch.Added {
			return errors.New("the unsupported-state resource was deleted or emitted an invalid event")
		}
		if err := storedStateChangedOnlyByVersion(before, event.Object, future); err != nil {
			return err
		}
		if event.Object.GetResourceVersion() == end {
			ended = true
		}
	}
	if ended {
		return nil
	}
	return errors.New("the stored-state history did not reach both exact API boundaries")
}

func storedStateCreatedNoWork(jobs []watchEvent[*batchv1.Job], pods []watchEvent[*corev1.Pod], object client.Object, familyLabel string) bool {
	if object == nil || object.GetUID() == "" || len(jobs) == 0 || len(pods) == 0 ||
		!slices.Contains([]string{labelSchema, labelMigration}, familyLabel) {
		return false
	}
	for _, event := range jobs {
		if event.Object == nil {
			return false
		}
		if event.Type == watch.Added && (event.Object.Labels[familyLabel] == object.GetName() ||
			slices.ContainsFunc(event.Object.OwnerReferences, func(owner metav1.OwnerReference) bool { return owner.UID == object.GetUID() })) {
			return false
		}
	}
	for _, event := range pods {
		if event.Object == nil {
			return false
		}
		if event.Type == watch.Added && event.Object.Labels[familyLabel] == object.GetName() {
			return false
		}
	}
	return true
}

func storedStateFamily(object client.Object) (kind, resource, familyLabel string) {
	switch object := object.(type) {
	case *ptahv1alpha1.PtahSchema:
		if object == nil {
			return "", "", ""
		}
		return "PtahSchema", "ptahschemas", labelSchema
	case *ptahv1alpha1.PtahMigration:
		if object == nil {
			return "", "", ""
		}
		return "PtahMigration", "ptahmigrations", labelMigration
	default:
		return "", "", ""
	}
}

func storedStateManagerIdentity(user string) bool {
	parts := strings.Split(user, ":")
	return len(parts) == 4 && parts[0] == "system" && parts[1] == "serviceaccount" && parts[2] != "" && parts[3] != ""
}

func storedStateHistorySQLControl(clients map[string]operationSQLClient, counts map[string]int, expected operationSQLClient) error {
	if expected.operation != "history" || expected.resourceUID == "" || expected.jobUID == "" || expected.podUID == "" {
		return errors.New("the stored-state SQL control has no exact History actor")
	}
	matches := 0
	for host, actor := range clients {
		if actor == expected && counts[host] > 0 {
			matches++
		}
	}
	if matches != 1 {
		return errors.New("the stored-state audit did not receive SQL from exactly one matching History actor")
	}
	return nil
}
