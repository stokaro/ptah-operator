package crdupgrade

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestWorkloadInventoryVerifyHookBootstrap(t *testing.T) {
	fixture := newHookInventoryFixture()
	fixture.jobs.result.Items = append(fixture.jobs.result.Items, batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: fixture.guard.ReleaseNamespace},
		Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: "default"}}},
	})
	fixture.pods.result.Items = append(fixture.pods.result.Items, corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated-pod", Namespace: fixture.guard.ReleaseNamespace},
		Spec:       corev1.PodSpec{ServiceAccountName: "default"},
	})

	if err := fixture.inventory.VerifyHookBootstrap(context.Background()); err != nil {
		t.Fatalf("VerifyHookBootstrap() error = %v", err)
	}
	if len(fixture.jobs.options) != 1 || fixture.jobs.options[0].Limit != workloadInventoryPageSize || fixture.jobs.options[0].Continue != "" {
		t.Fatalf("Job List options = %#v, want one complete initial request", fixture.jobs.options)
	}
	if len(fixture.pods.options) != 1 || fixture.pods.options[0].Limit != workloadInventoryPageSize || fixture.pods.options[0].LabelSelector != "" {
		t.Fatalf("Pod List options = %#v, want one complete namespace-wide request", fixture.pods.options)
	}
}

func TestWorkloadInventoryExhaustsLargePaginatedHookInventory(t *testing.T) {
	fixture := newHookInventoryFixture()
	identityJob := fixture.jobs.result.Items[0]
	identityPod := fixture.pods.result.Items[0]

	jobs := make([]batchv1.Job, 301)
	for index := 0; index < 300; index++ {
		jobs[index] = batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("unrelated-job-%03d", index),
				Namespace: fixture.guard.ReleaseNamespace,
				UID:       types.UID(fmt.Sprintf("unrelated-job-%03d", index)),
			},
			Spec: batchv1.JobSpec{Template: corev1PodTemplate("default")},
		}
	}
	jobs[300] = identityJob
	fixture.jobs.pages = map[string]*batchv1.JobList{
		"": {
			ListMeta: workloadInventoryListMeta("hook-rv", "job-next", 1),
			Items:    jobs[:300],
		},
		"job-next": {
			ListMeta: workloadInventoryListMeta("hook-rv", "", 0),
			Items:    jobs[300:],
		},
	}

	pods := make([]corev1.Pod, 1026)
	for index := 0; index < 1025; index++ {
		pods[index] = corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("unrelated-pod-%04d", index),
				Namespace: fixture.guard.ReleaseNamespace,
				UID:       types.UID(fmt.Sprintf("unrelated-pod-%04d", index)),
			},
			Spec: corev1.PodSpec{ServiceAccountName: "default"},
		}
	}
	pods[1025] = identityPod
	fixture.pods.pages = map[string]*corev1.PodList{
		"": {
			ListMeta: workloadInventoryListMeta("hook-rv", "pod-next-1", 526),
			Items:    pods[:500],
		},
		"pod-next-1": {
			ListMeta: workloadInventoryListMeta("hook-rv", "pod-next-2", 26),
			Items:    pods[500:1000],
		},
		"pod-next-2": {
			ListMeta: workloadInventoryListMeta("hook-rv", "", 0),
			Items:    pods[1000:],
		},
	}

	if err := fixture.inventory.VerifyHookBootstrap(context.Background()); err != nil {
		t.Fatalf("VerifyHookBootstrap() rejected complete paginated inventory: %v", err)
	}
	assertWorkloadInventoryListOptions(t, "Job", fixture.jobs.options, []string{"", "job-next"})
	assertWorkloadInventoryListOptions(t, "Pod", fixture.pods.options, []string{"", "pod-next-1", "pod-next-2"})
}

func TestWorkloadInventoryExhaustsLargePaginatedReplicaSetInventory(t *testing.T) {
	fixture := newRuntimeInventoryFixture()
	replicaSets := make([]appsv1.ReplicaSet, 0, 515)
	for _, replicaSet := range fixture.replicaSets.objects {
		replicaSets = append(replicaSets, *replicaSet.DeepCopy())
	}
	for index := 0; index < 513; index++ {
		replicaSets = append(replicaSets, appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("unrelated-rs-%03d", index),
				Namespace: fixture.guard.ReleaseNamespace,
				UID:       types.UID(fmt.Sprintf("unrelated-rs-%03d", index)),
			},
			Spec: appsv1.ReplicaSetSpec{Template: corev1PodTemplate("default")},
		})
	}
	fixture.replicaSets.pages = map[string]*appsv1.ReplicaSetList{
		"": {
			ListMeta: workloadInventoryListMeta("runtime-rv", "rs-next", int64(len(replicaSets)-500)),
			Items:    replicaSets[:500],
		},
		"rs-next": {
			ListMeta: workloadInventoryListMeta("runtime-rv", "", 0),
			Items:    replicaSets[500:],
		},
	}

	if err := fixture.inventory.VerifyRuntimeBeforeQuiesce(context.Background()); err != nil {
		t.Fatalf("VerifyRuntimeBeforeQuiesce() rejected complete paginated inventory: %v", err)
	}
	assertWorkloadInventoryListOptions(t, "ReplicaSet", fixture.replicaSets.options, []string{"", "rs-next"})
}

