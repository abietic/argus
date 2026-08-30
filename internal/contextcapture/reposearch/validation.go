package reposearch

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
)

var (
	exactCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	sha256Pattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func DecodeArtifact(data []byte) (Artifact, error) {
	if len(data) == 0 || int64(len(data)) > DefaultMaxArtifactBytes {
		return Artifact{}, fmt.Errorf("repository search context bytes must be non-empty and at most %d bytes", DefaultMaxArtifactBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var artifact Artifact
	if err := decoder.Decode(&artifact); err != nil {
		return Artifact{}, fmt.Errorf("decode repository search context: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Artifact{}, fmt.Errorf("decode repository search context: trailing JSON value")
		}
		return Artifact{}, fmt.Errorf("decode repository search context trailing data: %w", err)
	}
	if err := artifact.Validate(); err != nil {
		return Artifact{}, fmt.Errorf("validate repository search context: %w", err)
	}
	return artifact, nil
}

func (artifact Artifact) Validate() error {
	if artifact.SchemaVersion != SchemaVersion || artifact.ProviderID != ProviderID ||
		artifact.ProviderRevision != ProviderRevision {
		return fmt.Errorf("unsupported repository search context identity")
	}
	if !exactCommitPattern.MatchString(artifact.CommitOID) {
		return fmt.Errorf("commit_oid must be an exact lowercase Git object ID")
	}
	if artifact.TargetPaths == nil || artifact.TargetRanges == nil || artifact.Queries == nil || artifact.Matches == nil || artifact.Gaps == nil {
		return fmt.Errorf("repository search context collections must be explicit arrays")
	}
	targets := make(map[string]struct{}, len(artifact.TargetPaths))
	for index, value := range artifact.TargetPaths {
		if !validRepositoryPath(value) || index > 0 && value <= artifact.TargetPaths[index-1] {
			return fmt.Errorf("target_paths must be valid and uniquely sorted")
		}
		targets[value] = struct{}{}
	}
	previousRange := TargetRange{}
	for index, lineRange := range artifact.TargetRanges {
		if _, exists := targets[lineRange.Path]; !exists || lineRange.StartLine == 0 || lineRange.EndLine < lineRange.StartLine {
			return fmt.Errorf("target_ranges[%d] must bind a valid target path and line range", index)
		}
		if index > 0 && !targetRangeLess(previousRange, lineRange) {
			return fmt.Errorf("target_ranges must be uniquely sorted")
		}
		if index > 0 && previousRange.Path == lineRange.Path && lineRange.StartLine <= previousRange.EndLine {
			return fmt.Errorf("target_ranges must not overlap")
		}
		previousRange = lineRange
	}
	if len(artifact.TargetRanges) == 0 && artifact.QueryHaloLines != 0 ||
		len(artifact.TargetRanges) > 0 && artifact.QueryHaloLines != DefaultSelectionHaloLines {
		return fmt.Errorf("query_halo_lines does not match target range mode")
	}
	queries := make(map[string]Query, len(artifact.Queries))
	previousTerm := ""
	repositoryOccurrences := 0
	for index, query := range artifact.Queries {
		if !identifierPattern.MatchString(query.Term) || identifierPattern.FindString(query.Term) != query.Term ||
			!usefulTerm(query.Term) || query.Term <= previousTerm {
			return fmt.Errorf("queries[%d].term is invalid or not uniquely sorted", index)
		}
		if query.SourcePaths == nil || query.TargetOccurrences <= 0 || query.RepositoryOccurrences <= 0 ||
			query.MatchedFiles <= 0 || query.MatchedFiles > query.RepositoryOccurrences {
			return fmt.Errorf("queries[%d] counters are invalid", index)
		}
		for sourceIndex, sourcePath := range query.SourcePaths {
			if _, targeted := targets[sourcePath]; !targeted ||
				sourceIndex > 0 && sourcePath <= query.SourcePaths[sourceIndex-1] {
				return fmt.Errorf("queries[%d].source_paths must be a sorted target subset", index)
			}
		}
		if len(query.SourcePaths) == 0 {
			return fmt.Errorf("queries[%d].source_paths must not be empty", index)
		}
		queries[query.Term] = query
		previousTerm = query.Term
		repositoryOccurrences += query.RepositoryOccurrences
	}
	previousMatch := ""
	matchCounts := make(map[string]int)
	matchFiles := make(map[string]map[string]struct{})
	for index, match := range artifact.Matches {
		query, exists := queries[match.Term]
		if !exists || !validRepositoryPath(match.Path) || match.Line == 0 || match.Column == 0 ||
			!sha256Pattern.MatchString(match.LineSHA256) || !validText(match.LineText) {
			return fmt.Errorf("matches[%d] is invalid", index)
		}
		if _, targeted := targets[match.Path]; targeted {
			return fmt.Errorf("matches[%d] duplicates target content", index)
		}
		if len([]byte(match.LineText)) > DefaultMaxLineTextBytes || match.LineText == "" ||
			match.LineTextStart == 0 || match.LineTextStart > match.Column {
			return fmt.Errorf("matches[%d].line_text is invalid", index)
		}
		offset := int(match.Column - match.LineTextStart)
		if offset < 0 || offset+len(match.Term) > len([]byte(match.LineText)) ||
			string([]byte(match.LineText)[offset:offset+len(match.Term)]) != match.Term {
			return fmt.Errorf("matches[%d].line_text does not contain the bound term", index)
		}
		if !match.LineTextTruncated {
			if match.LineTextStart != 1 {
				return fmt.Errorf("matches[%d] full line must start at column 1", index)
			}
			digest := sha256.Sum256([]byte(match.LineText))
			if match.LineSHA256 != hex.EncodeToString(digest[:]) {
				return fmt.Errorf("matches[%d].line_sha256 does not bind line_text", index)
			}
		} else if len([]byte(match.LineText)) != DefaultMaxLineTextBytes {
			return fmt.Errorf("matches[%d] truncated line window must consume its full budget", index)
		}
		key := matchKey(match)
		if key <= previousMatch {
			return fmt.Errorf("matches must be uniquely sorted")
		}
		previousMatch = key
		matchCounts[match.Term]++
		if matchFiles[match.Term] == nil {
			matchFiles[match.Term] = map[string]struct{}{}
		}
		matchFiles[match.Term][match.Path] = struct{}{}
		if matchCounts[match.Term] > query.RepositoryOccurrences || len(matchFiles[match.Term]) > query.MatchedFiles {
			return fmt.Errorf("matches[%d] exceeds query counters", index)
		}
	}
	counters := []int{
		artifact.Coverage.FilesMatched, artifact.Coverage.FilesRetained, artifact.Coverage.FilesRead,
		artifact.Coverage.FilesUnavailable, artifact.Coverage.FilesSensitiveSkipped,
		artifact.Coverage.TargetFilesRequested, artifact.Coverage.TargetFilesFound,
		artifact.Coverage.CandidateTerms, artifact.Coverage.QueriesEmitted,
		artifact.Coverage.RepositoryOccurrences, artifact.Coverage.MatchesEmitted,
	}
	if slices.ContainsFunc(counters, func(value int) bool { return value < 0 }) ||
		artifact.Coverage.FilesRetained > artifact.Coverage.FilesMatched ||
		artifact.Coverage.FilesRead+artifact.Coverage.FilesUnavailable+artifact.Coverage.FilesSensitiveSkipped != artifact.Coverage.FilesRetained ||
		artifact.Coverage.TargetFilesFound > artifact.Coverage.TargetFilesRequested ||
		artifact.Coverage.QueriesEmitted != len(artifact.Queries) ||
		artifact.Coverage.QueriesEmitted > artifact.Coverage.CandidateTerms ||
		artifact.Coverage.RepositoryOccurrences != repositoryOccurrences ||
		artifact.Coverage.MatchesEmitted != len(artifact.Matches) ||
		artifact.Coverage.MatchesEmitted > artifact.Coverage.RepositoryOccurrences {
		return fmt.Errorf("coverage counters do not reconcile")
	}
	previousGap := ""
	for index, gap := range artifact.Gaps {
		switch gap.Code {
		case "artifact_budget_exceeded", "file_binary_file", "file_file_size_exceeded",
			"file_non_canonical_line_endings",
			"file_non_regular_file", "file_submodule_not_allowed", "file_symlink_not_allowed",
			"file_target_policy_excluded", "file_unavailable", "file_unsupported_text_encoding",
			"match_budget_exceeded", "no_repository_matches", "query_budget_exceeded",
			"query_candidate_budget_exceeded", "sensitive_credential_file", "sensitive_environment_file",
			"sensitive_key_file", "sensitive_state_file", "source_listing_partial",
			"target_file_unavailable", "target_no_search_terms":
		default:
			return fmt.Errorf("gaps[%d] has unsupported code %q", index, gap.Code)
		}
		if gap.Path != "" && !validRepositoryPath(gap.Path) || gap.Term != "" && !usefulTerm(gap.Term) {
			return fmt.Errorf("gaps[%d] has invalid path or term", index)
		}
		switch gap.Code {
		case "file_binary_file", "file_file_size_exceeded", "file_non_canonical_line_endings", "file_non_regular_file",
			"file_submodule_not_allowed", "file_symlink_not_allowed", "file_target_policy_excluded",
			"file_unavailable", "file_unsupported_text_encoding", "sensitive_credential_file",
			"sensitive_environment_file", "sensitive_key_file", "sensitive_state_file",
			"target_file_unavailable":
			if gap.Path == "" || gap.Term != "" {
				return fmt.Errorf("gaps[%d] requires only path evidence", index)
			}
		case "match_budget_exceeded":
			if gap.Path != "" || gap.Term == "" {
				return fmt.Errorf("gaps[%d] requires only term evidence", index)
			}
		default:
			if gap.Path != "" || gap.Term != "" {
				return fmt.Errorf("gaps[%d] does not accept path or term evidence", index)
			}
		}
		key := gapKey(gap)
		if key <= previousGap {
			return fmt.Errorf("gaps must be uniquely sorted")
		}
		previousGap = key
	}
	truncationGaps := []string{
		"artifact_budget_exceeded", "match_budget_exceeded", "query_budget_exceeded",
		"query_candidate_budget_exceeded", "source_listing_partial",
	}
	expectedTruncated := slices.ContainsFunc(truncationGaps, func(code string) bool { return hasGap(artifact.Gaps, code) })
	if artifact.Coverage.Truncated != expectedTruncated {
		return fmt.Errorf("truncated coverage does not match budget gaps")
	}
	return nil
}
