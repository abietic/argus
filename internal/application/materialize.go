package application

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"hash"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/source/gitadapter"
	"argus.local/argus/internal/targetmodel"
)

var ErrTargetNotReviewable = errors.New("target is not reviewable")

type TargetSource interface {
	MaterializeDiff(context.Context, gitadapter.Request) (gitadapter.Result, error)
	CaptureRevision(
		context.Context,
		gitadapter.RevisionRequest,
	) (gitadapter.RevisionCapture, error)
	RevalidateRevision(
		context.Context,
		string,
		string,
		string,
	) (gitadapter.RevisionRevalidation, error)
	ListFilesAtCommit(
		context.Context,
		string,
		string,
		[]string,
		[]string,
		int,
	) (gitadapter.RevisionFileList, error)
	ReadFileAtCommit(
		context.Context,
		string,
		string,
		string,
		int64,
	) (gitadapter.FileContent, error)
}

type scopeAdmissionTargetSource interface {
	ListFilesAtCommitWithAdmission(
		context.Context,
		string,
		string,
		gitadapter.ScopeAdmission,
		int,
	) (gitadapter.RevisionFileList, error)
}

type ArtifactWriter interface {
	PutArtifact(string, []byte) (runmodel.ArtifactRef, error)
	PutJSONArtifact(string, any) (runmodel.ArtifactRef, error)
}

type Materialization struct {
	RepositoryRoot string
	Target         MaterializedTarget
	TargetRef      runmodel.ArtifactRef
	Input          reviewcore.ReviewInput
	InputRef       runmodel.ArtifactRef
}

// Materialize freezes all bytes consumed by the deterministic review core.
// It reads file content from the resolved head commit, never from the mutable
// working tree.
func Materialize(
	ctx context.Context,
	source TargetSource,
	artifacts ArtifactWriter,
	request ReviewRequest,
	config LocalConfig,
) (Materialization, error) {
	if ctx == nil || source == nil || artifacts == nil {
		return Materialization{}, fmt.Errorf("materialization dependencies are required")
	}
	if err := request.Validate(config); err != nil {
		return Materialization{}, err
	}
	if _, err := frozenContextBytes(request.Contexts, config.MaxMaterializedBytes); err != nil {
		return Materialization{}, fmt.Errorf("%w: %v", ErrTargetNotReviewable, err)
	}
	if request.MaxFiles > config.MaxFiles {
		return Materialization{}, fmt.Errorf(
			"review request max_files=%d exceeds local hard limit %d",
			request.MaxFiles,
			config.MaxFiles,
		)
	}
	if request.MaxPatchBytes > config.MaxPatchBytes {
		return Materialization{}, fmt.Errorf(
			"review request max_patch_bytes=%d exceeds local hard limit %d",
			request.MaxPatchBytes,
			config.MaxPatchBytes,
		)
	}
	switch request.normalizedMode() {
	case reviewcore.TargetModeDiff:
		return materializeDiff(ctx, source, artifacts, request, config)
	case reviewcore.TargetModeSelection:
		allowed, err := targetPathAllowed(config, request.SelectionPath)
		if err != nil {
			return Materialization{}, err
		}
		if !allowed {
			return Materialization{}, fmt.Errorf(
				"%w: selected path %q is denied by target policy",
				ErrTargetNotReviewable,
				request.SelectionPath,
			)
		}
		return materializeSelection(ctx, source, artifacts, request, config)
	case reviewcore.TargetModeScope:
		return materializeScope(ctx, source, artifacts, request, config)
	default:
		return Materialization{}, fmt.Errorf("unsupported target mode %q", request.Mode)
	}
}

