package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

const (
	LocalRuntimeFileManifestSchemaVersion = "argus.local_runtime_file_manifest.v1alpha1"
	LocalRuntimeFileManifestContract      = LocalRuntimeFileManifestSchemaVersion
	LocalRuntimeFileManifestAuthority     = "local_host_observation"
	localRuntimeManifestMaxFiles          = 25_000
	localRuntimeManifestMaxBytes          = int64(256 << 20)
)

// LocalRuntimeFileManifest is a content-addressed observation made by the
// local Argus host. It is deliberately not a signature, platform attestation,
// provider statement, or proof of prompt/tool execution.
type LocalRuntimeFileManifest struct {
	SchemaVersion string                          `json:"schema_version"`
	Authority     string                          `json:"authority"`
	Node          LocalRuntimeFileManifestEntry   `json:"node"`
	Files         []LocalRuntimeFileManifestEntry `json:"files"`
	Packages      []LocalRuntimePackage           `json:"production_packages"`
}

type LocalRuntimeFileManifestEntry struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

type LocalRuntimePackage struct {
	Path      string `json:"path"`
	Version   string `json:"version"`
	Integrity string `json:"integrity,omitempty"`
}

func DecodeLocalRuntimeFileManifest(data []byte) (LocalRuntimeFileManifest, error) {
	var manifest LocalRuntimeFileManifest
	if err := decodeStageExecutionJSON(data, &manifest); err != nil {
		return LocalRuntimeFileManifest{}, fmt.Errorf("decode LocalRuntimeFileManifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return LocalRuntimeFileManifest{}, err
	}
	return manifest, nil
}

func (manifest LocalRuntimeFileManifest) Validate() error {
	if manifest.SchemaVersion != LocalRuntimeFileManifestSchemaVersion {
		return fmt.Errorf("unsupported LocalRuntimeFileManifest schema %q", manifest.SchemaVersion)
	}
	if manifest.Authority != LocalRuntimeFileManifestAuthority {
		return fmt.Errorf("runtime file manifest authority must be %q", LocalRuntimeFileManifestAuthority)
	}
	if manifest.Node.Path != "node" {
		return fmt.Errorf("runtime file manifest node path must be node")
	}
	if err := validateLocalRuntimeFileEntry(manifest.Node); err != nil {
		return fmt.Errorf("node: %w", err)
	}
	if len(manifest.Files) == 0 || len(manifest.Files) > localRuntimeManifestMaxFiles {
		return fmt.Errorf("runtime file manifest requires 1..%d files", localRuntimeManifestMaxFiles)
	}
	if manifest.Packages == nil {
		return fmt.Errorf("production_packages must be an explicit array")
	}
	var total int64
	seenPackageFiles := make(map[string]bool, len(manifest.Packages))
	for index, entry := range manifest.Files {
		if err := validateLocalRuntimeFileEntry(entry); err != nil {
			return fmt.Errorf("files[%d]: %w", index, err)
		}
		if entry.Path == "node" || !safeLocalRuntimeRelativePath(entry.Path) {
			return fmt.Errorf("files[%d].path is not a safe package-relative path", index)
		}
		if index > 0 && entry.Path <= manifest.Files[index-1].Path {
			return fmt.Errorf("files must be sorted and unique by path")
		}
		if entry.SizeBytes > localRuntimeManifestMaxBytes-total {
			return fmt.Errorf("runtime file manifest exceeds %d bytes", localRuntimeManifestMaxBytes)
		}
		total += entry.SizeBytes
		for _, item := range manifest.Packages {
			if strings.HasPrefix(entry.Path, item.Path+"/") {
				seenPackageFiles[item.Path] = true
			}
		}
	}
	hasPackageJSON := false
	hasLockfile := false
	hasWorkerModule := false
	for _, entry := range manifest.Files {
		hasPackageJSON = hasPackageJSON || entry.Path == "package.json"
		hasLockfile = hasLockfile || entry.Path == "package-lock.json"
		hasWorkerModule = hasWorkerModule || strings.HasPrefix(entry.Path, "dist/") &&
			strings.HasSuffix(entry.Path, ".js")
	}
	if !hasPackageJSON || !hasLockfile || !hasWorkerModule {
		return fmt.Errorf("runtime file manifest must contain package metadata, lockfile, and dist JavaScript")
	}
	for index, item := range manifest.Packages {
		if !safeLockedRuntimePackagePath(item.Path) || strings.TrimSpace(item.Version) == "" ||
			item.Version != strings.TrimSpace(item.Version) {
			return fmt.Errorf("production_packages[%d] is invalid", index)
		}
		if item.Integrity != "" && (item.Integrity != strings.TrimSpace(item.Integrity) ||
			len(item.Integrity) > 4096) {
			return fmt.Errorf("production_packages[%d].integrity is invalid", index)
		}
		if index > 0 && item.Path <= manifest.Packages[index-1].Path {
			return fmt.Errorf("production_packages must be sorted and unique by path")
		}
		if !seenPackageFiles[item.Path] {
			return fmt.Errorf("production package %q has no attested regular file", item.Path)
		}
	}
	return nil
}

func DigestLocalRuntimeFileManifest(manifest LocalRuntimeFileManifest) (string, error) {
	if err := manifest.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func validateLocalRuntimeFileEntry(entry LocalRuntimeFileManifestEntry) error {
	if entry.Path == "" || entry.SizeBytes < 0 {
		return fmt.Errorf("path and non-negative size_bytes are required")
	}
	return requireSHA256("sha256", entry.SHA256)
}

func safeLocalRuntimeRelativePath(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && !strings.Contains(value, `\`) &&
		!strings.HasPrefix(value, "/") && path.Clean(value) == value && value != "." &&
		value != ".." && !strings.HasPrefix(value, "../")
}

func safeLockedRuntimePackagePath(value string) bool {
	return safeLocalRuntimeRelativePath(value) && strings.HasPrefix(value, "node_modules/") &&
		value != "node_modules"
}
