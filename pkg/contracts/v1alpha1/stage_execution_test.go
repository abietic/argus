package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestStageExecutionContractsStrictRoundTripAndBinding(t *testing.T) {
	request := validStageExecutionRequest(t)
	requestData, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	decodedRequest, err := DecodeStageExecutionRequest(requestData)
	if err != nil {
		t.Fatalf("DecodeStageExecutionRequest() error = %v", err)
	}
	output := ArtifactBinding{
		Ref: ContentRef{
			URI: "artifact://local/sha256/output", SHA256: testDigest, SizeBytes: 10,
		},
		Contract: request.OutputContract,
	}
	result := StageExecutionResult{
		SchemaVersion: StageExecutionResultSchemaVersion,
		RequestSHA256: request.RequestSHA256,
		ExecutionID:   request.ExecutionID, Attempt: request.Attempt,
		Generation: request.Generation, FencingToken: request.FencingToken,
		IdempotencyKey: request.IdempotencyKey, Status: StageExecutionSucceeded,
		Output: &output, Completeness: "complete", CompletenessNotes: []string{},
		CapabilitySHA256: request.Capability.SHA256,
		RecordedAt:       time.Date(2026, time.July, 27, 10, 1, 0, 0, time.UTC),
	}
	resultData, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	decodedResult, err := DecodeStageExecutionResult(resultData)
	if err != nil {
		t.Fatalf("DecodeStageExecutionResult() error = %v", err)
	}
	if err := ValidateStageExecutionResultBinding(decodedRequest, decodedResult); err != nil {
		t.Fatalf("ValidateStageExecutionResultBinding() error = %v", err)
	}
}

