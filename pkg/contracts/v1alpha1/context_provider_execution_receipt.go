package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"time"
)

const ContextProviderExecutionReceiptSchemaVersion = "argus.context_provider_execution_receipt.v1alpha1"

type ContextProviderReceiptStatus string

const (
	ContextProviderReceiptSucceeded ContextProviderReceiptStatus = "succeeded"
	ContextProviderReceiptGap       ContextProviderReceiptStatus = "gap"
	ContextProviderRevisionMismatch                              = "context_revision_mismatch"
)

var contextProviderExactCommit = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

type ContextProviderTargetRange struct {
	Path      string `json:"path"`
	StartLine uint32 `json:"start_line"`
	EndLine   uint32 `json:"end_line"`
}

type ContextProviderExecutionReceipt struct {
	SchemaVersion    string                       `json:"schema_version"`
	ReceiptID        string                       `json:"receipt_id"`
	SHA256           string                       `json:"sha256"`
	ProviderID       string                       `json:"provider_id"`
	ProviderRevision string                       `json:"provider_revision"`
	Kind             string                       `json:"kind"`
	Adapter          VersionedRef                 `json:"adapter"`
	RequestSHA256    string                       `json:"request_sha256"`
	RepositoryID     string                       `json:"repository_id"`
	CommitOID        string                       `json:"commit_oid"`
	ObservedRevision string                       `json:"observed_revision"`
	TargetPaths      []string                     `json:"target_paths"`
	TargetRanges     []ContextProviderTargetRange `json:"target_ranges"`
	Status           ContextProviderReceiptStatus `json:"status"`
	ReasonCode       string                       `json:"reason_code"`
	ContextID        string                       `json:"context_id"`
	ContextDigest    string                       `json:"context_digest"`
	ContextContract  string                       `json:"context_contract"`
	TimeoutMS        int64                        `json:"timeout_ms"`
	StartedAt        time.Time                    `json:"started_at"`
	CompletedAt      time.Time                    `json:"completed_at"`
	DurationMS       int64                        `json:"duration_ms"`
	Authority        string                       `json:"authority"`
}

func DecodeContextProviderExecutionReceipt(data []byte) (ContextProviderExecutionReceipt, error) {
	var receipt ContextProviderExecutionReceipt
	if err := decodeAgentReviewShadowJSON(data, &receipt); err != nil {
		return ContextProviderExecutionReceipt{}, fmt.Errorf("decode ContextProviderExecutionReceipt: %w", err)
	}
	if err := receipt.Validate(); err != nil {
		return ContextProviderExecutionReceipt{}, err
	}
	return receipt, nil
}

func SealContextProviderExecutionReceipt(
	receipt ContextProviderExecutionReceipt,
) (ContextProviderExecutionReceipt, error) {
	receipt.SchemaVersion = ContextProviderExecutionReceiptSchemaVersion
	receipt.ReceiptID = ""
	receipt.SHA256 = ""
	digest, err := DigestContextProviderExecutionReceipt(receipt)
	if err != nil {
		return ContextProviderExecutionReceipt{}, err
	}
	receipt.ReceiptID = "context-provider-receipt-" + digest[:24]
	receipt.SHA256 = digest
	if err := receipt.Validate(); err != nil {
		return ContextProviderExecutionReceipt{}, err
	}
	return receipt, nil
}

