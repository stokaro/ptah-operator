package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlanSizeSchema(t *testing.T) {
	t.Parallel()
	for _, engine := range []string{"postgres", "mysql"} {
		base, err := planSizeSchema(engine, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		content, err := planSizeSchema(engine, 31, 5)
		if err != nil || len(content) != len(base)+36 || bytes.Count(content, []byte("<")) != 31 ||
			!bytes.Contains(content, []byte("xxxxx")) || !bytes.HasSuffix(content, []byte(";\n")) {
			t.Fatal("the generated SQL does not contain the exact declared native default")
		}
		if engine == "postgres" && !bytes.Contains(content, []byte(strings.Repeat("<", 31)+"xxxxx")) {
			t.Fatal("the PostgreSQL fixture changed its single TEXT default")
		}
		if engine == "mysql" && (bytes.Count(content, []byte("CREATE TABLE ")) != 100 || bytes.Contains(content, []byte("LONGTEXT"))) {
			t.Fatal("the MySQL fixture did not retain its fixed executable table set")
		}
	}
	for _, row := range []struct {
		engine           string
		repeated, suffix int
	}{{"postgresql", 1, 0}, {"mysql", -1, 0}, {"postgres", 1, -1}, {"mysql", 1, 128}, {"postgres", 9 << 20, 0}} {
		if _, err := planSizeSchema(row.engine, row.repeated, row.suffix); err == nil {
			t.Fatal("accepted a different engine or an unbounded fixture")
		}
	}
}

func TestPlanSizeSchemaDoesNotOverwriteEvidence(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := runPlanSizeSchema([]string{"postgres", "31", "5", path}); err != nil {
		t.Fatal(err)
	}
	want, _ := planSizeSchema("postgres", 31, 5)
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("the fixture file changed its declared bytes")
	}
	if err := runPlanSizeSchema([]string{"mysql", "32", "6", path}); err == nil {
		t.Fatal("overwrote an existing fixture")
	}
	got, _ = os.ReadFile(path)
	if !bytes.Equal(got, want) {
		t.Fatal("the refused overwrite changed evidence")
	}
}
