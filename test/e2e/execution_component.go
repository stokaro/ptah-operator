package e2e

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

// Change one declared component while retaining the exact work and all other
// execution contracts. A version declaration uses the same pinned build with
// or without its v prefix; it measures binding invalidation, not CLI migration.
type executionComponentChange struct {
	argument, original, replacement string
}

func ptahVersionAlias(version string) (string, error) {
	alias := "v" + version
	if strings.HasPrefix(version, "v") {
		alias = strings.TrimPrefix(version, "v")
	}
	if version == "" || alias == "" || len(version) > 128 || len(alias) > 128 ||
		strings.HasPrefix(version, "vv") || !utf8.ValidString(version) {
		return "", errors.New("the version-binding control needs an unambiguous pinned build declaration")
	}
	for _, character := range version {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return "", errors.New("the version-binding control contains an ambiguous character")
		}
	}
	return alias, nil
}

func (c executionComponentChange) valid() bool {
	if c.original == c.replacement || c.original == "" || c.replacement == "" {
		return false
	}
	switch c.argument {
	case "executor-image", "runner-image":
		return digestSuffix.MatchString(c.original) && digestSuffix.MatchString(c.replacement)
	case "ptah-version":
		alias, err := ptahVersionAlias(c.original)
		return err == nil && alias == c.replacement
	default:
		return false
	}
}

func (c executionComponentChange) reverse() executionComponentChange {
	return executionComponentChange{argument: c.argument, original: c.replacement, replacement: c.original}
}

func (c executionComponentChange) binding(before, after *ptahv1alpha1.ExecutionBindingStatus) bool {
	if !c.valid() || before == nil || after == nil || !executionEpoch.MatchString(before.Epoch) ||
		!executionEpoch.MatchString(after.Epoch) || before.Epoch == after.Epoch {
		return false
	}
	want := before.DeepCopy()
	want.Epoch = after.Epoch
	switch c.argument {
	case "executor-image":
		if before.ExecutorImage != c.original {
			return false
		}
		want.ExecutorImage = c.replacement
	case "ptah-version":
		if before.PtahVersion != c.original {
			return false
		}
		want.PtahVersion = c.replacement
	default:
		// The runner image is recorded, not an execution-binding component.
		return false
	}
	return equality.Semantic.DeepEqual(want, after)
}

func replaceControllerExecutionComponent(deployment *appsv1.Deployment, change executionComponentChange) error {
	if deployment == nil || deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType || !change.valid() {
		return errors.New("execution transition needs a valid component change and Recreate manager Deployment")
	}
	managers, arguments := 0, 0
	prefix := "--" + change.argument + "="
	for i := range deployment.Spec.Template.Spec.Containers {
		container := &deployment.Spec.Template.Spec.Containers[i]
		if container.Name != "manager" {
			continue
		}
		managers++
		for j, value := range container.Args {
			if strings.HasPrefix(value, prefix) {
				arguments++
				if value != prefix+change.original && value != prefix+change.replacement {
					return errors.New("the manager component changed outside the controlled rollout")
				}
				container.Args[j] = prefix + change.replacement
			}
		}
	}
	if managers != 1 || arguments != 1 {
		return errors.New("execution transition needs exactly one manager and component argument")
	}
	return nil
}
