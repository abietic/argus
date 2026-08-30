package application

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/source/gitadapter"
	"argus.local/argus/internal/targetmodel"
	"argus.local/argus/internal/workflow"
)

const (
	ConfigSchemaVersion = "argus.local_config.v1alpha1"

	MaterializedTargetSchemaVersion = targetmodel.MaterializedTargetSchemaVersion
	TargetSnapshotSchemaVersion     = targetmodel.TargetSnapshotSchemaVersion
	ContractChangeManifest          = targetmodel.ContractChangeManifest
	ContractSelectionManifest       = targetmodel.ContractSelectionManifest
	ContractScopeManifest           = targetmodel.ContractScopeManifest
	ContractCanonicalPatch          = targetmodel.ContractCanonicalPatch
	ContractSelectionContent        = targetmodel.ContractSelectionContent
	ContractFileContent             = targetmodel.ContractFileContent
	ContractMaterializedTarget      = targetmodel.ContractMaterializedTarget
	ContractReviewInput             = reviewcore.ReviewInputSchemaVersion

	selectionSourceCommit  = targetmodel.SelectionSourceCommit
	selectionSourceOverlay = targetmodel.SelectionSourceOverlay
)

type TargetSnapshot = targetmodel.TargetSnapshot
type DiffSnapshot = targetmodel.DiffSnapshot
type SelectionSnapshot = targetmodel.SelectionSnapshot
type SelectionRange = targetmodel.SelectionRange
type SymbolSelector = targetmodel.SymbolSelector
type ScopeSnapshot = targetmodel.ScopeSnapshot
type MaterializedTarget = targetmodel.MaterializedTarget
type TargetFileRef = targetmodel.TargetFileRef
type SelectionManifest = targetmodel.SelectionManifest
type ScopeManifest = targetmodel.ScopeManifest
type ScopeManifestFile = targetmodel.ScopeManifestFile
type ScopeCoverage = targetmodel.ScopeCoverage

type LocalConfig struct {
	SchemaVersion        string                  `json:"schema_version"`
	ID                   string                  `json:"id"`
	Revision             string                  `json:"revision"`
	MaxAttempts          int                     `json:"max_attempts"`
	MaxPatchBytes        int64                   `json:"max_patch_bytes"`
	MaxFiles             int                     `json:"max_files"`
	MaxFileContentBytes  int64                   `json:"max_file_content_bytes"`
	MaxMaterializedBytes int64                   `json:"max_materialized_bytes"`
	AllowedModes         []reviewcore.TargetMode `json:"allowed_modes"`
	TargetInclude        []string                `json:"target_include"`
	TargetExclude        []string                `json:"target_exclude"`
	RemoteWrites         string                  `json:"remote_writes"`
}

type ReviewRequest struct {
	RepositoryPath string
	Mode           reviewcore.TargetMode

	BaseRevision string
	HeadRevision string

	Revision        string
	SelectionPath   string
	StartLine       uint32
	EndLine         uint32
	SelectionRanges []SelectionRange
	SelectionSymbol *SymbolSelector
	OverlayContent  *string

	Include []string
	Exclude []string

	MaxPatchBytes int64
	MaxFiles      int

	// Contexts are already-frozen provider facts outside the target
	// authorization boundary. They never add ReviewRegions.
	Contexts []reviewcore.ContextBinding
	// ContextProviderIDs are invocation-local built-ins that must first be
	// frozen into the resolved default ConfigBundle.
	ContextProviderIDs []string
}

type ReplayRequest struct {
	SourceRunID         string
	StartStage          string
	Variable            runmodel.ReplayVariable
	VariantConfigBundle *reviewconfig.ConfigBundle
	// VariantWorkflow is required only for a workflow replay. The ConfigBundle
	// carries its exact identity; this value supplies the separately admitted
	// executable definition whose bytes are persisted into the new snapshot.
	VariantWorkflow *workflow.Definition
}

