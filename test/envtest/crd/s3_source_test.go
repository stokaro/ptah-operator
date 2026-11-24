package crd_test

import (
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// s3Digest is an object digest every S3 rule admits.
const s3Digest = "sha256:5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"

// s3Source selects path from a TarGzip object, which every rule of both
// kinds admits.
func s3Source(path string) map[string]any {
	return map[string]any{
		"endpoint": "https://objects.example.com", "region": "us-east-1", "bucket": "orders-releases",
		"key": "orders/1.4.0/orders.tar.gz", "digest": s3Digest, "format": "TarGzip", "path": path,
	}
}

// asS3Source replaces the base resource's OCI source with an S3 object.
func asS3Source(source string) func(*testing.T, *unstructured.Unstructured) {
	return func(t *testing.T, object *unstructured.Unstructured) {
		unstructured.RemoveNestedField(object.Object, "spec", source, "ociRef")
		unstructured.RemoveNestedField(object.Object, "spec", source, "verificationPolicyFrom")
		set(t, object, s3Source("db/schema.hcl"), "spec", source, "s3")
	}
}

// s3Setting is an S3 source with one field of it set to value.
func s3Setting(source string, value any, field ...string) func(*testing.T, *unstructured.Unstructured) {
	return both(asS3Source(source), setting(value, append([]string{"spec", source, "s3"}, field...)...))
}

// s3Refusals are the rules of an S3 source, which read the same under
// spec.desired and spec.artifact.
func s3Refusals(source string) []refusal {
	field := "spec." + source
	s3 := field + ".s3"
	rows := []refusal{
		{
			name:   source + ".s3 beside ociRef",
			mutate: setting(s3Source("db/schema.hcl"), "spec", source, "s3"),
			want:   []cause{{field, "set exactly one of ociRef, configMap, git, https and s3"}},
		},
		{
			name:   source + ".s3 beside https",
			mutate: both(asS3Source(source), setting(httpsSource("db/schema.hcl"), "spec", source, "https")),
			want:   []cause{{field, "set exactly one of ociRef, configMap, git, https and s3"}},
		},
		{
			name:   source + ".s3 with a verification policy",
			mutate: both(removing("spec", source, "ociRef"), setting(s3Source("db/schema.hcl"), "spec", source, "s3")),
			want:   []cause{{field, "verificationPolicyFrom is required with ociRef and refused with configMap, git, https and s3"}},
		},
		{
			name:   source + ".s3 with registry credentials",
			mutate: both(asS3Source(source), setting(map[string]any{"name": "registry-credentials"}, "spec", source, "registryAuthFrom")),
			want:   []cause{{field, "registryAuthFrom is refused with configMap, git, https and s3"}},
		},
		{
			name:   source + ".s3 with a registry transport",
			mutate: both(asS3Source(source), setting(map[string]any{"caFrom": map[string]any{"name": "registry-ca", "key": "ca.crt"}}, "spec", source, "transport")),
			want:   []cause{{field, "transport is refused with configMap, git, https and s3"}},
		},
		{
			name:   source + ".s3.digest without sha256:",
			mutate: s3Setting(source, strings.TrimPrefix(s3Digest, "sha256:"), "digest"),
			want:   []cause{{s3 + ".digest", "should match"}},
		},
		{
			name:   source + ".s3.digest in uppercase",
			mutate: s3Setting(source, strings.ToUpper(s3Digest), "digest"),
			want:   []cause{{s3 + ".digest", "should match"}},
		},
		{
			name:   source + ".s3.format outside its enum",
			mutate: s3Setting(source, "Tar", "format"),
			want:   []cause{{s3 + ".format", "Unsupported value"}},
		},
		{
			name:   source + ".s3 File with a nested path",
			mutate: both(s3Setting(source, "File", "format"), setting("db/schema.hcl", "spec", source, "s3", "path")),
			want:   []cause{{s3, "with format File, path is the one name the object is given"}},
		},
		{
			name: source + ".s3.transport.caFrom optional",
			mutate: s3Setting(source, map[string]any{"caFrom": map[string]any{"name": "objects-ca", "key": "ca.pem", "optional": true}},
				"transport"),
			want: []cause{{s3 + ".transport", "caFrom must name a required ConfigMap key"}},
		},
		{
			name:   source + ".s3.transport.caFrom without a key",
			mutate: s3Setting(source, map[string]any{"caFrom": map[string]any{"name": "objects-ca", "key": ""}}, "transport"),
			want:   []cause{{s3 + ".transport", "caFrom must name a required ConfigMap key"}},
		},
		{
			name:   source + ".s3.authFrom.name empty",
			mutate: s3Setting(source, map[string]any{"name": ""}, "authFrom"),
			want:   []cause{{s3 + ".authFrom.name", "should be at least 1 chars long"}},
		},
		{
			name:   source + ".s3.authFrom.name past 253 bytes",
			mutate: s3Setting(source, map[string]any{"name": strings.Repeat("a", 254)}, "authFrom"),
			want:   []cause{{s3 + ".authFrom.name", "Too long"}},
		},
		{
			// The endpoint matches its pattern, so the length is the one rule
			// it breaks.
			name:   source + ".s3.endpoint past 267 bytes",
			mutate: s3Setting(source, "https://"+strings.Repeat("e", 260), "endpoint"),
			want:   []cause{{s3 + ".endpoint", "Too long"}},
		},
		{
			name:   source + ".s3.region past 64 bytes",
			mutate: s3Setting(source, strings.Repeat("r", 65), "region"),
			want:   []cause{{s3 + ".region", "Too long"}},
		},
		{
			name:   source + ".s3.region empty",
			mutate: s3Setting(source, "", "region"),
			want:   []cause{{s3 + ".region", "should be at least 1 chars long"}},
		},
		{
			name:   source + ".s3.bucket under 3 bytes",
			mutate: s3Setting(source, "ab", "bucket"),
			want:   []cause{{s3 + ".bucket", "should be at least 3 chars long"}},
		},
		{
			name:   source + ".s3.bucket past 63 bytes",
			mutate: s3Setting(source, strings.Repeat("b", 64), "bucket"),
			want:   []cause{{s3 + ".bucket", "Too long"}},
		},
		{
			// The API server runs no CEL rule over an object the schema
			// refused, so the schema's bound is all this row reports.
			name:   source + ".s3.key past 1024 characters",
			mutate: s3Setting(source, strings.Repeat("k", 1025), "key"),
			want:   []cause{{s3 + ".key", "Too long"}},
		},
		{
			// The schema bounds characters and the rule bounds bytes: 513
			// two-byte characters are within the first and past the second,
			// which is what S3 counts.
			name:   source + ".s3.key past 1024 bytes in 513 characters",
			mutate: s3Setting(source, strings.Repeat("é", 513), "key"),
			want:   []cause{{s3, "key holds at most 1024 bytes"}},
		},
		{
			name:   source + ".s3.versionId empty",
			mutate: s3Setting(source, "", "versionId"),
			want:   []cause{{s3 + ".versionId", "should be at least 1 chars long"}},
		},
		{
			name:   source + ".s3.versionId past 1024 bytes",
			mutate: s3Setting(source, strings.Repeat("v", 1025), "versionId"),
			want:   []cause{{s3 + ".versionId", "Too long"}},
		},
		{
			name:   source + ".s3.path past 1024 bytes",
			mutate: s3Setting(source, strings.Repeat("p", 1025), "path"),
			want:   []cause{{s3 + ".path", "Too long"}},
		},
	}
	for _, required := range []string{"endpoint", "region", "bucket", "key", "digest", "format", "path"} {
		rows = append(rows, refusal{
			name:   fmt.Sprintf("%s.s3.%s is required", source, required),
			mutate: both(asS3Source(source), removing("spec", source, "s3", required)),
			want:   []cause{{s3 + "." + required, "Required value"}},
		})
	}
	// The endpoint is an authority alone: no path, which would turn a
	// path-style request into another bucket's, no credential, query or
	// fragment, and a host the pattern holds to ASCII, as an HTTPS source's
	// is.
	for _, endpoint := range []string{
		"http://objects.example.com", "s3://objects.example.com", "https://objects.example.com/",
		"https://objects.example.com/orders-releases", "https://bot:key@objects.example.com", "https://bot@objects.example.com",
		"https://objects.example.com?x-id=GetObject", "https://objects.example.com#v1", "https://objects_example.com",
		"https://objİcts.example.com", "https://obj%C4%B0cts.example.com", "https://objects.example.com:",
		"https://objects.example.com:123456", "https://[fe80::1%25eth0]", "https://[1.2.3.4]", "https://objects example.com",
	} {
		rows = append(rows, refusal{
			name:   fmt.Sprintf("%s.s3.endpoint %q", source, endpoint),
			mutate: s3Setting(source, endpoint, "endpoint"),
			want:   []cause{{s3 + ".endpoint", "should match"}},
		})
	}
	for _, region := range []string{"US-EAST-1", "eu_west_1", "eu west", "eu-west-1\n"} {
		rows = append(rows, refusal{
			name:   fmt.Sprintf("%s.s3.region %q", source, region),
			mutate: s3Setting(source, region, "region"),
			want:   []cause{{s3 + ".region", "should match"}},
		})
	}
	for _, bucket := range []string{"Orders", "-orders", "orders-", ".orders", "orders.", "orders/releases", "orders releases"} {
		rows = append(rows, refusal{
			name:   fmt.Sprintf("%s.s3.bucket %q", source, bucket),
			mutate: s3Setting(source, bucket, "bucket"),
			want:   []cause{{s3 + ".bucket", "should match"}},
		})
	}
	// The key is sent as a path: an element a client or a proxy would
	// normalize away is another object, so the pattern refuses each.
	for _, key := range []string{
		"/orders.tar.gz", "orders//orders.tar.gz", "./orders.tar.gz", "orders/./orders.tar.gz", "orders/../orders.tar.gz",
		"..", ".", "orders/", "orders/.", "orders/..", "ord\x01ers.tar.gz", "ord\x7fers.tar.gz", "orders.tar.gz\n",
	} {
		rows = append(rows, refusal{
			name:   fmt.Sprintf("%s.s3.key %q", source, key),
			mutate: s3Setting(source, key, "key"),
			want:   []cause{{s3 + ".key", "should match"}},
		})
	}
	for _, version := range []string{"a version", "versión", "version\t1"} {
		rows = append(rows, refusal{
			name:   fmt.Sprintf("%s.s3.versionId %q", source, version),
			mutate: s3Setting(source, version, "versionId"),
			want:   []cause{{s3 + ".versionId", "should match"}},
		})
	}
	for _, path := range []string{"../schema.hcl", "/schema.hcl", "db//schema.hcl", "db/./schema.hcl", "db\\schema.hcl", "db/", "db/sch\x01ema.hcl"} {
		rows = append(rows, refusal{
			name:   fmt.Sprintf("%s.s3.path %q", source, path),
			mutate: s3Setting(source, path, "path"),
			want:   []cause{{s3 + ".path", "should match"}},
		})
	}
	return rows
}

// s3SchemaRefusals are the rules a schema adds to an S3 source: it selects
// one file.
func s3SchemaRefusals() []refusal {
	return []refusal{
		{
			name:   "desired.s3 selecting the archive root",
			mutate: s3Setting("desired", ".", "path"),
			want:   []cause{{"spec", "desired.s3.path selects one file for a schema"}},
		},
		{
			name:   "desired.s3 File named .",
			mutate: both(s3Setting("desired", "File", "format"), setting(".", "spec", "desired", "s3", "path")),
			want: []cause{
				{"spec.desired.s3", "with format File, path is the one name the object is given"},
				{"spec", "desired.s3.path selects one file for a schema"},
			},
		},
	}
}

// s3MigrationRefusals are the rules a migration adds to an S3 source: its
// directory comes in an archive.
func s3MigrationRefusals() []refusal {
	return []refusal{
		{
			name:   "artifact.s3 as a File",
			mutate: both(s3Setting("artifact", "File", "format"), setting("migrations", "spec", "artifact", "s3", "path")),
			want:   []cause{{"spec", "artifact.s3.format is TarGzip or Zip: a migration directory comes in an archive"}},
		},
	}
}

// Each S3 bound is held at its end, and each pattern to the values the
// object stores of the providers it is written for: a refusal row cannot tell
// a rule that refuses too much.
func TestS3SourceRulesAdmitTheirBounds(t *testing.T) {
	plane.Require(t)
	t.Parallel()

	namespace := newNamespace(t, "s3-bounds")
	tests := []struct {
		name   string
		base   func() *unstructured.Unstructured
		mutate func(*testing.T, *unstructured.Unstructured)
	}{
		{
			name: "PtahSchema from an S3 File with a version, a source policy, a key pair and a CA",
			base: schemaBase(namespace),
			mutate: both(asS3Source("desired"), both(
				setting(map[string]any{
					"endpoint": "https://objects.example.com:9000", "region": "us-east-1", "bucket": "inventory-schemas",
					"key": "inventory/1.4.0/schema.hcl", "versionId": "3HL4kqtJlcpXroDTDmJ+rmSpXd3dIbrHY",
					"digest": s3Digest, "format": "File", "path": "schema.hcl",
					"authFrom":  map[string]any{"name": "inventory-objects"},
					"transport": map[string]any{"caFrom": map[string]any{"name": "objects-ca", "key": "ca.pem"}},
				}, "spec", "desired", "s3"),
				setting(map[string]any{"name": "source-policy", "key": "policy.yaml"}, "spec", "desired", "sourcePolicyFrom"))),
		},
		{
			name:   "PtahSchema from a Zip member in S3",
			base:   schemaBase(namespace),
			mutate: s3Setting("desired", "Zip", "format"),
		},
		{
			name:   "PtahMigration from the root of an S3 TarGzip",
			base:   migrationBase(namespace),
			mutate: s3Setting("artifact", ".", "path"),
		},
		{name: "PtahSchema from an S3 endpoint of 267 bytes", base: schemaBase(namespace),
			mutate: s3Setting("desired", "https://"+strings.Repeat("e", 253)+":65535", "endpoint")},
		{name: "PtahSchema from an S3 endpoint of 9 bytes", base: schemaBase(namespace),
			mutate: s3Setting("desired", "https://e", "endpoint")},
		{name: "PtahMigration from an S3 IPv6 endpoint and port", base: migrationBase(namespace),
			mutate: s3Setting("artifact", "https://[2001:db8::1]:9000", "endpoint")},
		{name: "PtahMigration from an S3 IPv4 endpoint", base: migrationBase(namespace),
			mutate: s3Setting("artifact", "https://192.0.2.10", "endpoint")},
		{name: "PtahSchema from an S3 endpoint in capitals", base: schemaBase(namespace),
			mutate: s3Setting("desired", "https://Objects.Example.COM", "endpoint")},
		{name: "PtahSchema from an S3 region of 64 bytes", base: schemaBase(namespace),
			mutate: s3Setting("desired", strings.Repeat("r", 64), "region")},
		{name: "PtahSchema from the S3 region auto", base: schemaBase(namespace),
			mutate: s3Setting("desired", "auto", "region")},
		{name: "PtahSchema from an S3 bucket of 3 bytes", base: schemaBase(namespace),
			mutate: s3Setting("desired", "a.b", "bucket")},
		{name: "PtahSchema from an S3 bucket of 63 bytes", base: schemaBase(namespace),
			mutate: s3Setting("desired", strings.Repeat("b", 63), "bucket")},
		{
			// An underscore is in no bucket a provider creates today, and in
			// buckets an older us-east-1 still serves, so the pattern keeps it.
			name: "PtahSchema from an S3 bucket with an underscore", base: schemaBase(namespace),
			mutate: s3Setting("desired", "orders_releases", "bucket"),
		},
		{name: "PtahSchema from an S3 key of 1024 bytes", base: schemaBase(namespace),
			mutate: s3Setting("desired", strings.Repeat("k", 1024), "key")},
		{name: "PtahSchema from an S3 key of 1024 bytes in 512 characters", base: schemaBase(namespace),
			mutate: s3Setting("desired", strings.Repeat("é", 512), "key")},
		{
			// A dot leading an element, a space, an "@" and a "+" are an
			// object's own name, sent escaped.
			name: "PtahSchema from an S3 key with dots, spaces and signs", base: schemaBase(namespace),
			mutate: s3Setting("desired", "..orders/.hidden/v 1.4+build@2/schema.tar.gz", "key"),
		},
		{name: "PtahSchema from an S3 version of 1024 bytes", base: schemaBase(namespace),
			mutate: s3Setting("desired", strings.Repeat("v", 1024), "versionId")},
		{name: "PtahSchema from the S3 version null", base: schemaBase(namespace),
			mutate: s3Setting("desired", "null", "versionId")},
		{name: "PtahMigration from an S3 path of 1024 bytes", base: migrationBase(namespace),
			mutate: s3Setting("artifact", strings.Repeat("p", 1024), "path")},
		{name: "PtahSchema from an S3 key pair Secret named at 253 bytes", base: schemaBase(namespace),
			mutate: s3Setting("desired", map[string]any{"name": strings.Repeat("a", 253)}, "authFrom")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			object := test.base()
			test.mutate(t, object)
			if err := dryRunCreate(object); err != nil {
				t.Fatalf("the API server refused %s %s at the bound: %v", object.GetKind(), object.GetName(), err)
			}
		})
	}
}
