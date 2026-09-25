package crdupgrade

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The frozen sequence-1 role contract is what an upgrade preflights the live
// ClusterRole against. While sequence 1 is unshipped it therefore has to
// describe the chart that publishes it, and a table that drifts from the chart
// refuses the upgrade before it starts -- in a cluster, twenty-five minutes into
// a lifecycle, three times over. The function's own comment says so and nothing
// measured it, which is how a change that removed one grant from the chart and
// left the frozen table alone reached CI twice.
//
// This is that measurement. The day a version ships, the contract freezes with
// it and the premise here lapses; support/ptah.json is what says whether that
// has happened.

type ptahSupportCatalog struct {
	Releases []struct {
		Operator string `json:"operator"`
		Stage    string `json:"stage"`
	} `json:"releases"`
}

func TestTheUnshippedSequenceContractDescribesTheChartThatPublishesIt(t *testing.T) {
	t.Parallel()
	catalog := readSupportCatalog(t)
	if len(catalog.Releases) == 0 {
		t.Fatal("support/ptah.json declares no release, so this check cannot tell whether the contract is frozen")
	}
	shipped := false
	for _, release := range catalog.Releases {
		if release.Operator != "edge" || release.Stage != "development" {
			shipped = true
		}
	}
	if shipped {
		t.Skipf("a release has shipped, so the sequence-%d contract is frozen and no longer follows the chart", CurrentReleaseSequence)
	}
	if CurrentReleaseSequence != 1 {
		t.Fatalf("release sequence is %d while support/ptah.json still declares only an unshipped edge release", CurrentReleaseSequence)
	}
	rollout := &RolloutGuard{
		ReleaseNamespace:      "ptah-system",
		ReleaseName:           "ptah-operator",
		ReleaseSequence:       CurrentReleaseSequence,
		ManagerImage:          "ghcr.io/stokaro/ptah-operator@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		CoordinationNamespace: "ptah-system",
	}
	frozen := sequence1ControllerClusterRoleRules(candidateControllerRoleIdentity(rollout))
	current := currentControllerClusterRoleRules(rollout)
	if len(frozen) == 0 || len(current) == 0 {
		t.Fatal("one of the two contracts is empty, so this check compares nothing")
	}
	if !reflect.DeepEqual(frozen, current) {
		t.Fatalf("the frozen sequence-1 ClusterRole contract and the rules the chart publishes differ:\nfrozen  = %#v\ncurrent = %#v",
			frozen, current)
	}
}

func readSupportCatalog(t *testing.T) ptahSupportCatalog {
	t.Helper()
	path := filepath.Join("..", "..", "support", "ptah.json")
	content, err := os.ReadFile(path) //nolint:gosec // A path under the repository.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var catalog ptahSupportCatalog
	if err := json.Unmarshal(content, &catalog); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return catalog
}