func materializeDiff(
	ctx context.Context,
	source TargetSource,
	artifacts ArtifactWriter,
	request ReviewRequest,
	config LocalConfig,
) (Materialization, error) {
	maxPatchBytes := request.MaxPatchBytes
	if maxPatchBytes == 0 {
		maxPatchBytes = config.MaxPatchBytes
	}
	maxFiles := request.MaxFiles
	if maxFiles == 0 {
		maxFiles = config.MaxFiles
	}
	result, err := source.MaterializeDiff(ctx, gitadapter.Request{
		RepositoryPath: request.RepositoryPath,
		BaseRevision:   request.BaseRevision,
		HeadRevision:   request.HeadRevision,
		Limits: gitadapter.Limits{
			MaxPatchBytes: maxPatchBytes,
			MaxFiles:      maxFiles,
		},
	})
	if err != nil {
		return Materialization{}, fmt.Errorf("materialize Git diff: %w", err)
	}
	if err := validateDiffResult(ctx, result, request, maxPatchBytes, maxFiles); err != nil {
		return Materialization{}, fmt.Errorf("validate Git diff capture: %w", err)
	}
	if result.Snapshot == nil || result.Manifest == nil {
		return Materialization{}, fmt.Errorf("%w: source returned no target snapshot", ErrTargetNotReviewable)
	}
	if result.Snapshot.SchemaVersion != gitadapter.TargetSnapshotSchemaVersion {
		return Materialization{}, fmt.Errorf("source returned an unsupported diff target snapshot")
	}
	sourceSnapshotDigest, err := gitadapter.DigestTargetSnapshot(*result.Snapshot)
	if err != nil || sourceSnapshotDigest != result.Snapshot.SHA256 {
		return Materialization{}, fmt.Errorf("source returned an invalid diff target snapshot digest")
	}
	if !result.Patch.Included {
		return Materialization{}, fmt.Errorf(
			"%w: canonical patch bytes were not retained (%s)",
			ErrTargetNotReviewable,
			formatReasons(result.Reasons),
		)
	}
	if len(result.Patch.Data) == 0 {
		return Materialization{}, fmt.Errorf("%w: Git diff contains no changes", ErrTargetNotReviewable)
	}
	admittedPatch, admittedManifest, err := admitDiffArtifacts(result)
	if err != nil {
		return Materialization{}, err
	}
	if err := validateDiffTargetPolicy(admittedManifest, config); err != nil {
		return Materialization{}, err
	}

	patchRef, err := artifacts.PutArtifact(ContractCanonicalPatch, admittedPatch)
	if err != nil {
		return Materialization{}, fmt.Errorf("persist canonical patch: %w", err)
	}
	if patchRef.SHA256 != admittedManifest.PatchSHA256 ||
		patchRef.SizeBytes != admittedManifest.PatchSize {
		return Materialization{}, fmt.Errorf("persisted patch identity differs from source capture")
	}
	manifestRef, err := artifacts.PutJSONArtifact(ContractChangeManifest, admittedManifest)
	if err != nil {
		return Materialization{}, fmt.Errorf("persist change manifest: %w", err)
	}

	changes := append([]gitadapter.FileChange(nil), admittedManifest.Files...)
	sort.Slice(changes, func(left, right int) bool {
		return changes[left].Path < changes[right].Path
	})
	fileRefs := make([]TargetFileRef, 0, len(changes))
	reviewFiles := make([]reviewcore.FileManifestEntry, 0, len(changes))
	completenessReasons := append(
		[]gitadapter.Reason{},
		result.Snapshot.CompletenessReason...,
	)
	contextBytes, err := frozenContextBytes(request.Contexts, config.MaxMaterializedBytes)
	if err != nil {
		return Materialization{}, err
	}
	if admittedManifest.PatchSize > config.MaxMaterializedBytes-contextBytes {
		return Materialization{}, fmt.Errorf(
			"%w: canonical patch plus context exceeds max_materialized_bytes=%d",
			ErrTargetNotReviewable,
			config.MaxMaterializedBytes,
		)
	}
	materializedBytes := admittedManifest.PatchSize + contextBytes
	for index, change := range changes {
		if err := ctx.Err(); err != nil {
			return Materialization{}, err
		}
		if index > 0 && change.Path == changes[index-1].Path {
			return Materialization{}, fmt.Errorf("change manifest contains duplicate path %q", change.Path)
		}
		if !change.Included {
			fileRefs = append(fileRefs, TargetFileRef{
				Path:         change.Path,
				Status:       change.Status,
				Completeness: gitadapter.CompletenessSkipped,
				Reasons:      reasonsFromCodes(change.SkippedReasons),
			})
			continue
		}
		if change.Status == gitadapter.ChangeDeleted {
			fileRefs = append(fileRefs, TargetFileRef{
				Path:         change.Path,
				Status:       change.Status,
				Completeness: gitadapter.CompletenessSkipped,
				Reasons: []gitadapter.Reason{{
					Code:   gitadapter.ReasonFileNotFound,
					Detail: "file is deleted in the exact head commit",
				}},
			})
			continue
		}

		content, readErr := source.ReadFileAtCommit(
			ctx,
			result.RepositoryRoot,
			result.Snapshot.Head.CommitOID,
			change.Path,
			config.MaxFileContentBytes,
		)
		if readErr != nil {
			return Materialization{}, fmt.Errorf("read exact head file %q: %w", change.Path, readErr)
		}
		if err := validateFileContent(
			content,
			result.Snapshot.Head.CommitOID,
			result.Snapshot.Repository.ObjectFormat,
			change.Path,
			nil,
			false,
			config.MaxFileContentBytes,
		); err != nil {
			return Materialization{}, fmt.Errorf(
				"validate exact head file %q: %w",
				change.Path,
				err,
			)
		}
		targetFile := TargetFileRef{
			Path:         change.Path,
			Status:       change.Status,
			SHA256:       content.SHA256,
			SizeBytes:    content.SizeBytes,
			Completeness: content.Completeness,
			Reasons:      append([]gitadapter.Reason{}, content.Reasons...),
		}
		var text *string
		if content.Included {
			if materializedBytes+content.SizeBytes > config.MaxMaterializedBytes {
				reason := gitadapter.Reason{
					Code: gitadapter.ReasonMaterializedBytesExceeded,
					Detail: fmt.Sprintf(
						"retaining %q would exceed max_materialized_bytes=%d",
						change.Path,
						config.MaxMaterializedBytes,
					),
				}
				targetFile.Completeness = gitadapter.CompletenessSkipped
				targetFile.Reasons = []gitadapter.Reason{reason}
				completenessReasons = append(
					completenessReasons,
					reasonForPath(change.Path, reason),
				)
				fileRefs = append(fileRefs, targetFile)
				reviewFiles = append(reviewFiles, reviewcore.FileManifestEntry{
					Path: change.Path, SHA256: content.SHA256,
					SizeBytes: content.SizeBytes, Content: nil,
				})
				continue
			}
			contentRef, putErr := artifacts.PutArtifact(ContractFileContent, content.Data)
			if putErr != nil {
				return Materialization{}, fmt.Errorf("persist exact head file %q: %w", change.Path, putErr)
			}
			if contentRef.SHA256 != content.SHA256 ||
				contentRef.SizeBytes != content.SizeBytes {
				return Materialization{}, fmt.Errorf(
					"persisted file identity differs from source capture for %q",
					change.Path,
				)
			}
			targetFile.ContentRef = &contentRef
			materializedBytes += content.SizeBytes
			value := string(content.Data)
			text = &value
		} else {
			for _, reason := range content.Reasons {
				completenessReasons = append(
					completenessReasons,
					reasonForPath(change.Path, reason),
				)
			}
		}
		fileRefs = append(fileRefs, targetFile)
		if content.SHA256 != "" {
			reviewFiles = append(reviewFiles, reviewcore.FileManifestEntry{
				Path:      change.Path,
				SHA256:    content.SHA256,
				SizeBytes: content.SizeBytes,
				Content:   text,
			})
		}
	}

	snapshot, err := SealTargetSnapshot(TargetSnapshot{
		SchemaVersion:  TargetSnapshotSchemaVersion,
		Mode:           reviewcore.TargetModeDiff,
		Repository:     result.Snapshot.Repository,
		Base:           result.Snapshot.Base,
		Head:           result.Snapshot.Head,
		ManifestSHA256: manifestRef.SHA256,
		Diff: &DiffSnapshot{
			PatchSHA256:    admittedManifest.PatchSHA256,
			PatchSizeBytes: admittedManifest.PatchSize,
			PatchFormat:    gitadapter.CanonicalPatchVersion,
		},
		DirtyState:         result.Snapshot.DirtyState,
		Completeness:       completenessFromReasons(completenessReasons),
		CompletenessReason: completenessReasons,
		CapturedAt:         result.Snapshot.CapturedAt,
		CapturedBy:         result.Snapshot.CapturedBy,
		GitVersion:         result.Snapshot.GitVersion,
	})
	if err != nil {
		return Materialization{}, fmt.Errorf("seal diff target snapshot: %w", err)
	}
	if err := targetmodel.ValidateDiffManifest(admittedManifest, snapshot); err != nil {
		return Materialization{}, fmt.Errorf("validate change manifest closure: %w", err)
	}
	target := MaterializedTarget{
		SchemaVersion: MaterializedTargetSchemaVersion,
		Snapshot:      snapshot,
		ManifestRef:   manifestRef,
		PatchRef:      &patchRef,
		FileRefs:      fileRefs,
		Contexts:      cloneContextBindings(request.Contexts),
	}
	if err := target.Validate(); err != nil {
		return Materialization{}, fmt.Errorf("validate materialized target: %w", err)
	}
	targetRef, err := artifacts.PutJSONArtifact(ContractMaterializedTarget, target)
	if err != nil {
		return Materialization{}, fmt.Errorf("persist materialized target: %w", err)
	}

	input := reviewcore.ReviewInput{
		SchemaVersion:  reviewcore.ReviewInputSchemaVersion,
		TargetID:       snapshot.TargetSnapshotID,
		TargetMode:     reviewcore.TargetModeDiff,
		CanonicalPatch: string(admittedPatch),
		Regions:        []reviewcore.ReviewRegion{},
		Files:          reviewFiles,
		Contexts:       cloneContextBindings(request.Contexts),
	}
	if err := input.Validate(); err != nil {
		return Materialization{}, fmt.Errorf("validate review input: %w", err)
	}
	inputRef, err := artifacts.PutJSONArtifact(ContractReviewInput, input)
	if err != nil {
		return Materialization{}, fmt.Errorf("persist review input: %w", err)
	}
	return Materialization{
		RepositoryRoot: result.RepositoryRoot,
		Target:         target,
		TargetRef:      targetRef,
		Input:          input,
		InputRef:       inputRef,
	}, nil
}

