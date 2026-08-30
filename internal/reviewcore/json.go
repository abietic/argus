package reviewcore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

func DecodeReviewInput(data []byte) (ReviewInput, error) {
	return decodeStrict(data, "ReviewInput", func(value ReviewInput) error {
		return value.Validate()
	})
}

func DecodeEvidence(data []byte) (Evidence, error) {
	return decodeStrict(data, "Evidence", func(value Evidence) error {
		return value.Validate()
	})
}

func DecodeCandidateFinding(data []byte) (CandidateFinding, error) {
	return decodeStrict(data, "CandidateFinding", func(value CandidateFinding) error {
		return value.Validate()
	})
}

func DecodeDetectionGap(data []byte) (DetectionGap, error) {
	return decodeStrict(data, "DetectionGap", func(value DetectionGap) error {
		return value.Validate()
	})
}

func DecodeFinding(data []byte) (Finding, error) {
	return decodeStrict(data, "Finding", func(value Finding) error {
		return value.Validate()
	})
}

func DecodeFindingDecision(data []byte) (FindingDecision, error) {
	return decodeStrict(data, "FindingDecision", func(value FindingDecision) error {
		return value.Validate()
	})
}

func DecodeStageResult(data []byte) (StageResult, error) {
	return decodeStrict(data, "StageResult", func(value StageResult) error {
		return value.Validate()
	})
}

func DecodeReport(data []byte) (Report, error) {
	return decodeStrict(data, "Report", func(value Report) error {
		return value.Validate()
	})
}

func decodeStrict[T any](data []byte, label string, validate func(T) error) (T, error) {
	var zero T
	if err := rejectDuplicateFields(data); err != nil {
		return zero, fmt.Errorf("decode %s structure: %w", label, err)
	}
	var value T
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return zero, fmt.Errorf("decode %s: %w", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return zero, fmt.Errorf("%s contains multiple JSON values", label)
		}
		return zero, fmt.Errorf("decode trailing %s data: %w", label, err)
	}
	if err := validate(value); err != nil {
		return zero, fmt.Errorf("validate %s: %w", label, err)
	}
	return value, nil
}

func rejectDuplicateFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := walkJSON(decoder, "$"); err != nil {
		return err
	}
	if _, err := decoder.Token(); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	return fmt.Errorf("multiple JSON values")
}

func walkJSON(decoder *json.Decoder, location string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			if keyErr != nil {
				return keyErr
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("%s contains a non-string object key", location)
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("%s contains duplicate field %q", location, key)
			}
			seen[key] = struct{}{}
			if err := walkJSON(decoder, location+"."+key); err != nil {
				return err
			}
		}
		closing, closingErr := decoder.Token()
		if closingErr != nil {
			return closingErr
		}
		if closing != json.Delim('}') {
			return fmt.Errorf("%s object is not closed", location)
		}
	case '[':
		index := 0
		for decoder.More() {
			if err := walkJSON(decoder, fmt.Sprintf("%s[%d]", location, index)); err != nil {
				return err
			}
			index++
		}
		closing, closingErr := decoder.Token()
		if closingErr != nil {
			return closingErr
		}
		if closing != json.Delim(']') {
			return fmt.Errorf("%s array is not closed", location)
		}
	default:
		return fmt.Errorf("%s starts with unexpected delimiter %q", location, delimiter)
	}
	return nil
}
