package agentanalytics

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const maxProjectionJSONDepth = 64

// rejectDuplicateProjectionJSONFields closes the ambiguity left by
// encoding/json's last-key-wins behavior. The depth limit prevents a hostile
// artifact from turning validation into unbounded recursion; host-produced
// ProjectionSnapshot JSON is currently less than ten levels deep.
func rejectDuplicateProjectionJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := inspectProjectionJSONValue(decoder, 0); err != nil {
		return err
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("invalid trailing projection JSON: %w", err)
		}
		return fmt.Errorf("unexpected trailing projection JSON token %v", token)
	}
	return nil
}

func inspectProjectionJSONValue(decoder *json.Decoder, depth int) error {
	if depth > maxProjectionJSONDepth {
		return fmt.Errorf("projection JSON exceeds maximum depth %d", maxProjectionJSONDepth)
	}
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("invalid projection JSON: %w", err)
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return fmt.Errorf("read projection JSON object key: %w", err)
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("projection JSON object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate projection JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := inspectProjectionJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return fmt.Errorf("projection JSON object has invalid closing token")
		}
	case '[':
		for decoder.More() {
			if err := inspectProjectionJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return fmt.Errorf("projection JSON array has invalid closing token")
		}
	default:
		return fmt.Errorf("projection JSON has unexpected delimiter %q", delimiter)
	}
	return nil
}
