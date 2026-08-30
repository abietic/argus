package training

import (
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/abietic/argus/internal/runmodel"
)

func validSHA256(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (ref TrainingComponentRef) Validate() error {
	for _, field := range []struct{ name, value string }{{"base_model.id", ref.ID}, {"base_model.revision", ref.Revision}} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if !validSHA256(ref.SHA256) {
		return fmt.Errorf("base_model.sha256 must be lowercase SHA-256")
	}
	return nil
}

func (parameters JobHyperparameters) Validate() error {
	if parameters.Epochs == 0 || parameters.Epochs > 100 {
		return fmt.Errorf("epochs must be in [1,100]")
	}
	if parameters.BatchSize == 0 || parameters.BatchSize > 65536 {
		return fmt.Errorf("batch_size must be in [1,65536]")
	}
	if parameters.LearningRateMicros == 0 || parameters.LearningRateMicros > 1_000_000_000 {
		return fmt.Errorf("learning_rate_micros must be in [1,1000000000]")
	}
	if parameters.Seed > 9_007_199_254_740_991 {
		return fmt.Errorf("seed exceeds portable integer range")
	}
	return nil
}

func (request JobPrepareRequest) Validate() error {
	if request.SchemaVersion != JobPrepareRequestSchemaVersion {
		return fmt.Errorf("unsupported training job prepare request schema %q", request.SchemaVersion)
	}
	for _, field := range []struct{ name, value string }{
		{"job_id", request.JobID}, {"export_id", request.ExportID}, {"provider_id", request.ProviderID},
		{"provider_profile_revision", request.ProviderProfileRevision},
	} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if err := request.ExportBundleRef.Validate(); err != nil {
		return fmt.Errorf("export_bundle_ref: %w", err)
	}
	if request.ExportBundleRef.Contract != runmodel.ContractTrainingExportBundle {
		return fmt.Errorf("export_bundle_ref contract must be %q", runmodel.ContractTrainingExportBundle)
	}
	if err := request.BaseModel.Validate(); err != nil {
		return err
	}
	if request.Objective != TrainingObjectiveSFT {
		return fmt.Errorf("objective must be %q", TrainingObjectiveSFT)
	}
	if err := request.Hyperparameters.Validate(); err != nil {
		return err
	}
	if request.CreatedAt.IsZero() || request.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("created_at must be non-zero UTC")
	}
	return nil
}

func sealProviderJobReceipt(receipt ProviderJobReceipt) (ProviderJobReceipt, error) {
	receipt.SchemaVersion = ProviderJobReceiptSchemaVersion
	receipt.SHA256 = ""
	digest, err := runmodel.DigestJSON(receipt)
	if err != nil {
		return ProviderJobReceipt{}, err
	}
	receipt.SHA256 = digest
	if err := receipt.Validate(); err != nil {
		return ProviderJobReceipt{}, err
	}
	return receipt, nil
}

func (receipt ProviderJobReceipt) Validate() error {
	if receipt.SchemaVersion != ProviderJobReceiptSchemaVersion {
		return fmt.Errorf("unsupported provider job receipt schema %q", receipt.SchemaVersion)
	}
	for _, field := range []struct{ name, value string }{{"provider_id", receipt.ProviderID}, {"external_job_id", receipt.ExternalJobID}} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if receipt.Status != JobStatusSubmitted && receipt.Status != JobStatusSucceeded && receipt.Status != JobStatusFailed && receipt.Status != JobStatusCanceled {
		return fmt.Errorf("invalid provider job receipt status %q", receipt.Status)
	}
	if receipt.Status == JobStatusSucceeded {
		if err := validateID("output_model_id", receipt.OutputModelID); err != nil {
			return err
		}
	} else if receipt.OutputModelID != "" {
		return fmt.Errorf("only succeeded receipt may have output_model_id")
	}
	if receipt.Authority != TrainingReceiptAuthority || receipt.ContainsSecret {
		return fmt.Errorf("provider receipt safety declarations are invalid")
	}
	if receipt.ObservedAt.IsZero() || receipt.ObservedAt.Location() != time.UTC {
		return fmt.Errorf("observed_at must be non-zero UTC")
	}
	copy := receipt
	copy.SHA256 = ""
	digest, err := runmodel.DigestJSON(copy)
	if err != nil {
		return err
	}
	if receipt.SHA256 != digest {
		return fmt.Errorf("provider job receipt SHA-256 does not match content")
	}
	return nil
}

func (request JobObservationRequest) Validate() error {
	if request.SchemaVersion != JobObservationRequestSchemaVersion {
		return fmt.Errorf("unsupported training job observation request schema %q", request.SchemaVersion)
	}
	if err := validateID("job_id", request.JobID); err != nil {
		return err
	}
	if !validSHA256(request.ExpectedPlanSHA256) {
		return fmt.Errorf("expected_plan_sha256 must be lowercase SHA-256")
	}
	if err := request.Receipt.Validate(); err != nil {
		return fmt.Errorf("receipt: %w", err)
	}
	if request.ObservedAt.IsZero() || request.ObservedAt.Location() != time.UTC || !request.ObservedAt.Equal(request.Receipt.ObservedAt) {
		return fmt.Errorf("observed_at must be non-zero UTC and equal receipt observed_at")
	}
	return nil
}