func materializeSelection(
	ctx context.Context,
	source TargetSource,
	artifacts ArtifactWriter,
	request ReviewRequest,
	config LocalConfig,
) (Materialization, error) {
	capture, err := source.CaptureRevision(ctx, gitadapter.RevisionRequest{
		RepositoryPath: request.RepositoryPath,
		Revision:       request.Revision,
	})
	if err != nil {
		return Materialization{}, fmt.Errorf("capture selection revision: %w", err)
	}
	if err := validateRevisionCapture(
		capture,
		request.RepositoryPath,
		request.Revision,
	); err != nil {
		return Materialization{}, fmt.Errorf("validate selection revision capture: %w", err)
	}
	content := gitadapter.FileContent{}
	sourceKind := selectionSourceCommit
	if request.OverlayContent == nil {
		content, err = source.ReadFileAtCommit(
			ctx,
			capture.RepositoryRoot,
			capture.Revision.CommitOID,
			request.SelectionPath,
			config.MaxFileContentBytes,
		)
		if err != nil {
			return Materialization{}, fmt.Errorf("read selected file: %w", err)
		}
	} else {
		sourceKind = selectionSourceOverlay
		data := []byte(*request.OverlayContent)
		if int64(len(data)) > config.MaxFileContentBytes {
			return Materialization{}, fmt.Errorf(
				"%w: selection overlay is %d bytes (max_file_content_bytes=%d)",
				ErrTargetNotReviewable,
				len(data),
				config.MaxFileContentBytes,
			)
		}
		content = gitadapter.FileContent{
			SchemaVersion: gitadapter.FileContentSchemaVersion,
			CommitOID:     capture.Revision.CommitOID,
			Path:          request.SelectionPath,
			SHA256:        sha256Hex(data),
			SizeBytes:     int64(len(data)),
			Included:      true,
			Completeness:  gitadapter.CompletenessComplete,
			Reasons:       []gitadapter.Reason{},
			Data:          data,
		}
	}
	if err := validateFileContent(
		content,
		capture.Revision.CommitOID,
		capture.Repository.ObjectFormat,
		request.SelectionPath,
		nil,
		request.OverlayContent != nil,
		config.MaxFileContentBytes,
	); err != nil {
		return Materialization{}, fmt.Errorf("validate selected file capture: %w", err)
	}
	if !content.Included {
		return Materialization{}, fmt.Errorf(
			"%w: selected file is unavailable (%s)",
			ErrTargetNotReviewable,
			formatReasons(content.Reasons),
		)
	}
	contextBytes, err := frozenContextBytes(request.Contexts, config.MaxMaterializedBytes)
	if err != nil {
		return Materialization{}, err
	}
	if content.SizeBytes > config.MaxMaterializedBytes-contextBytes {
		return Materialization{}, fmt.Errorf(
			"%w: selected file and contexts use %d bytes (max_materialized_bytes=%d)",
			ErrTargetNotReviewable,
			content.SizeBytes+contextBytes,
			config.MaxMaterializedBytes,
		)
	}
	effectiveRanges, symbol, err := resolveSelectionSelector(string(content.Data), request)
	if err != nil {
		return Materialization{}, fmt.Errorf("%w: %v", ErrTargetNotReviewable, err)
	}
	selected, err := canonicalSelectionRangesContent(string(content.Data), effectiveRanges)
	if err != nil {
		return Materialization{}, fmt.Errorf("%w: %v", ErrTargetNotReviewable, err)
	}
	fileRef, err := artifacts.PutArtifact(ContractFileContent, content.Data)
	if err != nil {
		return Materialization{}, fmt.Errorf("persist selected file: %w", err)
	}
	if fileRef.SHA256 != content.SHA256 || fileRef.SizeBytes != content.SizeBytes {
		return Materialization{}, fmt.Errorf("persisted selected file identity changed")
	}
	selectionRef, err := artifacts.PutArtifact(ContractSelectionContent, selected)
	if err != nil {
		return Materialization{}, fmt.Errorf("persist selected content: %w", err)
	}
	revalidation, err := source.RevalidateRevision(
		ctx,
		capture.RepositoryRoot,
		capture.Revision.Requested,
		capture.Revision.CommitOID,
	)
	if err != nil {
		return Materialization{}, fmt.Errorf("revalidate selection revision: %w", err)
	}
	if err := validateRevisionRevalidation(revalidation, capture); err != nil {
		return Materialization{}, fmt.Errorf("validate selection revision revalidation: %w", err)
	}
	reasons := append([]gitadapter.Reason{}, capture.Reasons...)
	reasons = append(reasons, revalidation.Reasons...)
	completeness := completenessFromReasons(reasons)
	manifest := SelectionManifest{
		SchemaVersion:          ContractSelectionManifest,
		Repository:             capture.Repository,
		Revision:               capture.Revision,
		Path:                   request.SelectionPath,
		StartLine:              request.StartLine,
		EndLine:                request.EndLine,
		EffectiveRanges:        slices.Clone(effectiveRanges),
		Symbol:                 cloneSymbolSelector(symbol),
		SourceKind:             sourceKind,
		FileSHA256:             fileRef.SHA256,
		FileSizeBytes:          fileRef.SizeBytes,
		SelectionContentSHA256: selectionRef.SHA256,
		SelectionSizeBytes:     selectionRef.SizeBytes,
		Completeness:           completeness,
		Reasons:                append([]gitadapter.Reason{}, reasons...),
	}
	manifestRef, err := artifacts.PutJSONArtifact(ContractSelectionManifest, manifest)
	if err != nil {
		return Materialization{}, fmt.Errorf("persist selection manifest: %w", err)
	}
	snapshot, err := SealTargetSnapshot(TargetSnapshot{
		SchemaVersion:  TargetSnapshotSchemaVersion,
		Mode:           reviewcore.TargetModeSelection,
		Repository:     capture.Repository,
		Base:           capture.Revision,
		Head:           capture.Revision,
		ManifestSHA256: manifestRef.SHA256,
		Selection: &SelectionSnapshot{
			Path: request.SelectionPath, StartLine: request.StartLine, EndLine: request.EndLine,
			EffectiveRanges: slices.Clone(effectiveRanges),
			Symbol:          cloneSymbolSelector(symbol),
			SourceKind:      sourceKind, FileSHA256: fileRef.SHA256, FileSizeBytes: fileRef.SizeBytes,
			SelectionContentSHA256: selectionRef.SHA256,
			SelectionSizeBytes:     selectionRef.SizeBytes,
		},
		DirtyState:         capture.DirtyState,
		Completeness:       completeness,
		CompletenessReason: reasons,
		CapturedAt:         capture.CapturedAt,
		CapturedBy:         capture.CapturedBy,
		GitVersion:         capture.GitVersion,
	})
	if err != nil {
		return Materialization{}, fmt.Errorf("seal selection target snapshot: %w", err)
	}
	if err := manifest.ValidateAgainst(snapshot); err != nil {
		return Materialization{}, fmt.Errorf("validate selection manifest closure: %w", err)
	}
	target := MaterializedTarget{
		SchemaVersion:       MaterializedTargetSchemaVersion,
		Snapshot:            snapshot,
		ManifestRef:         manifestRef,
		SelectionContentRef: &selectionRef,
		FileRefs: []TargetFileRef{{
			Path: request.SelectionPath, SHA256: content.SHA256,
			SizeBytes: content.SizeBytes, ContentRef: &fileRef,
			Completeness: gitadapter.CompletenessComplete, Reasons: []gitadapter.Reason{},
		}},
		Contexts: cloneContextBindings(request.Contexts),
	}
	if err := target.Validate(); err != nil {
		return Materialization{}, fmt.Errorf("validate materialized selection: %w", err)
	}
	targetRef, err := artifacts.PutJSONArtifact(ContractMaterializedTarget, target)
	if err != nil {
		return Materialization{}, fmt.Errorf("persist materialized selection: %w", err)
	}
	text := string(content.Data)
	regions := make([]reviewcore.ReviewRegion, 0, len(effectiveRanges))
	for _, lineRange := range effectiveRanges {
		regions = append(regions, reviewcore.ReviewRegion{
			Path: request.SelectionPath, StartLine: lineRange.StartLine,
			EndLine: lineRange.EndLine, SHA256: content.SHA256,
		})
	}
	input := reviewcore.ReviewInput{
		SchemaVersion:  reviewcore.ReviewInputSchemaVersion,
		TargetID:       snapshot.TargetSnapshotID,
		TargetMode:     reviewcore.TargetModeSelection,
		CanonicalPatch: "",
		Regions:        regions,
		Files: []reviewcore.FileManifestEntry{{
			Path: request.SelectionPath, SHA256: content.SHA256,
			SizeBytes: content.SizeBytes, Content: &text,
		}},
		Contexts: cloneContextBindings(request.Contexts),
	}
	return persistReviewInput(artifacts, capture.RepositoryRoot, target, targetRef, input)
}

