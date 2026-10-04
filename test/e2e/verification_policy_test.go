package e2e

import (
	"strings"
	"testing"

	ptahv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func TestVerificationPolicyDecisionBindsObjectAndSelectedBytes(t *testing.T) {
	t.Parallel()
	digest := func(letter string) string { return "sha256:" + strings.Repeat(letter, 64) }
	for _, mode := range []string{"verification-policy-uid", "verification-policy-content"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			before := verificationPolicyIdentity{uid: "original-policy", digest: digest("a")}
			after := before
			if mode == "verification-policy-uid" {
				after.uid = "replacement-policy"
			} else {
				after.digest = digest("b")
			}
			old := verificationPolicyDecision{
				uid: "old-plan", fingerprint: "old-fingerprint", verification: before,
				resource: ptahv1alpha1.ImmutableObjectReference{Name: "resource", UID: "resource-uid"},
				artifact: digest("c"), target: digest("d"), realm: digest("e"),
			}
			current := old
			current.uid, current.fingerprint, current.verification = "new-plan", "new-fingerprint", after
			if err := changedVerificationPolicyDecision(old, current, before, after, mode); err != nil {
				t.Fatal(err)
			}
			for name, mutate := range map[string]func(*verificationPolicyDecision){
				"old plan UID":       func(p *verificationPolicyDecision) { p.uid = old.uid },
				"no plan UID":        func(p *verificationPolicyDecision) { p.uid = "" },
				"old fingerprint":    func(p *verificationPolicyDecision) { p.fingerprint = old.fingerprint },
				"no fingerprint":     func(p *verificationPolicyDecision) { p.fingerprint = "" },
				"old policy binding": func(p *verificationPolicyDecision) { p.verification = before },
				"unrelated policy":   func(p *verificationPolicyDecision) { p.verification.uid = "unrelated" },
				"no policy bytes":    func(p *verificationPolicyDecision) { p.verification.digest = "" },
				"wrong policy bytes": func(p *verificationPolicyDecision) { p.verification.digest = digest("f") },
				"another resource":   func(p *verificationPolicyDecision) { p.resource.UID = "replacement-resource" },
				"another artifact":   func(p *verificationPolicyDecision) { p.artifact = digest("f") },
				"another target":     func(p *verificationPolicyDecision) { p.target = digest("f") },
				"another realm":      func(p *verificationPolicyDecision) { p.realm = digest("f") },
			} {
				candidate := current
				mutate(&candidate)
				if changedVerificationPolicyDecision(old, candidate, before, after, mode) == nil {
					t.Errorf("accepted %s", name)
				}
			}
			for name, mutate := range map[string]func(*verificationPolicyDecision){
				"missing resource": func(p *verificationPolicyDecision) { p.resource.UID = "" },
				"missing artifact": func(p *verificationPolicyDecision) { p.artifact = "" },
				"missing target":   func(p *verificationPolicyDecision) { p.target = "" },
				"missing realm":    func(p *verificationPolicyDecision) { p.realm = "" },
			} {
				left, right := old, current
				mutate(&left)
				mutate(&right)
				if changedVerificationPolicyDecision(left, right, before, after, mode) == nil {
					t.Errorf("accepted matching empty inputs: %s", name)
				}
			}
			wrongOriginal := old
			wrongOriginal.verification = after
			if changedVerificationPolicyDecision(wrongOriginal, current, before, after, mode) == nil {
				t.Fatal("accepted an original plan that never bound the original policy")
			}
			if changedVerificationPolicyDecision(old, current, verificationPolicyIdentity{}, after, mode) == nil ||
				changedVerificationPolicyDecision(old, current, before, verificationPolicyIdentity{}, mode) == nil ||
				changedVerificationPolicyDecision(old, current, before, after, "unknown") == nil {
				t.Fatal("accepted missing policy evidence or an undeclared transition")
			}
			otherMode := "verification-policy-uid"
			if mode == otherMode {
				otherMode = "verification-policy-content"
			}
			if changedVerificationPolicyDecision(old, current, before, after, otherMode) == nil {
				t.Fatal("the object-identity and selected-content proofs are interchangeable")
			}
		})
	}
}