func sealJobPlan(plan JobPlan) (JobPlan, error) {
	plan.SchemaVersion = JobPlanSchemaVersion
	plan.SHA256 = ""
	digest, err := runmodel.DigestJSON(plan)
	if err != nil {
		return JobPlan{}, err
	}
	plan.SHA256 = digest
	if err := plan.Validate(); err != nil {
		return JobPlan{}, err
	}
	return plan, nil
}

func (plan JobPlan) Validate() error {
	if plan.SchemaVersion != JobPlanSchemaVersion {
		return fmt.Errorf("unsupported training job plan schema %q", plan.SchemaVersion)
	}
	if err := plan.Request.Validate(); err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if !validSHA256(plan.ExportBundleSHA256) {
		return fmt.Errorf("export bundle digest binding is invalid")
	}
	if plan.PortableFormat != PortableRecordsFormat || plan.ExecutionMode != TrainingExecutionExternalManual || plan.RemoteSideEffects != "deny" || plan.ReceiptAuthority != TrainingReceiptAuthority || plan.PromotionEligible {
		return fmt.Errorf("training job safety declarations are invalid")
	}
	if plan.Observations == nil || len(plan.Observations) > 2 {
		return fmt.Errorf("observations must be an explicit array with at most two items")
	}
	if err := validateID("created_by", plan.CreatedBy); err != nil {
		return err
	}
	if err := validateID("updated_by", plan.UpdatedBy); err != nil {
		return err
	}
	if plan.UpdatedAt.IsZero() || plan.UpdatedAt.Location() != time.UTC || plan.UpdatedAt.Before(plan.Request.CreatedAt) {
		return fmt.Errorf("updated_at must be UTC and not precede creation")
	}
	wantStatus := JobStatusPrepared
	externalJobID := ""
	previousAt := plan.Request.CreatedAt
	for index, observation := range plan.Observations {
		if err := observation.Receipt.Validate(); err != nil {
			return fmt.Errorf("observations[%d]: %w", index, err)
		}
		if err := observation.ReceiptRef.Validate(); err != nil || observation.ReceiptRef.Contract != runmodel.ContractTrainingProviderJobReceipt {
			return fmt.Errorf("observations[%d] receipt ref binding is invalid", index)
		}
		if observation.Receipt.ProviderID != plan.Request.ProviderID || observation.RecordedAt.Location() != time.UTC || !observation.RecordedAt.Equal(observation.Receipt.ObservedAt) || observation.RecordedAt.Before(previousAt) {
			return fmt.Errorf("observations[%d] provider/time binding is invalid", index)
		}
		if err := validateID("recorded_by", observation.RecordedBy); err != nil {
			return err
		}
		if index == 0 {
			if observation.Receipt.Status != JobStatusSubmitted {
				return fmt.Errorf("first observation must be submitted")
			}
			externalJobID = observation.Receipt.ExternalJobID
		} else if wantStatus != JobStatusSubmitted || observation.Receipt.Status == JobStatusSubmitted || observation.Receipt.ExternalJobID != externalJobID {
			return fmt.Errorf("terminal observation transition is invalid")
		}
		wantStatus = observation.Receipt.Status
		previousAt = observation.RecordedAt
	}
	if plan.Status != wantStatus || !plan.UpdatedAt.Equal(previousAt) {
		return fmt.Errorf("job status/update time does not match observations")
	}
	copy := plan
	copy.SHA256 = ""
	digest, err := runmodel.DigestJSON(copy)
	if err != nil {
		return err
	}
	if plan.SHA256 != digest {
		return fmt.Errorf("training job plan SHA-256 does not match content")
	}
	return nil
}

func DecodeJobPrepareRequest(data []byte) (JobPrepareRequest, error) {
	var value JobPrepareRequest
	if err := decodeStrict(data, &value); err != nil {
		return value, fmt.Errorf("decode training job prepare request: %w", err)
	}
	return value, value.Validate()
}

func DecodeProviderJobReceipt(data []byte) (ProviderJobReceipt, error) {
	var value ProviderJobReceipt
	if err := decodeStrict(data, &value); err != nil {
		return value, fmt.Errorf("decode provider job receipt: %w", err)
	}
	return value, value.Validate()
}

func DecodeJobObservationRequest(data []byte) (JobObservationRequest, error) {
	var value JobObservationRequest
	if err := decodeStrict(data, &value); err != nil {
		return value, fmt.Errorf("decode training job observation request: %w", err)
	}
	return value, value.Validate()
}

func DecodeJobPlan(data []byte) (JobPlan, error) {
	var value JobPlan
	if err := decodeStrict(data, &value); err != nil {
		return value, fmt.Errorf("decode training job plan: %w", err)
	}
	return value, value.Validate()
}

func jobPlanExtends(previous, next JobPlan) bool {
	if !reflect.DeepEqual(previous.Request, next.Request) || previous.ExportBundleSHA256 != next.ExportBundleSHA256 || previous.PortableFormat != next.PortableFormat || previous.ExecutionMode != next.ExecutionMode || previous.RemoteSideEffects != next.RemoteSideEffects || previous.ReceiptAuthority != next.ReceiptAuthority || previous.PromotionEligible != next.PromotionEligible || previous.CreatedBy != next.CreatedBy || len(next.Observations) != len(previous.Observations)+1 {
		return false
	}
	return reflect.DeepEqual(previous.Observations, next.Observations[:len(previous.Observations)])
}