func materializeScope(
	ctx context.Context,
	source TargetSource,
	artifacts ArtifactWriter,
	request ReviewRequest,
	config LocalConfig,
) (Materialization, error) {
	capture, err := source.CaptureRevision(ctx, gitadapter.RevisionRequest{
		RepositoryPath: request.RepositoryPath,
		Revision:       request.Revision,
	})
	if err != nil {
		return Materialization{}, fmt.Errorf("capture scope revision: %w", err)
	}
	if err := validateRevisionCapture(
		capture,
		request.RepositoryPath,
		request.Revision,
	); err != nil {
		return Materialization{}, fmt.Errorf("validate scope revision capture: %w", err)
	}
	maxFiles := request.MaxFiles
	if maxFiles == 0 {
		maxFiles = config.MaxFiles
	}
	admittedInclude := append([]string(nil), request.Include...)
	admittedExclude := append([]string(nil), request.Exclude...)
	admittedPolicyInclude := append([]string(nil), config.TargetInclude...)
	admittedPolicyExclude := append([]string(nil), config.TargetExclude...)
	scopeSource, ok := source.(scopeAdmissionTargetSource)
	if !ok {
		return Materialization{}, fmt.Errorf(
			"target source does not support pre-quota scope policy admission",
		)
	}
	listing, err := scopeSource.ListFilesAtCommitWithAdmission(
		ctx,
		capture.RepositoryRoot,
		capture.Revision.CommitOID,
		gitadapter.ScopeAdmission{
			Include:       append([]string(nil), admittedInclude...),
			Exclude:       append([]string(nil), admittedExclude...),
			PolicyInclude: append([]string(nil), admittedPolicyInclude...),
			PolicyExclude: append([]string(nil), admittedPolicyExclude...),
		},
		maxFiles,
	)
	if err != nil {
		return Materialization{}, fmt.Errorf("enumerate scope target: %w", err)
	}
	if err := validateRevisionFileList(
		listing,
		capture,
		admittedInclude,
		admittedExclude,
		admittedPolicyInclude,
		admittedPolicyExclude,
		maxFiles,
	); err != nil {
		return Materialization{}, fmt.Errorf("validate scope file listing: %w", err)
	}
	fileRefs := make([]TargetFileRef, 0, len(listing.Files))
	manifestFiles := make([]ScopeManifestFile, 0, len(listing.Files))
	reviewFiles := make([]reviewcore.FileManifestEntry, 0, len(listing.Files))
	regions := make([]reviewcore.ReviewRegion, 0, len(listing.Files))
	includedFiles := 0
	reasons := append([]gitadapter.Reason{}, capture.Reasons...)
	reasons = append(reasons, listing.Reasons...)
	contextBytes, err := frozenContextBytes(request.Contexts, config.MaxMaterializedBytes)
	if err != nil {
		return Materialization{}, err
	}
	materializedBytes := contextBytes
	for _, entry := range listing.Files {
		if err := ctx.Err(); err != nil {
			return Materialization{}, err
		}
		content, readErr := source.ReadFileAtCommit(
			ctx,
			capture.RepositoryRoot,
			capture.Revision.CommitOID,
			entry.Path,
			config.MaxFileContentBytes,
		)
		if readErr != nil {
			return Materialization{}, fmt.Errorf("read exact scope file %q: %w", entry.Path, readErr)
		}
		if err := validateFileContent(
			content,
			capture.Revision.CommitOID,
			capture.Repository.ObjectFormat,
			entry.Path,
			&entry,
			false,
			config.MaxFileContentBytes,
		); err != nil {
			return Materialization{}, fmt.Errorf(
				"validate exact scope file %q: %w",
				entry.Path,
				err,
			)
		}
		targetFile := TargetFileRef{
			Path: entry.Path, SHA256: content.SHA256, SizeBytes: content.SizeBytes,
			Completeness: content.Completeness,
			Reasons:      append([]gitadapter.Reason{}, content.Reasons...),
		}
		manifestFile := ScopeManifestFile{
			Path: entry.Path, Mode: entry.Mode, ObjectType: entry.Type,
			ObjectOID: entry.ObjectOID, Language: entry.Language,
			SHA256: content.SHA256, SizeBytes: content.SizeBytes,
			Completeness: content.Completeness,
			Reasons:      append([]gitadapter.Reason{}, content.Reasons...),
		}
		if content.Included {
			if materializedBytes+content.SizeBytes > config.MaxMaterializedBytes {
				reason := gitadapter.Reason{
					Code: gitadapter.ReasonMaterializedBytesExceeded,
					Detail: fmt.Sprintf(
						"retaining %q would exceed max_materialized_bytes=%d",
						entry.Path,
						config.MaxMaterializedBytes,
					),
				}
				targetFile.Completeness = gitadapter.CompletenessSkipped
				targetFile.Reasons = []gitadapter.Reason{reason}
				manifestFile.Completeness = gitadapter.CompletenessSkipped
				manifestFile.Reasons = []gitadapter.Reason{reason}
				reasons = append(reasons, reasonForPath(entry.Path, reason))
				fileRefs = append(fileRefs, targetFile)
				manifestFiles = append(manifestFiles, manifestFile)
				continue
			}
			contentRef, putErr := artifacts.PutArtifact(ContractFileContent, content.Data)
			if putErr != nil {
				return Materialization{}, fmt.Errorf("persist exact scope file %q: %w", entry.Path, putErr)
			}
			if contentRef.SHA256 != content.SHA256 ||
				contentRef.SizeBytes != content.SizeBytes {
				return Materialization{}, fmt.Errorf(
					"persisted scope file identity differs for %q",
					entry.Path,
				)
			}
			targetFile.ContentRef = &contentRef
			materializedBytes += content.SizeBytes
			text := string(content.Data)
			reviewFiles = append(reviewFiles, reviewcore.FileManifestEntry{
				Path: entry.Path, SHA256: content.SHA256,
				SizeBytes: content.SizeBytes, Content: &text,
			})
			lineCount := contentLineCount(text)
			if lineCount > 0 {
				regions = append(regions, reviewcore.ReviewRegion{
					Path: entry.Path, StartLine: 1, EndLine: uint32(lineCount),
					SHA256: content.SHA256,
				})
			}
			includedFiles++
		}
		fileRefs = append(fileRefs, targetFile)
		manifestFiles = append(manifestFiles, manifestFile)
		for _, reason := range content.Reasons {
			reasons = append(reasons, reasonForPath(entry.Path, reason))
		}
	}
	revalidation, err := source.RevalidateRevision(
		ctx,
		capture.RepositoryRoot,
		capture.Revision.Requested,
		capture.Revision.CommitOID,
	)
	if err != nil {
		return Materialization{}, fmt.Errorf("revalidate scope revision: %w", err)
	}
	if err := validateRevisionRevalidation(revalidation, capture); err != nil {
		return Materialization{}, fmt.Errorf("validate scope revision revalidation: %w", err)
	}
	reasons = append(reasons, revalidation.Reasons...)
	skippedFiles := listing.Coverage.MatchedFiles - includedFiles
	completeness := gitadapter.CompletenessComplete
	if skippedFiles != 0 || len(reasons) != 0 {
		completeness = gitadapter.CompletenessPartial
	}
	coverage := ScopeCoverage{
		ScannedFiles:         listing.Coverage.ScannedFiles,
		MatchedFiles:         listing.Coverage.MatchedFiles,
		IncludedFiles:        includedFiles,
		SkippedFiles:         skippedFiles,
		ExcludedFiles:        listing.Coverage.ExcludedFiles,
		RequestExcludedFiles: listing.Coverage.RequestExcludedFiles,
		PolicyExcludedFiles:  listing.Coverage.PolicyExcludedFiles,
	}
	manifest := ScopeManifest{
		SchemaVersion: ContractScopeManifest,
		Repository:    capture.Repository, Revision: capture.Revision,
		Include: append([]string{}, listing.Include...),
		Exclude: append([]string{}, listing.Exclude...),
		Files:   manifestFiles, Coverage: coverage,
		Completeness: completeness, Reasons: append([]gitadapter.Reason{}, reasons...),
	}
	manifestRef, err := artifacts.PutJSONArtifact(ContractScopeManifest, manifest)
	if err != nil {
		return Materialization{}, fmt.Errorf("persist scope manifest: %w", err)
	}
	snapshot, err := SealTargetSnapshot(TargetSnapshot{
		SchemaVersion:  TargetSnapshotSchemaVersion,
		Mode:           reviewcore.TargetModeScope,
		Repository:     capture.Repository,
		Base:           capture.Revision,
		Head:           capture.Revision,
		ManifestSHA256: manifestRef.SHA256,
		Scope: &ScopeSnapshot{
			Include:       append([]string{}, listing.Include...),
			Exclude:       append([]string{}, listing.Exclude...),
			MatchedFiles:  coverage.MatchedFiles,
			IncludedFiles: coverage.IncludedFiles,
			SkippedFiles:  coverage.SkippedFiles,
		},
		DirtyState:         capture.DirtyState,
		Completeness:       completeness,
		CompletenessReason: reasons,
		CapturedAt:         capture.CapturedAt,
		CapturedBy:         capture.CapturedBy,
		GitVersion:         capture.GitVersion,
	})
	if err != nil {
		return Materialization{}, fmt.Errorf("seal scope target snapshot: %w", err)
	}
	if err := manifest.ValidateAgainst(snapshot); err != nil {
		return Materialization{}, fmt.Errorf("validate scope manifest closure: %w", err)
	}
	target := MaterializedTarget{
		SchemaVersion: MaterializedTargetSchemaVersion,
		Snapshot:      snapshot, ManifestRef: manifestRef,
		FileRefs: fileRefs, Contexts: cloneContextBindings(request.Contexts),
	}
	if err := target.Validate(); err != nil {
		return Materialization{}, fmt.Errorf("validate materialized scope: %w", err)
	}
	targetRef, err := artifacts.PutJSONArtifact(ContractMaterializedTarget, target)
	if err != nil {
		return Materialization{}, fmt.Errorf("persist materialized scope: %w", err)
	}
	input := reviewcore.ReviewInput{
		SchemaVersion: reviewcore.ReviewInputSchemaVersion,
		TargetID:      snapshot.TargetSnapshotID, TargetMode: reviewcore.TargetModeScope,
		CanonicalPatch: "", Regions: regions, Files: reviewFiles,
		Contexts: cloneContextBindings(request.Contexts),
	}
	return persistReviewInput(artifacts, capture.RepositoryRoot, target, targetRef, input)
}

