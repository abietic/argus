package platformapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/findingdecision"
)

func decodeStrict[T any](data []byte, target *T) error {
	if err := rejectDuplicateFields(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func DecodePrincipal(data []byte) (Principal, error) {
	return decodeAndValidate(data, func(value Principal) error { return value.Validate() })
}

func DecodeMutationInput(data []byte) (MutationInput, error) {
	return decodeAndValidate(data, func(value MutationInput) error { return value.Validate() })
}

func DecodeGovernanceBatchCommand(data []byte) (GovernanceBatchCommand, error) {
	return decodeAndValidate(data, func(value GovernanceBatchCommand) error { return value.Validate() })
}

func DecodeEvaluationBatchResumeCommand(data []byte) (EvaluationBatchResumeCommand, error) {
	return decodeAndValidate(data, func(value EvaluationBatchResumeCommand) error { return value.Validate() })
}

func DecodeExperimentBatchSubmitCommand(data []byte) (ExperimentBatchSubmitCommand, error) {
	return decodeAndValidate(data, func(value ExperimentBatchSubmitCommand) error { return value.Validate() })
}

func DecodeRepeatabilityBatchSubmitCommand(data []byte) (RepeatabilityBatchSubmitCommand, error) {
	return decodeAndValidate(data, func(value RepeatabilityBatchSubmitCommand) error { return value.Validate() })
}

func DecodeAgentComponentPublishCommand(data []byte) (AgentComponentPublishCommand, error) {
	return decodeAndValidate(data, func(value AgentComponentPublishCommand) error { return value.Validate() })
}

func DecodeCaseImportCommand(data []byte) (CaseImportCommand, error) {
	return decodeAndValidate(data, func(value CaseImportCommand) error { return value.Validate() })
}

func DecodeTrustKeyCommand(data []byte) (TrustKeyCommand, error) {
	return decodeAndValidate(data, func(value TrustKeyCommand) error { return value.Validate() })
}

func DecodeTrustKeyRevocationCommand(data []byte) (TrustKeyRevocationCommand, error) {
	return decodeAndValidate(data, func(value TrustKeyRevocationCommand) error { return value.Validate() })
}

func DecodeConfigCreateCommand(data []byte) (ConfigCreateCommand, error) {
	return decodeAndValidate(data, func(value ConfigCreateCommand) error { return value.Validate() })
}

func DecodeConfigTransitionCommand(data []byte) (ConfigTransitionCommand, error) {
	return decodeAndValidate(data, func(value ConfigTransitionCommand) error { return value.Validate() })
}

func DecodeConfigResolutionQuery(data []byte) (ConfigResolutionQuery, error) {
	return decodeAndValidate(data, func(value ConfigResolutionQuery) error { return value.Validate() })
}

func localAPIReviewWriteValidationPrincipal() Principal {
	return Principal{
		SchemaVersion: PrincipalSchemaVersion,
		Actor:         "schema-validator", ActorKind: findingdecision.ActorHuman,
		Roles:           []evaluation.Role{},
		FindingRoles:    []findingdecision.Role{findingdecision.RoleFindingReviewer},
		Permissions:     []Permission{PermissionReviewWrite},
		ProfileRevision: "schema-validator-v1",
	}
}

func DecodeFindingDecisionWriteCommand(data []byte) (FindingDecisionWriteCommand, error) {
	return decodeAndValidate(data, func(value FindingDecisionWriteCommand) error {
		return value.Validate(
			"schema-run", "schema-finding", localAPIReviewWriteValidationPrincipal(),
		)
	})
}

func DecodeFeedbackWriteCommand(data []byte) (FeedbackWriteCommand, error) {
	return decodeAndValidate(data, func(value FeedbackWriteCommand) error {
		_, err := value.Fact(
			"schema-run", "schema-finding", localAPIReviewWriteValidationPrincipal(),
		)
		return err
	})
}

func DecodeOutcomeWriteCommand(data []byte) (OutcomeWriteCommand, error) {
	return decodeAndValidate(data, func(value OutcomeWriteCommand) error {
		_, err := value.Fact(
			"schema-run", "schema-finding", localAPIReviewWriteValidationPrincipal(),
		)
		return err
	})
}

func decodeAndValidate[T any](data []byte, validate func(T) error) (T, error) {
	var value T
	if err := decodeStrict(data, &value); err != nil {
		return value, err
	}
	if err := validate(value); err != nil {
		return value, err
	}
	return value, nil
}

func rejectDuplicateFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := inspectJSONValue(decoder); err != nil {
		return err
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
		return fmt.Errorf("unexpected trailing JSON token %v", token)
	}
	return nil
}

func inspectJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	delimiter, objectOrArray := token.(json.Delim)
	if !objectOrArray {
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
				return fmt.Errorf("JSON object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := inspectJSONValue(decoder); err != nil {
				return err
			}
		}
		if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
			return fmt.Errorf("JSON object has invalid closing token")
		}
	case '[':
		for decoder.More() {
			if err := inspectJSONValue(decoder); err != nil {
				return err
			}
		}
		if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
			return fmt.Errorf("JSON array has invalid closing token")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}
