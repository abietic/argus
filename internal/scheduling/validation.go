package scheduling

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func (policy Policy) Validate() error {
	if policy.SchemaVersion != PolicySchemaVersion {
		return fmt.Errorf("unsupported scheduling policy schema %q", policy.SchemaVersion)
	}
	if err := validateID("revision", policy.Revision); err != nil {
		return err
	}
	if policy.GlobalQueueLimit < 1 || policy.GlobalActiveLimit < 1 {
		return fmt.Errorf("global queue and active limits must be positive")
	}
	for name, duration := range map[string]time.Duration{
		"aging_interval":    policy.AgingInterval,
		"admission_timeout": policy.AdmissionTimeout,
		"lease_duration":    policy.LeaseDuration,
		"unknown_timeout":   policy.UnknownTimeout,
	} {
		if duration <= 0 {
			return fmt.Errorf("%s must be positive", name)
		}
	}
	if len(policy.Classes) != len(workloadClassOrder) {
		return fmt.Errorf("classes must define all four workload classes exactly once")
	}
	totalPool := 0
	for index, expected := range workloadClassOrder {
		class := policy.Classes[index]
		if class.Class != expected {
			return fmt.Errorf("classes[%d] is %q, want %q", index, class.Class, expected)
		}
		if class.PoolLimit < 1 || class.QueueLimit < 1 {
			return fmt.Errorf("classes[%d] pool and queue limits must be positive", index)
		}
		if class.QueueLimit > policy.GlobalQueueLimit {
			return fmt.Errorf("classes[%d] queue limit exceeds global queue limit", index)
		}
		if class.BasePriority < -1000 || class.BasePriority > 1000 {
			return fmt.Errorf("classes[%d] base priority is out of range", index)
		}
		totalPool += class.PoolLimit
	}
	if totalPool > policy.GlobalActiveLimit {
		return fmt.Errorf("sum of class pools exceeds global active limit")
	}
	if policy.DefaultTenantQuota.TenantID != "" {
		return fmt.Errorf("default tenant quota must have an empty tenant_id")
	}
	if err := validateQuota(policy.DefaultTenantQuota, false); err != nil {
		return fmt.Errorf("default_tenant_quota: %w", err)
	}
	for index, quota := range policy.TenantQuotas {
		if err := validateQuota(quota, true); err != nil {
			return fmt.Errorf("tenant_quotas[%d]: %w", index, err)
		}
		if index > 0 && policy.TenantQuotas[index-1].TenantID >= quota.TenantID {
			return fmt.Errorf("tenant_quotas must be sorted and unique")
		}
	}
	return nil
}