func persistReviewInput(
	artifacts ArtifactWriter,
	repositoryRoot string,
	target MaterializedTarget,
	targetRef runmodel.ArtifactRef,
	input reviewcore.ReviewInput,
) (Materialization, error) {
	if err := input.Validate(); err != nil {
		return Materialization{}, fmt.Errorf("validate review input: %w", err)
	}
	inputRef, err := artifacts.PutJSONArtifact(ContractReviewInput, input)
	if err != nil {
		return Materialization{}, fmt.Errorf("persist review input: %w", err)
	}
	return Materialization{
		RepositoryRoot: repositoryRoot,
		Target:         target, TargetRef: targetRef,
		Input: input, InputRef: inputRef,
	}, nil
}

func validateRevisionCapture(
	capture gitadapter.RevisionCapture,
	requestedRepositoryPath string,
	requestedRevision string,
) error {
	if capture.SchemaVersion != gitadapter.RevisionCaptureSchemaVersion {
		return fmt.Errorf("unsupported schema %q", capture.SchemaVersion)
	}
	if capture.RepositoryRoot == "" ||
		!filepath.IsAbs(capture.RepositoryRoot) ||
		filepath.Clean(capture.RepositoryRoot) != capture.RepositoryRoot {
		return fmt.Errorf("repository_root must be a clean absolute path")
	}
	canonicalRoot, err := canonicalRepositoryRoot(requestedRepositoryPath)
	if err != nil {
		return fmt.Errorf("resolve requested repository root: %w", err)
	}
	if capture.RepositoryRoot != canonicalRoot {
		return fmt.Errorf(
			"repository_root = %q, want requested canonical root %q",
			capture.RepositoryRoot,
			canonicalRoot,
		)
	}
	if capture.Repository.Kind != "local_git" ||
		capture.Repository.RepositoryID != expectedLocalRepositoryID(canonicalRoot) ||
		capture.Repository.ObjectFormat != "sha1" &&
			capture.Repository.ObjectFormat != "sha256" {
		return fmt.Errorf("repository identity is invalid")
	}
	if capture.Revision.Requested != requestedRevision {
		return fmt.Errorf(
			"requested revision = %q, want %q",
			capture.Revision.Requested,
			requestedRevision,
		)
	}
	if !exactOIDMatchesObjectFormat(
		capture.Revision.CommitOID,
		capture.Repository.ObjectFormat,
	) {
		return fmt.Errorf("commit_oid does not match repository object format")
	}
	switch capture.DirtyState {
	case gitadapter.DirtyStateClean, gitadapter.DirtyStateDirty:
	case gitadapter.DirtyStateUnknown:
		if !hasReasonCode(capture.Reasons, gitadapter.ReasonDirtyStateUnknown) {
			return fmt.Errorf("unknown dirty_state requires dirty_state_unknown reason")
		}
	default:
		return fmt.Errorf("unsupported dirty_state %q", capture.DirtyState)
	}
	if capture.Reasons == nil || capture.CapturedAt.IsZero() ||
		capture.CapturedBy == "" || capture.GitVersion == "" {
		return fmt.Errorf("capture metadata is incomplete")
	}
	return nil
}

func validateDiffResult(
	ctx context.Context,
	result gitadapter.Result,
	request ReviewRequest,
	maxPatchBytes int64,
	maxFiles int,
) error {
	if result.Snapshot == nil || result.Manifest == nil {
		return fmt.Errorf("snapshot and manifest are required")
	}
	snapshot := *result.Snapshot
	manifest := *result.Manifest
	if snapshot.SchemaVersion != gitadapter.TargetSnapshotSchemaVersion {
		return fmt.Errorf("unsupported target snapshot schema %q", snapshot.SchemaVersion)
	}
	canonicalRoot, err := canonicalRepositoryRoot(request.RepositoryPath)
	if err != nil {
		return fmt.Errorf("resolve requested repository root: %w", err)
	}
	if result.RepositoryRoot != canonicalRoot {
		return fmt.Errorf(
			"repository_root = %q, want requested canonical root %q",
			result.RepositoryRoot,
			canonicalRoot,
		)
	}
	if snapshot.Repository.Kind != "local_git" ||
		snapshot.Repository.RepositoryID != expectedLocalRepositoryID(canonicalRoot) ||
		snapshot.Repository.ObjectFormat != "sha1" &&
			snapshot.Repository.ObjectFormat != "sha256" {
		return fmt.Errorf("repository identity is invalid")
	}
	if snapshot.Base.Requested != request.BaseRevision ||
		snapshot.Head.Requested != request.HeadRevision {
		return fmt.Errorf("base/head requested revisions do not match the review request")
	}
	if !exactOIDMatchesObjectFormat(
		snapshot.Base.CommitOID,
		snapshot.Repository.ObjectFormat,
	) ||
		!exactOIDMatchesObjectFormat(
			snapshot.Head.CommitOID,
			snapshot.Repository.ObjectFormat,
		) {
		return fmt.Errorf("base/head commit OIDs do not match repository object format")
	}
	if snapshot.DirtyState != gitadapter.DirtyStateClean &&
		snapshot.DirtyState != gitadapter.DirtyStateDirty &&
		snapshot.DirtyState != gitadapter.DirtyStateUnknown {
		return fmt.Errorf("unsupported dirty_state %q", snapshot.DirtyState)
	}
	if snapshot.DirtyState == gitadapter.DirtyStateUnknown &&
		!hasReasonCode(snapshot.CompletenessReason, gitadapter.ReasonDirtyStateUnknown) {
		return fmt.Errorf("unknown dirty_state requires dirty_state_unknown reason")
	}
	if snapshot.CompletenessReason == nil || snapshot.CapturedAt.IsZero() ||
		snapshot.CapturedBy == "" || snapshot.GitVersion == "" {
		return fmt.Errorf("target snapshot capture metadata is incomplete")
	}
	if err := validateCompletenessState(
		"target snapshot",
		snapshot.Completeness,
		snapshot.CompletenessReason,
	); err != nil {
		return err
	}
	snapshotDigest, err := gitadapter.DigestTargetSnapshot(snapshot)
	if err != nil || snapshotDigest != snapshot.SHA256 {
		return fmt.Errorf("target snapshot digest does not match its fields")
	}
	if result.Patch.FormatVersion != gitadapter.CanonicalPatchVersion ||
		result.Patch.SizeBytes < 0 ||
		result.Patch.SHA256 != snapshot.PatchSHA256 ||
		result.Patch.SizeBytes != snapshot.PatchSizeBytes ||
		snapshot.CanonicalPatch != gitadapter.CanonicalPatchVersion {
		return fmt.Errorf("canonical patch identity is inconsistent")
	}
	if err := targetmodel.ValidateDigest("canonical patch sha256", result.Patch.SHA256); err != nil {
		return err
	}
	manifestDigest, err := gitadapter.DigestManifest(manifest)
	if err != nil || manifestDigest != snapshot.ManifestSHA256 {
		return fmt.Errorf("change manifest digest does not match target snapshot")
	}
	if result.Patch.Included {
		if result.Patch.SizeBytes != int64(len(result.Patch.Data)) ||
			result.Patch.SHA256 != sha256Hex(result.Patch.Data) ||
			result.Patch.SizeBytes > maxPatchBytes {
			return fmt.Errorf("canonical patch sha256/size/data are inconsistent")
		}
		inspection, inspectErr := reviewcore.InspectCanonicalPatch(
			ctx,
			string(result.Patch.Data),
		)
		if inspectErr != nil {
			return fmt.Errorf("inspect canonical patch: %w", inspectErr)
		}
		if len(inspection.Files) != manifest.Coverage.TotalFiles ||
			len(inspection.Files) != manifest.Coverage.DiffFileHeaders ||
			inspection.TotalHunks != manifest.Coverage.TotalHunks {
			return fmt.Errorf(
				"canonical patch file/hunk coverage does not exactly match the manifest",
			)
		}
		for index, file := range manifest.Files {
			patchFile := inspection.Files[index]
			if patchFile.Hunks != file.HunkCount ||
				!patchFile.PathKnown ||
				patchFile.Path != file.Path ||
				!file.Included {
				return fmt.Errorf(
					"canonical patch file %d does not exactly match retained manifest path %q",
					index,
					file.Path,
				)
			}
		}
	} else if len(result.Patch.Data) != 0 {
		return fmt.Errorf("excluded canonical patch must omit data")
	}
	if result.Completeness != snapshot.Completeness ||
		!slices.Equal(result.Reasons, snapshot.CompletenessReason) {
		return fmt.Errorf("result completeness does not match target snapshot")
	}
	if manifest.Coverage.IncludedFiles > maxFiles || len(manifest.Files) > maxFiles {
		return fmt.Errorf("change manifest exceeds admitted max_files=%d", maxFiles)
	}
	admissionSnapshot := TargetSnapshot{
		Mode:         reviewcore.TargetModeDiff,
		Base:         snapshot.Base,
		Head:         snapshot.Head,
		Completeness: snapshot.Completeness,
		CompletenessReason: append(
			[]gitadapter.Reason(nil),
			snapshot.CompletenessReason...,
		),
		Diff: &DiffSnapshot{
			PatchSHA256:    snapshot.PatchSHA256,
			PatchSizeBytes: snapshot.PatchSizeBytes,
			PatchFormat:    snapshot.CanonicalPatch,
		},
	}
	if err := targetmodel.ValidateDiffManifest(manifest, admissionSnapshot); err != nil {
		return fmt.Errorf("change manifest closure: %w", err)
	}
	return nil
}

