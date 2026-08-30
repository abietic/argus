// Package contextcapture owns shared, provider-neutral context capture
// invariants. Concrete adapters remain in child packages.
package contextcapture

import (
	"fmt"
	"regexp"
)

const RevisionMismatchReasonCode = "context_revision_mismatch"

var exactRevisionPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// RevisionMismatchError proves that an exact-revision context source returned
// data for a different Git object. Callers must retain this as a typed gap and
// must not consume the mismatched data.
type RevisionMismatchError struct {
	Operation string
	Expected  string
	Observed  string
}

func (failure *RevisionMismatchError) Error() string {
	if failure == nil {
		return "<nil>"
	}
	return fmt.Sprintf(
		"%s returned revision %q; expected %q",
		failure.Operation,
		failure.Observed,
		failure.Expected,
	)
}

func RequireExactRevision(operation, expected, observed string) error {
	if !IsExactRevision(expected) {
		return fmt.Errorf("%s expected revision is not an exact lowercase Git object ID", operation)
	}
	if !IsExactRevision(observed) {
		return fmt.Errorf("%s observed revision is not an exact lowercase Git object ID", operation)
	}
	if observed == expected {
		return nil
	}
	return &RevisionMismatchError{
		Operation: operation,
		Expected:  expected,
		Observed:  observed,
	}
}

func IsExactRevision(value string) bool {
	return exactRevisionPattern.MatchString(value)
}
