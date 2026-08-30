package reviewconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

const ConfigResolutionReceiptSchemaVersion = "argus.config_resolution_receipt.v1alpha1"

const (
	ConfigResolutionOriginLifecyclePublished = "lifecycle_published"
	ConfigResolutionOriginReplayVariant      = "replay_variant_derived"
)

// PublishedRevisionBinding is host-owned provenance for one exact revision
// selected by the lifecycle ledger. AssignmentSHA256 binds the complete
// active selector projection and resolution context without making rollout
// internals part of the effective ConfigBundle.
type PublishedRevisionBinding struct {
	Source           SourceRef `json:"source"`
	RevisionSHA256   string    `json:"revision_sha256"`
	PublishEventID   string    `json:"publish_event_id"`
	PublishSequence  uint64    `json:"publish_sequence"`
	PublishedAt      time.Time `json:"published_at"`
	AssignmentSHA256 string    `json:"assignment_sha256"`
}

// ConfigResolutionReceipt is separate from ConfigBundle so the receipt can
// bind the final bundle digest without introducing a self-reference. It is a
// local governance proof that must still be verified against the trusted
// config ledger; it is not a signature or remote attestation.
type ConfigResolutionReceipt struct {
	SchemaVersion string `json:"schema_version"`
	ReceiptID     string `json:"receipt_id"`
	SHA256        string `json:"sha256"`
	Origin        string `json:"origin"`

	Context       ResolutionContext           `json:"context"`
	BundleID      string                      `json:"bundle_id"`
	BundleSHA256  string                      `json:"bundle_sha256"`
	Revisions     []PublishedRevisionBinding  `json:"published_revisions"`
	ReplayVariant *ReplayVariantConfigBinding `json:"replay_variant,omitempty"`
}

// ReplayVariantConfigBinding proves that an experimental ConfigBundle was
// derived from one exact governed resolution rather than being presented as a
// newly lifecycle-published configuration.
type ReplayVariantConfigBinding struct {
	SourceReceiptID     string   `json:"source_receipt_id"`
	SourceReceiptSHA256 string   `json:"source_receipt_sha256"`
	BaselineBundleID    string   `json:"baseline_bundle_id"`
	BaselineSHA256      string   `json:"baseline_sha256"`
	Variable            string   `json:"variable"`
	ChangedFields       []string `json:"changed_fields"`
}

// NewConfigResolutionReceipt seals host-derived lifecycle bindings to one
// already-valid ConfigBundle. Callers cannot use this constructor as a
// substitute for ledger verification; only configrepo owns that authority.
func NewConfigResolutionReceipt(
	bundle ConfigBundle,
	bindings []PublishedRevisionBinding,
) (ConfigResolutionReceipt, error) {
	if err := bundle.Validate(); err != nil {
		return ConfigResolutionReceipt{}, fmt.Errorf("validate receipt ConfigBundle: %w", err)
	}
	receipt := ConfigResolutionReceipt{
		SchemaVersion: ConfigResolutionReceiptSchemaVersion,
		Origin:        ConfigResolutionOriginLifecyclePublished,
		Context:       bundle.Context,
		BundleID:      bundle.BundleID,
		BundleSHA256:  bundle.SHA256,
		Revisions:     slices.Clone(bindings),
	}
	digest, err := DigestConfigResolutionReceipt(receipt)
	if err != nil {
		return ConfigResolutionReceipt{}, err
	}
	receipt.SHA256 = digest
	receipt.ReceiptID = "config-resolution-" + digest[:24]
	if err := receipt.ValidateAgainst(bundle); err != nil {
		return ConfigResolutionReceipt{}, err
	}
	return receipt, nil
}

