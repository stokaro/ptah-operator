package sourceview_test

import (
	"reflect"
	"testing"

	operatorv1alpha1 "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/sourceview"
)

const (
	commit        = "3f1c2b0a9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a"
	contentDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	policyDigest  = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	ociDigest     = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
)

var policy = &operatorv1alpha1.SourcePolicyBinding{UID: "policy-uid", Digest: policyDigest}

func TestFromSchemaNamesTheKindItResolvedAndWhatItResolvedTo(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name   string
		status operatorv1alpha1.SchemaSourceStatus
		want   []sourceview.Field
	}{
		{
			name: "a Git branch under a source policy",
			status: operatorv1alpha1.SchemaSourceStatus{
				Git: &operatorv1alpha1.GitSourceRevision{
					URL: "https://git.example.com/acme/app.git", Ref: operatorv1alpha1.GitRef{Branch: "main"},
					Path: "db/schema.sql", Commit: commit, ContentDigest: contentDigest,
				},
				SourcePolicy: policy,
			},
			want: []sourceview.Field{
				{"Source", "Git https://git.example.com/acme/app.git"},
				{"Ref", "branch main"},
				{"Path", "db/schema.sql"},
				{"Commit", commit},
				{"Content", contentDigest},
				{"Source policy", policyDigest},
			},
		},
		{
			name: "a ConfigMap and its snapshot",
			status: operatorv1alpha1.SchemaSourceStatus{
				ConfigMap: &operatorv1alpha1.ConfigMapSourceRevision{Name: "app-schema", UID: "cm-uid"},
				Snapshot:  &operatorv1alpha1.SourceSnapshotBinding{Name: "ptah-src-0a1b2c3d4e5f6a7b8c9d0e1f", ContentDigest: contentDigest},
			},
			want: []sourceview.Field{
				{"Source", "ConfigMap app-schema"},
				{"Snapshot", "ptah-src-0a1b2c3d4e5f6a7b8c9d0e1f"},
				{"Content", contentDigest},
				{"Source policy", ""},
			},
		},
		{
			// An OCI source prints what it printed before the other kinds
			// existed.
			name: "a verified OCI artifact",
			status: operatorv1alpha1.SchemaSourceStatus{
				ResolvedReference: "oci://registry.example/acme/app@" + ociDigest, Digest: ociDigest, Verified: true,
			},
			want: []sourceview.Field{
				{"Artifact", "oci://registry.example/acme/app@" + ociDigest},
				{"Verified", "true"},
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if got := sourceview.FromSchema(row.status).Fields(); !reflect.DeepEqual(got, row.want) {
				t.Fatalf("fields = %q, want %q", got, row.want)
			}
		})
	}
}

func TestFromSchemaKeepsEachKindToItsOwnFields(t *testing.T) {
	t.Parallel()

	git := sourceview.FromSchema(operatorv1alpha1.SchemaSourceStatus{
		Git: &operatorv1alpha1.GitSourceRevision{
			URL: "ssh://git@git.example.com/acme/app.git", Ref: operatorv1alpha1.GitRef{Tag: "v1.4.0"},
			Path: "db/schema.sql", Commit: commit, ContentDigest: contentDigest,
		},
	})
	want := &sourceview.View{
		Kind: sourceview.KindGit, Repository: "ssh://git@git.example.com/acme/app.git",
		Ref: &operatorv1alpha1.GitRef{Tag: "v1.4.0"}, Path: "db/schema.sql", Commit: commit, ContentDigest: contentDigest,
	}
	if !reflect.DeepEqual(git, want) {
		t.Fatalf("view = %+v, want %+v", git, want)
	}
	if got := sourceview.FromSchema(operatorv1alpha1.SchemaSourceStatus{}); got != nil {
		t.Fatalf("an unresolved schema has a source view: %+v", got)
	}
	var unresolved *sourceview.View
	if fields := unresolved.Fields(); fields != nil {
		t.Fatalf("an absent view printed %q", fields)
	}
}

func TestFromMigrationReadsTheArtifactBindingAndTheConfigMapBesideIt(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name      string
		artifact  *operatorv1alpha1.SourceAccessBinding
		configMap *operatorv1alpha1.ConfigMapSourceRevision
		policy    *operatorv1alpha1.SourcePolicyBinding
		want      *sourceview.View
	}{
		{
			name: "a pinned Git commit",
			artifact: &operatorv1alpha1.SourceAccessBinding{Git: &operatorv1alpha1.GitSourceBinding{
				URL: "https://git.example.com/acme/orders.git", Ref: operatorv1alpha1.GitRef{Commit: commit},
				Commit: commit, Path: "db/migrations", ContentDigest: contentDigest,
			}},
			policy: policy,
			want: &sourceview.View{
				Kind: sourceview.KindGit, Repository: "https://git.example.com/acme/orders.git",
				Ref: &operatorv1alpha1.GitRef{Commit: commit}, Path: "db/migrations", Commit: commit,
				ContentDigest: contentDigest, SourcePolicyDigest: policyDigest,
			},
		},
		{
			name:      "a ConfigMap snapshot",
			artifact:  &operatorv1alpha1.SourceAccessBinding{Snapshot: &operatorv1alpha1.SourceSnapshotBinding{Name: "ptah-src-ffffffffffffffffffffffff", ContentDigest: contentDigest}},
			configMap: &operatorv1alpha1.ConfigMapSourceRevision{Name: "orders-migrations"},
			want: &sourceview.View{
				Kind: sourceview.KindConfigMap, ConfigMap: "orders-migrations",
				Snapshot: "ptah-src-ffffffffffffffffffffffff", ContentDigest: contentDigest,
			},
		},
		{
			name:     "an OCI artifact",
			artifact: &operatorv1alpha1.SourceAccessBinding{ResolvedReference: "oci://registry.example/acme/orders@" + ociDigest, Digest: ociDigest},
			want:     &sourceview.View{Kind: sourceview.KindOCI, Reference: "oci://registry.example/acme/orders@" + ociDigest, Digest: ociDigest},
		},
		{name: "nothing resolved yet"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if got := sourceview.FromMigration(row.artifact, row.configMap, row.policy); !reflect.DeepEqual(got, row.want) {
				t.Fatalf("view = %+v, want %+v", got, row.want)
			}
		})
	}
}

func TestFieldsSpellTheRefTheSourceAskedFor(t *testing.T) {
	t.Parallel()

	for ref, want := range map[operatorv1alpha1.GitRef]string{
		{Branch: "main"}:   "branch main",
		{Tag: "v1.4.0"}:    "tag v1.4.0",
		{Commit: commit}:   "commit " + commit,
		{Branch: "v1.4.0"}: "branch v1.4.0",
	} {
		view := sourceview.FromSchema(operatorv1alpha1.SchemaSourceStatus{Git: &operatorv1alpha1.GitSourceRevision{
			URL: "https://git.example.com/acme/app.git", Ref: ref, Path: "db/schema.sql", Commit: commit, ContentDigest: contentDigest,
		}})
		var got string
		for _, field := range view.Fields() {
			if field.Name == "Ref" {
				got = field.Value
			}
		}
		if got != want {
			t.Errorf("ref %+v printed %q, want %q", ref, got, want)
		}
	}
}
