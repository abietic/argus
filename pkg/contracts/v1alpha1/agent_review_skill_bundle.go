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
	AgentReviewSkillMaxBytes       = 64 << 10
	AgentReviewWorkerMaxSkillCount = 16
)

// AgentReviewWorkerSkill carries one governed review-phase skill through the
// local stdio boundary. The content is instruction-bearing Markdown, so the
// worker must use these exact bytes instead of reopening a mutable file by ID.
type AgentReviewWorkerSkill struct {
	Ref           VersionedRef    `json:"ref"`
	Artifact      ArtifactBinding `json:"artifact"`
	ContentBase64 string          `json:"content_base64"`
}

func (transport AgentReviewWorkerSkill) Validate() error {
	if err := transport.Ref.validate("review_skill.ref"); err != nil {
		return err
	}
	if err := transport.Artifact.validate("review_skill.artifact", true); err != nil {
		return err
	}
	if transport.Artifact.Contract != AgentStagePlanSkillContract {
		return fmt.Errorf(
			"review_skill.artifact.contract must be %q",
			AgentStagePlanSkillContract,
		)
	}
	if transport.Ref.SHA256 != transport.Artifact.Ref.SHA256 {
		return fmt.Errorf("review skill ref and artifact must bind the same content SHA-256")
	}
	content, err := decodeCanonicalBase64("review_skill.content_base64", transport.ContentBase64)
	if err != nil {
		return err
	}
	if len(content) > AgentReviewSkillMaxBytes {
		return fmt.Errorf("review skill exceeds %d bytes", AgentReviewSkillMaxBytes)
	}
	if strings.TrimSpace(string(content)) == "" {
		return fmt.Errorf("review skill content must not be empty")
	}
	if err := requireBoundedAgentReviewText(
		"review_skill.content",
		string(content),
		AgentReviewSkillMaxBytes,
		false,
	); err != nil {
		return err
	}
	if int64(len(content)) != transport.Artifact.Ref.SizeBytes {
		return fmt.Errorf("review skill size does not match its artifact binding")
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != transport.Ref.SHA256 {
		return fmt.Errorf("review skill bytes do not match their bound SHA-256")
	}
	return nil
}

func DecodeAgentReviewWorkerSkill(
	transport AgentReviewWorkerSkill,
) ([]byte, error) {
	if err := transport.Validate(); err != nil {
		return nil, err
	}
	content, err := decodeCanonicalBase64("review_skill.content_base64", transport.ContentBase64)
	return bytes.Clone(content), err
}

func NewAgentReviewWorkerSkill(
	ref VersionedRef,
	artifact ArtifactBinding,
	content []byte,
) (AgentReviewWorkerSkill, error) {
	transport := AgentReviewWorkerSkill{
		Ref: ref, Artifact: artifact,
		ContentBase64: base64.StdEncoding.EncodeToString(content),
	}
	if err := transport.Validate(); err != nil {
		return AgentReviewWorkerSkill{}, err
	}
	return transport, nil
}
