package e2e

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAlReloadWaitsForConfigAndRuleProjection(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	files := map[string]string{"prometheus.yml": "new config", "upgrade-rules.yaml": "new rules"}
	polls, reloads := 0, 0
	err := alReloadProjectedMonitoring(ctx, files, func(_ context.Context, name string) ([]byte, error) {
		if name == "prometheus.yml" {
			polls++
			if polls == 1 {
				return []byte("old config referring to the previous rule file"), nil
			}
		} else if polls == 2 {
			// The new payload is visible before its new top-level file link.
			return nil, nil
		}
		return []byte(files[name]), nil
	}, func(context.Context) error {
		reloads++
		if polls < 3 {
			return errors.New("reloaded across a ConfigMap projection boundary")
		}
		return nil
	})
	if err != nil || reloads != 1 || polls != 3 {
		t.Fatalf("projection did not precede a single reload: polls=%d reloads=%d error=%v", polls, reloads, err)
	}
}

func TestAlReloadPreservesReadAndReloadFailures(t *testing.T) {
	t.Parallel()
	failure := errors.New("monitoring failure")
	for _, stage := range []string{"read", "reload"} {
		t.Run(stage, func(t *testing.T) {
			reloads := 0
			err := alReloadProjectedMonitoring(t.Context(), map[string]string{"prometheus.yml": "config"},
				func(context.Context, string) ([]byte, error) {
					if stage == "read" {
						return nil, failure
					}
					return []byte("config"), nil
				}, func(context.Context) error { reloads++; return failure })
			want := 0
			if stage == "reload" {
				want = 1
			}
			if !errors.Is(err, failure) || reloads != want {
				t.Fatalf("failure was hidden or retried: reloads=%d error=%v", reloads, err)
			}
		})
	}
}
