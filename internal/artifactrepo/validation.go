package artifactrepo

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"argus.local/argus/internal/store/local"
)

const maxSubjectRoles = 64

func (subject Subject) Validate() error {
	if err := validateID("tenant_id", subject.TenantID); err != nil {
		return err
	}
	if err := validateID("workspace_id", subject.WorkspaceID); err != nil {
		return err
	}
	if subject.Roles == nil || len(subject.Roles) > maxSubjectRoles ||
		!sortedUniqueStrings(subject.Roles) {
		return fmt.Errorf("roles must be an explicit sorted unique array")
	}
	return nil
}

func (mutation Mutation) Validate() error {
	if err := validateID("idempotency_key", mutation.IdempotencyKey); err != nil {
		return err
	}
	if err := validateText("actor", mutation.Actor); err != nil {
		return err
	}
	if err := validateText("audit", mutation.Audit); err != nil {
		return err
	}
	if mutation.At.IsZero() || mutation.At.Location() != time.UTC {
		return fmt.Errorf("mutation at must be a non-zero UTC timestamp")
	}
	return nil
}

func (metadata Metadata) Validate() error {
	if metadata.SchemaVersion != metadataSchemaVersion {
		return fmt.Errorf("unsupported artifact metadata schema %q", metadata.SchemaVersion)
	}
	for name, value := range map[string]string{
		"object_id": metadata.ObjectID, "authority": metadata.Authority,
		"tenant_id": metadata.TenantID, "workspace_id": metadata.WorkspaceID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if !lowerSHA256(metadata.ObjectID) {
		return fmt.Errorf("object_id must be a lowercase SHA-256 digest")
	}
	if metadata.Authority != strings.ToLower(metadata.Authority) {
		return fmt.Errorf("authority must be lowercase canonical form")
	}
	if err := validateText("contract", metadata.Contract); err != nil {
		return err
	}
	if len(metadata.AllowedUses) == 0 || !slices.IsSorted(metadata.AllowedUses) {
		return fmt.Errorf("allowed_uses must be a sorted non-empty array")
	}
	for index, use := range metadata.AllowedUses {
		if index > 0 && use == metadata.AllowedUses[index-1] {
			return fmt.Errorf("allowed_uses contains duplicate %q", use)
		}
		if err := use.Validate(); err != nil {
			return err
		}
	}
	if err := validateReadClass(metadata.AllowedUses); err != nil {
		return err
	}
	if err := validateLocalRef(metadata.Content); err != nil {
		return err
	}
	if err := validateText("created_by", metadata.CreatedBy); err != nil {
		return err
	}
	if metadata.CreatedAt.IsZero() || metadata.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("created_at must be a non-zero UTC timestamp")
	}
	expected := objectID(
		metadata.Authority,
		metadata.TenantID,
		metadata.WorkspaceID,
		metadata.Contract,
		metadata.Content.SHA256,
		metadata.Content.SizeBytes,
		metadata.AllowedUses,
	)
	if metadata.ObjectID != expected {
		return fmt.Errorf("object_id does not match immutable metadata")
	}
	return nil
}

func (use Use) Validate() error {
	switch use {
	case UseRead, UsePublication, UseEvaluation, UseTraining, UseExport,
		UseSensitiveProcess, UseSensitiveRead:
		return nil
	default:
		return fmt.Errorf("unsupported artifact use %q", use)
	}
}

func requiredUseRole(use Use) string {
	switch use {
	case UsePublication:
		return RolePublicationUse
	case UseEvaluation:
		return RoleEvaluationUse
	case UseTraining:
		return RoleTrainingUse
	case UseExport:
		return RoleExportUse
	case UseSensitiveProcess:
		return RoleSensitiveProcessor
	case UseSensitiveRead:
		return RoleSensitiveReader
	default:
		return ""
	}
}

func validateReadClass(uses []Use) error {
	regular := slices.Contains(uses, UseRead)
	sensitive := slices.Contains(uses, UseSensitiveProcess) ||
		slices.Contains(uses, UseSensitiveRead)
	if regular == sensitive {
		return fmt.Errorf("allowed_uses must select exactly one regular or sensitive read class")
	}
	if sensitive && (!slices.Contains(uses, UseSensitiveProcess) ||
		!slices.Contains(uses, UseSensitiveRead)) {
		return fmt.Errorf("sensitive artifacts must allow both processing and audited read")
	}
	return nil
}

func (request SensitiveAccessRequest) Validate() error {
	if err := validateID("request_id", request.RequestID); err != nil {
		return err
	}
	if err := validateText("actor", request.Actor); err != nil {
		return err
	}
	switch request.Purpose {
	case SensitiveAccessLocalDebug,
		SensitiveAccessEvaluationReplay,
		SensitiveAccessIncidentInvestigation:
	default:
		return fmt.Errorf("unsupported sensitive access purpose %q", request.Purpose)
	}
	if request.At.IsZero() || request.At.Location() != time.UTC {
		return fmt.Errorf("sensitive access at must be a non-zero UTC timestamp")
	}
	return nil
}

