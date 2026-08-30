package application

import (
	"context"
	"fmt"
	"time"
)

const maxAgentJSONSafeInteger uint64 = 1<<53 - 1

// AgentStageExecutionAuthority is the current host-observed workload lease
// that authorizes one formal provider execution. It is not supplied by the
// caller or derived from plan content. A later generation or run cancellation
// invalidates this authority even when the immutable plan is unchanged.
type AgentStageExecutionAuthority struct {
	WorkloadID   string
	LeaseID      string
	LeaseWorker  string
	Attempt      int
	Generation   int
	FencingToken uint64
	AcquiredAt   time.Time
	ExpiresAt    time.Time
}

func (authority AgentStageExecutionAuthority) validate(at time.Time) error {
	for name, value := range map[string]string{
		"workload_id":     authority.WorkloadID,
		"lease_id":        authority.LeaseID,
		"lease_worker_id": authority.LeaseWorker,
	} {
		if err := validateLocalIdentity(name, value); err != nil {
			return err
		}
	}
	if authority.Attempt < 1 || authority.Generation < 1 ||
		uint64(authority.Attempt) > maxAgentJSONSafeInteger ||
		uint64(authority.Generation) > maxAgentJSONSafeInteger ||
		authority.FencingToken == 0 || authority.FencingToken > maxAgentJSONSafeInteger {
		return fmt.Errorf(
			"lease attempt, generation, and fencing_token must be JSON-safe positive integers",
		)
	}
	if authority.AcquiredAt.IsZero() || authority.AcquiredAt.Location() != time.UTC ||
		authority.ExpiresAt.IsZero() || authority.ExpiresAt.Location() != time.UTC ||
		!authority.ExpiresAt.After(authority.AcquiredAt) {
		return fmt.Errorf("lease timestamps must be ordered non-zero UTC timestamps")
	}
	if at.IsZero() || at.Location() != time.UTC {
		return fmt.Errorf("authority observation time must be a non-zero UTC timestamp")
	}
	if authority.AcquiredAt.After(at) {
		return fmt.Errorf("formal agent execution lease is not yet acquired")
	}
	if !authority.ExpiresAt.After(at) {
		return fmt.Errorf("formal agent execution lease is expired")
	}
	return nil
}

func (authority AgentStageExecutionAuthority) sameLeaseIdentity(
	other AgentStageExecutionAuthority,
) bool {
	return authority.WorkloadID == other.WorkloadID &&
		authority.LeaseID == other.LeaseID &&
		authority.LeaseWorker == other.LeaseWorker &&
		authority.Attempt == other.Attempt &&
		authority.Generation == other.Generation &&
		authority.FencingToken == other.FencingToken
}

// AgentStageExecutionAuthorityResolver is implemented by the scheduling or
// platform adapter. Current must fail closed for canceled, terminal, expired,
// missing, or non-active leases and must return the exact active generation.
type AgentStageExecutionAuthorityResolver interface {
	CurrentAgentStageExecutionAuthority(
		context.Context,
		AgentPlanningSubject,
		string,
		string,
	) (AgentStageExecutionAuthority, error)
}
