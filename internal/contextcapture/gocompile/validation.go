package gocompile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

var (
	exactCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	sha256Pattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func DecodeArtifact(data []byte) (Artifact, error) {
	if len(data) == 0 || int64(len(data)) > DefaultMaxArtifactBytes {
		return Artifact{}, fmt.Errorf("Go compile context bytes must be non-empty and at most %d bytes", DefaultMaxArtifactBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var artifact Artifact
	if err := decoder.Decode(&artifact); err != nil {
		return Artifact{}, fmt.Errorf("decode Go compile context: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Artifact{}, fmt.Errorf("decode Go compile context: trailing JSON value")
		}
		return Artifact{}, fmt.Errorf("decode Go compile context trailing data: %w", err)
	}
	if err := artifact.Validate(); err != nil {
		return Artifact{}, fmt.Errorf("validate Go compile context: %w", err)
	}
	return artifact, nil
}

func (toolchain Toolchain) Validate() error {
	if !validMessage(toolchain.GoVersion) || !validMessage(toolchain.GOOS) || !validMessage(toolchain.GOARCH) {
		return fmt.Errorf("toolchain identity is invalid")
	}
	if toolchain.CGOEnabled || toolchain.RepositoryCodeExecute ||
		toolchain.ModuleNetwork != "disabled_goproxy_off" ||
		toolchain.Authority != "local_host_unattested" {
		return fmt.Errorf("toolchain safety boundary is invalid")
	}
	return nil
}

func (artifact Artifact) Validate() error {
	if artifact.SchemaVersion != SchemaVersion || artifact.ProviderID != ProviderID ||
		artifact.ProviderRevision != ProviderRevision {
		return fmt.Errorf("unsupported Go compile context identity")
	}
	if !exactCommitPattern.MatchString(artifact.CommitOID) {
		return fmt.Errorf("commit_oid must be an exact lowercase Git object ID")
	}
	if err := artifact.Toolchain.Validate(); err != nil {
		return err
	}
	if artifact.TargetPaths == nil || artifact.Packages == nil || artifact.Gaps == nil {
		return fmt.Errorf("Go compile context collections must be explicit arrays")
	}
	for index, value := range artifact.TargetPaths {
		if !validRepositoryPath(value) || index > 0 && value <= artifact.TargetPaths[index-1] {
			return fmt.Errorf("target_paths must be valid and uniquely sorted")
		}
	}
	previousPackage := ""
	diagnostics := 0
	passed, failed, unavailable := 0, 0, 0
	for index, compiled := range artifact.Packages {
		if !validPackage(compiled.Package) || compiled.Package <= previousPackage {
			return fmt.Errorf("packages must be valid and uniquely sorted")
		}
		previousPackage = compiled.Package
		if compiled.Diagnostics == nil || !sha256Pattern.MatchString(compiled.OutputSHA256) ||
			compiled.OutputSizeBytes < 0 {
			return fmt.Errorf("packages[%d] evidence binding is invalid", index)
		}
		switch compiled.Status {
		case "passed":
			if !compiled.CommandExecuted || !compiled.TestSourcesUsed || compiled.ExitCode != 0 {
				return fmt.Errorf("packages[%d] passed without a successful compile command", index)
			}
			passed++
		case "failed":
			if !compiled.CommandExecuted || !compiled.TestSourcesUsed || compiled.ExitCode <= 0 {
				return fmt.Errorf("packages[%d] failed without a failed compile command", index)
			}
			failed++
		case "unavailable":
			if compiled.CommandExecuted {
				if !compiled.TestSourcesUsed || compiled.ExitCode <= 0 {
					return fmt.Errorf("packages[%d] unavailable command evidence is invalid", index)
				}
			} else if compiled.TestSourcesUsed || compiled.ExitCode != -1 ||
				compiled.OutputSizeBytes != 0 || compiled.OutputSHA256 != emptySHA256() {
				return fmt.Errorf("packages[%d] unavailable non-execution evidence is invalid", index)
			}
			unavailable++
		default:
			return fmt.Errorf("packages[%d] has unsupported status %q", index, compiled.Status)
		}
		previousDiagnostic := ""
		for diagnosticIndex, diagnostic := range compiled.Diagnostics {
			if !validRepositoryPath(diagnostic.Path) || diagnostic.Line == 0 || !validMessage(diagnostic.Message) {
				return fmt.Errorf("packages[%d].diagnostics[%d] is invalid", index, diagnosticIndex)
			}
			key := diagnosticKey(diagnostic)
			if key <= previousDiagnostic {
				return fmt.Errorf("package diagnostics must be uniquely sorted")
			}
			previousDiagnostic = key
			diagnostics++
		}
	}
	counters := []int{
		artifact.Coverage.FilesDiscovered, artifact.Coverage.FilesMaterialized,
		artifact.Coverage.FilesUnavailable, artifact.Coverage.TargetFilesRequested,
		artifact.Coverage.TargetGoFilesFound, artifact.Coverage.PackagesRequested,
		artifact.Coverage.PackagesPassed, artifact.Coverage.PackagesFailed,
		artifact.Coverage.PackagesUnavailable, artifact.Coverage.DiagnosticsEmitted,
	}
	if slices.ContainsFunc(counters, func(value int) bool { return value < 0 }) ||
		artifact.Coverage.FilesMaterialized+artifact.Coverage.FilesUnavailable != artifact.Coverage.FilesDiscovered ||
		artifact.Coverage.TargetGoFilesFound > artifact.Coverage.TargetFilesRequested ||
		artifact.Coverage.PackagesRequested != len(artifact.Packages) ||
		artifact.Coverage.PackagesPassed != passed || artifact.Coverage.PackagesFailed != failed ||
		artifact.Coverage.PackagesUnavailable != unavailable ||
		artifact.Coverage.DiagnosticsEmitted != diagnostics {
		return fmt.Errorf("coverage counters do not reconcile")
	}
	previousGap := ""
	for index, gap := range artifact.Gaps {
		switch gap.Code {
		case "artifact_budget_exceeded", "cgo_not_supported", "compile_failure_unlocalized", "compile_input_incomplete",
			"dependency_unavailable", "file_binary_file", "file_file_size_exceeded",
			"file_non_canonical_line_endings",
			"file_non_regular_file", "file_submodule_not_allowed", "file_symlink_not_allowed",
			"file_target_policy_excluded", "file_unavailable", "file_unsupported_text_encoding",
			"embedded_assets_not_materialized", "local_replace_not_supported", "module_file_unavailable",
			"precompiled_object_not_supported", "source_directive_scan_failed", "source_listing_partial",
			"target_file_unavailable", "target_language_unsupported":
		default:
			return fmt.Errorf("gaps[%d] has unsupported code %q", index, gap.Code)
		}
		if gap.Path != "" && !validRepositoryPath(gap.Path) || gap.Package != "" && !validPackage(gap.Package) {
			return fmt.Errorf("gaps[%d] has invalid path or package", index)
		}
		key := gapKey(gap)
		if key <= previousGap {
			return fmt.Errorf("gaps must be uniquely sorted")
		}
		previousGap = key
	}
	if artifact.Coverage.Truncated != hasGap(artifact.Gaps, "artifact_budget_exceeded") {
		return fmt.Errorf("truncated coverage does not match artifact budget gap")
	}
	return nil
}

func validPackage(value string) bool {
	if value == "." {
		return true
	}
	return strings.HasPrefix(value, "./") && validRepositoryPath(strings.TrimPrefix(value, "./"))
}