func TestWorkloadInventoryEvaluatesEveryPaginatedObject(t *testing.T) {
	t.Run("pre-staged Job on final page", func(t *testing.T) {
		fixture := newHookInventoryFixture()
		first := fixture.jobs.result.Items[0]
		futureServiceAccount := strings.Split(fixture.guard.HookServiceAccountName, "-crd-v")[0] + "-crd-v2-0123456789ab"
		fixture.jobs.pages = map[string]*batchv1.JobList{
			"": {
				ListMeta: workloadInventoryListMeta("1", "next", 1),
				Items:    []batchv1.Job{first},
			},
			"next": {
				ListMeta: workloadInventoryListMeta("1", "", 0),
				Items: []batchv1.Job{{
					ObjectMeta: metav1.ObjectMeta{Name: "future", Namespace: fixture.guard.ReleaseNamespace, UID: "future"},
					Spec:       batchv1.JobSpec{Template: corev1PodTemplate(futureServiceAccount)},
				}},
			},
		}

		err := fixture.inventory.VerifyHookBootstrap(context.Background())
		if err == nil || !strings.Contains(err.Error(), "pre-staged hook Job") {
			t.Fatalf("VerifyHookBootstrap() error = %v, want final-page Job rejection", err)
		}
	})

	t.Run("pre-staged Pod on final page", func(t *testing.T) {
		fixture := newHookInventoryFixture()
		first := fixture.pods.result.Items[0]
		futureServiceAccount := strings.Split(fixture.guard.HookServiceAccountName, "-crd-v")[0] + "-crd-v2-0123456789ab"
		fixture.pods.pages = map[string]*corev1.PodList{
			"": {
				ListMeta: workloadInventoryListMeta("1", "next", 1),
				Items:    []corev1.Pod{first},
			},
			"next": {
				ListMeta: workloadInventoryListMeta("1", "", 0),
				Items: []corev1.Pod{{
					ObjectMeta: metav1.ObjectMeta{Name: "future", Namespace: fixture.guard.ReleaseNamespace, UID: "future"},
					Spec:       corev1.PodSpec{ServiceAccountName: futureServiceAccount},
				}},
			},
		}

		err := fixture.inventory.VerifyHookBootstrap(context.Background())
		if err == nil || !strings.Contains(err.Error(), "protected non-candidate ServiceAccount") {
			t.Fatalf("VerifyHookBootstrap() error = %v, want final-page Pod rejection", err)
		}
	})

	t.Run("protected ReplicaSet on final page", func(t *testing.T) {
		fixture := newRuntimeInventoryFixture()
		protected := make([]appsv1.ReplicaSet, 0, len(fixture.replicaSets.objects))
		for _, replicaSet := range fixture.replicaSets.objects {
			protected = append(protected, *replicaSet.DeepCopy())
		}
		fixture.replicaSets.pages = map[string]*appsv1.ReplicaSetList{
			"": {
				ListMeta: workloadInventoryListMeta("1", "next", int64(len(protected))),
				Items: []appsv1.ReplicaSet{{
					ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: fixture.guard.ReleaseNamespace, UID: "unrelated"},
					Spec:       appsv1.ReplicaSetSpec{Template: corev1PodTemplate("default")},
				}},
			},
			"next": {
				ListMeta: workloadInventoryListMeta("1", "", 0),
				Items:    protected,
			},
		}

		if err := fixture.inventory.VerifyRuntimeBeforeQuiesce(context.Background()); err != nil {
			t.Fatalf("VerifyRuntimeBeforeQuiesce() did not evaluate valid final-page ReplicaSet: %v", err)
		}
	})
}

