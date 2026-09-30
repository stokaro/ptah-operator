// Command runnerprotocolfixture prepares a Go build overlay for an explicitly
// incompatible runner. It changes one declaration in copied source; the
// shipping source and contract remain untouched. This fixture measures refusal
// of an unsupported installation, not compatibility with a future protocol.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"

	"github.com/stokaro/ptah-operator/internal/runner"
)

type fixtureRecord struct {
	SupportedProtocol int    `json:"supportedProtocol"`
	RefusedProtocol   int    `json:"refusedProtocol"`
	SourceSHA256      string `json:"sourceSHA256"`
	OverlaySHA256     string `json:"overlaySHA256"`
	Scope             string `json:"scope"`
}

const fixtureScope = "unsupported runner refusal only; no future protocol compatibility"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("runnerprotocolfixture", flag.ContinueOnError)
	source := flags.String("source", "internal/runner/protocol.go", "the shipping runner protocol declaration")
	directory := flags.String("directory", "", "directory for copied source, overlay and provenance")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *directory == "" {
		return errors.New("provide --directory and no positional arguments")
	}
	return writeOverlay(*source, *directory, runner.ProtocolVersion)
}

// Locate the one top-level integer constant by syntax, rather than replacing
// its spelling in comments, other declarations or unrelated source.
func incompatibleProtocolSource(source []byte, supported int) ([]byte, error) {
	if supported < 1 || supported == int(^uint(0)>>1) {
		return nil, errors.New("the fixture has no supported protocol and representable successor")
	}
	positions := token.NewFileSet()
	file, err := parser.ParseFile(positions, "protocol.go", source, parser.ParseComments)
	if err != nil || file.Name.Name != "runner" {
		return nil, errors.New("the fixture requires valid runner package source")
	}
	var value *ast.BasicLit
	matches := 0
	for _, declaration := range file.Decls {
		group, ok := declaration.(*ast.GenDecl)
		if !ok || group.Tok != token.CONST {
			continue
		}
		for _, entry := range group.Specs {
			spec := entry.(*ast.ValueSpec)
			for _, name := range spec.Names {
				if name.Name != "ProtocolVersion" {
					continue
				}
				matches++
				if len(spec.Names) != 1 || len(spec.Values) != 1 {
					return nil, errors.New("the protocol declaration is ambiguous")
				}
				value, ok = spec.Values[0].(*ast.BasicLit)
				if !ok || value.Kind != token.INT || value.Value != strconv.Itoa(supported) {
					return nil, errors.New("the fixture's source does not declare the shipping protocol")
				}
			}
		}
	}
	if matches != 1 || value == nil {
		return nil, errors.New("the fixture needs exactly one ProtocolVersion constant")
	}
	start, end := positions.Position(value.Pos()).Offset, positions.Position(value.End()).Offset
	updated := append([]byte{}, source[:start]...)
	updated = append(updated, strconv.Itoa(supported+1)...)
	updated = append(updated, source[end:]...)
	return updated, nil
}

func sourceSHA256(source []byte) string {
	digest := sha256.Sum256(source)
	return hex.EncodeToString(digest[:])
}

func writeOverlay(sourcePath, directory string, supported int) error {
	sourcePath, err := filepath.Abs(sourcePath)
	if err != nil {
		return err
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		return err
	}
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		return err
	}
	updated, err := incompatibleProtocolSource(source, supported)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	copyPath := filepath.Join(directory, "protocol.go")
	if copyPath == sourcePath {
		return errors.New("the fixture must not overwrite shipping source")
	}
	if err := os.WriteFile(copyPath, updated, 0o644); err != nil {
		return err
	}
	overlay, err := json.Marshal(map[string]map[string]string{"Replace": {sourcePath: copyPath}})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, "overlay.json"), overlay, 0o644); err != nil {
		return err
	}
	record, err := json.MarshalIndent(fixtureRecord{
		SupportedProtocol: supported, RefusedProtocol: supported + 1,
		SourceSHA256: sourceSHA256(source), OverlaySHA256: sourceSHA256(updated), Scope: fixtureScope,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, "provenance.json"), append(record, '\n'), 0o644)
}
