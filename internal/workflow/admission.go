package workflow

import (
	"fmt"

	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

// ExecutorCapabilities is the platform-attested capability snapshot for one
// executor profile. These fields describe effective runtime authority, not
// requested flags.
type ExecutorCapabilities struct {
	WorkspaceWrite      bool
	RemoteWrite         bool
	UnrestrictedNetwork bool
	ProjectConfigLoad   bool
	BackgroundExecution bool
}

type AdmissionPolicy struct {
	Replay           bool
	DefinitionSHA256 string
	Executors        map[string]ExecutorCapabilities
}

// AdmitReview binds ReviewSpec policy, declared stage effects, and attested
// executor capabilities. A declaration of "none" never overrides broader
// runtime authority.
func AdmitReview(
	spec contractsv1alpha1.ReviewSpec,
	definition Definition,
	policy AdmissionPolicy,
) error {
	if err := spec.Validate(); err != nil {
		return fmt.Errorf("validate ReviewSpec: %w", err)
	}
	if err := definition.Validate(); err != nil {
		return fmt.Errorf("validate WorkflowDefinition: %w", err)
	}
	if spec.WorkflowRef.ID != definition.ID || spec.WorkflowRef.Revision != definition.Revision {
		return fmt.Errorf(
			"ReviewSpec workflow %s@%s does not match definition %s@%s",
			spec.WorkflowRef.ID,
			spec.WorkflowRef.Revision,
			definition.ID,
			definition.Revision,
		)
	}
	if policy.DefinitionSHA256 == "" || policy.DefinitionSHA256 != spec.WorkflowRef.SHA256 {
		return fmt.Errorf("WorkflowDefinition digest does not match ReviewSpec")
	}
	if policy.Executors == nil {
		return fmt.Errorf("executor capability snapshots are required")
	}
	for _, stage := range definition.Stages {
		capabilities, ok := policy.Executors[stage.Executor]
		if !ok {
			return fmt.Errorf("stage %q executor %q has no capability snapshot", stage.ID, stage.Executor)
		}
		if capabilities.WorkspaceWrite || capabilities.UnrestrictedNetwork ||
			capabilities.ProjectConfigLoad || capabilities.BackgroundExecution {
			return fmt.Errorf("stage %q executor %q exceeds review-safe authority", stage.ID, stage.Executor)
		}
		switch stage.SideEffect {
		case SideEffectNone:
			if capabilities.RemoteWrite {
				return fmt.Errorf("stage %q declares no side effect but executor can write remotely", stage.ID)
			}
		case SideEffectRemotePublish:
			if spec.RemoteWrites != "allow" {
				return fmt.Errorf("stage %q requires remote writes denied by ReviewSpec", stage.ID)
			}
			if !capabilities.RemoteWrite {
				return fmt.Errorf("stage %q declares remote publish without attested capability", stage.ID)
			}
		}
		if policy.Replay && (stage.ReplayPolicy == ReplayPolicyForbidden ||
			stage.SideEffect != SideEffectNone ||
			capabilities.RemoteWrite) {
			return fmt.Errorf("stage %q is not safe for replay", stage.ID)
		}
	}
	return nil
}
