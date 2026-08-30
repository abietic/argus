package scheduling

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func DecodePolicy(data []byte) (Policy, error) {
	return decodeStrict(data, "Policy", func(value Policy) error {
		return value.Validate()
	})
}

func DecodeWorkloadSpec(data []byte) (WorkloadSpec, error) {
	return decodeStrict(data, "WorkloadSpec", func(value WorkloadSpec) error {
		return value.Validate()
	})
}

func DecodeCallback(data []byte) (Callback, error) {
	return decodeStrict(data, "Callback", func(value Callback) error {
		return value.Validate()
	})
}

func decodeStrict[T any](
	data []byte,
	label string,
	validate func(T) error,
) (T, error) {
	var value T
	if err := decodeStrictJSON(data, &value); err != nil {
		return value, fmt.Errorf("decode %s: %w", label, err)
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