func admitDiffArtifacts(
	result gitadapter.Result,
) ([]byte, gitadapter.ChangeManifest, error) {
	if result.Manifest == nil {
		return nil, gitadapter.ChangeManifest{}, fmt.Errorf("diff manifest is required")
	}
	manifest := *result.Manifest
	manifest.Files = append([]gitadapter.FileChange{}, result.Manifest.Files...)
	manifest.Reasons = append([]gitadapter.Reason{}, result.Manifest.Reasons...)
	retainFiles := len(manifest.Files)
	if retainFiles == 0 {
		return nil, gitadapter.ChangeManifest{}, fmt.Errorf(
			"%w: diff retained no safely attributable changed file",
			ErrTargetNotReviewable,
		)
	}
	patch := append([]byte(nil), result.Patch.Data...)
	if retainFiles < manifest.Coverage.TotalFiles {
		starts := []int{}
		if bytes.HasPrefix(patch, []byte("diff --git ")) {
			starts = append(starts, 0)
		}
		needle := []byte("\ndiff --git ")
		for offset := 0; ; {
			index := bytes.Index(patch[offset:], needle)
			if index < 0 {
				break
			}
			start := offset + index + 1
			starts = append(starts, start)
			offset = start + len("diff --git ")
		}
		if len(starts) != manifest.Coverage.TotalFiles || retainFiles >= len(starts) {
			return nil, gitadapter.ChangeManifest{}, fmt.Errorf(
				"cannot derive an exact retained canonical patch prefix",
			)
		}
		patch = append([]byte(nil), patch[:starts[retainFiles]]...)
	}
	manifest.PatchSHA256 = sha256Hex(patch)
	manifest.PatchSize = int64(len(patch))
	return patch, manifest, nil
}

func canonicalRepositoryRoot(repositoryPath string) (string, error) {
	canonical, err := filepath.EvalSymlinks(repositoryPath)
	if err != nil {
		return "", err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", err
	}
	return filepath.Clean(canonical), nil
}

func expectedLocalRepositoryID(repositoryRoot string) string {
	digest := sha256.Sum256([]byte(repositoryRoot))
	return fmt.Sprintf("local-%x", digest[:16])
}

func validateRevisionRevalidation(
	revalidation gitadapter.RevisionRevalidation,
	capture gitadapter.RevisionCapture,
) error {
	if revalidation.Requested != capture.Revision.Requested ||
		revalidation.ExpectedCommitOID != capture.Revision.CommitOID {
		return fmt.Errorf("requested/expected commit do not match the revision capture")
	}
	if revalidation.Reasons == nil {
		return fmt.Errorf("revalidation reasons must be an explicit array")
	}
	switch revalidation.State {
	case gitadapter.RevisionStateUnchanged:
		if revalidation.ActualCommitOID != capture.Revision.CommitOID ||
			len(revalidation.Reasons) != 0 {
			return fmt.Errorf("unchanged revalidation has inconsistent actual commit or reasons")
		}
	case gitadapter.RevisionStateMoved:
		if !exactOIDMatchesObjectFormat(
			revalidation.ActualCommitOID,
			capture.Repository.ObjectFormat,
		) ||
			revalidation.ActualCommitOID == capture.Revision.CommitOID ||
			len(revalidation.Reasons) != 1 ||
			revalidation.Reasons[0].Code != gitadapter.ReasonRevisionMoved {
			return fmt.Errorf("moved revalidation has inconsistent actual commit or reasons")
		}
	case gitadapter.RevisionStateMissing:
		if revalidation.ActualCommitOID != "" ||
			len(revalidation.Reasons) != 1 ||
			revalidation.Reasons[0].Code != gitadapter.ReasonRevisionMissing {
			return fmt.Errorf("missing revalidation has inconsistent actual commit or reasons")
		}
	default:
		return fmt.Errorf("unsupported revalidation state %q", revalidation.State)
	}
	return nil
}