func DefaultLocalConfig() LocalConfig {
	return LocalConfig{
		SchemaVersion:        ConfigSchemaVersion,
		ID:                   "local-default",
		Revision:             "1",
		MaxAttempts:          2,
		MaxPatchBytes:        gitadapter.DefaultMaxPatchBytes,
		MaxFiles:             gitadapter.DefaultMaxFiles,
		MaxFileContentBytes:  2 << 20,
		MaxMaterializedBytes: gitadapter.DefaultMaxPatchBytes,
		AllowedModes: []reviewcore.TargetMode{
			reviewcore.TargetModeDiff,
			reviewcore.TargetModeScope,
			reviewcore.TargetModeSelection,
		},
		TargetInclude: []string{"**"},
		TargetExclude: []string{},
		RemoteWrites:  "deny",
	}
}

func (config LocalConfig) Validate() error {
	if config.SchemaVersion != ConfigSchemaVersion || config.ID == "" || config.Revision == "" {
		return fmt.Errorf("local config identity is invalid")
	}
	if config.MaxAttempts < 1 ||
		config.MaxPatchBytes < 1 || config.MaxFiles < 1 ||
		config.MaxFileContentBytes < 1 || config.MaxMaterializedBytes < 1 {
		return fmt.Errorf("local config limits and rules are invalid")
	}
	if config.AllowedModes == nil || len(config.AllowedModes) == 0 ||
		!slices.IsSorted(config.AllowedModes) {
		return fmt.Errorf("local config allowed_modes must be an explicit sorted non-empty array")
	}
	for index, mode := range config.AllowedModes {
		if index > 0 && mode == config.AllowedModes[index-1] {
			return fmt.Errorf("local config allowed_modes contains duplicate %q", mode)
		}
		switch mode {
		case reviewcore.TargetModeDiff, reviewcore.TargetModeSelection, reviewcore.TargetModeScope:
		default:
			return fmt.Errorf("local config contains unsupported target mode %q", mode)
		}
	}
	if config.TargetInclude == nil || len(config.TargetInclude) == 0 ||
		config.TargetExclude == nil {
		return fmt.Errorf("local config target patterns must be explicit")
	}
	for name, patterns := range map[string][]string{
		"target_include": config.TargetInclude,
		"target_exclude": config.TargetExclude,
	} {
		if !slices.IsSorted(patterns) {
			return fmt.Errorf("local config %s must be sorted", name)
		}
		for index, pattern := range patterns {
			if index > 0 && pattern == patterns[index-1] {
				return fmt.Errorf("local config %s contains duplicate %q", name, pattern)
			}
			if err := targetmodel.ValidateScopePattern(name, pattern); err != nil {
				return err
			}
		}
	}
	if config.RemoteWrites != "deny" {
		return fmt.Errorf("local config must deny remote writes")
	}
	return nil
}