func TestWorkloadInventoryRejectsPreStagedHookObjects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*hookInventoryFixture)
		want   string
	}{
		{
			name: "future Job",
			mutate: func(f *hookInventoryFixture) {
				futureServiceAccount := strings.Split(f.guard.HookServiceAccountName, "-crd-v")[0] + "-crd-v2-0123456789ab"
				f.jobs.result.Items = append(f.jobs.result.Items, batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{Name: "future-hook", Namespace: f.guard.ReleaseNamespace, UID: "future-job"},
					Spec:       batchv1.JobSpec{Template: corev1PodTemplate(futureServiceAccount)},
				})
			},
			want: "pre-staged hook Job",
		},
		{
			name: "future Pod",
			mutate: func(f *hookInventoryFixture) {
				futureServiceAccount := strings.Split(f.guard.HookServiceAccountName, "-crd-v")[0] + "-crd-v2-0123456789ab"
				f.pods.result.Items = append(f.pods.result.Items, corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "future-hook-pod", Namespace: f.guard.ReleaseNamespace},
					Spec:       corev1.PodSpec{ServiceAccountName: futureServiceAccount},
				})
			},
			want: "protected non-candidate ServiceAccount",
		},
		{
			name: "pre-staged cleanup Job",
			mutate: func(f *hookInventoryFixture) {
				cleanupServiceAccount := strings.Split(f.guard.HookServiceAccountName, "-crd-v")[0] + "-cleanup-v1-0123456789ab"
				f.jobs.result.Items = append(f.jobs.result.Items, batchv1.Job{
					ObjectMeta: metav1.ObjectMeta{Name: "cleanup-hook", Namespace: f.guard.ReleaseNamespace, UID: "cleanup-job"},
					Spec:       batchv1.JobSpec{Template: corev1PodTemplate(cleanupServiceAccount)},
				})
			},
			want: "pre-staged hook Job",
		},
		{
			name: "pre-staged cleanup Pod",
			mutate: func(f *hookInventoryFixture) {
				cleanupServiceAccount := strings.Split(f.guard.HookServiceAccountName, "-crd-v")[0] + "-cleanup-v1-0123456789ab"
				f.pods.result.Items = append(f.pods.result.Items, corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "cleanup-hook-pod", Namespace: f.guard.ReleaseNamespace},
					Spec:       corev1.PodSpec{ServiceAccountName: cleanupServiceAccount},
				})
			},
			want: "protected non-candidate ServiceAccount",
		},
		{
			name: "direct Pod",
			mutate: func(f *hookInventoryFixture) {
				f.pods.result.Items[0].OwnerReferences = nil
			},
			want: "exactly one Job owner reference",
		},
		{
			name: "wrong controller UID label",
			mutate: func(f *hookInventoryFixture) {
				f.pods.result.Items[0].Labels[batchv1.ControllerUidLabel] = "foreign"
			},
			want: batchv1.ControllerUidLabel,
		},
		{
			name: "wrong generated name",
			mutate: func(f *hookInventoryFixture) {
				f.pods.result.Items[0].GenerateName = "foreign-"
			},
			want: "do not link to parent prefix",
		},
		{
			name: "extra candidate Pod",
			mutate: func(f *hookInventoryFixture) {
				duplicate := f.pods.result.Items[0].DeepCopy()
				duplicate.Name = f.jobs.result.Items[0].Name + "-def34"
				duplicate.UID = "identity-pod-two"
				f.pods.result.Items = append(f.pods.result.Items, *duplicate)
			},
			want: "more than one protected Pod",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newHookInventoryFixture()
			test.mutate(fixture)
			err := fixture.inventory.VerifyHookBootstrap(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("VerifyHookBootstrap() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestWorkloadInventoryHookBootstrapFailsClosedOnLists(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*hookInventoryFixture)
		want   string
	}{
		{name: "nil Jobs", mutate: func(f *hookInventoryFixture) { f.jobs.result = nil }, want: "nil page"},
		{name: "repeated Jobs continuation", mutate: func(f *hookInventoryFixture) { f.jobs.result.Continue = "next" }, want: "repeated current continue token"},
		{name: "remaining Jobs without continuation", mutate: func(f *hookInventoryFixture) { remaining := int64(1); f.jobs.result.RemainingItemCount = &remaining }, want: "malformed remainingItemCount"},
		{name: "nil Pods", mutate: func(f *hookInventoryFixture) { f.pods.result = nil }, want: "nil page"},
		{name: "foreign Pod", mutate: func(f *hookInventoryFixture) { f.pods.result.Items[0].Namespace = "foreign" }, want: "foreign or incomplete Pod"},
		{name: "Job API error", mutate: func(f *hookInventoryFixture) { f.jobs.err = errors.New("unavailable") }, want: "unavailable"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newHookInventoryFixture()
			test.mutate(fixture)
			err := fixture.inventory.VerifyHookBootstrap(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("VerifyHookBootstrap() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestWorkloadInventoryPageStateRejectsMalformedPagination(t *testing.T) {
	tests := []struct {
		name string
		run  func(*inventoryPageState) error
		want string
	}{
		{
			name: "sharded subset",
			run: func(state *inventoryPageState) error {
				return state.validatePage("Pod", "", metav1.ListMeta{ShardInfo: &metav1.ShardInfo{Selector: "0/2"}}, 1, workloadInventoryPageSize)
			},
			want: "sharded subset",
		},
		{
			name: "oversized page",
			run: func(state *inventoryPageState) error {
				return state.validatePage("Pod", "", metav1.ListMeta{}, int(workloadInventoryPageSize)+1, workloadInventoryPageSize)
			},
			want: "exceeding requested limit 500",
		},
		{
			name: "empty page returning continuation",
			run: func(state *inventoryPageState) error {
				return state.validatePage("Pod", "", metav1.ListMeta{Continue: "next"}, 0, workloadInventoryPageSize)
			},
			want: "empty continued page",
		},
		{
			name: "empty fetched continuation",
			run: func(state *inventoryPageState) error {
				return state.validatePage("Pod", "next", metav1.ListMeta{}, 0, workloadInventoryPageSize)
			},
			want: "empty continued page",
		},
		{
			name: "resource version changes",
			run: func(state *inventoryPageState) error {
				if err := state.validatePage("Pod", "", metav1.ListMeta{ResourceVersion: "1", Continue: "next"}, 1, workloadInventoryPageSize); err != nil {
					return err
				}
				return state.validatePage("Pod", "next", metav1.ListMeta{ResourceVersion: "2"}, 1, workloadInventoryPageSize)
			},
			want: "resourceVersion changed",
		},
		{
			name: "negative remaining count",
			run: func(state *inventoryPageState) error {
				remaining := int64(-1)
				return state.validatePage("Pod", "", metav1.ListMeta{RemainingItemCount: &remaining}, 1, workloadInventoryPageSize)
			},
			want: "malformed remainingItemCount -1",
		},
		{
			name: "zero remaining count with continuation",
			run: func(state *inventoryPageState) error {
				remaining := int64(0)
				return state.validatePage("Pod", "", metav1.ListMeta{Continue: "next", RemainingItemCount: &remaining}, 1, workloadInventoryPageSize)
			},
			want: "malformed remainingItemCount 0",
		},
		{
			name: "repeated current token",
			run: func(state *inventoryPageState) error {
				if err := state.validatePage("Pod", "", metav1.ListMeta{Continue: "next"}, 1, workloadInventoryPageSize); err != nil {
					return err
				}
				return state.validatePage("Pod", "next", metav1.ListMeta{Continue: "next"}, 1, workloadInventoryPageSize)
			},
			want: "repeated current continue token",
		},
		{
			name: "repeated prior token",
			run: func(state *inventoryPageState) error {
				if err := state.validatePage("Pod", "", metav1.ListMeta{Continue: "first"}, 1, workloadInventoryPageSize); err != nil {
					return err
				}
				if err := state.validatePage("Pod", "first", metav1.ListMeta{Continue: "second"}, 1, workloadInventoryPageSize); err != nil {
					return err
				}
				return state.validatePage("Pod", "second", metav1.ListMeta{Continue: "first"}, 1, workloadInventoryPageSize)
			},
			want: "repeated prior continue token",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.run(newInventoryPageState())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("pagination error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestWorkloadInventoryPageStateRejectsDuplicateObjects(t *testing.T) {
	t.Run("namespace and name", func(t *testing.T) {
		state := newInventoryPageState()
		if err := state.observeObject("Pod", "operators", "same", "first"); err != nil {
			t.Fatalf("observe first object: %v", err)
		}
		err := state.observeObject("Pod", "operators", "same", "second")
		if err == nil || !strings.Contains(err.Error(), "operators/same more than once") {
			t.Fatalf("duplicate name error = %v", err)
		}
	})

	t.Run("UID when present", func(t *testing.T) {
		state := newInventoryPageState()
		if err := state.observeObject("Pod", "operators", "first", "shared"); err != nil {
			t.Fatalf("observe first object: %v", err)
		}
		err := state.observeObject("Pod", "operators", "second", "shared")
		if err == nil || !strings.Contains(err.Error(), "share UID shared") {
			t.Fatalf("duplicate UID error = %v", err)
		}
	})

	t.Run("empty UIDs are not identities", func(t *testing.T) {
		state := newInventoryPageState()
		if err := state.observeObject("Pod", "operators", "first", ""); err != nil {
			t.Fatalf("observe first object: %v", err)
		}
		if err := state.observeObject("Pod", "operators", "second", ""); err != nil {
			t.Fatalf("observe second object: %v", err)
		}
	})
}

func TestWorkloadInventoryRejectsNilContinuedPages(t *testing.T) {
	t.Run("Jobs", func(t *testing.T) {
		fixture := newHookInventoryFixture()
		first := fixture.jobs.result.DeepCopy()
		first.ResourceVersion = "1"
		first.Continue = "next"
		fixture.jobs.pages = map[string]*batchv1.JobList{"": first, "next": nil}
		err := fixture.inventory.VerifyHookBootstrap(context.Background())
		if err == nil || !strings.Contains(err.Error(), "Job inventory returned a nil page") {
			t.Fatalf("VerifyHookBootstrap() error = %v", err)
		}
	})

	t.Run("Pods", func(t *testing.T) {
		fixture := newHookInventoryFixture()
		first := fixture.pods.result.DeepCopy()
		first.ResourceVersion = "1"
		first.Continue = "next"
		fixture.pods.pages = map[string]*corev1.PodList{"": first, "next": nil}
		err := fixture.inventory.VerifyHookBootstrap(context.Background())
		if err == nil || !strings.Contains(err.Error(), "returned a nil page") {
			t.Fatalf("VerifyHookBootstrap() error = %v", err)
		}
	})

	t.Run("ReplicaSets", func(t *testing.T) {
		fixture := newRuntimeInventoryFixture()
		first := fixture.replicaSets.objectList()
		first.ResourceVersion = "1"
		first.Continue = "next"
		fixture.replicaSets.pages = map[string]*appsv1.ReplicaSetList{"": first, "next": nil}
		err := fixture.inventory.VerifyRuntimeBeforeQuiesce(context.Background())
		if err == nil || !strings.Contains(err.Error(), "returned a nil page") {
			t.Fatalf("VerifyRuntimeBeforeQuiesce() error = %v", err)
		}
	})
}

func TestWorkloadInventoryVerifyRuntimeBeforeQuiesce(t *testing.T) {
	fixture := newRuntimeInventoryFixture()
	fixture.pods.result.Items = append(fixture.pods.result.Items, corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "database", Namespace: fixture.guard.ReleaseNamespace},
		Spec:       corev1.PodSpec{ServiceAccountName: "database"},
	})

	if err := fixture.inventory.VerifyRuntimeBeforeQuiesce(context.Background()); err != nil {
		t.Fatalf("VerifyRuntimeBeforeQuiesce() error = %v", err)
	}
	if len(fixture.replicaSets.options) != 1 || fixture.replicaSets.options[0].Limit != workloadInventoryPageSize || fixture.replicaSets.options[0].LabelSelector != "" {
		t.Fatalf("ReplicaSet List options = %#v, want one complete namespace-wide request", fixture.replicaSets.options)
	}
	if len(fixture.deployments.gets) != 2 {
		t.Fatalf("Deployment Get calls = %v, want both fixed runtime roots", fixture.deployments.gets)
	}
}

func TestWorkloadInventoryVerifiesDormantRuntimeReplicaSets(t *testing.T) {
	fixture := newRuntimeInventoryFixture()
	currentName := fixture.pods.result.Items[0].OwnerReferences[0].Name
	dormant := fixture.replicaSets.objects[currentName].DeepCopy()
	oldHash := "5a6b7c8d9f"
	dormant.Name = fixture.guard.ControllerDeploymentName + "-" + oldHash
	dormant.UID = types.UID(dormant.Name + "-uid")
	dormant.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = oldHash
	dormant.Spec.Selector.MatchLabels[appsv1.DefaultDeploymentUniqueLabelKey] = oldHash
	dormant.Spec.Template.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = oldHash
	dormant.Labels["example.test/runtime-label"] = "older-controller"
	dormant.Spec.Template.Labels["example.test/runtime-label"] = "older-controller"
	zero := int32(0)
	dormant.Spec.Replicas = &zero
	fixture.replicaSets.objects[dormant.Name] = dormant

	if err := fixture.inventory.VerifyRuntimeBeforeQuiesce(context.Background()); err != nil {
		t.Fatalf("VerifyRuntimeBeforeQuiesce() dormant exact chain error = %v", err)
	}

	dormant.OwnerReferences[0].UID = "forged-deployment"
	if err := fixture.inventory.VerifyRuntimeBeforeQuiesce(context.Background()); err == nil || !strings.Contains(err.Error(), dormant.Name) {
		t.Fatalf("VerifyRuntimeBeforeQuiesce() forged dormant error = %v, want named rejection", err)
	}
}

func TestWorkloadInventoryRuntimeReplicaSetListFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*runtimeInventoryFixture)
		want   string
	}{
		{
			name: "nil result",
			mutate: func(f *runtimeInventoryFixture) {
				f.replicaSets.nilList = true
			},
			want: "nil page",
		},
		{
			name: "continued result",
			mutate: func(f *runtimeInventoryFixture) {
				f.replicaSets.list = f.replicaSets.objectList()
				f.replicaSets.list.Continue = "next"
			},
			want: "repeated current continue token",
		},
		{
			name: "API error",
			mutate: func(f *runtimeInventoryFixture) {
				f.replicaSets.listErr = errors.New("unavailable")
			},
			want: "unavailable",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRuntimeInventoryFixture()
			test.mutate(fixture)
			err := fixture.inventory.VerifyRuntimeBeforeQuiesce(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("VerifyRuntimeBeforeQuiesce() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestWorkloadInventoryRejectsUnsafeRuntimeParentChains(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*runtimeInventoryFixture)
		want   string
	}{
		{
			name: "direct Pod",
			mutate: func(f *runtimeInventoryFixture) {
				f.pods.result.Items[0].OwnerReferences = nil
			},
			want: "exactly one ReplicaSet owner reference",
		},
		{
			name: "stale ReplicaSet UID",
			mutate: func(f *runtimeInventoryFixture) {
				f.pods.result.Items[0].OwnerReferences[0].UID = "stale"
			},
			want: "foreign or stale ReplicaSet",
		},
		{
			name: "forged Deployment UID",
			mutate: func(f *runtimeInventoryFixture) {
				replicaSet := f.replicaSets.objects[f.pods.result.Items[0].OwnerReferences[0].Name]
				replicaSet.OwnerReferences[0].UID = "forged"
			},
			want: "stale, or non-controlling Deployment owner reference",
		},
		{
			name: "forged ReplicaSet selector",
			mutate: func(f *runtimeInventoryFixture) {
				replicaSet := f.replicaSets.objects[f.pods.result.Items[0].OwnerReferences[0].Name]
				replicaSet.Spec.Selector.MatchLabels["app.kubernetes.io/instance"] = "foreign"
			},
			want: "selector does not extend expected Deployment",
		},
		{
			name: "forged Pod labels",
			mutate: func(f *runtimeInventoryFixture) {
				f.pods.result.Items[0].Labels["app.kubernetes.io/component"] = "foreign"
			},
			want: "labels do not match live ReplicaSet",
		},
		{
			name: "forged generated name",
			mutate: func(f *runtimeInventoryFixture) {
				f.pods.result.Items[0].GenerateName = "foreign-"
			},
			want: "do not link to parent prefix",
		},
		{
			name: "foreign ReplicaSet response",
			mutate: func(f *runtimeInventoryFixture) {
				replicaSet := f.replicaSets.objects[f.pods.result.Items[0].OwnerReferences[0].Name]
				replicaSet.Namespace = "foreign"
			},
			want: "foreign or incomplete ReplicaSet",
		},
		{
			name: "nil Deployment response",
			mutate: func(f *runtimeInventoryFixture) {
				f.deployments.nilNames[f.guard.ControllerDeploymentName] = true
			},
			want: "nil result",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRuntimeInventoryFixture()
			test.mutate(fixture)
			err := fixture.inventory.VerifyRuntimeBeforeQuiesce(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("VerifyRuntimeBeforeQuiesce() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestWorkloadInventoryProtectedRuntimePodsRemain(t *testing.T) {
	fixture := newRuntimeInventoryFixture()
	remain, err := fixture.inventory.ProtectedRuntimePodsRemain(context.Background())
	if err != nil {
		t.Fatalf("ProtectedRuntimePodsRemain() error = %v", err)
	}
	if !remain {
		t.Fatal("ProtectedRuntimePodsRemain() = false, want true")
	}

	fixture.pods.result.Items = []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: fixture.guard.ReleaseNamespace},
		Spec:       corev1.PodSpec{ServiceAccountName: "default"},
	}}
	remain, err = fixture.inventory.ProtectedRuntimePodsRemain(context.Background())
	if err != nil {
		t.Fatalf("ProtectedRuntimePodsRemain() after removal error = %v", err)
	}
	if remain {
		t.Fatal("ProtectedRuntimePodsRemain() after removal = true, want false")
	}

	fixture.pods.result.Continue = "next"
	if _, err := fixture.inventory.ProtectedRuntimePodsRemain(context.Background()); err == nil || !strings.Contains(err.Error(), "repeated current continue token") {
		t.Fatalf("ProtectedRuntimePodsRemain() continued error = %v", err)
	}
}

type hookInventoryFixture struct {
	guard     *RolloutGuard
	inventory *WorkloadInventory
	jobs      *workloadInventoryJobClient
	pods      *workloadInventoryPodClient
}

func newHookInventoryFixture() *hookInventoryFixture {
	guard := workloadInventoryTestGuard()
	jobName := HookIdentityProbeJobName(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, guard.ManagerImage)
	jobUID := types.UID("identity-job")
	job := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: guard.ReleaseNamespace, UID: jobUID},
		Spec:       batchv1.JobSpec{Template: corev1PodTemplate(guard.HookServiceAccountName)},
	}
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:         jobName + "-abc12",
			GenerateName: jobName + "-",
			Namespace:    guard.ReleaseNamespace,
			UID:          "identity-pod",
			Labels: map[string]string{
				batchv1.ControllerUidLabel: string(jobUID),
				batchv1.JobNameLabel:       jobName,
				"controller-uid":           string(jobUID),
				"job-name":                 jobName,
			},
			OwnerReferences: []metav1.OwnerReference{controllerReference("batch/v1", "Job", jobName, jobUID)},
		},
		Spec: corev1.PodSpec{ServiceAccountName: guard.HookServiceAccountName},
	}
	jobs := &workloadInventoryJobClient{result: &batchv1.JobList{Items: []batchv1.Job{job}}}
	pods := &workloadInventoryPodClient{result: &corev1.PodList{Items: []corev1.Pod{pod}}}
	return &hookInventoryFixture{
		guard:     guard,
		inventory: NewWorkloadInventory(guard, pods, jobs, nil, nil),
		jobs:      jobs,
		pods:      pods,
	}
}

type runtimeInventoryFixture struct {
	guard       *RolloutGuard
	inventory   *WorkloadInventory
	pods        *workloadInventoryPodClient
	replicaSets *workloadInventoryReplicaSetClient
	deployments *workloadInventoryDeploymentClient
}

func newRuntimeInventoryFixture() *runtimeInventoryFixture {
	guard := workloadInventoryTestGuard()
	controllerDeployment, controllerReplicaSet, controllerPod := runtimeInventoryChain(
		guard,
		guard.ControllerDeploymentName,
		guard.ControllerServiceAccountName,
		"controller",
		"6f7d8c9b5a",
	)
	certificateDeployment, certificateReplicaSet, certificatePod := runtimeInventoryChain(
		guard,
		guard.CertificateDeploymentName,
		guard.CertificateDeploymentName,
		"certificate-rotation",
		"7c8d9b5a6f",
	)
	pods := &workloadInventoryPodClient{result: &corev1.PodList{Items: []corev1.Pod{*controllerPod, *certificatePod}}}
	replicaSets := &workloadInventoryReplicaSetClient{objects: map[string]*appsv1.ReplicaSet{
		controllerReplicaSet.Name:  controllerReplicaSet,
		certificateReplicaSet.Name: certificateReplicaSet,
	}}
	deployments := &workloadInventoryDeploymentClient{
		objects: map[string]*appsv1.Deployment{
			controllerDeployment.Name:  controllerDeployment,
			certificateDeployment.Name: certificateDeployment,
		},
		nilNames: map[string]bool{},
	}
	return &runtimeInventoryFixture{
		guard:       guard,
		inventory:   NewWorkloadInventory(guard, pods, nil, replicaSets, deployments),
		pods:        pods,
		replicaSets: replicaSets,
		deployments: deployments,
	}
}

func runtimeInventoryChain(guard *RolloutGuard, deploymentName, serviceAccount, component, hash string) (*appsv1.Deployment, *appsv1.ReplicaSet, *corev1.Pod) {
	deploymentUID := types.UID(deploymentName + "-uid")
	selector := map[string]string{
		"app.kubernetes.io/name":      "ptah-operator",
		"app.kubernetes.io/instance":  guard.ReleaseName,
		"app.kubernetes.io/component": component,
	}
	templateLabels := copyStringMap(selector)
	templateLabels["example.test/runtime-label"] = component
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Namespace: guard.ReleaseNamespace,
			UID:       deploymentUID,
			Annotations: map[string]string{
				helmReleaseNameAnnotation:      guard.ReleaseName,
				helmReleaseNamespaceAnnotation: guard.ReleaseNamespace,
			},
			Labels: map[string]string{
				managedByLabel:                "Helm",
				instanceLabel:                 guard.ReleaseName,
				"app.kubernetes.io/component": component,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: copyStringMap(selector)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: copyStringMap(templateLabels)},
				Spec:       corev1.PodSpec{ServiceAccountName: serviceAccount},
			},
		},
	}
	replicaSetLabels := copyStringMap(templateLabels)
	replicaSetLabels[appsv1.DefaultDeploymentUniqueLabelKey] = hash
	replicaSetSelector := copyStringMap(selector)
	replicaSetSelector[appsv1.DefaultDeploymentUniqueLabelKey] = hash
	replicaSetName := deploymentName + "-" + hash
	replicaSetUID := types.UID(replicaSetName + "-uid")
	replicaSet := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            replicaSetName,
			Namespace:       guard.ReleaseNamespace,
			UID:             replicaSetUID,
			Labels:          copyStringMap(replicaSetLabels),
			OwnerReferences: []metav1.OwnerReference{controllerReference("apps/v1", "Deployment", deploymentName, deploymentUID)},
		},
		Spec: appsv1.ReplicaSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: copyStringMap(replicaSetSelector)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: copyStringMap(replicaSetLabels)},
				Spec:       corev1.PodSpec{ServiceAccountName: serviceAccount},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            replicaSetName + "-abc12",
			GenerateName:    replicaSetName + "-",
			Namespace:       guard.ReleaseNamespace,
			UID:             types.UID(replicaSetName + "-pod"),
			Labels:          copyStringMap(replicaSetLabels),
			OwnerReferences: []metav1.OwnerReference{controllerReference("apps/v1", "ReplicaSet", replicaSetName, replicaSetUID)},
		},
		Spec: corev1.PodSpec{ServiceAccountName: serviceAccount},
	}
	return deployment, replicaSet, pod
}

