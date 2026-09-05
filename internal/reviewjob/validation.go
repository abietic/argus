package reviewjob

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/formalreview"
	"github.com/abietic/argus/internal/piexecution"
	"github.com/abietic/argus/internal/reviewcore"
)

const (
	minimumExecutionTimeoutSeconds = uint32(30)
	maximumExecutionTimeoutSeconds = uint32(24 * 60 * 60)
)

func (request Request) Validate() error {
	if request.SchemaVersion != RequestSchemaVersion {
		return fmt.Errorf("unsupported review job request schema %q", request.SchemaVersion)
	}
	if request.ExecutionTimeoutSeconds < minimumExecutionTimeoutSeconds ||
		request.ExecutionTimeoutSeconds > maximumExecutionTimeoutSeconds {
		return fmt.Errorf(
			"execution_timeout_seconds must be between %d and %d",
			minimumExecutionTimeoutSeconds,
			maximumExecutionTimeoutSeconds,
		)
	}
	switch request.ExecutionProfile {
	case DeterministicExecutionProfile:
		if request.SourceRunID != "" {
			return fmt.Errorf("source_run_id is only valid for formal_pi_review_v1")
		}
		if request.RepositoryPath == "" || !filepath.IsAbs(request.RepositoryPath) ||
			filepath.Clean(request.RepositoryPath) != request.RepositoryPath {
			return fmt.Errorf("repository_path must be a clean absolute path")
		}
		if request.Mode == string(reviewcore.TargetModeScope) {
			if !slices.IsSorted(request.Include) || !slices.IsSorted(request.Exclude) {
				return fmt.Errorf("scope include and exclude must be sorted")
			}
		}
		return request.ApplicationRequest().Validate(application.DefaultLocalConfig())
	case FormalPiExecutionProfile:
		if err := validateID("source_run_id", request.SourceRunID); err != nil {
			return err
		}
		if request.RepositoryPath != "" || request.Mode != "" || request.BaseRevision != "" ||
			request.HeadRevision != "" || request.Revision != "" || request.SelectionPath != "" ||
			request.StartLine != 0 || request.EndLine != 0 || request.SelectionRanges != nil ||
			request.SelectionSymbol != nil || request.Include != nil || request.Exclude != nil ||
			request.OverlayContent != nil || request.Contexts != nil {
			return fmt.Errorf("formal_pi_review_v1 accepts only source_run_id and execution timeout")
		}
		return nil
	default:
		return fmt.Errorf("unsupported execution_profile %q", request.ExecutionProfile)
	}
}