func TestStageExecutionContractsRejectTampering(t *testing.T) {
	request := validStageExecutionRequest(t)
	tests := []struct {
		name   string
		mutate func(*StageExecutionRequest)
		want   string
	}{
		{
			name: "request content tamper",
			mutate: func(request *StageExecutionRequest) {
				request.Plan.Ref.SHA256 = strings.Repeat("b", 64)
			},
			want: "request_sha256 does not match",
		},
		{
			name: "capability content tamper",
			mutate: func(request *StageExecutionRequest) {
				request.Capability.RuntimeID = "other-runtime"
			},
			want: "capability sha256",
		},
		{
			name: "write authority",
			mutate: func(request *StageExecutionRequest) {
				request.Capability.Authority.RemoteWrites = "allow"
				digest, err := DigestExecutorCapability(request.Capability)
				if err != nil {
					t.Fatal(err)
				}
				request.Capability.SHA256 = digest
			},
			want: "must be denied",
		},
		{
			name: "direct model egress",
			mutate: func(request *StageExecutionRequest) {
				request.Capability.Authority.ModelEgress = "direct"
				digest, err := DigestExecutorCapability(request.Capability)
				if err != nil {
					t.Fatal(err)
				}
				request.Capability.SHA256 = digest
			},
			want: "model_egress must be",
		},
		{
			name: "tool network authority",
			mutate: func(request *StageExecutionRequest) {
				request.Capability.Authority.ToolNetwork = "allow"
				digest, err := DigestExecutorCapability(request.Capability)
				if err != nil {
					t.Fatal(err)
				}
				request.Capability.SHA256 = digest
			},
			want: "tool_network must be denied",
		},
		{
			name: "unfrozen workspace reads",
			mutate: func(request *StageExecutionRequest) {
				request.Capability.Authority.WorkspaceReads = "all"
				digest, err := DigestExecutorCapability(request.Capability)
				if err != nil {
					t.Fatal(err)
				}
				request.Capability.SHA256 = digest
			},
			want: "workspace_reads must be",
		},
		{
			name: "delegation authority",
			mutate: func(request *StageExecutionRequest) {
				request.Capability.Authority.MaxDelegationDepth = 1
				digest, err := DigestExecutorCapability(request.Capability)
				if err != nil {
					t.Fatal(err)
				}
				request.Capability.SHA256 = digest
			},
			want: "max_delegation_depth must be zero",
		},
		{
			name: "late generation",
			mutate: func(request *StageExecutionRequest) {
				request.Generation = 0
			},
			want: "must be positive",
		},
		{
			name: "missing workload authority",
			mutate: func(request *StageExecutionRequest) {
				request.WorkloadID = ""
			},
			want: "workload_id",
		},
		{
			name: "missing lease authority",
			mutate: func(request *StageExecutionRequest) {
				request.LeaseID = ""
			},
			want: "lease_id",
		},
		{
			name: "missing lease worker authority",
			mutate: func(request *StageExecutionRequest) {
				request.LeaseWorkerID = ""
			},
			want: "lease_worker_id",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := request
			candidate.Upstream = make([]ArtifactBinding, len(request.Upstream))
			copy(candidate.Upstream, request.Upstream)
			test.mutate(&candidate)
			if err := candidate.Validate(); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}

	result := StageExecutionResult{
		SchemaVersion: StageExecutionResultSchemaVersion,
		RequestSHA256: request.RequestSHA256,
		ExecutionID:   request.ExecutionID, Attempt: request.Attempt,
		Generation: request.Generation + 1, FencingToken: request.FencingToken,
		IdempotencyKey: request.IdempotencyKey, Status: StageExecutionFailed,
		Failure:      &StageExecutionFailure{Code: "failed", Message: "failed"},
		Completeness: "partial", CompletenessNotes: []string{"failed"},
		CapabilitySHA256: request.Capability.SHA256,
		RecordedAt:       time.Date(2026, time.July, 27, 10, 1, 0, 0, time.UTC),
	}
	if err := ValidateStageExecutionResultBinding(request, result); err == nil ||
		!strings.Contains(err.Error(), "exact request fencing") {
		t.Fatalf("late result binding error = %v", err)
	}
}

func TestExecutorCapabilityDigestCoversExactAuthority(t *testing.T) {
	base := validStageExecutionRequest(t).Capability
	baseDigest := base.SHA256
	tests := []struct {
		name   string
		mutate func(*ExecutionAuthority)
	}{
		{"allowed tools", func(authority *ExecutionAuthority) {
			authority.AllowedTools = []string{"read_file"}
		}},
		{"model egress", func(authority *ExecutionAuthority) {
			authority.ModelEgress = "other"
		}},
		{"tool network", func(authority *ExecutionAuthority) {
			authority.ToolNetwork = "allow"
		}},
		{"workspace reads", func(authority *ExecutionAuthority) {
			authority.WorkspaceReads = "other"
		}},
		{"workspace writes", func(authority *ExecutionAuthority) {
			authority.WorkspaceWrites = "allow"
		}},
		{"remote writes", func(authority *ExecutionAuthority) {
			authority.RemoteWrites = "allow"
		}},
		{"delegation depth", func(authority *ExecutionAuthority) {
			authority.MaxDelegationDepth = 1
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			candidate.Authority.AllowedTools = append(
				[]string(nil),
				base.Authority.AllowedTools...,
			)
			test.mutate(&candidate.Authority)
			digest, err := DigestExecutorCapability(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if digest == baseDigest {
				t.Fatal("authority mutation did not change capability digest")
			}
		})
	}
}

func TestExecutorCapabilityDigestCoversExactTrust(t *testing.T) {
	base := validStageExecutionRequest(t).Capability
	tests := []struct {
		name   string
		mutate func(*ExecutorTrust)
	}{
		{"authority", func(trust *ExecutorTrust) {
			trust.Authority = ExecutorTrustAuthorityPlatform
		}},
		{"capability verifier id", func(trust *ExecutorTrust) {
			trust.CapabilityVerifier.ID = "other-capability-verifier"
		}},
		{"capability verifier revision", func(trust *ExecutorTrust) {
			trust.CapabilityVerifier.Revision = "2"
		}},
		{"capability verifier sha", func(trust *ExecutorTrust) {
			trust.CapabilityVerifier.SHA256 = strings.Repeat("b", 64)
		}},
		{"callback verifier id", func(trust *ExecutorTrust) {
			trust.CallbackVerifier.ID = "other-callback-verifier"
		}},
		{"callback verifier revision", func(trust *ExecutorTrust) {
			trust.CallbackVerifier.Revision = "2"
		}},
		{"callback verifier sha", func(trust *ExecutorTrust) {
			trust.CallbackVerifier.SHA256 = strings.Repeat("c", 64)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			test.mutate(&candidate.Trust)
			digest, err := DigestExecutorCapability(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if digest == base.SHA256 {
				t.Fatal("trust mutation did not change capability digest")
			}
		})
	}

	for _, trust := range []ExecutorTrust{
		{},
		{
			Authority:          ExecutorTrustAuthorityLocalHost,
			CapabilityVerifier: base.Trust.CapabilityVerifier,
		},
		{
			Authority:          "unknown",
			CapabilityVerifier: base.Trust.CapabilityVerifier,
			CallbackVerifier:   base.Trust.CallbackVerifier,
		},
	} {
		candidate := base
		candidate.Trust = trust
		digest, err := DigestExecutorCapability(candidate)
		if err != nil {
			t.Fatal(err)
		}
		candidate.SHA256 = digest
		if err := candidate.Validate(); err == nil {
			t.Fatalf("Validate() accepted invalid trust: %+v", trust)
		}
	}
}

func TestExecutorCapabilityDigestCoversExactRuntimeRevision(t *testing.T) {
	base := validStageExecutionRequest(t).Capability
	for _, mutate := range []func(*ExecutorCapabilitySnapshot){
		func(capability *ExecutorCapabilitySnapshot) {
			capability.RuntimeRevision = "2"
		},
		func(capability *ExecutorCapabilitySnapshot) {
			capability.RuntimeSHA256 = strings.Repeat("b", 64)
		},
	} {
		candidate := base
		mutate(&candidate)
		digest, err := DigestExecutorCapability(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if digest == base.SHA256 {
			t.Fatal("runtime identity mutation did not change capability digest")
		}
	}

	missingRevision := base
	missingRevision.RuntimeRevision = ""
	if err := missingRevision.Validate(); err == nil ||
		!strings.Contains(err.Error(), "runtime_revision") {
		t.Fatalf("Validate() missing runtime revision error = %v", err)
	}
	invalidRuntimeSHA := base
	invalidRuntimeSHA.RuntimeSHA256 = "not-a-sha256"
	if err := invalidRuntimeSHA.Validate(); err == nil ||
		!strings.Contains(err.Error(), "runtime_sha256") {
		t.Fatalf("Validate() invalid runtime sha error = %v", err)
	}
}

func TestSealStageExecutionRequestPreservesUpstreamOrder(t *testing.T) {
	request := stageExecutionRequestContent(t)
	request.Upstream = []ArtifactBinding{
		{
			Ref: ContentRef{
				URI:    "artifact://local/sha256/upstream-z",
				SHA256: strings.Repeat("b", 64), SizeBytes: 11,
			},
			Contract: "argus.stage_result.v1alpha1",
		},
		{
			Ref: ContentRef{
				URI:    "artifact://local/sha256/upstream-a",
				SHA256: strings.Repeat("c", 64), SizeBytes: 12,
			},
			Contract: "argus.stage_result.v1alpha1",
		},
	}
	sealed, err := SealStageExecutionRequest(request)
	if err != nil {
		t.Fatalf("SealStageExecutionRequest() error = %v", err)
	}
	if got := sealed.Upstream[0].Ref.URI; got != request.Upstream[0].Ref.URI {
		t.Fatalf("first upstream URI = %q, want caller order %q", got, request.Upstream[0].Ref.URI)
	}
	if got := sealed.Upstream[1].Ref.URI; got != request.Upstream[1].Ref.URI {
		t.Fatalf("second upstream URI = %q, want caller order %q", got, request.Upstream[1].Ref.URI)
	}

	reordered := request
	reordered.Upstream = []ArtifactBinding{request.Upstream[1], request.Upstream[0]}
	resealed, err := SealStageExecutionRequest(reordered)
	if err != nil {
		t.Fatalf("SealStageExecutionRequest(reordered) error = %v", err)
	}
	if resealed.RequestSHA256 == sealed.RequestSHA256 {
		t.Fatal("upstream order did not contribute to request_sha256")
	}
	if resealed.Upstream[0].Ref.URI != reordered.Upstream[0].Ref.URI {
		t.Fatal("SealStageExecutionRequest() reordered upstream")
	}
}

func TestSealStageExecutionRequestDetachesMutableSlices(t *testing.T) {
	request := stageExecutionRequestContent(t)
	request.Upstream = []ArtifactBinding{
		{
			Ref: ContentRef{
				URI:    "artifact://local/sha256/upstream",
				SHA256: strings.Repeat("b", 64), SizeBytes: 11,
			},
			Contract: "argus.stage_result.v1alpha1",
		},
	}
	request.Capability.Authority.AllowedTools = []string{"read_file"}
	digest, err := DigestExecutorCapability(request.Capability)
	if err != nil {
		t.Fatal(err)
	}
	request.Capability.SHA256 = digest

	sealed, err := SealStageExecutionRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	request.Upstream[0].Contract = "argus.mutated.v1alpha1"
	request.Capability.Authority.AllowedTools[0] = "mutated_tool"
	if sealed.Upstream[0].Contract == request.Upstream[0].Contract ||
		sealed.Capability.Authority.AllowedTools[0] ==
			request.Capability.Authority.AllowedTools[0] {
		t.Fatal("sealed request retained caller-owned slice aliases")
	}
}

func TestStageExecutionRequestRequiresExactAgentStagePlanContract(t *testing.T) {
	request := validStageExecutionRequest(t)
	request.Plan.Contract = "argus.other_plan.v1alpha1"
	if err := request.Validate(); err == nil ||
		!strings.Contains(err.Error(), "plan.contract must be") {
		t.Fatalf("Validate() error = %v, want exact plan contract rejection", err)
	}
}

func TestStageExecutionContractsRequireExactArtifactContracts(t *testing.T) {
	request := validStageExecutionRequest(t)

	wrongSnapshot := request
	wrongSnapshot.ExecutionSnapshot.Contract = "argus.other_snapshot.v1alpha1"
	if err := wrongSnapshot.Validate(); err == nil ||
		!strings.Contains(err.Error(), "execution_snapshot.contract must be") {
		t.Fatalf("Validate() wrong execution snapshot contract error = %v", err)
	}

	wrongReviewInput := request
	wrongReviewInput.ReviewInput.Contract = "argus.other_input.v1alpha1"
	if err := wrongReviewInput.Validate(); err == nil ||
		!strings.Contains(err.Error(), "review_input.contract must be") {
		t.Fatalf("Validate() wrong review input contract error = %v", err)
	}

	result := validStageExecutionResult(request)
	trace := ArtifactBinding{
		Ref: ContentRef{
			URI:       "artifact://hailix/trace-manifest",
			SHA256:    strings.Repeat("b", 64),
			SizeBytes: 1,
		},
		Contract: "hailix.other_trace.v1alpha1",
	}
	result.TraceManifest = &trace
	if err := result.Validate(); err == nil ||
		!strings.Contains(err.Error(), "trace_manifest.contract must be") {
		t.Fatalf("Validate() wrong trace manifest contract error = %v", err)
	}
	receipts := ArtifactBinding{
		Ref: ContentRef{
			URI:       "artifact://hailix/agent-execution-receipts",
			SHA256:    strings.Repeat("c", 64),
			SizeBytes: 1,
		},
		Contract: "argus.other_receipts.v1alpha1",
	}
	result = validStageExecutionResult(request)
	result.AgentExecutionReceipts = &receipts
	if err := result.Validate(); err == nil ||
		!strings.Contains(err.Error(), "agent_execution_receipts.contract must be") {
		t.Fatalf("Validate() wrong execution receipt contract error = %v", err)
	}

	receipts.Contract = AgentExecutionReceiptCollectionSchemaVersion
	failed := result
	failed.Status = StageExecutionFailed
	failed.Output = nil
	failed.Failure = &StageExecutionFailure{Code: "failed", Message: "failed"}
	failed.Completeness = "partial"
	failed.CompletenessNotes = []string{"failed"}
	if err := failed.Validate(); err == nil ||
		!strings.Contains(err.Error(), "must be present together") {
		t.Fatalf("Validate() failed result with unpaired receipts error = %v", err)
	}
	taskEvidence := ArtifactBinding{
		Ref: ContentRef{
			URI:       "artifact://hailix/agent-task-evidence",
			SHA256:    strings.Repeat("d", 64),
			SizeBytes: 1,
		},
		Contract: AgentReviewTaskEvidenceCollectionSchemaVersion,
	}
	failed.AgentTaskEvidence = &taskEvidence
	if err := failed.Validate(); err != nil {
		t.Fatalf("Validate() rejected paired failed diagnostic evidence: %v", err)
	}
	rawCandidates := ArtifactBinding{
		Ref: ContentRef{
			URI:       "artifact://hailix/agent-raw-candidates",
			SHA256:    strings.Repeat("e", 64),
			SizeBytes: 1,
		},
		Contract: AgentReviewRawCandidateCollectionSchemaVersion,
	}
	failed.AgentRawCandidates = &rawCandidates
	if err := failed.Validate(); err == nil ||
		!strings.Contains(err.Error(), "forbids output/raw candidate evidence") {
		t.Fatalf("Validate() failed result with raw candidates error = %v", err)
	}
}

func TestStageExecutionContractsRejectUnsafeWireIntegers(t *testing.T) {
	maxSafeUint := stageExecutionMaxJSONInteger
	maxSafeInt := int(maxSafeUint)
	if uint64(maxSafeInt) != maxSafeUint {
		t.Skip("platform int cannot represent the JSON-safe boundary")
	}
	unsafeInt := maxSafeInt + 1

	requestTests := []struct {
		name   string
		mutate func(*StageExecutionRequest)
	}{
		{"attempt", func(request *StageExecutionRequest) { request.Attempt = unsafeInt }},
		{"generation", func(request *StageExecutionRequest) { request.Generation = unsafeInt }},
		{"fencing token", func(request *StageExecutionRequest) {
			request.FencingToken = maxSafeUint + 1
		}},
		{"artifact size", func(request *StageExecutionRequest) {
			request.Plan.Ref.SizeBytes = int64(maxSafeUint + 1)
		}},
	}
	for _, test := range requestTests {
		t.Run("request "+test.name, func(t *testing.T) {
			request := validStageExecutionRequest(t)
			test.mutate(&request)
			if err := request.Validate(); err == nil ||
				!strings.Contains(err.Error(), "JSON safe integer") {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}

	resultTests := []struct {
		name   string
		mutate func(*StageExecutionResult)
	}{
		{"attempt", func(result *StageExecutionResult) { result.Attempt = unsafeInt }},
		{"generation", func(result *StageExecutionResult) { result.Generation = unsafeInt }},
		{"fencing token", func(result *StageExecutionResult) {
			result.FencingToken = maxSafeUint + 1
		}},
		{"artifact size", func(result *StageExecutionResult) {
			result.Output.Ref.SizeBytes = int64(maxSafeUint + 1)
		}},
	}
	for _, test := range resultTests {
		t.Run("result "+test.name, func(t *testing.T) {
			result := validStageExecutionResult(validStageExecutionRequest(t))
			test.mutate(&result)
			if err := result.Validate(); err == nil ||
				!strings.Contains(err.Error(), "JSON safe integer") {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}

	boundary := stageExecutionRequestContent(t)
	boundary.Attempt = maxSafeInt
	boundary.Generation = maxSafeInt
	boundary.FencingToken = maxSafeUint
	boundary.Plan.Ref.SizeBytes = int64(maxSafeUint)
	sealed, err := SealStageExecutionRequest(boundary)
	if err != nil {
		t.Fatalf("SealStageExecutionRequest(max safe integers) error = %v", err)
	}
	result := validStageExecutionResult(sealed)
	result.Output.Ref.SizeBytes = int64(maxSafeUint)
	if err := result.Validate(); err != nil {
		t.Fatalf("StageExecutionResult.Validate(max safe integers) error = %v", err)
	}
}

func TestStageExecutionResultBoundsAndValidatesCompletenessNotes(t *testing.T) {
	request := validStageExecutionRequest(t)
	for _, note := range []string{"", " leading", "control\n", strings.Repeat("x", 257)} {
		result := validStageExecutionResult(request)
		result.Completeness = "partial"
		result.CompletenessNotes = []string{note}
		if err := result.Validate(); err == nil ||
			!strings.Contains(err.Error(), "completeness_notes[0]") {
			t.Fatalf("note %q Validate() error = %v", note, err)
		}
	}
	result := validStageExecutionResult(request)
	result.Completeness = "partial"
	result.CompletenessNotes = make(
		[]string,
		stageExecutionMaxCompletenessNotes+1,
	)
	for index := range result.CompletenessNotes {
		result.CompletenessNotes[index] = "bounded-note"
	}
	if err := result.Validate(); err == nil ||
		!strings.Contains(err.Error(), "at most") {
		t.Fatalf("oversized completeness_notes Validate() error = %v", err)
	}
}

func TestStageExecutionResultRequiresNonEmptyJSONArtifacts(t *testing.T) {
	request := validStageExecutionRequest(t)
	result := validStageExecutionResult(request)
	result.Output.Ref.SizeBytes = 0
	if err := result.Validate(); err == nil ||
		!strings.Contains(err.Error(), "output.ref.size_bytes") {
		t.Fatalf("zero-byte output Validate() error = %v", err)
	}

	result = validStageExecutionResult(request)
	trace := ArtifactBinding{
		Ref: ContentRef{
			URI:       "artifact://hailix/trace-manifest",
			SHA256:    strings.Repeat("b", 64),
			SizeBytes: 0,
		},
		Contract: StageExecutionTraceManifestContract,
	}
	result.TraceManifest = &trace
	if err := result.Validate(); err == nil ||
		!strings.Contains(err.Error(), "trace_manifest.ref.size_bytes") {
		t.Fatalf("zero-byte trace manifest Validate() error = %v", err)
	}
}

func TestStageExecutionResultRequiresAndEchoesRequestSHA256(t *testing.T) {
	request := validStageExecutionRequest(t)
	result := validStageExecutionResult(request)

	missing := result
	missing.RequestSHA256 = ""
	if err := missing.Validate(); err == nil || !strings.Contains(err.Error(), "request_sha256") {
		t.Fatalf("Validate() missing request sha error = %v", err)
	}

	wrong := result
	wrong.RequestSHA256 = strings.Repeat("b", 64)
	if err := wrong.Validate(); err != nil {
		t.Fatalf("Validate() wrong but well-formed request sha error = %v", err)
	}
	if err := ValidateStageExecutionResultBinding(request, wrong); err == nil ||
		!strings.Contains(err.Error(), "request_sha256 does not match exact request") {
		t.Fatalf("binding wrong request sha error = %v", err)
	}
}

func TestDecodeStageExecutionRequestRejectsUnknownField(t *testing.T) {
	request := validStageExecutionRequest(t)
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data[:len(data)-1], []byte(`,"unknown":true}`)...)
	if _, err := DecodeStageExecutionRequest(data); err == nil {
		t.Fatal("DecodeStageExecutionRequest() accepted unknown field")
	}
}

func TestDecodeStageExecutionContractsRejectNullDuplicateAndTrailingJSON(t *testing.T) {
	request := validStageExecutionRequest(t)
	requestData, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	resultData, err := json.Marshal(validStageExecutionResult(request))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		decode func([]byte) error
		data   []byte
	}{
		{
			name: "null request",
			decode: func(data []byte) error {
				_, err := DecodeStageExecutionRequest(data)
				return err
			},
			data: []byte("null"),
		},
		{
			name: "null plan",
			decode: func(data []byte) error {
				_, err := DecodeStageExecutionRequest(data)
				return err
			},
			data: replaceStageExecutionJSONField(t, requestData, "plan", nil),
		},
		{
			name: "null upstream",
			decode: func(data []byte) error {
				_, err := DecodeStageExecutionRequest(data)
				return err
			},
			data: replaceStageExecutionJSONField(t, requestData, "upstream", nil),
		},
		{
			name: "duplicate request field",
			decode: func(data []byte) error {
				_, err := DecodeStageExecutionRequest(data)
				return err
			},
			data: append(requestData[:len(requestData)-1], []byte(`,"execution_id":"other"}`)...),
		},
		{
			name: "trailing request JSON",
			decode: func(data []byte) error {
				_, err := DecodeStageExecutionRequest(data)
				return err
			},
			data: append(append([]byte{}, requestData...), []byte(" true")...),
		},
		{
			name: "null result",
			decode: func(data []byte) error {
				_, err := DecodeStageExecutionResult(data)
				return err
			},
			data: []byte("null"),
		},
		{
			name: "unknown result field",
			decode: func(data []byte) error {
				_, err := DecodeStageExecutionResult(data)
				return err
			},
			data: append(resultData[:len(resultData)-1], []byte(`,"unknown":true}`)...),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.decode(test.data); err == nil {
				t.Fatal("decode accepted invalid JSON contract")
			}
		})
	}
}

func validStageExecutionRequest(t *testing.T) StageExecutionRequest {
	t.Helper()
	request := stageExecutionRequestContent(t)
	sealed, err := SealStageExecutionRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func stageExecutionRequestContent(t *testing.T) StageExecutionRequest {
	t.Helper()
	capability := ExecutorCapabilitySnapshot{
		RuntimeKind: "codex-acp", RuntimeID: "worker-1", RuntimeRevision: "1",
		RuntimeSHA256: testDigest, BuildIdentity: "build-1",
		Authority: ExecutionAuthority{
			AllowedTools:       []string{},
			ModelEgress:        AgentStageModelEgressProviderBrokerOnly,
			ToolNetwork:        AgentStageSideEffectsDeny,
			WorkspaceReads:     AgentStageWorkspaceReadFrozenInputOnly,
			WorkspaceWrites:    AgentStageSideEffectsDeny,
			RemoteWrites:       AgentStageSideEffectsDeny,
			MaxDelegationDepth: 0,
		},
		Trust: testExecutorTrust(testDigest),
	}
	digest, err := DigestExecutorCapability(capability)
	if err != nil {
		t.Fatal(err)
	}
	capability.SHA256 = digest
	return StageExecutionRequest{
		SchemaVersion: StageExecutionRequestSchemaVersion,
		ExecutionID:   "execution-1", ReviewRunID: "run-1",
		TenantID: "tenant-1", WorkspaceID: "workspace-1",
		WorkloadID: "workload-1", LeaseID: "lease-1", LeaseWorkerID: "worker-1",
		Stage:   VersionedRef{ID: "detect", Revision: "1", SHA256: testDigest},
		Attempt: 1, Generation: 1, FencingToken: 1,
		IdempotencyKey: "execution-1-detect-1",
		Plan: ArtifactBinding{
			Ref: ContentRef{
				URI:    "artifact://local/sha256/agent-stage-plan",
				SHA256: testDigest, SizeBytes: 10,
			},
			Contract: AgentStagePlanSchemaVersion,
		},
		ExecutionSnapshot: ArtifactBinding{
			Ref: ContentRef{
				URI: "artifact://local/sha256/snapshot", SHA256: testDigest, SizeBytes: 10,
			},
			Contract: StageExecutionSnapshotContract,
		},
		ReviewInput: ArtifactBinding{
			Ref: ContentRef{
				URI: "artifact://local/sha256/input", SHA256: testDigest, SizeBytes: 10,
			},
			Contract: StageExecutionReviewInputContract,
		},
		Upstream:       []ArtifactBinding{},
		OutputContract: "argus.stage_result.v1alpha1",
		Capability:     capability,
		Deadline:       time.Date(2026, time.July, 27, 10, 0, 0, 0, time.UTC),
		SideEffects:    "deny",
	}
}

func testExecutorTrust(digest string) ExecutorTrust {
	return ExecutorTrust{
		Authority: ExecutorTrustAuthorityLocalHost,
		CapabilityVerifier: VersionedRef{
			ID: "test-capability-verifier", Revision: "1", SHA256: digest,
		},
		CallbackVerifier: VersionedRef{
			ID: "test-callback-verifier", Revision: "1", SHA256: digest,
		},
	}
}

func validStageExecutionResult(request StageExecutionRequest) StageExecutionResult {
	output := ArtifactBinding{
		Ref: ContentRef{
			URI: "artifact://local/sha256/output", SHA256: testDigest, SizeBytes: 10,
		},
		Contract: request.OutputContract,
	}
	return StageExecutionResult{
		SchemaVersion:     StageExecutionResultSchemaVersion,
		RequestSHA256:     request.RequestSHA256,
		ExecutionID:       request.ExecutionID,
		Attempt:           request.Attempt,
		Generation:        request.Generation,
		FencingToken:      request.FencingToken,
		IdempotencyKey:    request.IdempotencyKey,
		Status:            StageExecutionSucceeded,
		Output:            &output,
		Completeness:      "complete",
		CompletenessNotes: []string{},
		CapabilitySHA256:  request.Capability.SHA256,
		RecordedAt:        time.Date(2026, time.July, 27, 10, 1, 0, 0, time.UTC),
	}
}

func replaceStageExecutionJSONField(
	t *testing.T,
	data []byte,
	field string,
	value any,
) []byte {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	object[field] = value
	replaced, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return replaced
}
