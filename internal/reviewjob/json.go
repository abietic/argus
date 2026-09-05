package reviewjob

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// UnmarshalJSON preserves the distinction between absent optional frozen
// inputs and explicit null. Formal jobs cannot carry a second input authority.
func (request *Request) UnmarshalJSON(data []byte) error {
	type plainRequest Request
	var decoded plainRequest
	if err := decodeStrict(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, name := range []string{"overlay_content", "contexts"} {
		if value, exists := fields[name]; exists {
			if decoded.ExecutionProfile == FormalPiExecutionProfile {
				return fmt.Errorf("formal_pi_review_v1 does not accept %s", name)
			}
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return fmt.Errorf("%s must be omitted rather than null", name)
			}
		}
	}
	*request = Request(decoded)
	return nil
}
