package main

import (
	"encoding/json"
	"fmt"
	"os"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	"github.com/stokaro/ptah-operator/internal/crdupgrade"
)

func main() {
	if len(os.Args) != 2 {
		_, _ = fmt.Fprintln(os.Stderr, "usage: crdschemadigest CRD_FILE")
		os.Exit(2)
	}
	yamlBytes, err := os.ReadFile(os.Args[1])
	if err != nil {
		fail("read CRD: %v", err)
	}
	jsonBytes, err := yaml.YAMLToJSON(yamlBytes)
	if err != nil {
		fail("decode CRD YAML: %v", err)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := json.Unmarshal(jsonBytes, crd); err != nil {
		fail("decode CRD: %v", err)
	}
	digest, err := crdupgrade.ComputeSchemaDigest(crd)
	if err != nil {
		fail("compute schema digest: %v", err)
	}
	_, _ = fmt.Fprintln(os.Stdout, digest)
}

func fail(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "crdschemadigest: "+format+"\n", args...)
	os.Exit(1)
}
