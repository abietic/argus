package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestLocalRuntimeFileManifestStrictRoundTrip(t *testing.T) {
	manifest := validLocalRuntimeFileManifest()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeLocalRuntimeFileManifest(data)
	if err != nil {
		t.Fatalf("DecodeLocalRuntimeFileManifest() error = %v", err)
	}
	if digest, err := DigestLocalRuntimeFileManifest(decoded); err != nil || len(digest) != 64 {
		t.Fatalf("DigestLocalRuntimeFileManifest() = %q, %v", digest, err)
	}
	unknown := append([]byte{}, data[:len(data)-1]...)
	unknown = append(unknown, []byte(`,"unknown":true}`)...)
	if _, err := DecodeLocalRuntimeFileManifest(unknown); err == nil ||
		!strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown field error = %v", err)
	}
	if _, err := DecodeLocalRuntimeFileManifest(append(data, []byte(` {}`)...)); err == nil {
		t.Fatal("decoder accepted trailing JSON")
	}
}

func TestLocalRuntimeFileManifestRejectsIncompleteOrUnsortedClosure(t *testing.T) {
	tests := []struct {
		name string
		edit func(*LocalRuntimeFileManifest)
		want string
	}{
		{"authority", func(value *LocalRuntimeFileManifest) { value.Authority = "platform_attested" }, "authority"},
		{"unsafe path", func(value *LocalRuntimeFileManifest) { value.Files[0].Path = "../escape" }, "safe package-relative"},
		{"unsorted", func(value *LocalRuntimeFileManifest) { value.Files[0], value.Files[1] = value.Files[1], value.Files[0] }, "sorted"},
		{"missing lockfile", func(value *LocalRuntimeFileManifest) { value.Files = append(value.Files[:2], value.Files[3:]...) }, "metadata"},
		{"package without files", func(value *LocalRuntimeFileManifest) { value.Packages[0].Path = "node_modules/other" }, "no attested"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := validLocalRuntimeFileManifest()
			test.edit(&manifest)
			if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}
}

func validLocalRuntimeFileManifest() LocalRuntimeFileManifest {
	return LocalRuntimeFileManifest{
		SchemaVersion: LocalRuntimeFileManifestSchemaVersion,
		Authority:     LocalRuntimeFileManifestAuthority,
		Node: LocalRuntimeFileManifestEntry{
			Path: "node", SizeBytes: 4, SHA256: runtimeManifestTestDigest("node"),
		},
		Files: []LocalRuntimeFileManifestEntry{
			{Path: "dist/worker.js", SizeBytes: 4, SHA256: runtimeManifestTestDigest("worker")},
			{Path: "node_modules/runtime/index.js", SizeBytes: 4, SHA256: runtimeManifestTestDigest("runtime")},
			{Path: "package-lock.json", SizeBytes: 4, SHA256: runtimeManifestTestDigest("lock")},
			{Path: "package.json", SizeBytes: 4, SHA256: runtimeManifestTestDigest("package")},
		},
		Packages: []LocalRuntimePackage{{
			Path: "node_modules/runtime", Version: "1.0.0", Integrity: "sha512-fixture",
		}},
	}
}

func runtimeManifestTestDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