func PolicyDigest(policy Policy) (string, error) {
	if err := policy.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(policy)
	if err != nil {
		return "", fmt.Errorf("marshal scheduling policy: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (spec WorkloadSpec) Validate() error {
	if spec.SchemaVersion != WorkloadSchemaVersion {
		return fmt.Errorf("unsupported workload schema %q", spec.SchemaVersion)
	}
	for name, value := range map[string]string{
		"workload_id": spec.WorkloadID,
		"run_id":      spec.RunID,
		"tenant_id":   spec.TenantID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if err := spec.Class.Validate(); err != nil {
		return err
	}
	if spec.Priority < -100 || spec.Priority > 100 {
		return fmt.Errorf("priority must be between -100 and 100")
	}
	if err := validateArtifactRef("input_ref", spec.InputRef); err != nil {
		return err
	}
	if !isUTC(spec.SubmittedAt) || !isUTC(spec.ExecutionDeadline) {
		return fmt.Errorf("submitted_at and execution_deadline must be non-zero UTC timestamps")
	}
	if !spec.ExecutionDeadline.After(spec.SubmittedAt) {
		return fmt.Errorf("execution_deadline must be after submitted_at")
	}
	return nil
}

func (fact AdmissionFact) Validate() error {
	if err := fact.Decision.Validate(); err != nil {
		return err
	}
	switch fact.Reason {
	case ReasonCapacityAvailable, ReasonCapacityBusy, ReasonRunCanceled,
		ReasonGlobalQueueFull, ReasonClassQueueFull, ReasonTenantQueueFull:
	default:
		return fmt.Errorf("unsupported admission reason %q", fact.Reason)
	}
	if fact.GlobalQueueDepth < 0 || fact.ClassQueueDepth < 0 ||
		fact.TenantQueueDepth < 0 {
		return fmt.Errorf("queue depths must not be negative")
	}
	if !isUTC(fact.AdmissionDeadline) || !isUTC(fact.RecordedAt) {
		return fmt.Errorf("admission timestamps must be non-zero UTC")
	}
	if fact.AdmissionDeadline.Before(fact.RecordedAt) {
		return fmt.Errorf("admission deadline must not precede recorded_at")
	}
	return nil
}

func (lease DispatchLease) Validate() error {
	for name, value := range map[string]string{
		"lease_id": lease.LeaseID, "workload_id": lease.WorkloadID,
		"worker_id": lease.WorkerID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if lease.Attempt < 1 || lease.Generation < 1 || lease.FencingToken == 0 {
		return fmt.Errorf("attempt, generation, and fencing_token must be positive")
	}
	if !isUTC(lease.AcquiredAt) || !isUTC(lease.LastHeartbeat) || !isUTC(lease.ExpiresAt) {
		return fmt.Errorf("lease timestamps must be non-zero UTC")
	}
	if lease.LastHeartbeat.Before(lease.AcquiredAt) ||
		!lease.ExpiresAt.After(lease.LastHeartbeat) {
		return fmt.Errorf("lease heartbeat and expiry ordering is invalid")
	}
	return nil
}

func (request ClaimRequest) Validate() error {
	if err := validateID("idempotency_key", request.IdempotencyKey); err != nil {
		return err
	}
	if request.WorkloadID != "" {
		if err := validateID("workload_id", request.WorkloadID); err != nil {
			return err
		}
	}
	if err := validateID("worker_id", request.WorkerID); err != nil {
		return err
	}
	if len(request.SupportedClasses) == 0 {
		return fmt.Errorf("supported_classes must be non-empty")
	}
	previous := -1
	for index, class := range request.SupportedClasses {
		if err := class.Validate(); err != nil {
			return fmt.Errorf("supported_classes[%d]: %w", index, err)
		}
		rank := classRank(class)
		if rank <= previous {
			return fmt.Errorf("supported_classes must use canonical order and be unique")
		}
		previous = rank
	}
	if !isUTC(request.At) {
		return fmt.Errorf("claim time must be a non-zero UTC timestamp")
	}
	return nil
}

func (heartbeat Heartbeat) Validate() error {
	for name, value := range map[string]string{
		"idempotency_key": heartbeat.IdempotencyKey,
		"lease_id":        heartbeat.LeaseID,
		"workload_id":     heartbeat.WorkloadID,
		"worker_id":       heartbeat.WorkerID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if heartbeat.Attempt < 1 || heartbeat.Generation < 1 ||
		heartbeat.FencingToken == 0 {
		return fmt.Errorf("attempt, generation, and fencing_token must be positive")
	}
	if !isUTC(heartbeat.At) {
		return fmt.Errorf("heartbeat time must be a non-zero UTC timestamp")
	}
	return nil
}

func (callback Callback) Validate() error {
	if callback.SchemaVersion != CallbackSchemaVersion {
		return fmt.Errorf("unsupported callback schema %q", callback.SchemaVersion)
	}
	for name, value := range map[string]string{
		"idempotency_key": callback.IdempotencyKey,
		"lease_id":        callback.LeaseID,
		"workload_id":     callback.WorkloadID,
		"worker_id":       callback.WorkerID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if callback.Attempt < 1 || callback.Generation < 1 || callback.FencingToken == 0 {
		return fmt.Errorf("attempt, generation, and fencing_token must be positive")
	}
	if err := callback.Status.Validate(); err != nil {
		return err
	}
	if callback.OutputRefs == nil {
		return fmt.Errorf("output_refs must be an explicit array")
	}
	for index, ref := range callback.OutputRefs {
		if err := validateArtifactRef(fmt.Sprintf("output_refs[%d]", index), ref); err != nil {
			return err
		}
		if index > 0 && callback.OutputRefs[index-1] >= ref {
			return fmt.Errorf("output_refs must be sorted and unique")
		}
	}
	switch callback.Status {
	case CallbackSucceeded:
		if len(callback.OutputRefs) == 0 || callback.FailureCode != "" {
			return fmt.Errorf("succeeded callback requires output and forbids failure_code")
		}
	case CallbackFailed:
		if len(callback.OutputRefs) != 0 {
			return fmt.Errorf("failed callback forbids output")
		}
		if err := validateID("failure_code", callback.FailureCode); err != nil {
			return err
		}
	case CallbackUnknown:
		if len(callback.OutputRefs) != 0 || callback.FailureCode != "" {
			return fmt.Errorf("unknown callback forbids output and failure_code")
		}
	}
	if !isUTC(callback.OccurredAt) {
		return fmt.Errorf("occurred_at must be a non-zero UTC timestamp")
	}
	return nil
}

func (mutation Mutation) Validate() error {
	if err := validateID("idempotency_key", mutation.IdempotencyKey); err != nil {
		return err
	}
	if err := validateText("actor", mutation.Actor, 256, false); err != nil {
		return err
	}
	if err := validateText("audit", mutation.Audit, 4096, true); err != nil {
		return err
	}
	if !isUTC(mutation.At) {
		return fmt.Errorf("mutation time must be a non-zero UTC timestamp")
	}
	return nil
}

func (class WorkloadClass) Validate() error {
	if slices.Contains(workloadClassOrder[:], class) {
		return nil
	}
	return fmt.Errorf("unsupported workload class %q", class)
}

func (decision AdmissionDecision) Validate() error {
	switch decision {
	case AdmissionAdmitted, AdmissionQueued, AdmissionRejected, AdmissionThrottled:
		return nil
	default:
		return fmt.Errorf("unsupported admission decision %q", decision)
	}
}

func (status CallbackStatus) Validate() error {
	switch status {
	case CallbackSucceeded, CallbackFailed, CallbackUnknown:
		return nil
	default:
		return fmt.Errorf("unsupported callback status %q", status)
	}
}

func validateQuota(quota TenantQuota, requireID bool) error {
	if requireID {
		if err := validateID("tenant_id", quota.TenantID); err != nil {
			return err
		}
	}
	if quota.MaxQueued < 1 || quota.MaxActive < 1 {
		return fmt.Errorf("max_queued and max_active must be positive")
	}
	if quota.PriorityBias < -1000 || quota.PriorityBias > 1000 {
		return fmt.Errorf("priority_bias is out of range")
	}
	return nil
}

func (policy Policy) classPolicy(class WorkloadClass) ClassPolicy {
	return policy.Classes[classRank(class)]
}

func (policy Policy) tenantQuota(tenantID string) TenantQuota {
	index, found := slices.BinarySearchFunc(
		policy.TenantQuotas,
		tenantID,
		func(quota TenantQuota, id string) int {
			return strings.Compare(quota.TenantID, id)
		},
	)
	if found {
		return policy.TenantQuotas[index]
	}
	result := policy.DefaultTenantQuota
	result.TenantID = tenantID
	return result
}

func classRank(class WorkloadClass) int {
	for index, candidate := range workloadClassOrder {
		if candidate == class {
			return index
		}
	}
	return -1
}

func minTime(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}

func isUTC(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}

func validateArtifactRef(name, value string) error {
	if value == "" || len(value) > 2048 || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) {
		return fmt.Errorf("%s is invalid", name)
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("%s: parse artifact reference: %w", name, err)
	}
	if parsed.Scheme != "artifact" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Path == "" || parsed.Path == "/" ||
		path.Clean(parsed.Path) != parsed.Path ||
		parsed.EscapedPath() != parsed.Path {
		return fmt.Errorf("%s must use a safe artifact://authority/path reference", name)
	}
	return nil
}

func validateID(name, value string) error {
	if value == "" || len(value) > 256 || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) {
		return fmt.Errorf("%s must be non-empty, trimmed UTF-8 within 256 bytes", name)
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) ||
			!(unicode.IsLetter(character) || unicode.IsDigit(character) ||
				strings.ContainsRune("._~:/@+-", character)) {
			return fmt.Errorf("%s contains unsafe characters", name)
		}
	}
	return nil
}

func validateText(name, value string, maxBytes int, allowSpace bool) error {
	if value == "" || len(value) > maxBytes || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) {
		return fmt.Errorf("%s must be non-empty, trimmed UTF-8 within %d bytes", name, maxBytes)
	}
	for _, character := range value {
		if unicode.IsControl(character) || !allowSpace && unicode.IsSpace(character) {
			return fmt.Errorf("%s contains unsafe characters", name)
		}
	}
	return nil
}
