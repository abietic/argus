package v1alpha1

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const (
	AgentReviewPromptBundleSchemaVersion = "argus.agent_review_prompt_bundle.v1alpha1"
	AgentReviewPromptBundleMaxBytes      = 64 << 10
	agentReviewPromptFieldMaxBytes       = 16 << 10
)

// AgentReviewPromptBundle is the governed, behavior-bearing portion of the Pi
// review prompt. The worker always wraps these role instructions in its
// code-owned safety baseline, so a configuration revision can tune review
// behavior without weakening tool, credential, or output-contract controls.
type AgentReviewPromptBundle struct {
	SchemaVersion            string `json:"schema_version"`
	Revision                 string `json:"revision"`
	ContextSystemPrompt      string `json:"context_system_prompt"`
	ReviewSystemPrompt       string `json:"review_system_prompt"`
	VerificationSystemPrompt string `json:"verification_system_prompt"`
	TerminalFinalizerPrompt  string `json:"terminal_finalizer_prompt"`
}

// AgentReviewWorkerPromptBundle carries an exact immutable prompt artifact
// through the local stdio trust boundary. Ref, Artifact, and the decoded bytes
// must all bind the same SHA-256; Revision must also agree with the decoded
// prompt bundle.
type AgentReviewWorkerPromptBundle struct {
	Ref           VersionedRef    `json:"ref"`
	Artifact      ArtifactBinding `json:"artifact"`
	ContentBase64 string          `json:"content_base64"`
}

func DecodeAgentReviewPromptBundle(data []byte) (AgentReviewPromptBundle, error) {
	if len(data) == 0 || len(data) > AgentReviewPromptBundleMaxBytes {
		return AgentReviewPromptBundle{}, fmt.Errorf(
			"prompt bundle must be between 1 and %d bytes",
			AgentReviewPromptBundleMaxBytes,
		)
	}
	var bundle AgentReviewPromptBundle
	if err := decodeAgentReviewShadowJSON(data, &bundle); err != nil {
		return AgentReviewPromptBundle{}, fmt.Errorf("decode AgentReviewPromptBundle: %w", err)
	}
	if err := bundle.Validate(); err != nil {
		return AgentReviewPromptBundle{}, err
	}
	return bundle, nil
}

func (bundle AgentReviewPromptBundle) Validate() error {
	if bundle.SchemaVersion != AgentReviewPromptBundleSchemaVersion {
		return fmt.Errorf("unsupported AgentReviewPromptBundle schema %q", bundle.SchemaVersion)
	}
	if err := requireIdentifier("revision", bundle.Revision); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"context_system_prompt":      bundle.ContextSystemPrompt,
		"review_system_prompt":       bundle.ReviewSystemPrompt,
		"verification_system_prompt": bundle.VerificationSystemPrompt,
		"terminal_finalizer_prompt":  bundle.TerminalFinalizerPrompt,
	} {
		if err := requireBoundedAgentReviewText(
			name,
			value,
			agentReviewPromptFieldMaxBytes,
			true,
		); err != nil {
			return err
		}
	}
	return nil
}

func (transport AgentReviewWorkerPromptBundle) Validate() error {
	if err := transport.Ref.validate("prompt_bundle.ref"); err != nil {
		return err
	}
	if err := transport.Artifact.validate("prompt_bundle.artifact", true); err != nil {
		return err
	}
	if transport.Artifact.Contract != AgentStagePlanPromptContract {
		return fmt.Errorf(
			"prompt_bundle.artifact.contract must be %q",
			AgentStagePlanPromptContract,
		)
	}
	if transport.Ref.SHA256 != transport.Artifact.Ref.SHA256 {
		return fmt.Errorf("prompt bundle ref and artifact must bind the same content SHA-256")
	}
	content, err := decodeCanonicalBase64("prompt_bundle.content_base64", transport.ContentBase64)
	if err != nil {
		return err
	}
	if len(content) > AgentReviewPromptBundleMaxBytes {
		return fmt.Errorf("prompt bundle exceeds %d bytes", AgentReviewPromptBundleMaxBytes)
	}
	if int64(len(content)) != transport.Artifact.Ref.SizeBytes {
		return fmt.Errorf("prompt bundle size does not match its artifact binding")
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != transport.Ref.SHA256 {
		return fmt.Errorf("prompt bundle bytes do not match their bound SHA-256")
	}
	bundle, err := DecodeAgentReviewPromptBundle(content)
	if err != nil {
		return err
	}
	if bundle.Revision != transport.Ref.Revision {
		return fmt.Errorf("prompt bundle revision does not match its versioned ref")
	}
	return nil
}

func DecodeAgentReviewWorkerPromptBundle(
	transport AgentReviewWorkerPromptBundle,
) (AgentReviewPromptBundle, []byte, error) {
	if err := transport.Validate(); err != nil {
		return AgentReviewPromptBundle{}, nil, err
	}
	content, err := decodeCanonicalBase64("prompt_bundle.content_base64", transport.ContentBase64)
	if err != nil {
		return AgentReviewPromptBundle{}, nil, err
	}
	bundle, err := DecodeAgentReviewPromptBundle(content)
	return bundle, bytes.Clone(content), err
}

func NewAgentReviewWorkerPromptBundle(
	ref VersionedRef,
	artifact ArtifactBinding,
	content []byte,
) (AgentReviewWorkerPromptBundle, error) {
	transport := AgentReviewWorkerPromptBundle{
		Ref: ref, Artifact: artifact,
		ContentBase64: base64.StdEncoding.EncodeToString(content),
	}
	if err := transport.Validate(); err != nil {
		return AgentReviewWorkerPromptBundle{}, err
	}
	return transport, nil
}

func DefaultAgentReviewPromptBundle() AgentReviewPromptBundle {
	return AgentReviewPromptBundle{
		SchemaVersion:            AgentReviewPromptBundleSchemaVersion,
		Revision:                 "v0",
		ContextSystemPrompt:      "You are the context collector. Build the smallest evidence-backed context needed to review this change group. Do not decide whether a defect exists.",
		ReviewSystemPrompt:       "You are an evidence-driven code reviewer. Find real defects introduced or exposed by the supplied target, not style issues. A candidate must include a concrete trigger, impact and source anchor inside this change group.",
		VerificationSystemPrompt: "You are an independent defect verifier. Try to falsify the supplied claim using source evidence. Confirm only when a concrete triggering path and impact remain after checking relevant code; reject disproven claims; use inconclusive when required evidence is unavailable.",
		TerminalFinalizerPrompt:  "Repository exploration is closed. Call the required terminal submit tool now with the best evidence-backed result allowed by its schema. Do not call any other tool or answer with prose.",
	}
}

func MarshalDefaultAgentReviewPromptBundle() ([]byte, error) {
	bundle := DefaultAgentReviewPromptBundle()
	if err := bundle.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("marshal default AgentReviewPromptBundle: %w", err)
	}
	return data, nil
}

func decodeCanonicalBase64(name, value string) ([]byte, error) {
	if value == "" {
		return nil, fmt.Errorf("%s must not be empty", name)
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("%s must be canonical standard base64: %w", name, err)
	}
	if len(decoded) == 0 || base64.StdEncoding.EncodeToString(decoded) != value {
		return nil, fmt.Errorf("%s must decode to non-empty canonical standard base64", name)
	}
	return decoded, nil
}
