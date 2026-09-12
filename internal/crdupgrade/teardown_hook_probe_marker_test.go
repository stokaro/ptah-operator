package crdupgrade

import (
	"testing"
)

// The probe ConfigMap is the one release object Helm is told to keep, so the
// last teardown hook is what removes it. The target it retires must name that
// object and refuse anything that is not the exact one.
func TestHookIdentityProbeMarkerTargetNamesAndVerifiesTheKeptProbe(t *testing.T) {
	rollout, _, _, _ := readyRolloutGuard()

	target, err := HookIdentityProbeMarkerTarget(rollout)
	if err != nil {
		t.Fatalf("HookIdentityProbeMarkerTarget() error = %v", err)
	}
	want := HookIdentityProbeObjectName(rollout.ReleaseNamespace, rollout.ReleaseName, rollout.ReleaseSequence, rollout.ManagerImage)
	if target.Name != want {
		t.Fatalf("probe marker name = %q, want %q", target.Name, want)
	}
	if target.Verify == nil {
		t.Fatal("probe marker target has no verifier")
	}

	probe := predecessorHookProbeObject(rollout)
	probe.UID = "probe-uid"
	probe.ResourceVersion = "42"
	if err := target.Verify(probe); err != nil {
		t.Fatalf("verify of the exact probe object failed: %v", err)
	}

	for name, mutate := range map[string]func(){
		"foreign label":      func() { probe.Labels["app.kubernetes.io/component"] = "something-else" },
		"foreign annotation": func() { probe.Annotations["helm.sh/resource-policy"] = "delete" },
		"changed data":       func() { probe.Data["probe"] = "tampered" },
	} {
		t.Run(name, func(t *testing.T) {
			original := probe.DeepCopy()
			mutate()
			if err := target.Verify(probe); err == nil {
				t.Fatalf("a %s was accepted as the exact probe object", name)
			}
			probe.Labels, probe.Annotations, probe.Data = original.Labels, original.Annotations, original.Data
		})
	}

	if _, err := HookIdentityProbeMarkerTarget(nil); err == nil {
		t.Fatal("a nil rollout produced a probe marker target")
	}
}
