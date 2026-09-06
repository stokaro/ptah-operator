package crdupgrade

import (
	"strings"
	"testing"
)

func TestTeardownIdentityNamesAreDistinctBoundedAndDeterministic(t *testing.T) {
	hook := strings.Repeat("a", 30) + "-crd-v12-0123456789ab"
	cleanup, err := TeardownServiceAccountName(hook, 12)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup == hook || cleanup != strings.Repeat("a", 30)+"-cleanup-v12-0123456789ab" || len(cleanup) > 63 {
		t.Fatalf("cleanup identity = %q", cleanup)
	}
	quiesce, err := TeardownQuiesceJobName(hook)
	if err != nil {
		t.Fatal(err)
	}
	if quiesce == hook || quiesce != strings.Repeat("a", 30)+"-quiesce-v12-0123456789ab" || len(quiesce) > 63 {
		t.Fatalf("quiesce Job name = %q", quiesce)
	}
	otherSequence, err := TeardownQuiesceJobName(strings.Repeat("a", 30) + "-crd-v13-0123456789ab")
	if err != nil {
		t.Fatal(err)
	}
	if otherSequence == quiesce {
		t.Fatalf("quiesce Job names collided across release sequences: %q", quiesce)
	}
	privilege, err := TeardownPrivilegeRoleName(hook)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := TeardownGuardRoleName(hook)
	if err != nil {
		t.Fatal(err)
	}
	if privilege != strings.Repeat("a", 24)+"-cleanup-priv-v12-0123456789ab" || len(privilege) > 63 {
		t.Fatalf("privilege role name = %q", privilege)
	}
	if guard != strings.Repeat("a", 24)+"-cleanup-guard-v12-0123456789ab" || len(guard) > 63 || guard == privilege {
		t.Fatalf("residual guard role name = %q", guard)
	}
}

func TestTeardownIdentityRejectsMalformedHookIdentity(t *testing.T) {
	for _, hook := range []string{"", "ptah-crd-v2-0123456789ab", "ptah-crd-v1-not-a-digest"} {
		if _, err := TeardownServiceAccountName(hook, 1); err == nil {
			t.Fatalf("malformed hook identity %q was accepted", hook)
		}
	}
	if _, err := TeardownServiceAccountName("ptah-crd-v1-0123456789ab", 0); err == nil {
		t.Fatal("non-positive release sequence was accepted")
	}
	for _, hook := range []string{"", "ptah-crd-v0-0123456789ab", "ptah-crd-v1-not-a-digest"} {
		if _, err := TeardownQuiesceJobName(hook); err == nil {
			t.Fatalf("malformed hook identity %q formed a quiesce Job", hook)
		}
		if _, err := TeardownPrivilegeRoleName(hook); err == nil {
			t.Fatalf("malformed hook identity %q formed a privilege role", hook)
		}
		if _, err := TeardownGuardRoleName(hook); err == nil {
			t.Fatalf("malformed hook identity %q formed a residual guard role", hook)
		}
	}
}
