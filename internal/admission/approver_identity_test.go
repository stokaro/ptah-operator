package admission

import (
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
)

func authenticatedApprover() authenticationv1.UserInfo {
	return authenticationv1.UserInfo{
		Username: "approver@example.com",
		UID:      "approver-uid",
		Groups:   []string{"system:authenticated", "release-managers"},
	}
}

// The recorded groups are the normalized form of the authenticated ones:
// trimmed, deduplicated and sorted. Recording them any other way is what the
// comparison refuses.
func recordedApprover() operatorv1alpha1.ApprovalIdentity {
	return operatorv1alpha1.ApprovalIdentity{
		Username: "approver@example.com",
		UID:      "approver-uid",
		Groups:   []string{"release-managers", "system:authenticated"},
	}
}

// An approval is the durable record that a person agreed to one exact plan,
// and these two functions are the whole of what binds that record to the
// person who made the request. Everything downstream -- the audit trail, the
// consumed-approval status, the event -- repeats the name they wrote here.
//
// Each field could be dropped from the comparison with the admission suite
// green, in both families. A dropped one lets an approval be created carrying
// somebody else's identity, and nothing later re-derives it: the request that
// could have contradicted the record is gone by then.
func TestAnApprovalNamesNobodyButItsRequester(t *testing.T) {
	t.Parallel()

	stamped := metav1.NewTime(time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC))

	for _, row := range []struct {
		name  string
		user  func(*authenticationv1.UserInfo)
		claim func(*operatorv1alpha1.ApprovalIdentity)
		blank string
	}{
		{
			name: "the request carries no authenticated username",
			user: func(user *authenticationv1.UserInfo) { user.Username = "  " },
		},
		{
			// The record names one person and another made the request.
			name:  "the record names another person",
			claim: func(identity *operatorv1alpha1.ApprovalIdentity) { identity.Username = "someone.else@example.com" },
		},
		{
			// The same display name under a different account.
			name:  "the record names another account",
			claim: func(identity *operatorv1alpha1.ApprovalIdentity) { identity.UID = "someone-elses-uid" },
		},
		{
			name: "the record claims a group the requester does not have",
			claim: func(identity *operatorv1alpha1.ApprovalIdentity) {
				identity.Groups = append(identity.Groups, "cluster-admins")
			},
		},
		{
			name: "the record drops a group the requester has",
			claim: func(identity *operatorv1alpha1.ApprovalIdentity) {
				identity.Groups = identity.Groups[:1]
			},
		},
		{
			// Normalization deduplicates, so a record that repeats one is not
			// the normalized form of anything the requester presented.
			name: "the record repeats a group",
			claim: func(identity *operatorv1alpha1.ApprovalIdentity) {
				identity.Groups = []string{"release-managers", "release-managers", "system:authenticated"}
			},
		},
		{
			name:  "the mutating webhook stamped no request UID",
			blank: "mutationRequestUID",
		},
		{
			name:  "the mutating webhook stamped no time",
			blank: "approvedAt",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			user := authenticatedApprover()
			if row.user != nil {
				row.user(&user)
			}
			identity := recordedApprover()
			if row.claim != nil {
				row.claim(&identity)
			}
			requestUID := "mutating-webhook-request-uid"
			approvedAt := stamped
			switch row.blank {
			case "mutationRequestUID":
				requestUID = " "
			case "approvedAt":
				approvedAt = metav1.Time{}
			}

			schemaSpec := operatorv1alpha1.PtahSchemaApprovalSpec{
				Approver: identity, MutationRequestUID: requestUID, ApprovedAt: approvedAt,
			}
			if err := identityMatchesRequest(schemaSpec, user); err == nil {
				t.Error("a schema approval naming somebody other than its requester was accepted")
			}
			migrationSpec := operatorv1alpha1.PtahMigrationApprovalSpec{
				Approver: identity, MutationRequestUID: requestUID, ApprovedAt: approvedAt,
			}
			if err := migrationIdentityMatchesRequest(migrationSpec, user); err == nil {
				t.Error("a migration approval naming somebody other than its requester was accepted")
			}
		})
	}
}

// The control: an approval that does name its requester is accepted, in both
// families. Without it every row above would pass against a comparison that
// refuses everything.
func TestAnApprovalNamingItsRequesterIsAccepted(t *testing.T) {
	t.Parallel()

	user := authenticatedApprover()
	identity := recordedApprover()
	stamped := metav1.NewTime(time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC))

	schemaSpec := operatorv1alpha1.PtahSchemaApprovalSpec{
		Approver: identity, MutationRequestUID: "mutating-webhook-request-uid", ApprovedAt: stamped,
	}
	if err := identityMatchesRequest(schemaSpec, user); err != nil {
		t.Fatalf("a schema approval naming its requester was refused: %v", err)
	}
	migrationSpec := operatorv1alpha1.PtahMigrationApprovalSpec{
		Approver: identity, MutationRequestUID: "mutating-webhook-request-uid", ApprovedAt: stamped,
	}
	if err := migrationIdentityMatchesRequest(migrationSpec, user); err != nil {
		t.Fatalf("a migration approval naming its requester was refused: %v", err)
	}
}