func workloadInventoryTestGuard() *RolloutGuard {
	managerImage := "ghcr.io/stokaro/ptah-operator@sha256:" + strings.Repeat("a", 64)
	guard := &RolloutGuard{
		ReleaseName:                  "ptah-e2e",
		ReleaseNamespace:             "ptah-e2e",
		ReleaseSequence:              1,
		ManagerImage:                 managerImage,
		ControllerServiceAccountName: "ptah-e2e-controller",
		ControllerDeploymentName:     "ptah-e2e-controller",
		ControllerReplicas:           1,
		CertificateDeploymentName:    "ptah-e2e-cert-rotator",
	}
	guard.HookServiceAccountName = "ptah-e2e-operator-crd-v1-" + hookIdentityDigest(guard.ReleaseNamespace, guard.ReleaseName, guard.ReleaseSequence, managerImage)[:12]
	return guard
}

func corev1PodTemplate(serviceAccount string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: serviceAccount}}
}

func controllerReference(apiVersion, kind, name string, uid types.UID) metav1.OwnerReference {
	controller := true
	blockOwnerDeletion := true
	return metav1.OwnerReference{
		APIVersion:         apiVersion,
		Kind:               kind,
		Name:               name,
		UID:                uid,
		Controller:         &controller,
		BlockOwnerDeletion: &blockOwnerDeletion,
	}
}

