package v1alpha1

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	AgentReviewKnowledgeMaxBytes       = 128 << 10
	AgentReviewWorkerMaxKnowledgeCount = 8
)

// AgentReviewWorkerKnowledge carries one governed repository or business-line
// knowledge component through the local stdio boundary. Knowledge is untrusted
// reference data: exact transport prevents drift but does not grant authority.
type AgentReviewWorkerKnowledge struct {
	Ref           VersionedRef    `json:"ref"`
	Artifact      ArtifactBinding `json:"artifact"`
	ContentBase64 string          `json:"content_base64"`
}

func (transport AgentReviewWorkerKnowledge) Validate() error {
	if err := transport.Ref.validate("knowledge_pack.ref"); err != nil {
		return err
	}
	if err := transport.Artifact.validate("knowledge_pack.artifact", true); err != nil {
		return err
	}
	if transport.Artifact.Contract != AgentStagePlanKnowledgeContract {
		return fmt.Errorf(
			"knowledge_pack.artifact.contract must be %q",
			AgentStagePlanKnowledgeContract,
		)
	}
	if transport.Ref.SHA256 != transport.Artifact.Ref.SHA256 {
		return fmt.Errorf("knowledge ref and artifact must bind the same content SHA-256")
	}
	content, err := decodeCanonicalBase64("knowledge_pack.content_base64", transport.ContentBase64)
	if err != nil {
		return err
	}
	if len(content) > AgentReviewKnowledgeMaxBytes {
		return fmt.Errorf("knowledge pack exceeds %d bytes", AgentReviewKnowledgeMaxBytes)
	}
	if strings.TrimSpace(string(content)) == "" {
		return fmt.Errorf("knowledge pack content must not be empty")
	}
	if err := requireBoundedAgentReviewText(
		"knowledge_pack.content", string(content), AgentReviewKnowledgeMaxBytes, false,
	); err != nil {
		return err
	}
	if int64(len(content)) != transport.Artifact.Ref.SizeBytes {
		return fmt.Errorf("knowledge pack size does not match its artifact binding")
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != transport.Ref.SHA256 {
		return fmt.Errorf("knowledge pack bytes do not match their bound SHA-256")
	}
	return nil
}

func DecodeAgentReviewWorkerKnowledge(
	transport AgentReviewWorkerKnowledge,
) ([]byte, error) {
	if err := transport.Validate(); err != nil {
		return nil, err
	}
	content, err := decodeCanonicalBase64("knowledge_pack.content_base64", transport.ContentBase64)
	return bytes.Clone(content), err
}

func NewAgentReviewWorkerKnowledge(
	ref VersionedRef,
	artifact ArtifactBinding,
	content []byte,
) (AgentReviewWorkerKnowledge, error) {
	transport := AgentReviewWorkerKnowledge{
		Ref: ref, Artifact: artifact,
		ContentBase64: base64.StdEncoding.EncodeToString(content),
	}
	if err := transport.Validate(); err != nil {
		return AgentReviewWorkerKnowledge{}, err
	}
	return transport, nil
}
