package crdupgrade

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestRuntimeQuotaValidateStatusResourcesAtMost(t *testing.T) {
	t.Parallel()

	expected := corev1.ResourceList{
		corev1.ResourceCPU:                        resource.MustParse("100m"),
		corev1.ResourceMemory:                     resource.MustParse("1Gi"),
		corev1.ResourceEphemeralStorage:           resource.MustParse("2Gi"),
		corev1.ResourceName("hugepages-2Mi"):      resource.MustParse("64Mi"),
		corev1.ResourceName("example.com/gpu"):    resource.MustParse("2"),
		corev1.ResourceName("example.com/tokens"): resource.MustParse("1000m"),
	}

	tests := []struct {
		name   string
		actual corev1.ResourceList
		want   string
	}{
		{name: "nil"},
		{name: "empty", actual: corev1.ResourceList{}},
		{
			name: "known nonnegative subset below expected",
			actual: corev1.ResourceList{
				corev1.ResourceCPU:                     resource.MustParse("28m"),
				corev1.ResourceMemory:                  resource.MustParse("1023Mi"),
				corev1.ResourceEphemeralStorage:        resource.MustParse("1Gi"),
				corev1.ResourceName("hugepages-2Mi"):   resource.MustParse("32Mi"),
				corev1.ResourceName("example.com/gpu"): resource.MustParse("1"),
			},
		},
		{
			name: "explicit zeros",
			actual: corev1.ResourceList{
				corev1.ResourceCPU:                     resource.MustParse("0"),
				corev1.ResourceMemory:                  resource.MustParse("0"),
				corev1.ResourceEphemeralStorage:        resource.MustParse("0"),
				corev1.ResourceName("hugepages-2Mi"):   resource.MustParse("0"),
				corev1.ResourceName("example.com/gpu"): resource.MustParse("0"),
			},
		},
		{
			name: "equivalent quantity spelling",
			actual: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1024Mi"),
			},
		},
		{
			name: "negative",
			actual: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("-1m"),
			},
			want: "must not be negative",
		},
		{
			name: "unknown zero",
			actual: corev1.ResourceList{
				corev1.ResourceName("example.com/unknown"): resource.MustParse("0"),
			},
			want: "outside the fixed runtime Pod spec",
		},
		{
			name: "CPU above expected",
			actual: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("101m"),
			},
			want: "cpu=101m exceeds",
		},
		{
			name: "memory above expected",
			actual: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1025Mi"),
			},
			want: "memory=1025Mi exceeds",
		},
		{
			name: "ephemeral storage above expected",
			actual: corev1.ResourceList{
				corev1.ResourceEphemeralStorage: resource.MustParse("2049Mi"),
			},
			want: "ephemeral-storage=2049Mi exceeds",
		},
		{
			name: "huge page above expected",
			actual: corev1.ResourceList{
				corev1.ResourceName("hugepages-2Mi"): resource.MustParse("66Mi"),
			},
			want: "hugepages-2Mi=66Mi exceeds",
		},
		{
			name: "extended resource above expected",
			actual: corev1.ResourceList{
				corev1.ResourceName("example.com/gpu"): resource.MustParse("3"),
			},
			want: "example.com/gpu=3 exceeds",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := runtimeQuotaValidateStatusResourcesAtMost("status resources", tc.actual, expected)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("runtimeQuotaValidateStatusResourcesAtMost() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runtimeQuotaValidateStatusResourcesAtMost() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}
