// Package configdefaults constructs the explicit fail-closed platform baseline
// used by the local runtime and contract fixtures. It does not own resolution
// or lifecycle; those remain in reviewconfig and configrepo.
package configdefaults

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"

	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/workflow"
)

type Options struct {
	ID               string
	Revision         string
	MaxFiles         int
	MaxPatchBytes    int64
	MaxInputBytes    int64
	MaxOutputBytes   int64
	MaxAttempts      int
	AllowedModes     []string
	TargetInclude    []string
	TargetExclude    []string
	ContextProviders []reviewconfig.ContextProviderDefinition
	Context          reviewconfig.ResolutionContext
}

// Revision returns the complete fail-closed platform baseline as a lifecycle
// revision. Callers may persist and publish it before layering narrower
// revisions; Bundle resolves the same revision directly for static/local use.
func Revision(
	options Options,
	definition workflow.Definition,
) (reviewconfig.Revision, error) {
	if options.ID == "" || options.Revision == "" ||
		options.MaxFiles <= 0 || options.MaxPatchBytes <= 0 ||
		options.MaxInputBytes <= 0 ||
		options.MaxOutputBytes <= 0 || options.MaxAttempts <= 0 {
		return reviewconfig.Revision{}, fmt.Errorf("default config options are incomplete")
	}
	allowedModes := slices.Clone(options.AllowedModes)
	if allowedModes == nil {
		allowedModes = []string{"diff", "scope", "selection"}
	}
	targetInclude := slices.Clone(options.TargetInclude)
	if targetInclude == nil {
		targetInclude = []string{"**"}
	}
	targetExclude := slices.Clone(options.TargetExclude)
	if targetExclude == nil {
		targetExclude = []string{}
	}
	contextProviders := slices.Clone(options.ContextProviders)
	if contextProviders == nil {
		contextProviders = []reviewconfig.ContextProviderDefinition{}
	}
	workflowDigest, err := workflow.DigestDefinition(definition)
	if err != nil {
		return reviewconfig.Revision{}, err
	}
	markerDetector := reviewconfig.VersionedRef{
		ID:       reviewcore.DetectorFixtureMarkerID,
		Revision: reviewcore.DetectorFixtureMarkerRevision,
		SHA256: descriptorDigest(
			reviewcore.DetectorFixtureMarkerID + "@" +
				reviewcore.DetectorFixtureMarkerRevision,
		),
	}
	goASTDetector := reviewconfig.VersionedRef{
		ID:       reviewcore.DetectorGoASTID,
		Revision: reviewcore.DetectorGoASTRevision,
		SHA256: descriptorDigest(
			reviewcore.DetectorGoASTID + "@" + reviewcore.DetectorGoASTRevision,
		),
	}
	maxTokens := int64(100_000)
	maxCostMicros := int64(1_000_000)
	stageTimeoutMS := int64(30_000)
	maxConcurrency := 1
	contextProviderMaxConcurrency := 4
	minimumEvidence := 1
	minimumSeverity := "info"
	humanReviewInconclusive := true
	maxComments := 0
	retentionDays := int64(30)
	redaction := reviewconfig.RedactionStrict
	platform := reviewconfig.Revision{
		SchemaVersion: reviewconfig.RevisionSchemaVersion,
		ID:            options.ID,
		Revision:      options.Revision,
		Scope:         reviewconfig.ScopePlatform,
		Selector:      reviewconfig.Selector{},
		Patch: reviewconfig.ConfigPatch{
			Target: &reviewconfig.TargetPatch{
				AllowedModes: &reviewconfig.SetPatch{
					Add: allowedModes, Remove: []string{},
				},
				Include:       &reviewconfig.SetPatch{Add: targetInclude, Remove: []string{}},
				Exclude:       &reviewconfig.SetPatch{Add: targetExclude, Remove: []string{}},
				MaxFiles:      &options.MaxFiles,
				MaxPatchBytes: &options.MaxPatchBytes,
			},
			RulePack: &reviewconfig.RulePackPatch{
				Rules: &reviewconfig.OrderedRulePatch{
					Upsert: []reviewconfig.RuleDefinition{
						{
							ID: "unfinished-work", Revision: "1", Kind: "deterministic",
							Detector: markerDetector, Languages: []string{"go"},
							PathPrefixes: []string{},
							EvidenceKinds: []string{
								"file_content", "patch_line", "target_line",
							},
							Severity: "info", Enabled: true,
						},
						{
							ID: "explicit-bug-marker", Revision: "1", Kind: "deterministic",
							Detector: markerDetector, Languages: []string{"go"},
							PathPrefixes: []string{},
							EvidenceKinds: []string{
								"file_content", "patch_line", "target_line",
							},
							Severity: "high", Enabled: true,
						},
						{
							ID:       reviewcore.RuleGoContextCancelDiscarded,
							Revision: "1",
							Kind:     "deterministic",
							Detector: goASTDetector,
							Languages: []string{
								"go",
							},
							PathPrefixes: []string{},
							EvidenceKinds: []string{
								"file_content", "patch_line", "target_line",
							},
							Severity: "high", Enabled: true,
						},
					},
					Remove: []string{},
				},
			},
			Workflow: &reviewconfig.WorkflowPatch{
				Definition: &reviewconfig.VersionedRef{
					ID: definition.ID, Revision: definition.Revision, SHA256: workflowDigest,
				},
			},
			Execution: &reviewconfig.ExecutionPatch{
				AgentProfile: &reviewconfig.VersionedRef{
					ID:       "deterministic-local",
					Revision: "1",
					SHA256:   descriptorDigest("deterministic-local-agent@1"),
				},
				ModelProfile: &reviewconfig.VersionedRef{
					ID:       "none",
					Revision: "1",
					SHA256:   descriptorDigest("no-model@1"),
				},
				AllowedTools: &reviewconfig.SetPatch{Add: []string{}, Remove: []string{}},
				ContextProviders: &reviewconfig.OrderedContextProviderPatch{
					Upsert: contextProviders,
					Remove: []string{},
				},
				ContextProviderMaxConcurrency: &contextProviderMaxConcurrency,
			},
			Budget: &reviewconfig.BudgetPatch{
				MaxInputBytes:  &options.MaxInputBytes,
				MaxOutputBytes: &options.MaxOutputBytes,
				MaxTokens:      &maxTokens,
				MaxCostMicros:  &maxCostMicros,
				StageTimeoutMS: &stageTimeoutMS,
				MaxAttempts:    &options.MaxAttempts,
				MaxConcurrency: &maxConcurrency,
			},
			Verification: &reviewconfig.VerificationPatch{
				RequiredEvidenceKinds: &reviewconfig.SetPatch{
					Add:    []string{"file_content", "patch_line", "target_line"},
					Remove: []string{},
				},
				MinimumIndependentEvidence: &minimumEvidence,
				PartialEvidence:            permission(reviewconfig.PermissionDeny),
			},
			Adjudication: &reviewconfig.AdjudicationPatch{
				MinimumSeverity:         &minimumSeverity,
				HumanReviewInconclusive: &humanReviewInconclusive,
				AutomaticPublication:    permission(reviewconfig.PermissionDeny),
			},
			Publication: &reviewconfig.PublicationPatch{
				RemoteWrites: permission(reviewconfig.PermissionDeny),
				Channels:     &reviewconfig.SetPatch{Add: []string{}, Remove: []string{}},
				MaxComments:  &maxComments,
			},
			Data: &reviewconfig.DataPatch{
				RetentionDays: &retentionDays,
				Redaction:     &redaction,
				Training:      permission(reviewconfig.PermissionDeny),
				Export:        permission(reviewconfig.PermissionDeny),
			},
		},
	}
	if err := platform.Validate(); err != nil {
		return reviewconfig.Revision{}, err
	}
	return platform, nil
}

func Bundle(
	options Options,
	definition workflow.Definition,
) (reviewconfig.ConfigBundle, error) {
	resolutionContext := options.Context
	if resolutionContext.TenantID == "" {
		resolutionContext = reviewconfig.ResolutionContext{
			TenantID:       "local",
			OrganizationID: "local",
			RepositoryID:   "local",
			InvocationID:   "local-default",
		}
	}
	platform, err := Revision(options, definition)
	if err != nil {
		return reviewconfig.ConfigBundle{}, err
	}
	return reviewconfig.Resolve(resolutionContext, []reviewconfig.Revision{platform})
}

func descriptorDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func permission(value reviewconfig.Permission) *reviewconfig.Permission {
	return &value
}
