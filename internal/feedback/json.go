package feedback

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func DecodeFeedbackJSON(data []byte) (Feedback, error) {
	var feedback Feedback
	if err := decodeStrictJSON(data, &feedback); err != nil {
		return Feedback{}, fmt.Errorf("decode feedback JSON: %w", err)
	}
	if err := feedback.Validate(); err != nil {
		return Feedback{}, fmt.Errorf("validate feedback: %w", err)
	}
	return feedback, nil
}

func DecodeOutcomeJSON(data []byte) (Outcome, error) {
	var outcome Outcome
	if err := decodeStrictJSON(data, &outcome); err != nil {
		return Outcome{}, fmt.Errorf("decode outcome JSON: %w", err)
	}
	if err := outcome.Validate(); err != nil {
		return Outcome{}, fmt.Errorf("validate outcome: %w", err)
	}
	return outcome, nil
}

func decodeStrictJSON(data []byte, out any) error {
	if err := rejectDuplicateJSONFields(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("multiple JSON values are not allowed")
	}
	return fmt.Errorf("invalid trailing JSON: %w", err)
}

func rejectDuplicateJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := inspectJSONValue(decoder); err != nil {
		return err
	}
	token, err := decoder.Token()
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return fmt.Errorf("unexpected trailing JSON token %v", token)
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
		if err != nil {
			return fmt.Errorf("close JSON object: %w", err)
		}
		if end != json.Delim('}') {
			return fmt.Errorf("JSON object has invalid closing token")
		}
	case '[':
		for decoder.More() {
			if err := inspectJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("close JSON array: %w", err)
		}
		if end != json.Delim(']') {
			return fmt.Errorf("JSON array has invalid closing token")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}
