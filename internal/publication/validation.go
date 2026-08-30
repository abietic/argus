package publication

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func (intent Intent) Validate() error {
	if intent.SchemaVersion != IntentSchemaVersion {
		return fmt.Errorf("unsupported publication intent schema %q", intent.SchemaVersion)
	}
	for name, value := range map[string]string{
		"grant_id": intent.GrantID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	return validateUTC("created_at", intent.CreatedAt)
}

func DecodeIntentJSON(data []byte) (Intent, error) {
	if err := rejectDuplicateJSONFields(data); err != nil {
		return Intent{}, err
	}
	var intent Intent
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&intent); err != nil {
		return Intent{}, fmt.Errorf("decode publication intent: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Intent{}, fmt.Errorf("publication intent contains trailing JSON")
		}
		return Intent{}, fmt.Errorf("decode publication intent trailing JSON: %w", err)
	}
	if err := intent.Validate(); err != nil {
		return Intent{}, err
	}
	return intent, nil
}

func DecodeRequestJSON(data []byte) (Request, error) {
	if err := rejectDuplicateJSONFields(data); err != nil {
		return Request{}, err
	}
	var request Request
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return Request{}, fmt.Errorf("decode publication request: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Request{}, fmt.Errorf("publication request contains trailing JSON")
		}
		return Request{}, fmt.Errorf("decode publication request trailing JSON: %w", err)
	}
	if err := request.Validate(); err != nil {
		return Request{}, err
	}
	return request, nil
}

