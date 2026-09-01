package piexecution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const (
	LocalRuntimeFileManifestSchemaVersion = contractsv1alpha1.LocalRuntimeFileManifestSchemaVersion
	LocalRuntimeFileManifestAuthority     = contractsv1alpha1.LocalRuntimeFileManifestAuthority
	maxLocalRuntimePackageFiles           = 25_000
	maxLocalRuntimePackageBytes           = int64(256 << 20)
)

// LocalRuntimeFileManifest binds the executable package bytes used by the
// local Pi adapter. It is a host observation, not a signature, remote runtime
// attestation, or provider statement. Paths are package-relative so local
// machine layout is not persisted into governed component artifacts.
type LocalRuntimeFileManifest = contractsv1alpha1.LocalRuntimeFileManifest
type LocalRuntimeFileManifestEntry = contractsv1alpha1.LocalRuntimeFileManifestEntry
type LocalRuntimePackage = contractsv1alpha1.LocalRuntimePackage

type packageLockDocument struct {
	LockfileVersion int                         `json:"lockfileVersion"`
	Packages        map[string]packageLockEntry `json:"packages"`
}

type packageLockEntry struct {
	Version   string `json:"version"`
	Integrity string `json:"integrity"`
	Dev       bool   `json:"dev"`
}

type RuntimeFileVerifier interface {
	Verify(context.Context, string, string, string) error
}

type LocalRuntimeFileVerifier struct{}

func (LocalRuntimeFileVerifier) Verify(
	ctx context.Context,
	nodePath string,
	workerScript string,
	wantSHA256 string,
) error {
	_, _, digest, err := BuildLocalRuntimeFileManifest(ctx, nodePath, workerScript)
	if err != nil {
		return err
	}
	if digest != wantSHA256 {
		return fmt.Errorf("local Pi runtime file manifest changed: got %s, want %s", digest, wantSHA256)
	}
	return nil
}

