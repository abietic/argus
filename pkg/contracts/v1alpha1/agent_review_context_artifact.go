package v1alpha1

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

const (
	AgentReviewContextArtifactMaxBytes = 512 << 10
	AgentReviewWorkerMaxContextCount   = 16
)

// AgentReviewWorkerContextArtifact carries one already-frozen context result
// referenced by ReviewInput. It is evidence data, not an executable context
// provider adapter, and therefore cannot grant tools or side effects.
type AgentReviewWorkerContextArtifact struct {
	ContextID     string          `json:"context_id"`
	Kind          string          `json:"kind"`
	Revision      string          `json:"revision"`
	Artifact      ArtifactBinding `json:"artifact"`
	ContentBase64 string          `json:"content_base64"`
}

func (transport AgentReviewWorkerContextArtifact) Validate() error {
	if err := requireIdentifier("context_artifact.context_id", transport.ContextID); err != nil {
		return err
	}
	if err := requireIdentifier("context_artifact.kind", transport.Kind); err != nil {
		return err
	}
	if err := requireIdentifier("context_artifact.revision", transport.Revision); err != nil {
		return err
	}
	if err := transport.Artifact.validate("context_artifact.artifact", true); err != nil {
		return err
	}
	content, err := decodeCanonicalBase64(
		"context_artifact.content_base64",
		transport.ContentBase64,
	)
	if err != nil {
		return err
	}
	if len(content) == 0 || len(content) > AgentReviewContextArtifactMaxBytes {
		return fmt.Errorf(
			"context artifact must contain between 1 and %d bytes",
			AgentReviewContextArtifactMaxBytes,
		)
	}
	if err := requireBoundedAgentReviewText(
		"context_artifact.content",
		string(content),
		AgentReviewContextArtifactMaxBytes,
		false,
	); err != nil {
		return err
	}
	if int64(len(content)) != transport.Artifact.Ref.SizeBytes {
		return fmt.Errorf("context artifact size does not match its artifact binding")
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != transport.Artifact.Ref.SHA256 {
		return fmt.Errorf("context artifact bytes do not match their bound SHA-256")
	}
	return nil
}

func DecodeAgentReviewWorkerContextArtifact(
	transport AgentReviewWorkerContextArtifact,
) ([]byte, error) {
	if err := transport.Validate(); err != nil {
		return nil, err
	}
	content, err := decodeCanonicalBase64(
		"context_artifact.content_base64",
		transport.ContentBase64,
	)
	return bytes.Clone(content), err
}

func NewAgentReviewWorkerContextArtifact(
	contextID string,
	kind string,
	revision string,
	artifact ArtifactBinding,
	content []byte,
) (AgentReviewWorkerContextArtifact, error) {
	transport := AgentReviewWorkerContextArtifact{
		ContextID:     contextID,
		Kind:          kind,
		Revision:      revision,
		Artifact:      artifact,
		ContentBase64: base64.StdEncoding.EncodeToString(content),
	}
	if err := transport.Validate(); err != nil {
		return AgentReviewWorkerContextArtifact{}, err
	}
	return transport, nil
}