func NewReplayVariantConfigReceipt(
	variant ConfigBundle,
	baseline ConfigBundle,
	source ConfigResolutionReceipt,
	variable string,
	changedFields []string,
) (ConfigResolutionReceipt, error) {
	if err := source.ValidateAgainst(baseline); err != nil {
		return ConfigResolutionReceipt{}, fmt.Errorf("validate replay source receipt: %w", err)
	}
	if err := variant.Validate(); err != nil {
		return ConfigResolutionReceipt{}, fmt.Errorf("validate replay variant ConfigBundle: %w", err)
	}
	if variant.Context != baseline.Context ||
		!slices.Equal(variant.AppliedRevisions, baseline.AppliedRevisions) {
		return ConfigResolutionReceipt{}, fmt.Errorf(
			"replay variant must preserve source context and applied revisions",
		)
	}
	receipt := ConfigResolutionReceipt{
		SchemaVersion: ConfigResolutionReceiptSchemaVersion,
		Origin:        ConfigResolutionOriginReplayVariant,
		Context:       variant.Context,
		BundleID:      variant.BundleID,
		BundleSHA256:  variant.SHA256,
		Revisions:     slices.Clone(source.Revisions),
		ReplayVariant: &ReplayVariantConfigBinding{
			SourceReceiptID: source.ReceiptID, SourceReceiptSHA256: source.SHA256,
			BaselineBundleID: baseline.BundleID, BaselineSHA256: baseline.SHA256,
			Variable: variable, ChangedFields: slices.Clone(changedFields),
		},
	}
	digest, err := DigestConfigResolutionReceipt(receipt)
	if err != nil {
		return ConfigResolutionReceipt{}, err
	}
	receipt.SHA256 = digest
	receipt.ReceiptID = "config-resolution-" + digest[:24]
	if err := receipt.ValidateAgainst(variant); err != nil {
		return ConfigResolutionReceipt{}, err
	}
	return receipt, nil
}

