package v1alpha1

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestAgentReviewWorkerSkillBindsExactContentAndPlanIdentity(t *testing.T) {
	request := validAgentReviewWorkerRequest(t)
	if err := request.Validate(); err != nil {
		t.Fatalf("valid request: %v", err)
	}

	substituted := request
	substituted.ReviewSkills = append([]AgentReviewWorkerSkill{}, request.ReviewSkills...)
	substituted.ReviewSkills[0].ContentBase64 = base64.StdEncoding.EncodeToString(
		[]byte("# Substituted\n\nDifferent review behavior.\n"),
	)
	if err := substituted.Validate(); err == nil ||
		(!strings.Contains(err.Error(), "size") && !strings.Contains(err.Error(), "SHA-256")) {
		t.Fatalf("Validate() accepted review skill substitution: %v", err)
	}

	revisionDrift := request
	revisionDrift.ReviewSkills = append([]AgentReviewWorkerSkill{}, request.ReviewSkills...)
	revisionDrift.ReviewSkills[0].Ref.Revision = "changed-v1"
	if err := revisionDrift.Validate(); err == nil ||
		!strings.Contains(err.Error(), "plan.review_dimensions") {
		t.Fatalf("Validate() accepted review skill revision drift: %v", err)
	}

	missing := request
	missing.ReviewSkills = []AgentReviewWorkerSkill{}
	if err := missing.Validate(); err == nil || !strings.Contains(err.Error(), "between 1 and") {
		t.Fatalf("Validate() accepted missing review skills: %v", err)
	}
}
