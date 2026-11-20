package workload

import (
	"errors"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/runner"
)

// UnderExecutionBinding is this builder with the executor image and the Ptah
// version binding names in place of its own. It rebuilds the Job of a claim
// made under a binding a rollout has since replaced, while the rotation waits
// for that claim's Apply: the Job is held to the claim it was dispatched
// under, not to the executor this manager would dispatch now. Build refuses
// the claim otherwise, since its binding is stale for this builder.
//
// The manager identity stays this builder's. A caller carries the live Job's
// identity into the rebuild (CarryManagerIdentity), as it does for any Job a
// previous manager dispatched, and holds the template to the claim's admission
// snapshot.
//
// It refuses a binding of another controller-state version or runner
// protocol. Those decide what the claim means and what the runner's frame
// says, and this builder builds and this manager reads only its own.
func (b Builder) UnderExecutionBinding(binding *operatorv1alpha1.ExecutionBindingStatus) (Builder, error) {
	if binding == nil {
		return Builder{}, errors.New("execution binding is required")
	}
	if binding.ControllerStateVersion != b.ControllerStateVersion {
		return Builder{}, fmt.Errorf("execution binding controller state version %d is not this builder's %d",
			binding.ControllerStateVersion, b.ControllerStateVersion)
	}
	if binding.RunnerProtocolVersion != int32(runner.ProtocolVersion) {
		return Builder{}, fmt.Errorf("execution binding runner protocol %d is not this builder's %d",
			binding.RunnerProtocolVersion, runner.ProtocolVersion)
	}
	rebound := b
	rebound.ExecutorImage = binding.ExecutorImage
	rebound.PtahVersion = binding.PtahVersion
	if err := rebound.validate(); err != nil {
		return Builder{}, fmt.Errorf("execution binding: %w", err)
	}
	return rebound, nil
}

// BuildUnderBinding is Build under binding (UnderExecutionBinding).
func (b Builder) BuildUnderBinding(
	binding *operatorv1alpha1.ExecutionBindingStatus,
	schema *operatorv1alpha1.PtahSchema,
	operation operatorv1alpha1.ActiveOperationStatus,
	plan *operatorv1alpha1.PtahSchemaPlan,
) (*batchv1.Job, error) {
	rebound, err := b.UnderExecutionBinding(binding)
	if err != nil {
		return nil, err
	}
	return rebound.Build(schema, operation, plan)
}

// BuildMigrationUnderBinding is BuildMigration under binding
// (UnderExecutionBinding).
func (b Builder) BuildMigrationUnderBinding(
	binding *operatorv1alpha1.ExecutionBindingStatus,
	migration *operatorv1alpha1.PtahMigration,
	operation operatorv1alpha1.MigrationOperationStatus,
	plan *operatorv1alpha1.PtahMigrationPlan,
) (*batchv1.Job, error) {
	rebound, err := b.UnderExecutionBinding(binding)
	if err != nil {
		return nil, err
	}
	return rebound.BuildMigration(migration, operation, plan)
}
