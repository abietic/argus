package godeps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

var exactCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func DecodeArtifact(data []byte) (Artifact, error) {
	if len(data) == 0 || int64(len(data)) > DefaultMaxArtifactBytes {
		return Artifact{}, fmt.Errorf("Go dependency context bytes must be non-empty and at most %d bytes", DefaultMaxArtifactBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var artifact Artifact
	if err := decoder.Decode(&artifact); err != nil {
		return Artifact{}, fmt.Errorf("decode Go dependency context: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Artifact{}, fmt.Errorf("decode Go dependency context: trailing JSON value")
		}
		return Artifact{}, fmt.Errorf("decode Go dependency context trailing data: %w", err)
	}
	if err := artifact.Validate(); err != nil {
		return Artifact{}, fmt.Errorf("validate Go dependency context: %w", err)
	}
	return artifact, nil
}

func (artifact Artifact) Validate() error {
	if artifact.SchemaVersion != SchemaVersion || artifact.ProviderID != ProviderID ||
		artifact.ProviderRevision != ProviderRevision {
		return fmt.Errorf("unsupported Go dependency context identity")
	}
	if !exactCommitPattern.MatchString(artifact.CommitOID) {
		return fmt.Errorf("commit_oid must be an exact lowercase Git object ID")
	}
	if artifact.ModulePath != "" && !validText(artifact.ModulePath, 512) {
		return fmt.Errorf("module_path is invalid")
	}
	if artifact.TargetPaths == nil || artifact.Packages == nil || artifact.Imports == nil || artifact.Gaps == nil {
		return fmt.Errorf("Go dependency context collections must be explicit arrays")
	}
	for index, value := range artifact.TargetPaths {
		if err := validateRepositoryPath(value); err != nil {
			return fmt.Errorf("target_paths[%d]: %w", index, err)
		}
		if index > 0 && value <= artifact.TargetPaths[index-1] {
			return fmt.Errorf("target_paths must be uniquely sorted")
		}
	}
	previous := ""
	for index, value := range artifact.Packages {
		if value.Directory != "." {
			if err := validateRepositoryPath(value.Directory); err != nil {
				return fmt.Errorf("packages[%d].directory: %w", index, err)
			}
		}
		if !validIdentifier(value.Name) || value.Files == nil || !slices.IsSorted(value.Files) {
			return fmt.Errorf("packages[%d] has invalid name or unsorted files", index)
		}
		for fileIndex, file := range value.Files {
			if err := validateRepositoryPath(file); err != nil {
				return fmt.Errorf("packages[%d].files[%d]: %w", index, fileIndex, err)
			}
			if path.Dir(file) != value.Directory {
				return fmt.Errorf("packages[%d] contains a file from another directory", index)
			}
			if fileIndex > 0 && file == value.Files[fileIndex-1] {
				return fmt.Errorf("packages[%d] contains duplicate file %q", index, file)
			}
		}
		if index > 0 && value.Directory <= previous {
			return fmt.Errorf("packages must be uniquely sorted by directory")
		}
		previous = value.Directory
	}
	previous = ""
	for index, value := range artifact.Imports {
		if value.FromPackage != "." {
			if err := validateRepositoryPath(value.FromPackage); err != nil {
				return fmt.Errorf("imports[%d].from_package: %w", index, err)
			}
		}
		if !validText(value.ImportPath, 512) || value.Line == 0 {
			return fmt.Errorf("imports[%d] has invalid import path or line", index)
		}
		if err := validateRepositoryPath(value.Path); err != nil {
			return fmt.Errorf("imports[%d].path: %w", index, err)
		}
		if path.Dir(value.Path) != value.FromPackage {
			return fmt.Errorf("imports[%d] source path is outside from_package", index)
		}
		switch value.Resolution {
		case "internal_exact", "internal_unresolved":
			if value.ResolvedDirectory == "" {
				return fmt.Errorf("imports[%d] internal resolution requires a directory", index)
			}
			if value.ResolvedDirectory != "." {
				if err := validateRepositoryPath(value.ResolvedDirectory); err != nil {
					return fmt.Errorf("imports[%d].resolved_directory: %w", index, err)
				}
			}
		case "stdlib", "external":
			if value.ResolvedDirectory != "" {
				return fmt.Errorf("imports[%d] non-internal resolution claims a directory", index)
			}
		default:
			return fmt.Errorf("imports[%d] has unsupported resolution %q", index, value.Resolution)
		}
		key := importKey(value)
		if index > 0 && key <= previous {
			return fmt.Errorf("imports must be uniquely sorted")
		}
		previous = key
	}
	if artifact.Coverage.GoFilesDiscovered < 0 || artifact.Coverage.GoFilesParsed < 0 ||
		artifact.Coverage.GoFilesFailed < 0 || artifact.Coverage.TargetFilesRequested < 0 ||
		artifact.Coverage.TargetGoFilesFound < 0 || artifact.Coverage.PackagesEmitted < 0 ||
		artifact.Coverage.ImportsEmitted < 0 {
		return fmt.Errorf("coverage counters must not be negative")
	}
	if artifact.Coverage.GoFilesParsed+artifact.Coverage.GoFilesFailed != artifact.Coverage.GoFilesDiscovered ||
		artifact.Coverage.TargetGoFilesFound > artifact.Coverage.TargetFilesRequested ||
		artifact.Coverage.PackagesEmitted != len(artifact.Packages) ||
		artifact.Coverage.ImportsEmitted != len(artifact.Imports) {
		return fmt.Errorf("coverage counters do not reconcile")
	}
	previous = ""
	for index, gap := range artifact.Gaps {
		switch gap.Code {
		case "artifact_budget_exceeded", "dependency_file_listing_partial", "file_unavailable",
			"file_binary_file", "file_file_size_exceeded", "file_non_regular_file",
			"file_non_canonical_line_endings",
			"file_submodule_not_allowed", "file_symlink_not_allowed",
			"file_target_policy_excluded", "file_unsupported_text_encoding",
			"import_limit_exceeded", "import_path_invalid", "module_path_unavailable",
			"package_limit_exceeded", "package_name_conflict", "parse_failed",
			"target_file_unavailable", "target_language_unsupported":
		default:
			return fmt.Errorf("gaps[%d] has unsupported code %q", index, gap.Code)
		}
		if gap.Path != "" {
			if err := validateRepositoryPath(gap.Path); err != nil {
				return fmt.Errorf("gaps[%d].path: %w", index, err)
			}
		}
		key := gapKey(gap)
		if index > 0 && key <= previous {
			return fmt.Errorf("gaps must be uniquely sorted")
		}
		previous = key
	}
	if artifact.Coverage.Truncated !=
		(hasGap(artifact.Gaps, "artifact_budget_exceeded") ||
			hasGap(artifact.Gaps, "import_limit_exceeded") ||
			hasGap(artifact.Gaps, "package_limit_exceeded")) {
		return fmt.Errorf("truncated coverage does not match truncation gaps")
	}
	return nil
}

func validateRepositoryPath(value string) error {
	if !validText(value, 4096) || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") ||
		path.Clean(value) != value || value == "." || value == ".." || strings.HasPrefix(value, "../") {
		return fmt.Errorf("must be a clean relative repository path")
	}
	return nil
}

func validIdentifier(value string) bool {
	if value == "" || len(value) > 255 || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if unicode.IsLetter(char) || unicode.IsDigit(char) || char == '_' {
			continue
		}
		return false
	}
	return true
}