func workloadInventoryListMeta(resourceVersion, continueToken string, remaining int64) metav1.ListMeta {
	return metav1.ListMeta{
		ResourceVersion:    resourceVersion,
		Continue:           continueToken,
		RemainingItemCount: &remaining,
	}
}

func assertWorkloadInventoryListOptions(t *testing.T, kind string, options []metav1.ListOptions, wantTokens []string) {
	t.Helper()
	if len(options) != len(wantTokens) {
		t.Fatalf("%s List calls = %d, want %d: %#v", kind, len(options), len(wantTokens), options)
	}
	for index := range options {
		if options[index].Limit != workloadInventoryPageSize || options[index].Continue != wantTokens[index] || options[index].LabelSelector != "" || options[index].FieldSelector != "" {
			t.Fatalf("%s List options[%d] = %#v, want Limit=%d Continue=%q without selectors", kind, index, options[index], workloadInventoryPageSize, wantTokens[index])
		}
	}
}

type workloadInventoryJobClient struct {
	result  *batchv1.JobList
	pages   map[string]*batchv1.JobList
	errors  map[string]error
	err     error
	options []metav1.ListOptions
}

func (c *workloadInventoryJobClient) List(_ context.Context, options metav1.ListOptions) (*batchv1.JobList, error) {
	c.options = append(c.options, options)
	if c.err != nil {
		return nil, c.err
	}
	if err := c.errors[options.Continue]; err != nil {
		return nil, err
	}
	if c.pages != nil {
		page, found := c.pages[options.Continue]
		if !found {
			return nil, fmt.Errorf("unexpected Job continue token %q", options.Continue)
		}
		if page == nil {
			return nil, nil
		}
		return page.DeepCopy(), nil
	}
	if c.result == nil {
		return nil, nil
	}
	return c.result.DeepCopy(), nil
}