func (request Request) Validate() error {
	if request.SchemaVersion != RequestSchemaVersion {
		return fmt.Errorf("unsupported publication request schema %q", request.SchemaVersion)
	}
	for name, value := range map[string]string{
		"publication_id":               request.PublicationID,
		"idempotency_key":              request.IdempotencyKey,
		"tenant_id":                    request.TenantID,
		"workspace_id":                 request.WorkspaceID,
		"run_id":                       request.RunID,
		"provider":                     request.Provider,
		"repository_id":                request.RepositoryID,
		"change_id":                    request.ChangeID,
		"base_revision":                request.BaseRevision,
		"expected_head_revision":       request.ExpectedHeadRevision,
		"finding_id":                   request.FindingID,
		"fingerprint":                  request.Fingerprint,
		"decision_id":                  request.DecisionID,
		"grant_id":                     request.GrantID,
		"finding_source_contract":      request.FindingSourceContract,
		"channel":                      request.Channel,
		"expected_permission_revision": request.ExpectedPermissionRevision,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if request.ChangeKind != "pull_request" {
		return fmt.Errorf("unsupported publication change_kind %q", request.ChangeKind)
	}
	if err := validateSHA256("grant_sha256", request.GrantSHA256); err != nil {
		return err
	}
	if request.RunKind != RunKindReview {
		return fmt.Errorf("only a non-replay review run may publish remotely")
	}
	if request.RunStatus != RunStatusSucceeded {
		return fmt.Errorf("only a succeeded review run may publish remotely")
	}
	if request.TargetMode != TargetModeDiff {
		return fmt.Errorf("only an immutable diff target may publish remotely")
	}
	if request.BaseRevision == request.ExpectedHeadRevision {
		return fmt.Errorf("publication diff requires distinct base and head revisions")
	}
	if request.RemoteWrites != "allow" {
		return fmt.Errorf("publication request requires explicit remote_writes allow")
	}
	for name, ref := range map[string]ContentRef{
		"target_snapshot_ref": request.TargetSnapshotRef,
		"config_bundle_ref":   request.ConfigBundleRef,
		"finding_source_ref":  request.FindingSourceRef,
	} {
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	switch request.FindingSourceContract {
	case FindingSourceFindingSet, FindingSourceGovernedReviewReport:
	default:
		return fmt.Errorf("unsupported finding_source_contract %q", request.FindingSourceContract)
	}
	if err := validateSHA256("target_digest", request.TargetDigest); err != nil {
		return err
	}
	if request.Anchor.TargetDigest != request.TargetDigest {
		return fmt.Errorf("anchor target digest does not match the frozen target digest")
	}
	if err := request.Anchor.Validate(); err != nil {
		return err
	}
	if err := validateText("message", request.Message, 16<<10, false); err != nil {
		return err
	}
	if err := validateUTC("grant_expires_at", request.GrantExpiresAt); err != nil {
		return err
	}
	if err := validateUTC("created_at", request.CreatedAt); err != nil {
		return err
	}
	if !request.CreatedAt.Before(request.GrantExpiresAt) {
		return fmt.Errorf("publication request grant is expired")
	}
	return nil
}

func (ref ContentRef) Validate() error {
	if err := validateSHA256("sha256", ref.SHA256); err != nil {
		return err
	}
	if ref.SizeBytes < 0 {
		return fmt.Errorf("size_bytes must not be negative")
	}
	parsed, err := url.Parse(ref.URI)
	if err != nil {
		return fmt.Errorf("parse artifact URI: %w", err)
	}
	if parsed.Scheme != "artifact" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path == "" ||
		path.Clean(parsed.Path) != parsed.Path ||
		strings.Contains(parsed.Path, "/../") || strings.HasSuffix(parsed.Path, "/..") ||
		strings.Contains(parsed.EscapedPath(), "%2f") ||
		strings.Contains(parsed.EscapedPath(), "%2F") {
		return fmt.Errorf("content ref must be an artifact://authority/path URI")
	}
	return nil
}

func (anchor StableAnchor) Validate() error {
	if err := validateRepositoryPath("anchor.path", anchor.Path); err != nil {
		return err
	}
	if anchor.Side != "head" {
		return fmt.Errorf("publication anchor side must be head")
	}
	if anchor.StartLine == 0 || anchor.EndLine < anchor.StartLine {
		return fmt.Errorf("publication anchor line range is invalid")
	}
	return validateSHA256("anchor.target_digest", anchor.TargetDigest)
}

func (observation Revalidation) Validate() error {
	if observation.SchemaVersion != RevalidationSchemaVersion {
		return fmt.Errorf("unsupported publication revalidation schema %q",
			observation.SchemaVersion)
	}
	for name, value := range map[string]string{
		"provider_base_revision": observation.ProviderBaseRevision,
		"provider_head_revision": observation.ProviderHeadRevision,
		"permission_revision":    observation.PermissionRevision,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if err := validateSHA256("config_bundle_sha256", observation.ConfigBundleSHA256); err != nil {
		return err
	}
	if err := validateSHA256("finding_source_sha256", observation.FindingSourceSHA256); err != nil {
		return err
	}
	return validateUTC("checked_at", observation.CheckedAt)
}

func (request ProviderRequest) Validate() error {
	if request.SchemaVersion != ProviderRequestSchema {
		return fmt.Errorf("unsupported provider request schema %q", request.SchemaVersion)
	}
	for name, value := range map[string]string{
		"publication_id":      request.PublicationID,
		"idempotency_key":     request.IdempotencyKey,
		"grant_id":            request.GrantID,
		"provider":            request.Provider,
		"repository_id":       request.RepositoryID,
		"change_id":           request.ChangeID,
		"base_revision":       request.BaseRevision,
		"head_revision":       request.HeadRevision,
		"finding_id":          request.FindingID,
		"fingerprint":         request.Fingerprint,
		"decision_id":         request.DecisionID,
		"permission_revision": request.PermissionRevision,
		"channel":             request.Channel,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if request.ChangeKind != "pull_request" {
		return fmt.Errorf("unsupported provider publication change_kind %q", request.ChangeKind)
	}
	if err := request.Anchor.Validate(); err != nil {
		return err
	}
	if err := validateText("message", request.Message, 16<<10, false); err != nil {
		return err
	}
	return validateSHA256("request_sha256", request.RequestSHA256)
}

func (result ProviderResult) Validate() error {
	if result.SchemaVersion != ProviderResultSchema {
		return fmt.Errorf("unsupported provider result schema %q", result.SchemaVersion)
	}
	if err := validateID("idempotency_key", result.IdempotencyKey); err != nil {
		return err
	}
	switch result.Status {
	case ProviderResultPublished:
		if err := validateID("provider_request_id", result.ProviderRequestID); err != nil {
			return err
		}
		if err := validateID("comment_id", result.CommentID); err != nil {
			return err
		}
		if result.ReasonCode != "" {
			return fmt.Errorf("published result must not contain reason_code")
		}
	case ProviderResultRejected:
		if err := validateID("reason_code", result.ReasonCode); err != nil {
			return err
		}
		if result.CommentID != "" {
			return fmt.Errorf("rejected result must not contain comment_id")
		}
	case ProviderResultUnknown:
		if err := validateID("reason_code", result.ReasonCode); err != nil {
			return err
		}
		if result.CommentID != "" {
			return fmt.Errorf("unknown result must not contain comment_id")
		}
	case ProviderResultNotFound:
		if result.CommentID != "" || result.ReasonCode != "" {
			return fmt.Errorf("not_found result must not contain comment_id or reason_code")
		}
	default:
		return fmt.Errorf("unsupported provider result status %q", result.Status)
	}
	return validateUTC("observed_at", result.ObservedAt)
}

func (event LedgerEvent) Validate() error {
	if event.SchemaVersion != LedgerEventSchemaVersion {
		return fmt.Errorf("unsupported publication ledger schema %q", event.SchemaVersion)
	}
	for name, value := range map[string]string{
		"event_id":       event.EventID,
		"publication_id": event.PublicationID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if err := validateSHA256("request_sha256", event.RequestSHA256); err != nil {
		return err
	}
	if err := validateUTC("occurred_at", event.OccurredAt); err != nil {
		return err
	}
	switch event.Type {
	case EventRequested:
		if event.Request == nil || event.Revalidation != nil ||
			event.ProviderRequest != nil || event.ProviderResult != nil ||
			event.ReasonCode != "" {
			return fmt.Errorf("requested event has an invalid payload shape")
		}
		if err := event.Request.Validate(); err != nil {
			return err
		}
	case EventDispatchStarted:
		if event.Request != nil || event.Revalidation == nil ||
			event.ProviderRequest == nil || event.ProviderResult != nil ||
			event.ReasonCode != "" {
			return fmt.Errorf("dispatch_started event has an invalid payload shape")
		}
		if err := event.Revalidation.Validate(); err != nil {
			return err
		}
		if err := event.ProviderRequest.Validate(); err != nil {
			return err
		}
	case EventPublished, EventRejected, EventOutcomeUnknown,
		EventReconciledPublished, EventReconciledNotFound:
		if event.Request != nil || event.Revalidation != nil ||
			event.ProviderRequest != nil || event.ProviderResult == nil {
			return fmt.Errorf("%s event has an invalid payload shape", event.Type)
		}
		if err := event.ProviderResult.Validate(); err != nil {
			return err
		}
		want := map[EventType]ProviderResultStatus{
			EventPublished:           ProviderResultPublished,
			EventRejected:            ProviderResultRejected,
			EventOutcomeUnknown:      ProviderResultUnknown,
			EventReconciledPublished: ProviderResultPublished,
			EventReconciledNotFound:  ProviderResultNotFound,
		}[event.Type]
		if event.ProviderResult.Status != want {
			return fmt.Errorf("%s event contains provider status %q", event.Type,
				event.ProviderResult.Status)
		}
	default:
		return fmt.Errorf("unsupported publication event type %q", event.Type)
	}
	return nil
}

func DigestRequest(request Request) (string, error) {
	if err := request.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("marshal publication request: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func decodeEvent(data []byte) (LedgerEvent, error) {
	if err := rejectDuplicateJSONFields(data); err != nil {
		return LedgerEvent{}, err
	}
	var event LedgerEvent
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&event); err != nil {
		return LedgerEvent{}, fmt.Errorf("decode publication event: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return LedgerEvent{}, fmt.Errorf("publication event contains trailing JSON")
		}
		return LedgerEvent{}, fmt.Errorf("decode publication event trailing JSON: %w", err)
	}
	if err := event.Validate(); err != nil {
		return LedgerEvent{}, err
	}
	return event, nil
}

func rejectDuplicateJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return fmt.Errorf("validate publication JSON: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("validate publication JSON: trailing JSON value")
		}
		return fmt.Errorf("validate publication JSON: %w", err)
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate object field %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("object is not closed")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("array is not closed")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}

func validateID(name, value string) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 512 ||
		!utf8.ValidString(value) {
		return fmt.Errorf("%s is invalid", name)
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return fmt.Errorf("%s is invalid", name)
		}
	}
	return nil
}

func validateText(name, value string, max int, allowEmpty bool) error {
	if (!allowEmpty && value == "") || value != strings.TrimSpace(value) ||
		len(value) > max || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return fmt.Errorf("%s is invalid", name)
	}
	return nil
}

func validateSHA256(name, value string) error {
	if len(value) != sha256.Size*2 {
		return fmt.Errorf("%s must be a lowercase SHA-256", name)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || hex.EncodeToString(decoded) != value {
		return fmt.Errorf("%s must be a lowercase SHA-256", name)
	}
	return nil
}

func validateRepositoryPath(name, value string) error {
	if err := validateText(name, value, 4096, false); err != nil {
		return err
	}
	if value == "." || strings.HasPrefix(value, "/") || path.Clean(value) != value ||
		strings.Contains(value, "\\") || strings.Contains(value, ":") {
		return fmt.Errorf("%s must be a portable repository-relative path", name)
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("%s must be a portable repository-relative path", name)
		}
	}
	return nil
}

func validateUTC(name string, value time.Time) error {
	if value.IsZero() || value.Location() != time.UTC {
		return fmt.Errorf("%s must be a non-zero UTC timestamp", name)
	}
	return nil
}