func (config LocalConfig) Digest() (string, error) {
	if err := config.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("marshal local config: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (request ReviewRequest) normalizedMode() reviewcore.TargetMode {
	if request.Mode == "" {
		return reviewcore.TargetModeDiff
	}
	return request.Mode
}

func (request ReviewRequest) Validate(config LocalConfig) error {
	if err := config.Validate(); err != nil {
		return err
	}
	if !slices.Contains(config.AllowedModes, request.normalizedMode()) {
		return fmt.Errorf("target mode %q is denied by the resolved config", request.normalizedMode())
	}
	if request.RepositoryPath == "" || !filepath.IsAbs(request.RepositoryPath) ||
		filepath.Clean(request.RepositoryPath) != request.RepositoryPath {
		return fmt.Errorf("repository path must be a clean absolute path")
	}
	if request.MaxPatchBytes < 0 || request.MaxFiles < 0 {
		return fmt.Errorf("review request limits must not be negative")
	}
	if !slices.IsSorted(request.ContextProviderIDs) {
		return fmt.Errorf("context provider IDs must be sorted")
	}
	for index, providerID := range request.ContextProviderIDs {
		if providerID != "repository_search" && providerID != "go_ast" &&
			providerID != "go_dependencies" && providerID != "go_compile" {
			return fmt.Errorf("unsupported invocation context provider %q", providerID)
		}
		if index > 0 && providerID == request.ContextProviderIDs[index-1] {
			return fmt.Errorf("context provider IDs contain duplicate %q", providerID)
		}
	}
	switch request.normalizedMode() {
	case reviewcore.TargetModeDiff:
		if strings.TrimSpace(request.BaseRevision) == "" ||
			strings.TrimSpace(request.HeadRevision) == "" {
			return fmt.Errorf("diff target requires base and head revisions")
		}
		if request.Revision != "" || request.SelectionPath != "" ||
			request.StartLine != 0 || request.EndLine != 0 ||
			request.SelectionRanges != nil || request.SelectionSymbol != nil ||
			request.OverlayContent != nil || request.Include != nil || request.Exclude != nil {
			return fmt.Errorf("diff target contains fields from another target mode")
		}
	case reviewcore.TargetModeSelection:
		if strings.TrimSpace(request.Revision) == "" {
			return fmt.Errorf("selection target requires revision")
		}
		if err := targetmodel.ValidateRepositoryPath("selection path", request.SelectionPath); err != nil {
			return err
		}
		selectorForms := 0
		if request.StartLine != 0 || request.EndLine != 0 {
			selectorForms++
			if request.StartLine == 0 || request.EndLine < request.StartLine {
				return fmt.Errorf("selection target has an invalid legacy line range")
			}
		}
		if len(request.SelectionRanges) > 0 {
			selectorForms++
			if err := targetmodel.ValidateSelectionRanges(
				"selection ranges",
				request.SelectionRanges,
			); err != nil {
				return err
			}
		} else if request.SelectionRanges != nil {
			return fmt.Errorf("selection ranges must not be an empty array")
		}
		if request.SelectionSymbol != nil {
			selectorForms++
			if err := request.SelectionSymbol.Validate(); err != nil {
				return err
			}
		}
		if selectorForms != 1 {
			return fmt.Errorf("selection target requires exactly one selector form")
		}
		if request.BaseRevision != "" || request.HeadRevision != "" ||
			request.Include != nil || request.Exclude != nil {
			return fmt.Errorf("selection target contains fields from another target mode")
		}
		if request.OverlayContent != nil {
			if !utf8.ValidString(*request.OverlayContent) ||
				strings.ContainsRune(*request.OverlayContent, '\x00') ||
				strings.ContainsRune(*request.OverlayContent, '\r') {
				return fmt.Errorf("selection overlay must be LF-only UTF-8 text without NUL bytes")
			}
		}
	case reviewcore.TargetModeScope:
		if strings.TrimSpace(request.Revision) == "" {
			return fmt.Errorf("scope target requires revision")
		}
		if request.Include == nil || len(request.Include) == 0 || request.Exclude == nil {
			return fmt.Errorf("scope target requires include and an explicit exclude list")
		}
		if request.BaseRevision != "" || request.HeadRevision != "" ||
			request.SelectionPath != "" || request.StartLine != 0 || request.EndLine != 0 ||
			request.SelectionRanges != nil || request.SelectionSymbol != nil ||
			request.OverlayContent != nil {
			return fmt.Errorf("scope target contains fields from another target mode")
		}
		seen := make(map[string]struct{}, len(request.Include)+len(request.Exclude))
		for name, patterns := range map[string][]string{
			"include": request.Include,
			"exclude": request.Exclude,
		} {
			for _, pattern := range patterns {
				if err := targetmodel.ValidateScopePattern(name, pattern); err != nil {
					return err
				}
				key := name + "\x00" + pattern
				if _, duplicate := seen[key]; duplicate {
					return fmt.Errorf("%s contains duplicate pattern %q", name, pattern)
				}
				seen[key] = struct{}{}
			}
		}
	default:
		return fmt.Errorf("unsupported review target mode %q", request.Mode)
	}
	previousContextID := ""
	for index, binding := range request.Contexts {
		if err := binding.Validate(); err != nil {
			return fmt.Errorf("contexts[%d]: %w", index, err)
		}
		contextID := binding.ContextID()
		if index > 0 && contextID <= previousContextID {
			return fmt.Errorf("contexts must be uniquely sorted by context_id")
		}
		previousContextID = contextID
	}
	return nil
}

func SealTargetSnapshot(snapshot TargetSnapshot) (TargetSnapshot, error) {
	return targetmodel.SealTargetSnapshot(snapshot)
}

func DigestTargetSnapshot(snapshot TargetSnapshot) (string, error) {
	return targetmodel.DigestTargetSnapshot(snapshot)
}