func (receipt SensitiveAccessReceipt) Validate() error {
	if receipt.SchemaVersion != sensitiveAccessSchemaVersion {
		return fmt.Errorf("unsupported sensitive access receipt schema %q", receipt.SchemaVersion)
	}
	for name, value := range map[string]string{
		"receipt_id": receipt.ReceiptID, "request_id": receipt.RequestID,
		"authority": receipt.Authority, "tenant_id": receipt.TenantID,
		"workspace_id": receipt.WorkspaceID, "object_id": receipt.ObjectID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if !lowerSHA256(receipt.ReceiptID) || !lowerSHA256(receipt.ObjectID) ||
		!lowerSHA256(receipt.ContentSHA256) || receipt.SizeBytes < 0 {
		return fmt.Errorf("sensitive access receipt digests or size are invalid")
	}
	if err := validateText("contract", receipt.Contract); err != nil {
		return err
	}
	request := SensitiveAccessRequest{
		RequestID: receipt.RequestID, Actor: receipt.Actor,
		Purpose: receipt.Purpose, At: receipt.AuthorizedAt,
	}
	if err := request.Validate(); err != nil {
		return err
	}
	want := sensitiveAccessReceiptID(receipt)
	if receipt.ReceiptID != want {
		return fmt.Errorf("sensitive access receipt_id does not bind immutable fields")
	}
	return nil
}

func authorizeUseRole(subject Subject, use Use) error {
	required := requiredUseRole(use)
	if required != "" && !slices.Contains(subject.Roles, required) {
		return fmt.Errorf("%w: role %q is required for %s use", ErrUnauthorized, required, use)
	}
	return nil
}

func validateRef(ref Ref) (refIdentity, error) {
	if !lowerSHA256(ref.SHA256) || ref.SizeBytes < 0 {
		return refIdentity{}, fmt.Errorf("artifact ref digest or size is invalid")
	}
	if ref.URI == "" || len(ref.URI) > 1024 ||
		ref.URI != strings.TrimSpace(ref.URI) || !utf8.ValidString(ref.URI) {
		return refIdentity{}, fmt.Errorf("artifact ref URI is invalid")
	}
	if err := validateText("contract", ref.Contract); err != nil {
		return refIdentity{}, err
	}
	parsed, err := url.Parse(ref.URI)
	if err != nil {
		return refIdentity{}, fmt.Errorf("parse artifact ref: %w", err)
	}
	if parsed.Scheme != "artifact" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.ForceQuery || parsed.Host == "" || parsed.RawPath != "" {
		return refIdentity{}, fmt.Errorf("artifact ref URI is not a logical authority URI")
	}
	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	if len(parts) != 6 || parts[0] != "tenants" ||
		parts[2] != "workspaces" || parts[4] != "objects" {
		return refIdentity{}, fmt.Errorf("artifact ref URI has an unsupported path")
	}
	identity := refIdentity{
		authority: parsed.Host,
		tenantID:  parts[1], workspaceID: parts[3], objectID: parts[5],
	}
	for name, value := range map[string]string{
		"authority":    identity.authority,
		"tenant_id":    identity.tenantID,
		"workspace_id": identity.workspaceID,
		"object_id":    identity.objectID,
	} {
		if err := validateID(name, value); err != nil {
			return refIdentity{}, err
		}
	}
	if !lowerSHA256(identity.objectID) ||
		parsed.Path != identity.path() ||
		parsed.EscapedPath() != identity.path() ||
		ref.URI != canonicalURI(identity) {
		return refIdentity{}, fmt.Errorf("artifact ref URI is not canonical")
	}
	return identity, nil
}

func validateLocalRef(ref local.ArtifactRef) error {
	if !lowerSHA256(ref.SHA256) || ref.SizeBytes < 0 ||
		ref.URI != "artifact://local/sha256/"+ref.SHA256 {
		return fmt.Errorf("stored content ref is invalid")
	}
	return nil
}

func validateID(name string, value string) error {
	if value == "" || len(value) > 128 || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) || value == "." || value == ".." {
		return fmt.Errorf("%s is invalid", name)
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			strings.ContainsRune("._~-", character) {
			continue
		}
		return fmt.Errorf("%s is invalid", name)
	}
	return nil
}

func (binding mutationBinding) Validate() error {
	if binding.SchemaVersion != mutationSchemaVersion {
		return fmt.Errorf("unsupported mutation binding schema %q", binding.SchemaVersion)
	}
	for name, value := range map[string]string{
		"authority": binding.Authority, "tenant_id": binding.TenantID,
		"workspace_id": binding.WorkspaceID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if binding.Authority != strings.ToLower(binding.Authority) {
		return fmt.Errorf("authority must be lowercase canonical form")
	}
	if !lowerSHA256(binding.ObjectID) {
		return fmt.Errorf("mutation object_id must be a lowercase SHA-256 digest")
	}
	switch binding.Operation {
	case mutationPut:
		if binding.Reason != "" {
			return fmt.Errorf("put mutation must not have a reason")
		}
	case mutationQuarantine, mutationReleaseQuarantine, mutationTombstone:
		if err := validateText("reason", binding.Reason); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported mutation operation %q", binding.Operation)
	}
	return binding.Mutation.Validate()
}

func validateText(name string, value string) error {
	if value == "" || len(value) > 1024 || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) {
		return fmt.Errorf("%s is invalid", name)
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return fmt.Errorf("%s contains control characters", name)
		}
	}
	return nil
}

func sortedUniqueStrings(values []string) bool {
	return slices.IsSorted(values) &&
		!hasAdjacentDuplicate(values) &&
		!slices.ContainsFunc(values, func(value string) bool {
			return validateText("value", value) != nil
		})
}

func hasAdjacentDuplicate[T comparable](values []T) bool {
	for index := 1; index < len(values); index++ {
		if values[index] == values[index-1] {
			return true
		}
	}
	return false
}

func lowerSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func objectID(
	authority string,
	tenantID string,
	workspaceID string,
	contract string,
	digest string,
	size int64,
	uses []Use,
) string {
	payload := fmt.Sprintf(
		"%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%s",
		authority,
		tenantID,
		workspaceID,
		contract,
		digest,
		size,
		strings.Join(useStrings(uses), "\x00"),
	)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func useStrings(uses []Use) []string {
	values := make([]string, len(uses))
	for index, use := range uses {
		values[index] = string(use)
	}
	return values
}