func (request Request) ApplicationRequest() application.ReviewRequest {
	return application.ReviewRequest{
		RepositoryPath:  request.RepositoryPath,
		Mode:            reviewcore.TargetMode(request.Mode),
		BaseRevision:    request.BaseRevision,
		HeadRevision:    request.HeadRevision,
		Revision:        request.Revision,
		SelectionPath:   request.SelectionPath,
		StartLine:       request.StartLine,
		EndLine:         request.EndLine,
		SelectionRanges: slices.Clone(request.SelectionRanges),
		SelectionSymbol: cloneSymbol(request.SelectionSymbol),
		OverlayContent:  cloneString(request.OverlayContent),
		Contexts:        cloneContexts(request.Contexts),
		Include:         slices.Clone(request.Include),
		Exclude:         slices.Clone(request.Exclude),
	}
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneContexts(bindings []reviewcore.ContextBinding) []reviewcore.ContextBinding {
	result := slices.Clone(bindings)
	for index, binding := range result {
		if binding.Ref != nil {
			copy := *binding.Ref
			copy.Coverage.Spans = slices.Clone(copy.Coverage.Spans)
			copy.Coverage.Symbols = slices.Clone(copy.Coverage.Symbols)
			result[index].Ref = &copy
		}
		if binding.Gap != nil {
			copy := *binding.Gap
			copy.Coverage.Spans = slices.Clone(copy.Coverage.Spans)
			copy.Coverage.Symbols = slices.Clone(copy.Coverage.Symbols)
			result[index].Gap = &copy
		}
	}
	return result
}

func cloneSymbol(symbol *application.SymbolSelector) *application.SymbolSelector {
	if symbol == nil {
		return nil
	}
	copy := *symbol
	return &copy
}

func (mutation Mutation) Validate() error {
	if err := validateID("idempotency_key", mutation.IdempotencyKey); err != nil {
		return err
	}
	if err := validateID("actor", mutation.Actor); err != nil {
		return err
	}
	if err := validateText("audit", mutation.Audit, 4096); err != nil {
		return err
	}
	if mutation.At.IsZero() || mutation.At.Location() != time.UTC {
		return fmt.Errorf("mutation at must be a non-zero UTC timestamp")
	}
	return nil
}

func (command CancelCommand) Validate() error {
	if err := validateText("reason", command.Reason, 1024); err != nil {
		return err
	}
	return command.Mutation.Validate()
}

func (command Command) Validate() error {
	if command.SchemaVersion != CommandSchemaVersion {
		return fmt.Errorf("unsupported review job command schema %q", command.SchemaVersion)
	}
	for name, value := range map[string]string{
		"job_id": command.JobID, "run_id": command.RunID,
		"actor": command.Actor, "idempotency_key": command.IdempotencyKey,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if command.ExecutionProfile != command.Request.ExecutionProfile {
		return fmt.Errorf("execution_profile does not bind request")
	}
	wantJobID, deterministicRunID := deterministicIDs(command.Actor, command.IdempotencyKey)
	wantRunID := deterministicRunID
	if command.ExecutionProfile == FormalPiExecutionProfile {
		wantRunID = formalreview.FormalRunID(command.Request.SourceRunID, command.IdempotencyKey)
	}
	if command.JobID != wantJobID || command.RunID != wantRunID {
		return fmt.Errorf("command identity does not bind actor and idempotency key")
	}
	if err := command.Request.Validate(); err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if err := command.ConfigBundle.Validate(); err != nil {
		return fmt.Errorf("config_bundle: %w", err)
	}
	if err := command.ConfigReceipt.ValidateAgainst(command.ConfigBundle); err != nil {
		return fmt.Errorf("config_resolution_receipt: %w", err)
	}
	if err := validateText("audit", command.Audit, 4096); err != nil {
		return err
	}
	if command.SubmittedAt.IsZero() || command.SubmittedAt.Location() != time.UTC {
		return fmt.Errorf("submitted_at must be a non-zero UTC timestamp")
	}
	if command.ConfigBundle.Context.InvocationID != command.RunID {
		return fmt.Errorf("config bundle invocation does not bind run_id")
	}
	switch command.ExecutionProfile {
	case DeterministicExecutionProfile:
		if command.FormalRuntime != nil {
			return fmt.Errorf("deterministic review command cannot carry formal_runtime")
		}
		if err := application.ValidateReviewRequestAgainstBundle(
			command.Request.ApplicationRequest(), command.ConfigBundle,
		); err != nil {
			return fmt.Errorf("request is incompatible with frozen config: %w", err)
		}
	case FormalPiExecutionProfile:
		if command.FormalRuntime == nil {
			return fmt.Errorf("formal Pi command requires formal_runtime")
		}
		if err := validateFormalRuntime(*command.FormalRuntime); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported execution_profile %q", command.ExecutionProfile)
	}
	return nil
}

func validateFormalRuntime(binding FormalRuntimeBinding) error {
	if binding.Bootstrap.SchemaVersion != formalreview.LocalPiBootstrapSchemaVersion {
		return fmt.Errorf("formal_runtime bootstrap schema is unsupported")
	}
	if _, err := piexecution.NewCapabilityResolver(binding.Bootstrap.Manifest, binding.Pricing); err != nil {
		return fmt.Errorf("formal_runtime capability: %w", err)
	}
	if binding.Options.NodePath == "" || binding.Options.WorkerScript == "" {
		return fmt.Errorf("formal_runtime node and worker script are required")
	}
	return nil
}

func deterministicIDs(actor, idempotencyKey string) (jobID, runID string) {
	sum := sha256.Sum256([]byte(actor + "\x00" + idempotencyKey))
	digest := hex.EncodeToString(sum[:16])
	return "job-" + digest, "run-" + digest
}

// DeterministicIDs returns the immutable deterministic job and source run IDs.
// Callers must validate actor and idempotencyKey through Mutation.Validate.
func DeterministicIDs(actor, idempotencyKey string) (jobID, runID string) {
	return deterministicIDs(actor, idempotencyKey)
}

func validateID(name, value string) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 256 ||
		!utf8.ValidString(value) {
		return fmt.Errorf("%s is invalid", name)
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) ||
			!(unicode.IsLetter(character) || unicode.IsDigit(character) ||
				strings.ContainsRune("._~:/@+-", character)) {
			return fmt.Errorf("%s is invalid", name)
		}
	}
	return nil
}

func validateText(name, value string, maximum int) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maximum ||
		!utf8.ValidString(value) {
		return fmt.Errorf("%s must be non-empty, trimmed UTF-8 within %d bytes", name, maximum)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%s contains unsafe characters", name)
		}
	}
	return nil
}