func DigestContextProviderExecutionReceipt(receipt ContextProviderExecutionReceipt) (string, error) {
	receipt.ReceiptID = ""
	receipt.SHA256 = ""
	data, err := json.Marshal(receipt)
	if err != nil {
		return "", fmt.Errorf("marshal context provider execution receipt: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (receipt ContextProviderExecutionReceipt) Validate() error {
	if receipt.SchemaVersion != ContextProviderExecutionReceiptSchemaVersion {
		return fmt.Errorf("unsupported ContextProviderExecutionReceipt schema %q", receipt.SchemaVersion)
	}
	for name, value := range map[string]string{
		"receipt_id": receipt.ReceiptID, "provider_id": receipt.ProviderID,
		"provider_revision": receipt.ProviderRevision, "kind": receipt.Kind,
		"repository_id": receipt.RepositoryID, "context_id": receipt.ContextID,
		"authority": receipt.Authority,
	} {
		if err := requireIdentifier(name, value); err != nil {
			return err
		}
	}
	if receipt.Authority != "local_host_observation" {
		return fmt.Errorf("unsupported context provider receipt authority %q", receipt.Authority)
	}
	if err := receipt.Adapter.validate("adapter"); err != nil {
		return err
	}
	if err := requireSHA256("request_sha256", receipt.RequestSHA256); err != nil {
		return err
	}
	if err := requireSHA256("context_digest", receipt.ContextDigest); err != nil {
		return err
	}
	if !contextProviderExactCommit.MatchString(receipt.CommitOID) {
		return fmt.Errorf("commit_oid must be an exact lowercase Git object ID")
	}
	if receipt.TargetPaths == nil || !slices.IsSorted(receipt.TargetPaths) {
		return fmt.Errorf("target_paths must be an explicit sorted array")
	}
	for index, value := range receipt.TargetPaths {
		if err := requireRepositoryPath(fmt.Sprintf("target_paths[%d]", index), value); err != nil {
			return err
		}
		if index > 0 && value == receipt.TargetPaths[index-1] {
			return fmt.Errorf("target_paths contains duplicate %q", value)
		}
	}
	if receipt.TargetRanges == nil {
		return fmt.Errorf("target_ranges must be an explicit array")
	}
	targets := make(map[string]struct{}, len(receipt.TargetPaths))
	for _, value := range receipt.TargetPaths {
		targets[value] = struct{}{}
	}
	previousRange := ContextProviderTargetRange{}
	for index, targetRange := range receipt.TargetRanges {
		if err := requireRepositoryPath(fmt.Sprintf("target_ranges[%d].path", index), targetRange.Path); err != nil {
			return err
		}
		if _, exists := targets[targetRange.Path]; !exists || targetRange.StartLine == 0 || targetRange.EndLine < targetRange.StartLine {
			return fmt.Errorf("target_ranges[%d] must bind a valid target path and line range", index)
		}
		if index > 0 && !contextProviderTargetRangeLess(previousRange, targetRange) {
			return fmt.Errorf("target_ranges must be uniquely sorted")
		}
		previousRange = targetRange
	}
	switch receipt.Status {
	case ContextProviderReceiptSucceeded:
		if receipt.ReasonCode != "" || receipt.ContextContract == "" ||
			receipt.ObservedRevision != receipt.CommitOID {
			return fmt.Errorf("succeeded receipt requires exact observed revision, context contract, and no reason code")
		}
	case ContextProviderReceiptGap:
		if err := requireIdentifier("reason_code", receipt.ReasonCode); err != nil {
			return err
		}
		if receipt.ContextContract != "" {
			return fmt.Errorf("gap receipt must not claim a context contract")
		}
		if receipt.ReasonCode == ContextProviderRevisionMismatch {
			if !contextProviderExactCommit.MatchString(receipt.ObservedRevision) ||
				receipt.ObservedRevision == receipt.CommitOID {
				return fmt.Errorf("revision mismatch gap requires a distinct exact observed revision")
			}
		} else if receipt.ObservedRevision != "" {
			return fmt.Errorf("non-revision gap must not claim an observed revision")
		}
	default:
		return fmt.Errorf("unsupported context provider receipt status %q", receipt.Status)
	}
	if receipt.TimeoutMS <= 0 || receipt.DurationMS < 0 {
		return fmt.Errorf("receipt timeout/duration is invalid")
	}
	if err := validateAgentReviewTime("started_at", receipt.StartedAt); err != nil {
		return err
	}
	if err := validateAgentReviewTime("completed_at", receipt.CompletedAt); err != nil {
		return err
	}
	if receipt.CompletedAt.Before(receipt.StartedAt) || receipt.CompletedAt.Sub(receipt.StartedAt).Milliseconds() != receipt.DurationMS {
		return fmt.Errorf("receipt duration does not match timestamps")
	}
	digest, err := DigestContextProviderExecutionReceipt(receipt)
	if err != nil {
		return err
	}
	if receipt.SHA256 != digest || receipt.ReceiptID != "context-provider-receipt-"+digest[:24] {
		return fmt.Errorf("context provider receipt identity does not match content")
	}
	return nil
}

func contextProviderTargetRangeLess(left, right ContextProviderTargetRange) bool {
	if left.Path != right.Path {
		return left.Path < right.Path
	}
	if left.StartLine != right.StartLine {
		return left.StartLine < right.StartLine
	}
	return left.EndLine < right.EndLine
}
