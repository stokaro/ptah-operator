package main

import "fmt"

const preparationSourceRef = "refs/heads/master"

// The build ref names the producer of the existing bytes, even when a later
// tag publishes them. Callers must independently authenticate that exact ref,
// source commit, repository and workflow; this is only the manifest contract.
func releaseSourceRef(tag, ref string) (string, error) {
	if tag == "" {
		return "", fmt.Errorf("release source binding requires a release tag name")
	}
	tagRef := "refs/tags/" + tag
	if ref == "" {
		return tagRef, nil
	}
	if ref != tagRef && ref != preparationSourceRef {
		return "", fmt.Errorf("release build ref %q is neither %q nor %q", ref, tagRef, preparationSourceRef)
	}
	return ref, nil
}