// BuildLocalRuntimeFileManifest hashes the Node executable, all emitted
// dist/*.js modules, package metadata/lockfile, and every regular file in the
// lockfile's production dependency packages. Nested node_modules directories
// are visited through their own lockfile package entry to avoid duplicates.
func BuildLocalRuntimeFileManifest(
	ctx context.Context,
	nodePath string,
	workerScript string,
) (LocalRuntimeFileManifest, []byte, string, error) {
	if ctx == nil {
		return LocalRuntimeFileManifest{}, nil, "", fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return LocalRuntimeFileManifest{}, nil, "", err
	}
	if !cleanAbsoluteRuntimePath(nodePath) || !cleanAbsoluteRuntimePath(workerScript) {
		return LocalRuntimeFileManifest{}, nil, "", fmt.Errorf("node and worker paths must be clean absolute paths")
	}
	node, err := runtimeFileEntry(ctx, nodePath, "node", 0)
	if err != nil {
		return LocalRuntimeFileManifest{}, nil, "", fmt.Errorf("inspect Node executable: %w", err)
	}
	root := filepath.Dir(filepath.Dir(workerScript))
	distRoot := filepath.Join(root, "dist")
	relativeWorker, err := filepath.Rel(distRoot, workerScript)
	if err != nil || relativeWorker == "." || filepath.IsAbs(relativeWorker) ||
		strings.HasPrefix(relativeWorker, ".."+string(os.PathSeparator)) ||
		filepath.Ext(relativeWorker) != ".js" {
		return LocalRuntimeFileManifest{}, nil, "", fmt.Errorf(
			"worker script must be a JavaScript entrypoint under the package dist directory",
		)
	}

	lockPath := filepath.Join(root, "package-lock.json")
	lockBytes, err := os.ReadFile(lockPath)
	if err != nil {
		return LocalRuntimeFileManifest{}, nil, "", fmt.Errorf("read package-lock.json: %w", err)
	}
	var lock packageLockDocument
	if err := json.Unmarshal(lockBytes, &lock); err != nil {
		return LocalRuntimeFileManifest{}, nil, "", fmt.Errorf("decode package-lock.json: %w", err)
	}
	if lock.LockfileVersion != 3 || lock.Packages == nil {
		return LocalRuntimeFileManifest{}, nil, "", fmt.Errorf("package-lock.json must use lockfileVersion 3 with packages")
	}

	rootFiles := []string{"package-lock.json", "package.json"}
	distEntries, err := os.ReadDir(distRoot)
	if err != nil {
		return LocalRuntimeFileManifest{}, nil, "", fmt.Errorf("read Pi dist directory: %w", err)
	}
	for _, entry := range distEntries {
		if entry.Type().IsRegular() && filepath.Ext(entry.Name()) == ".js" {
			rootFiles = append(rootFiles, filepath.Join("dist", entry.Name()))
		}
	}
	slices.Sort(rootFiles)
	files := make([]LocalRuntimeFileManifestEntry, 0, len(rootFiles))
	workerFound := false
	var packageBytes int64
	for _, relative := range rootFiles {
		entry, entryErr := runtimeFileEntry(
			ctx, filepath.Join(root, relative), filepath.ToSlash(relative), maxLocalRuntimePackageBytes,
		)
		if entryErr != nil {
			return LocalRuntimeFileManifest{}, nil, "", entryErr
		}
		packageBytes += entry.SizeBytes
		files = append(files, entry)
		if filepath.Clean(filepath.Join(root, relative)) == filepath.Clean(workerScript) {
			workerFound = true
		}
	}
	if !workerFound {
		return LocalRuntimeFileManifest{}, nil, "", fmt.Errorf("worker script is absent from dist/*.js closure")
	}

	packageKeys := make([]string, 0)
	for key, entry := range lock.Packages {
		if key == "" || entry.Dev {
			continue
		}
		if !validLockedPackagePath(key) || entry.Version == "" {
			return LocalRuntimeFileManifest{}, nil, "", fmt.Errorf("invalid production package lock entry %q", key)
		}
		packageKeys = append(packageKeys, key)
	}
	slices.Sort(packageKeys)
	packages := make([]LocalRuntimePackage, 0, len(packageKeys))
	for _, key := range packageKeys {
		if err := ctx.Err(); err != nil {
			return LocalRuntimeFileManifest{}, nil, "", err
		}
		locked := lock.Packages[key]
		packages = append(packages, LocalRuntimePackage{
			Path: filepath.ToSlash(key), Version: locked.Version, Integrity: locked.Integrity,
		})
		packageRoot := filepath.Join(root, filepath.FromSlash(key))
		walkErr := filepath.WalkDir(packageRoot, func(path string, item os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if path != packageRoot && item.IsDir() && item.Name() == "node_modules" {
				return filepath.SkipDir
			}
			if item.IsDir() {
				return nil
			}
			info, err := item.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("runtime dependency %q is not a regular non-symlink file", path)
			}
			relative, err := filepath.Rel(root, path)
			if err != nil || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
				return fmt.Errorf("runtime dependency escaped package root")
			}
			entry, err := runtimeFileEntry(ctx, path, filepath.ToSlash(relative), maxLocalRuntimePackageBytes)
			if err != nil {
				return err
			}
			packageBytes += entry.SizeBytes
			if packageBytes > maxLocalRuntimePackageBytes {
				return fmt.Errorf("local Pi runtime package exceeds %d bytes", maxLocalRuntimePackageBytes)
			}
			files = append(files, entry)
			if len(files) > maxLocalRuntimePackageFiles {
				return fmt.Errorf("local Pi runtime package exceeds %d files", maxLocalRuntimePackageFiles)
			}
			return nil
		})
		if walkErr != nil {
			return LocalRuntimeFileManifest{}, nil, "", fmt.Errorf("inspect production package %q: %w", key, walkErr)
		}
	}
	slices.SortFunc(files, func(left, right LocalRuntimeFileManifestEntry) int {
		return strings.Compare(left.Path, right.Path)
	})
	for index := 1; index < len(files); index++ {
		if files[index-1].Path == files[index].Path {
			return LocalRuntimeFileManifest{}, nil, "", fmt.Errorf("runtime file manifest contains duplicate %q", files[index].Path)
		}
	}
	manifest := LocalRuntimeFileManifest{
		SchemaVersion: LocalRuntimeFileManifestSchemaVersion,
		Authority:     LocalRuntimeFileManifestAuthority,
		Node:          node, Files: files, Packages: packages,
	}
	if err := manifest.Validate(); err != nil {
		return LocalRuntimeFileManifest{}, nil, "", fmt.Errorf(
			"validate local runtime file manifest: %w", err,
		)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return LocalRuntimeFileManifest{}, nil, "", err
	}
	digest := sha256.Sum256(data)
	return manifest, data, hex.EncodeToString(digest[:]), nil
}

func runtimeFileEntry(
	ctx context.Context,
	path string,
	relative string,
	maxBytes int64,
) (LocalRuntimeFileManifestEntry, error) {
	if err := ctx.Err(); err != nil {
		return LocalRuntimeFileManifestEntry{}, err
	}
	linkInfo, err := os.Lstat(path)
	if err != nil {
		return LocalRuntimeFileManifestEntry{}, err
	}
	if relative != "node" && linkInfo.Mode()&os.ModeSymlink != 0 {
		return LocalRuntimeFileManifestEntry{}, fmt.Errorf(
			"runtime file %q is not a regular non-symlink file", relative,
		)
	}
	file, err := os.Open(path)
	if err != nil {
		return LocalRuntimeFileManifestEntry{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return LocalRuntimeFileManifestEntry{}, err
	}
	if !info.Mode().IsRegular() || (maxBytes > 0 && info.Size() > maxBytes) {
		return LocalRuntimeFileManifestEntry{}, fmt.Errorf("runtime file %q is not a bounded regular file", relative)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, &contextReader{ctx: ctx, reader: file}); err != nil {
		return LocalRuntimeFileManifestEntry{}, err
	}
	return LocalRuntimeFileManifestEntry{
		Path: relative, SizeBytes: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

func validLockedPackagePath(value string) bool {
	if strings.Contains(value, `\`) {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(value))
	return strings.HasPrefix(filepath.ToSlash(clean), "node_modules/") &&
		!filepath.IsAbs(clean) && clean != "node_modules" &&
		!strings.HasPrefix(clean, ".."+string(os.PathSeparator))
}

func cleanAbsoluteRuntimePath(value string) bool {
	return value != "" && filepath.IsAbs(value) && filepath.Clean(value) == value
}