type workloadInventoryPodClient struct {
	result  *corev1.PodList
	pages   map[string]*corev1.PodList
	errors  map[string]error
	err     error
	options []metav1.ListOptions
}

func (c *workloadInventoryPodClient) List(_ context.Context, options metav1.ListOptions) (*corev1.PodList, error) {
	c.options = append(c.options, options)
	if c.err != nil {
		return nil, c.err
	}
	if err := c.errors[options.Continue]; err != nil {
		return nil, err
	}
	if c.pages != nil {
		page, found := c.pages[options.Continue]
		if !found {
			return nil, fmt.Errorf("unexpected Pod continue token %q", options.Continue)
		}
		if page == nil {
			return nil, nil
		}
		return page.DeepCopy(), nil
	}
	if c.result == nil {
		return nil, nil
	}
	return c.result.DeepCopy(), nil
}

type workloadInventoryReplicaSetClient struct {
	objects map[string]*appsv1.ReplicaSet
	options []metav1.ListOptions
	listErr error
	list    *appsv1.ReplicaSetList
	pages   map[string]*appsv1.ReplicaSetList
	errors  map[string]error
	nilList bool
}

func (c *workloadInventoryReplicaSetClient) List(_ context.Context, options metav1.ListOptions) (*appsv1.ReplicaSetList, error) {
	c.options = append(c.options, options)
	if c.nilList {
		return nil, c.listErr
	}
	if err := c.errors[options.Continue]; err != nil {
		return nil, err
	}
	if c.pages != nil {
		page, found := c.pages[options.Continue]
		if !found {
			return nil, fmt.Errorf("unexpected ReplicaSet continue token %q", options.Continue)
		}
		if page == nil {
			return nil, nil
		}
		return page.DeepCopy(), nil
	}
	if c.list != nil || c.listErr != nil {
		if c.list == nil {
			return nil, c.listErr
		}
		return c.list.DeepCopy(), c.listErr
	}
	return c.objectList(), nil
}

func (c *workloadInventoryReplicaSetClient) objectList() *appsv1.ReplicaSetList {
	result := &appsv1.ReplicaSetList{Items: make([]appsv1.ReplicaSet, 0, len(c.objects))}
	for _, object := range c.objects {
		result.Items = append(result.Items, *object.DeepCopy())
	}
	return result
}

type workloadInventoryDeploymentClient struct {
	objects  map[string]*appsv1.Deployment
	errors   map[string]error
	nilNames map[string]bool
	gets     []string
}

func (c *workloadInventoryDeploymentClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*appsv1.Deployment, error) {
	c.gets = append(c.gets, name)
	if err := c.errors[name]; err != nil {
		return nil, err
	}
	if c.nilNames[name] {
		return nil, nil
	}
	object, found := c.objects[name]
	if !found {
		return nil, fmt.Errorf("Deployment %s not found", name)
	}
	return object, nil
}