func DigestConfigResolutionReceipt(receipt ConfigResolutionReceipt) (string, error) {
	copy := receipt
	copy.ReceiptID = ""
	copy.SHA256 = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal ConfigResolutionReceipt: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func DecodeConfigResolutionReceipt(data []byte) (ConfigResolutionReceipt, error) {
	if len(bytes.TrimSpace(data)) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return ConfigResolutionReceipt{}, fmt.Errorf("ConfigResolutionReceipt must be a JSON object")
	}
	if err := rejectDuplicateJSONFields(data); err != nil {
		return ConfigResolutionReceipt{}, fmt.Errorf("decode ConfigResolutionReceipt: %w", err)
	}
	if err := rejectJSONNulls(data); err != nil {
		return ConfigResolutionReceipt{}, fmt.Errorf("decode ConfigResolutionReceipt: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var receipt ConfigResolutionReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return ConfigResolutionReceipt{}, fmt.Errorf("decode ConfigResolutionReceipt: %w", err)
	}
	var trailing any
	switch err := decoder.Decode(&trailing); {
	case errors.Is(err, io.EOF):
	case err == nil:
		return ConfigResolutionReceipt{}, fmt.Errorf("ConfigResolutionReceipt contains multiple JSON values")
	default:
		return ConfigResolutionReceipt{}, fmt.Errorf("decode trailing ConfigResolutionReceipt: %w", err)
	}
	if err := receipt.Validate(); err != nil {
		return ConfigResolutionReceipt{}, fmt.Errorf("validate ConfigResolutionReceipt: %w", err)
	}
	return receipt, nil
}

// Validate proves that the receipt is a canonical, self-consistent local
// governance record. It does not prove that the referenced publications exist;
// configrepo remains the authority for ledger verification.
func (receipt ConfigResolutionReceipt) Validate() error {
	if receipt.SchemaVersion != ConfigResolutionReceiptSchemaVersion {
		return fmt.Errorf("unsupported ConfigResolutionReceipt schema %q", receipt.SchemaVersion)
	}
	switch receipt.Origin {
	case ConfigResolutionOriginLifecyclePublished:
		if receipt.ReplayVariant != nil {
			return fmt.Errorf("lifecycle ConfigResolutionReceipt cannot contain replay_variant")
		}
	case ConfigResolutionOriginReplayVariant:
		if err := receipt.ReplayVariant.validate(); err != nil {
			return fmt.Errorf("ConfigResolutionReceipt replay_variant: %w", err)
		}
	default:
		return fmt.Errorf("ConfigResolutionReceipt origin is unsupported")
	}
	if err := receipt.Context.Validate(); err != nil {
		return fmt.Errorf("ConfigResolutionReceipt context: %w", err)
	}
	if !isBundleID(receipt.BundleID) {
		return fmt.Errorf("ConfigResolutionReceipt bundle_id is invalid")
	}
	if err := validateSHA256("bundle_sha256", receipt.BundleSHA256); err != nil {
		return err
	}
	if receipt.Revisions == nil || len(receipt.Revisions) == 0 {
		return fmt.Errorf("published_revisions must be an explicit non-empty array")
	}
	identities := make(map[string]struct{}, len(receipt.Revisions))
	selectors := make(map[string]struct{}, len(receipt.Revisions))
	eventIDs := make(map[string]struct{}, len(receipt.Revisions))
	sequences := make(map[uint64]struct{}, len(receipt.Revisions))
	lastRank, lastPathDepth, lastSelector := -1, -1, ""
	for index, binding := range receipt.Revisions {
		if err := binding.Source.Validate(); err != nil {
			return fmt.Errorf("published_revisions[%d].source: %w", index, err)
		}
		if binding.Source.Operation != "applied" {
			return fmt.Errorf("published_revisions[%d].source operation must be applied", index)
		}
		if err := validateReceiptSourceSelector(binding.Source, receipt.Context); err != nil {
			return fmt.Errorf("published_revisions[%d].source: %w", index, err)
		}
		identity := binding.Source.ID + "\x00" + binding.Source.Revision
		if _, duplicate := identities[identity]; duplicate {
			return fmt.Errorf("published_revisions contains duplicate source binding")
		}
		identities[identity] = struct{}{}
		selector := string(binding.Source.Scope) + "\x00" + binding.Source.Selector
		if _, duplicate := selectors[selector]; duplicate {
			return fmt.Errorf("published_revisions contains duplicate source selector")
		}
		selectors[selector] = struct{}{}
		rank := scopeRank(binding.Source.Scope)
		pathDepth := 0
		if binding.Source.Scope == ScopePath {
			pathPrefix := strings.TrimPrefix(
				binding.Source.Selector,
				receiptSourcePrefix(receipt.Context, ScopePath),
			)
			pathDepth = strings.Count(pathPrefix, "/") + 1
		}
		if rank < lastRank || rank == lastRank &&
			(pathDepth < lastPathDepth || pathDepth == lastPathDepth &&
				binding.Source.Selector < lastSelector) {
			return fmt.Errorf("published_revisions are not ordered by specificity")
		}
		lastRank, lastPathDepth, lastSelector = rank, pathDepth, binding.Source.Selector
		if err := validateSHA256("revision_sha256", binding.RevisionSHA256); err != nil {
			return fmt.Errorf("published_revisions[%d]: %w", index, err)
		}
		if err := validateSHA256("assignment_sha256", binding.AssignmentSHA256); err != nil {
			return fmt.Errorf("published_revisions[%d]: %w", index, err)
		}
		if err := validateID("publish_event_id", binding.PublishEventID); err != nil {
			return fmt.Errorf("published_revisions[%d]: %w", index, err)
		}
		if _, duplicate := eventIDs[binding.PublishEventID]; duplicate {
			return fmt.Errorf("published_revisions contains duplicate publish_event_id")
		}
		eventIDs[binding.PublishEventID] = struct{}{}
		if binding.PublishSequence == 0 || binding.PublishedAt.IsZero() ||
			binding.PublishedAt.Location() != time.UTC {
			return fmt.Errorf("published_revisions[%d] publication observation is invalid", index)
		}
		if _, duplicate := sequences[binding.PublishSequence]; duplicate {
			return fmt.Errorf("published_revisions contains duplicate publish_sequence")
		}
		sequences[binding.PublishSequence] = struct{}{}
	}
	if err := validateSHA256("sha256", receipt.SHA256); err != nil {
		return err
	}
	digest, err := DigestConfigResolutionReceipt(receipt)
	if err != nil {
		return err
	}
	if receipt.SHA256 != digest || receipt.ReceiptID != "config-resolution-"+digest[:24] {
		return fmt.Errorf("ConfigResolutionReceipt identity does not match its content")
	}
	return nil
}

func (binding *ReplayVariantConfigBinding) validate() error {
	if binding == nil {
		return fmt.Errorf("binding is required")
	}
	if !isConfigResolutionReceiptID(binding.SourceReceiptID) {
		return fmt.Errorf("source_receipt_id is invalid")
	}
	if err := validateSHA256("source_receipt_sha256", binding.SourceReceiptSHA256); err != nil {
		return err
	}
	if !isBundleID(binding.BaselineBundleID) {
		return fmt.Errorf("baseline_bundle_id is invalid")
	}
	if err := validateSHA256("baseline_sha256", binding.BaselineSHA256); err != nil {
		return err
	}
	if err := validateID("variable", binding.Variable); err != nil {
		return err
	}
	if binding.ChangedFields == nil || len(binding.ChangedFields) == 0 {
		return fmt.Errorf("changed_fields must be an explicit non-empty array")
	}
	for index, field := range binding.ChangedFields {
		if err := validateID("changed_fields", field); err != nil {
			return err
		}
		if index > 0 && field <= binding.ChangedFields[index-1] {
			return fmt.Errorf("changed_fields must be sorted and unique")
		}
	}
	return nil
}

func isConfigResolutionReceiptID(value string) bool {
	const prefix = "config-resolution-"
	if len(value) != len(prefix)+24 || !strings.HasPrefix(value, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(value, prefix)
	decoded, err := hex.DecodeString(suffix)
	return err == nil && hex.EncodeToString(decoded) == suffix
}

func (receipt ConfigResolutionReceipt) ValidateAgainst(bundle ConfigBundle) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	if err := bundle.Validate(); err != nil {
		return fmt.Errorf("validate receipt ConfigBundle: %w", err)
	}
	if receipt.Context != bundle.Context || receipt.BundleID != bundle.BundleID ||
		receipt.BundleSHA256 != bundle.SHA256 {
		return fmt.Errorf("ConfigResolutionReceipt does not bind the exact ConfigBundle")
	}
	if len(receipt.Revisions) != len(bundle.AppliedRevisions) {
		return fmt.Errorf("published_revisions must bind every applied revision")
	}
	for index, binding := range receipt.Revisions {
		if binding.Source != bundle.AppliedRevisions[index] {
			return fmt.Errorf("published_revisions[%d] does not bind the applied revision", index)
		}
	}
	return nil
}

func isBundleID(value string) bool {
	if len(value) != len("bundle-")+24 || !strings.HasPrefix(value, "bundle-") {
		return false
	}
	suffix := strings.TrimPrefix(value, "bundle-")
	decoded, err := hex.DecodeString(suffix)
	return err == nil && hex.EncodeToString(decoded) == suffix
}

func validateReceiptSourceSelector(source SourceRef, context ResolutionContext) error {
	prefix := receiptSourcePrefix(context, source.Scope)
	if source.Scope != ScopePath {
		if source.Selector != prefix {
			return fmt.Errorf("selector does not match receipt context")
		}
		return nil
	}
	if !strings.HasPrefix(source.Selector, prefix) {
		return fmt.Errorf("path selector does not match receipt context")
	}
	pathPrefix := strings.TrimPrefix(source.Selector, prefix)
	if err := validatePathPrefix("path selector", pathPrefix); err != nil ||
		!pathPrefixMatches(pathPrefix, context.Path) {
		return fmt.Errorf("path selector does not match receipt context")
	}
	return nil
}

func receiptSourcePrefix(context ResolutionContext, scope Scope) string {
	switch scope {
	case ScopePlatform:
		return "*"
	case ScopeTenant:
		return "tenant=" + context.TenantID
	case ScopeOrganization:
		return "tenant=" + context.TenantID + ";organization=" + context.OrganizationID
	case ScopeRepository:
		return "tenant=" + context.TenantID + ";organization=" + context.OrganizationID +
			";repository=" + context.RepositoryID
	case ScopePath:
		return "tenant=" + context.TenantID + ";organization=" + context.OrganizationID +
			";repository=" + context.RepositoryID + ";path="
	case ScopeInvocation:
		return "tenant=" + context.TenantID + ";organization=" + context.OrganizationID +
			";repository=" + context.RepositoryID + ";invocation=" + context.InvocationID
	default:
		return ""
	}
}
