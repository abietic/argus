package evaluation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func DecodeEvaluationCase(data []byte) (EvaluationCase, error) {
	return decodeStrict(data, "EvaluationCase", func(value EvaluationCase) error {
		return value.Validate()
	})
}

func DecodeLabelCorrection(data []byte) (LabelCorrection, error) {
	return decodeStrict(data, "LabelCorrection", func(value LabelCorrection) error {
		return value.Validate()
	})
}

func DecodeCaseAnnotation(data []byte) (CaseAnnotation, error) {
	return decodeStrict(data, "CaseAnnotation", func(value CaseAnnotation) error {
		return value.Validate()
	})
}

func DecodeCaseReviewAssignment(data []byte) (CaseReviewAssignment, error) {
	return decodeStrict(data, "CaseReviewAssignment", func(value CaseReviewAssignment) error {
		return value.Validate()
	})
}

func DecodeCaseAdjudication(data []byte) (CaseAdjudication, error) {
	return decodeStrict(data, "CaseAdjudication", func(value CaseAdjudication) error {
		return value.Validate()
	})
}

func DecodeCaseActivation(data []byte) (CaseActivation, error) {
	return decodeStrict(data, "CaseActivation", func(value CaseActivation) error {
		return value.Validate()
	})
}

func DecodeCaseReopen(data []byte) (CaseReopen, error) {
	return decodeStrict(data, "CaseReopen", func(value CaseReopen) error {
		return value.Validate()
	})
}

func DecodeExposure(data []byte) (Exposure, error) {
	return decodeStrict(data, "Exposure", func(value Exposure) error {
		return value.Validate()
	})
}

func DecodePromotionVariant(data []byte) (PromotionVariant, error) {
	return decodeStrict(data, "PromotionVariant", func(value PromotionVariant) error {
		return value.Validate()
	})
}

func DecodeGateResult(data []byte) (GateResult, error) {
	return decodeStrict(data, "GateResult", func(value GateResult) error {
		return value.Validate()
	})
}

func decodeStrict[T any](
	data []byte,
	label string,
	validate func(T) error,
) (T, error) {
	var value T
	if err := rejectDuplicateJSONFields(data); err != nil {
		return value, fmt.Errorf("decode %s: %w", label, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("decode %s: %w", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return value, fmt.Errorf("decode %s: multiple JSON values are not allowed", label)
		}
		return value, fmt.Errorf("decode %s: invalid trailing JSON: %w", label, err)
	}
	if err := validate(value); err != nil {
		return value, fmt.Errorf("validate %s: %w", label, err)
	}
	return value, nil
}

func rejectDuplicateJSONFields(data []byte) error {
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
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return fmt.Errorf("read JSON object key: %w", err)
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
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return fmt.Errorf("JSON object has invalid closing token")
		}
	case '[':
		for decoder.More() {
			if err := inspectJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return fmt.Errorf("JSON array has invalid closing token")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}