func validateRevisionFileList(
	listing gitadapter.RevisionFileList,
	capture gitadapter.RevisionCapture,
	include []string,
	exclude []string,
	policyInclude []string,
	policyExclude []string,
	maxFiles int,
) error {
	if listing.SchemaVersion != gitadapter.RevisionFileListSchemaVersion {
		return fmt.Errorf("unsupported schema %q", listing.SchemaVersion)
	}
	if listing.CommitOID != capture.Revision.CommitOID {
		return fmt.Errorf(
			"commit_oid = %q, want captured commit %q",
			listing.CommitOID,
			capture.Revision.CommitOID,
		)
	}
	if listing.Include == nil || listing.Exclude == nil ||
		listing.PolicyInclude == nil || listing.PolicyExclude == nil ||
		!slices.Equal(listing.Include, include) ||
		!slices.Equal(listing.Exclude, exclude) ||
		!slices.Equal(listing.PolicyInclude, policyInclude) ||
		!slices.Equal(listing.PolicyExclude, policyExclude) {
		return fmt.Errorf(
			"request and policy include/exclude do not exactly match admission",
		)
	}
	if listing.Files == nil || listing.Reasons == nil {
		return fmt.Errorf("files and reasons must be explicit arrays")
	}
	coverage := listing.Coverage
	if coverage.ScannedFiles < 0 || coverage.MatchedFiles < 0 ||
		coverage.RetainedFiles < 0 || coverage.SkippedFiles < 0 ||
		coverage.ExcludedFiles < 0 ||
		coverage.RequestExcludedFiles < 0 ||
		coverage.PolicyExcludedFiles < 0 ||
		coverage.ExcludedFiles !=
			coverage.RequestExcludedFiles+coverage.PolicyExcludedFiles ||
		coverage.MatchedFiles != coverage.RetainedFiles+coverage.SkippedFiles ||
		coverage.RetainedFiles != len(listing.Files) ||
		coverage.MatchedFiles+coverage.ExcludedFiles > coverage.ScannedFiles ||
		len(listing.Files) > maxFiles {
		return fmt.Errorf("coverage is inconsistent")
	}
	switch {
	case coverage.MatchedFiles == 0:
		if listing.Completeness != gitadapter.CompletenessSkipped ||
			len(listing.Reasons) != 1 ||
			!hasReasonCode(listing.Reasons, gitadapter.ReasonNoMatchingFiles) {
			return fmt.Errorf("empty match set must be explicitly skipped")
		}
	case coverage.SkippedFiles > 0:
		if listing.Completeness != gitadapter.CompletenessPartial ||
			len(listing.Reasons) != 1 ||
			!hasReasonCode(listing.Reasons, gitadapter.ReasonFileLimitExceeded) {
			return fmt.Errorf("limited match set must be explicitly partial")
		}
	default:
		if listing.Completeness != gitadapter.CompletenessComplete ||
			len(listing.Reasons) != 0 {
			return fmt.Errorf("complete match set has inconsistent completeness metadata")
		}
	}

	admission := gitadapter.ScopeAdmission{
		Include:       include,
		Exclude:       exclude,
		PolicyInclude: policyInclude,
		PolicyExclude: policyExclude,
	}
	for index, entry := range listing.Files {
		if index > 0 && entry.Path <= listing.Files[index-1].Path {
			return fmt.Errorf("files must be uniquely sorted by path")
		}
		allowed, err := gitadapter.ScopeAdmissionIncludesPath(
			entry.Path,
			admission,
		)
		if err != nil {
			return fmt.Errorf("validate admitted file %q: %w", entry.Path, err)
		}
		if !allowed {
			return fmt.Errorf("file %q is outside the admitted scope", entry.Path)
		}
		if _, err := strconv.ParseUint(entry.Mode, 8, 32); err != nil {
			return fmt.Errorf("file %q has invalid mode %q", entry.Path, entry.Mode)
		}
		if entry.Type != "blob" && entry.Type != "commit" {
			return fmt.Errorf("file %q has invalid object type %q", entry.Path, entry.Type)
		}
		if entry.Type == "commit" && entry.Mode != "160000" ||
			entry.Type == "blob" && entry.Mode == "160000" {
			return fmt.Errorf("file %q has inconsistent submodule identity", entry.Path)
		}
		if !exactOIDMatchesObjectFormat(
			entry.ObjectOID,
			capture.Repository.ObjectFormat,
		) {
			return fmt.Errorf("file %q has invalid object_oid", entry.Path)
		}
		if entry.Language != gitadapter.DetectLanguage(entry.Path) {
			return fmt.Errorf("file %q has inconsistent language", entry.Path)
		}
	}
	return nil
}

func validateFileContent(
	content gitadapter.FileContent,
	expectedCommitOID string,
	objectFormat string,
	expectedPath string,
	expectedEntry *gitadapter.RevisionFileEntry,
	overlay bool,
	maxBytes int64,
) error {
	if content.SchemaVersion != gitadapter.FileContentSchemaVersion {
		return fmt.Errorf("unsupported schema %q", content.SchemaVersion)
	}
	if content.CommitOID != expectedCommitOID {
		return fmt.Errorf(
			"commit_oid = %q, want admitted commit %q",
			content.CommitOID,
			expectedCommitOID,
		)
	}
	if content.Path != expectedPath {
		return fmt.Errorf("path = %q, want %q", content.Path, expectedPath)
	}
	if content.SizeBytes < 0 || content.Reasons == nil {
		return fmt.Errorf("content metadata is incomplete")
	}
	if expectedEntry != nil {
		if content.Mode != expectedEntry.Mode ||
			content.BlobOID != expectedEntry.ObjectOID {
			return fmt.Errorf("mode/blob_oid do not match the admitted listing entry")
		}
		if content.Included &&
			(expectedEntry.Type != "blob" ||
				expectedEntry.Mode != "100644" &&
					expectedEntry.Mode != "100755") {
			return fmt.Errorf("included content must be an ordinary Git blob")
		}
	} else if overlay {
		if content.Mode != "" || content.BlobOID != "" {
			return fmt.Errorf("overlay must not claim a Git blob identity")
		}
	} else if content.Included {
		if content.Mode != "100644" && content.Mode != "100755" ||
			!exactOIDMatchesObjectFormat(
				content.BlobOID,
				objectFormat,
			) {
			return fmt.Errorf("included commit content has invalid mode/blob_oid")
		}
	}
	if content.SHA256 != "" {
		if err := targetmodel.ValidateDigest("file content sha256", content.SHA256); err != nil {
			return err
		}
	}
	if content.Included {
		if content.Completeness != gitadapter.CompletenessComplete ||
			len(content.Reasons) != 0 {
			return fmt.Errorf("included content must be complete without reasons")
		}
		if content.SizeBytes != int64(len(content.Data)) ||
			content.SizeBytes > maxBytes ||
			content.SHA256 != sha256Hex(content.Data) {
			return fmt.Errorf("included content sha256/size/data are inconsistent")
		}
		if !utf8.Valid(content.Data) ||
			strings.ContainsRune(string(content.Data), '\x00') ||
			strings.ContainsRune(string(content.Data), '\r') {
			return fmt.Errorf("included content must be LF-only UTF-8 text without NUL bytes")
		}
		if !overlay {
			blobOID, err := gitBlobOID(content.Data, objectFormat)
			if err != nil {
				return err
			}
			if content.BlobOID != blobOID {
				return fmt.Errorf("included content blob_oid does not match its data")
			}
		}
		return nil
	}
	if content.Completeness != gitadapter.CompletenessPartial &&
		content.Completeness != gitadapter.CompletenessSkipped {
		return fmt.Errorf("excluded content has invalid completeness %q", content.Completeness)
	}
	if len(content.Data) != 0 || len(content.Reasons) == 0 {
		return fmt.Errorf("excluded content must omit data and explain why")
	}
	oversizeWithoutContentDigest :=
		content.SHA256 == "" &&
			content.SizeBytes > 0 &&
			content.HasReason(gitadapter.ReasonFileSizeExceeded)
	if content.SHA256 == "" && content.SizeBytes != 0 && !oversizeWithoutContentDigest ||
		content.SHA256 != "" && content.SizeBytes == 0 {
		return fmt.Errorf("excluded content sha256/size metadata is inconsistent")
	}
	return nil
}

