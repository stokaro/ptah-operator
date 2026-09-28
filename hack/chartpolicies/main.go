// Command chartpolicies generates the chart templates that ship the
// admission policies defined in Go.
//
// Each policy is written once, as a Go value: the controller guards in
// internal/crdupgrade, the certificate rotator's Secret guard in
// internal/certrotation. The chart decides a handful of values in them when
// it renders a release -- the namespace and release name, the
// ServiceAccounts, the manager image, the controller-state version -- so a
// template cannot be a fixed document. The generator builds each policy with
// a sentinel in place of every such value and writes the Helm expression
// wherever a sentinel appears; everything else is copied out of the Go value
// as it is.
//
// make chart-policies runs it. verify-source runs it too and refuses a
// template it did not write, the way it refuses a hand-edited CRD.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	chart := flag.String("chart", filepath.Join("charts", "ptah-operator"), "the chart directory to write the templates into")
	flag.Usage = func() {
		_, _ = fmt.Fprintln(os.Stderr, "usage: chartpolicies [-chart DIR]")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*chart); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "chartpolicies: %v\n", err)
		os.Exit(1)
	}
}

func run(chart string) error {
	rendered, err := render()
	if err != nil {
		return err
	}
	for path, content := range rendered {
		target := filepath.Join(chart, filepath.FromSlash(path))
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil { //nolint:gosec // A chart template, world-readable like the rest of the chart.
			return err
		}
	}
	return nil
}

// render emits every generated template, keyed by its path inside the chart.
func render() (map[string]string, error) {
	rendered := make(map[string]string)
	for _, t := range templates(sentinels()) {
		if _, duplicate := rendered[t.path]; duplicate {
			return nil, fmt.Errorf("%s is generated twice", t.path)
		}
		content, err := emit(t)
		if err != nil {
			return nil, err
		}
		rendered[t.path] = content
	}
	return rendered, nil
}
