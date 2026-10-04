package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
)

type schemaInput struct {
	Slot       int      `json:"slot"`
	Band       string   `json:"band"`
	References []string `json:"references"`
}

type migrationInput struct {
	Slot          int      `json:"slot"`
	HistoryLength int      `json:"historyLength"`
	References    []string `json:"references"`
}

type rowInventory struct {
	Slot   int    `json:"slot"`
	Rows   int    `json:"rows"`
	SHA256 string `json:"sha256"`
}

type inputCatalog struct {
	SchemaVersion int              `json:"schemaVersion"`
	Engine        string           `json:"engine"`
	PtahCommit    string           `json:"ptahCommit"`
	InitialRows   []rowInventory   `json:"initialRows"`
	Schemas       []schemaInput    `json:"schemas"`
	Migrations    []migrationInput `json:"migrations"`
}

var capacityDigestReference = regexp.MustCompile(`^oci://[^\s@]+@sha256:[0-9a-f]{64}$`)
var capacityHexDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var capacityCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

func readInputCatalog(path string, load workload, ptahCommit string) (*inputCatalog, string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // The harness supplies its own catalog path.
	if err != nil {
		return nil, "", err
	}
	var catalog inputCatalog
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&catalog); err != nil {
		return nil, "", fmt.Errorf("read input catalog: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, "", fmt.Errorf("input catalog has trailing data")
	}
	if err := catalog.validate(load, ptahCommit); err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(raw)
	return &catalog, hex.EncodeToString(digest[:]), nil
}

func (c *inputCatalog) validate(load workload, ptahCommit string) error {
	if c.SchemaVersion != 1 || c.Engine != load.engine() || !capacityCommit.MatchString(ptahCommit) || c.PtahCommit != ptahCommit {
		return fmt.Errorf("input catalog must match the workload engine and pinned executor commit")
	}
	if load.ChangeBatch != 0 && load.ChangeBatch != 5 {
		return fmt.Errorf("varied inputs require changeBatch zero or five")
	}
	if load.Schemas != 10 || load.Migrations != 10 || len(c.Schemas) != 10 || len(c.Migrations) != 10 || len(c.InitialRows) != 20 {
		return fmt.Errorf("input catalog requires ten resources per family and twenty populated databases")
	}
	seenRows := map[string]bool{}
	for i, rows := range c.InitialRows {
		if rows.Slot != i || rows.Rows != 10000 || !capacityHexDigest.MatchString(rows.SHA256) || seenRows[rows.SHA256] {
			return fmt.Errorf("input slot %d lacks its distinct complete initial row inventory", i)
		}
		seenRows[rows.SHA256] = true
	}
	for i, row := range c.Schemas {
		if row.Slot != i || row.Band != []string{"small", "medium", "large"}[i%3] {
			return fmt.Errorf("schema input slot %d has the wrong identity or plan band", i)
		}
		if err := validateInputReferences(row.References, i < 5); err != nil {
			return fmt.Errorf("schema input slot %d: %w", i, err)
		}
	}
	for i, row := range c.Migrations {
		length := 128
		if i < 4 {
			length = 2
		} else if i < 7 {
			length = 32
		}
		if row.Slot != i || row.HistoryLength != length {
			return fmt.Errorf("migration input slot %d has the wrong identity or history length", i)
		}
		if err := validateInputReferences(row.References, i < 5); err != nil {
			return fmt.Errorf("migration input slot %d: %w", i, err)
		}
	}
	return nil
}

func validateInputReferences(references []string, changes bool) error {
	if len(references) != 10 {
		return fmt.Errorf("expected an initial artifact and nine update rounds")
	}
	seen := map[string]bool{}
	for _, ref := range references {
		if !capacityDigestReference.MatchString(ref) || changes && seen[ref] || !changes && ref != references[0] {
			return fmt.Errorf("missing immutable reference or incorrect change schedule")
		}
		seen[ref] = true
	}
	return nil
}

func (s *scenarios) schemaReference(index, round int) string {
	if s.in.catalog != nil {
		return s.in.catalog.Schemas[index].References[round]
	}
	return s.in.schemaRefs[round]
}

func (s *scenarios) migrationReference(index, round int) string {
	if s.in.catalog != nil {
		return s.in.catalog.Migrations[index].References[round]
	}
	return s.in.migrationRefs[round]
}
