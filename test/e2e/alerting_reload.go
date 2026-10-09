package e2e

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/stokaro/ptah-operator/test/e2e/harness"
)

// A ConfigMap update does not mean its files have reached the Pod. Reloading
// during projection can read the old config after a referenced rule is gone.
// Wait for every required file, then reload once; a reload failure is fatal.
func alReloadProjectedMonitoring(ctx context.Context, files map[string]string,
	read func(context.Context, string) ([]byte, error), reload func(context.Context) error,
) error {
	names := slices.Sorted(maps.Keys(files))
	if len(names) == 0 {
		return fmt.Errorf("no monitoring files were selected")
	}
	for _, name := range names {
		if files[name] == "" {
			return fmt.Errorf("monitoring file %s is empty", name)
		}
	}
	err := harness.Wait(ctx, "Prometheus to mount its updated monitoring files", alTimeout, time.Second,
		func(ctx context.Context) (bool, string, error) {
			for _, name := range names {
				body, err := read(ctx, name)
				if err != nil {
					return false, "", fmt.Errorf("read mounted %s: %w", name, err)
				}
				if string(body) != files[name] {
					return false, "waiting for projected " + name, nil
				}
			}
			return true, "", nil
		})
	if err != nil {
		return err
	}
	if err := reload(ctx); err != nil {
		return fmt.Errorf("reload Prometheus: %w", err)
	}
	return nil
}