func gitBlobOID(data []byte, objectFormat string) (string, error) {
	var hasher hash.Hash
	switch objectFormat {
	case "sha1":
		hasher = sha1.New()
	case "sha256":
		hasher = sha256.New()
	default:
		return "", fmt.Errorf("unsupported Git object format %q", objectFormat)
	}
	_, _ = fmt.Fprintf(hasher, "blob %d\x00", len(data))
	_, _ = hasher.Write(data)
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func exactOIDMatchesObjectFormat(oid string, objectFormat string) bool {
	length := 0
	switch objectFormat {
	case "sha1":
		length = 40
	case "sha256":
		length = 64
	default:
		return false
	}
	if len(oid) != length || strings.ToLower(oid) != oid {
		return false
	}
	_, err := hex.DecodeString(oid)
	return err == nil
}

func targetPathAllowed(config LocalConfig, repositoryPath string) (bool, error) {
	allowed, err := gitadapter.ScopeIncludesPath(
		repositoryPath,
		config.TargetInclude,
		config.TargetExclude,
	)
	if err != nil {
		return false, fmt.Errorf("evaluate resolved target policy for %q: %w", repositoryPath, err)
	}
	return allowed, nil
}

func validateDiffTargetPolicy(
	manifest gitadapter.ChangeManifest,
	config LocalConfig,
) error {
	if slices.Equal(config.TargetInclude, []string{"**"}) &&
		len(config.TargetExclude) == 0 {
		return nil
	}
	for _, file := range manifest.Files {
		allowed, err := targetPathAllowed(config, file.Path)
		if err != nil {
			return err
		}
		if !allowed {
			return fmt.Errorf(
				"%w: diff path %q is denied by target policy",
				ErrTargetNotReviewable,
				file.Path,
			)
		}
	}
	return nil
}

func reasonForPath(repositoryPath string, reason gitadapter.Reason) gitadapter.Reason {
	detail := repositoryPath
	if reason.Detail != "" {
		detail += ": " + reason.Detail
	}
	return gitadapter.Reason{Code: reason.Code, Detail: detail}
}

func validateCompletenessState(
	name string,
	state gitadapter.Completeness,
	reasons []gitadapter.Reason,
) error {
	if reasons == nil {
		return fmt.Errorf("%s reasons must be an explicit array", name)
	}
	switch state {
	case gitadapter.CompletenessComplete:
		if len(reasons) != 0 {
			return fmt.Errorf("%s is complete but contains reasons", name)
		}
	case gitadapter.CompletenessPartial, gitadapter.CompletenessSkipped:
		if len(reasons) == 0 {
			return fmt.Errorf("%s is %s but contains no reason", name, state)
		}
	default:
		return fmt.Errorf("%s has unsupported completeness %q", name, state)
	}
	return nil
}

func hasReasonCode(reasons []gitadapter.Reason, code gitadapter.ReasonCode) bool {
	for _, reason := range reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}

func canonicalSelectionContent(content string, startLine uint32, endLine uint32) ([]byte, error) {
	if content == "" {
		return nil, fmt.Errorf(
			"selection line range %d-%d exceeds 0 frozen lines",
			startLine,
			endLine,
		)
	}
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1]
	}
	if startLine == 0 || endLine < startLine || uint64(endLine) > uint64(len(lines)) {
		return nil, fmt.Errorf(
			"selection line range %d-%d exceeds %d frozen lines",
			startLine,
			endLine,
			len(lines),
		)
	}
	selected := strings.Join(lines[startLine-1:endLine], "\n") + "\n"
	return []byte(selected), nil
}

func canonicalSelectionRangesContent(
	content string,
	ranges []SelectionRange,
) ([]byte, error) {
	if err := targetmodel.ValidateSelectionRanges("selection ranges", ranges); err != nil {
		return nil, err
	}
	var selected bytes.Buffer
	for _, lineRange := range ranges {
		fragment, err := canonicalSelectionContent(
			content,
			lineRange.StartLine,
			lineRange.EndLine,
		)
		if err != nil {
			return nil, err
		}
		selected.Write(fragment)
	}
	return selected.Bytes(), nil
}

func resolveSelectionSelector(
	content string,
	request ReviewRequest,
) ([]SelectionRange, *SymbolSelector, error) {
	if request.StartLine != 0 || request.EndLine != 0 {
		return []SelectionRange{{
			StartLine: request.StartLine,
			EndLine:   request.EndLine,
		}}, nil, nil
	}
	if len(request.SelectionRanges) > 0 {
		return slices.Clone(request.SelectionRanges), nil, nil
	}
	if request.SelectionSymbol == nil {
		return nil, nil, fmt.Errorf("selection selector is missing")
	}
	lineRange, err := resolveGoSymbolRange(content, request.SelectionPath, *request.SelectionSymbol)
	if err != nil {
		return nil, nil, err
	}
	return []SelectionRange{lineRange}, cloneSymbolSelector(request.SelectionSymbol), nil
}

func resolveGoSymbolRange(
	content string,
	repositoryPath string,
	selector SymbolSelector,
) (SelectionRange, error) {
	if err := selector.Validate(); err != nil {
		return SelectionRange{}, err
	}
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(
		fileSet,
		repositoryPath,
		content,
		parser.SkipObjectResolution,
	)
	if err != nil {
		return SelectionRange{}, fmt.Errorf("parse Go source for symbol selection: %w", err)
	}
	var matches []ast.Node
	for _, declaration := range file.Decls {
		switch typed := declaration.(type) {
		case *ast.FuncDecl:
			if selector.Kind == "function" && typed.Recv == nil &&
				typed.Name.Name == selector.QualifiedName {
				matches = append(matches, typed)
			}
			if selector.Kind == "method" && typed.Recv != nil &&
				methodQualifiedName(typed) == selector.QualifiedName {
				matches = append(matches, typed)
			}
		case *ast.GenDecl:
			if selector.Kind != "type" {
				continue
			}
			for _, specification := range typed.Specs {
				typeSpec, ok := specification.(*ast.TypeSpec)
				if ok && typeSpec.Name.Name == selector.QualifiedName {
					matches = append(matches, typeSpec)
				}
			}
		}
	}
	if len(matches) != 1 {
		return SelectionRange{}, fmt.Errorf(
			"Go symbol %s %q resolved to %d declarations",
			selector.Kind,
			selector.QualifiedName,
			len(matches),
		)
	}
	start := fileSet.PositionFor(matches[0].Pos(), false).Line
	end := fileSet.PositionFor(matches[0].End(), false).Line
	if start < 1 || end < start {
		return SelectionRange{}, fmt.Errorf("resolved Go symbol has an invalid source range")
	}
	return SelectionRange{StartLine: uint32(start), EndLine: uint32(end)}, nil
}

func methodQualifiedName(declaration *ast.FuncDecl) string {
	if declaration == nil || declaration.Recv == nil || len(declaration.Recv.List) != 1 {
		return ""
	}
	receiver := receiverTypeName(declaration.Recv.List[0].Type)
	if receiver == "" {
		return ""
	}
	return receiver + "." + declaration.Name.Name
}

func receiverTypeName(expression ast.Expr) string {
	switch typed := expression.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.StarExpr:
		return receiverTypeName(typed.X)
	case *ast.IndexExpr:
		return receiverTypeName(typed.X)
	case *ast.IndexListExpr:
		return receiverTypeName(typed.X)
	default:
		return ""
	}
}

func cloneSymbolSelector(selector *SymbolSelector) *SymbolSelector {
	if selector == nil {
		return nil
	}
	cloned := *selector
	return &cloned
}

func cloneContextBindings(bindings []reviewcore.ContextBinding) []reviewcore.ContextBinding {
	if len(bindings) == 0 {
		return []reviewcore.ContextBinding{}
	}
	data, err := json.Marshal(bindings)
	if err != nil {
		panic(fmt.Sprintf("clone validated context bindings: %v", err))
	}
	var cloned []reviewcore.ContextBinding
	if err := json.Unmarshal(data, &cloned); err != nil {
		panic(fmt.Sprintf("clone validated context bindings: %v", err))
	}
	return cloned
}

func frozenContextBytes(
	bindings []reviewcore.ContextBinding,
	maxBytes int64,
) (int64, error) {
	var total int64
	for _, binding := range bindings {
		if binding.Ref == nil {
			continue
		}
		if binding.Ref.SizeBytes > maxBytes-total {
			return 0, fmt.Errorf(
				"context artifacts exceed max_materialized_bytes=%d",
				maxBytes,
			)
		}
		total += binding.Ref.SizeBytes
	}
	return total, nil
}

func contentLineCount(content string) int {
	if content == "" {
		return 0
	}
	count := strings.Count(content, "\n")
	if !strings.HasSuffix(content, "\n") {
		count++
	}
	return count
}

func completenessFromReasons(reasons []gitadapter.Reason) gitadapter.Completeness {
	if len(reasons) == 0 {
		return gitadapter.CompletenessComplete
	}
	return gitadapter.CompletenessPartial
}

func sha256Hex(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func reasonsFromCodes(codes []gitadapter.ReasonCode) []gitadapter.Reason {
	reasons := make([]gitadapter.Reason, 0, len(codes))
	for _, code := range codes {
		reasons = append(reasons, gitadapter.Reason{Code: code})
	}
	return reasons
}

func formatReasons(reasons []gitadapter.Reason) string {
	if len(reasons) == 0 {
		return "no reason supplied"
	}
	values := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		value := string(reason.Code)
		if reason.Detail != "" {
			value += ": " + reason.Detail
		}
		values = append(values, value)
	}
	return strings.Join(values, "; ")
}
