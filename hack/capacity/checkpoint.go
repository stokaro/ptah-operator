package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type databaseCheckpoint struct {
	Name       string    `json:"name"`
	Round      int       `json:"round"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	SHA256     string    `json:"sha256"`
}

type checkpointResult struct {
	Engine       string `json:"engine"`
	PtahCommit   string `json:"ptahCommit"`
	ChangeBatch  int    `json:"changeBatch"`
	Round        int    `json:"round"`
	Checkpoint   string `json:"checkpoint"`
	BundleSHA256 string `json:"bundleSHA256"`
	Slots        []struct {
		rowInventory
		Round            int    `json:"round"`
		Band             string `json:"band"`
		DefaultsVerified int    `json:"defaultsVerified"`
		HistoryLength    int    `json:"historyLength"`
	} `json:"slots"`
}

func validateCheckpoint(raw []byte, catalog *inputCatalog, bundleDigest, name string, round int) error {
	var result checkpointResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	if catalog == nil || len(catalog.InitialRows) != 20 || len(catalog.Schemas) != 10 || len(catalog.Migrations) != 10 || result.Engine != catalog.Engine || result.PtahCommit != catalog.PtahCommit ||
		result.ChangeBatch != 5 || result.Round != round || result.Checkpoint != name ||
		!capacityHexDigest.MatchString(bundleDigest) || result.BundleSHA256 != bundleDigest || len(result.Slots) != 20 {
		return fmt.Errorf("database checkpoint does not bind the workload, round and bundle")
	}
	for i, row := range result.Slots {
		expectedRound := 0
		if i%10 < 5 {
			expectedRound = round
		}
		if row.rowInventory != catalog.InitialRows[i] || row.Round != expectedRound {
			return fmt.Errorf("database checkpoint slot %d has stale or incomplete rows", i)
		}
		if i < 10 {
			if row.Band != catalog.Schemas[i].Band || row.DefaultsVerified != []int{1, 4, 16}[i%3] {
				return fmt.Errorf("database checkpoint slot %d lacks its schema defaults", i)
			}
		} else if row.HistoryLength != catalog.Migrations[i-10].HistoryLength+expectedRound {
			return fmt.Errorf("database checkpoint slot %d has the wrong migration history", i)
		}
	}
	return nil
}

func checkpointProbe(probe, state, directory string, catalog *inputCatalog) (func(context.Context, int, string) (databaseCheckpoint, error), error) {
	if probe == "" || state == "" || directory == "" || catalog == nil {
		return nil, fmt.Errorf("soak requires the database checkpoint probe, ownership state and populated inputs")
	}
	for _, path := range []string{probe, state} {
		if _, err := os.Stat(path); err != nil {
			return nil, err
		}
	}
	bundle, err := os.ReadFile(filepath.Join(directory, "bundle.json"))
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(bundle)
	bundleDigest := hex.EncodeToString(digest[:])
	return func(ctx context.Context, round int, name string) (databaseCheckpoint, error) {
		proof := databaseCheckpoint{Name: name, Round: round, StartedAt: time.Now().UTC()}
		logPath := filepath.Join(directory, name+"-probe.private.log")
		output, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return proof, err
		}
		defer output.Close()
		command := exec.CommandContext(ctx, "python3", probe, "--state", state, "--directory", directory, "--verify-changed", "5", "--verify-round", fmt.Sprint(round), "--checkpoint", name)
		command.Stdout, command.Stderr = output, output
		// Python's interrupt unwinds its finally blocks, including the owned
		// port-forward. Bound that cleanup as well as the database work itself.
		command.Cancel = func() error { return command.Process.Signal(os.Interrupt) }
		command.WaitDelay = 20 * time.Second
		if err := command.Run(); err != nil {
			return proof, fmt.Errorf("database checkpoint %s: %w (diagnostics: %s)", name, err, logPath)
		}
		raw, err := os.ReadFile(filepath.Join(directory, "checkpoints", name, "database-verification.json"))
		if err != nil {
			return proof, err
		}
		if err := validateCheckpoint(raw, catalog, bundleDigest, name, round); err != nil {
			return proof, err
		}
		digest := sha256.Sum256(raw)
		proof.SHA256 = hex.EncodeToString(digest[:])
		proof.FinishedAt = time.Now().UTC()
		return proof, nil
	}, nil
}
