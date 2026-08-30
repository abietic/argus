package reviewcore

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var hunkHeader = regexp.MustCompile(
	`^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@(?: .*)?$`,
)

const noNewlineMarker = `\ No newline at end of file`

type addedLine struct {
	Path      string
	NewLine   uint32
	PatchLine uint32
	Text      string
}

type parsedPatch struct {
	Added                  []addedLine
	DeletionContextTargets []addedLine
	Files                  []PatchFileSummary
}

// PatchFileSummary is the strict, offline-verifiable path/hunk identity of one
// file section in a canonical Git patch. PathKnown is false when Git emitted a
// quoted or otherwise non-portable path; callers must not guess authorization.
type PatchFileSummary struct {
	Path      string
	PathKnown bool
	Hunks     int
}

type PatchInspection struct {
	Files      []PatchFileSummary
	TotalHunks int
}

func InspectCanonicalPatch(ctx context.Context, patch string) (PatchInspection, error) {
	parsed, err := parseUnifiedDiffContext(ctx, patch)
	if err != nil {
		return PatchInspection{}, err
	}
	inspection := PatchInspection{
		Files: make([]PatchFileSummary, len(parsed.Files)),
	}
	copy(inspection.Files, parsed.Files)
	for _, file := range inspection.Files {
		inspection.TotalHunks += file.Hunks
	}
	return inspection, nil
}

// parseUnifiedDiffContext accepts canonical git-style unified patches. A file
// with a non-portable or quoted path is skipped as a whole rather than guessed;
// other admitted files in the same immutable patch remain reviewable.
func parseUnifiedDiffContext(ctx context.Context, patch string) (parsedPatch, error) {
	lines := strings.Split(strings.TrimSuffix(patch, "\n"), "\n")
	result := parsedPatch{
		Added: make([]addedLine, 0),
		Files: make([]PatchFileSummary, 0),
	}
	var currentPath string
	var oldPath string
	currentFile := -1
	var newLine uint64
	var oldRemaining uint64
	var newRemaining uint64
	var hunkStartLine uint32
	inHunk := false
	allowNoNewlineMarker := false
	skipCurrent := false
	sawDiff := false
	sawFile := false
	sawHunk := false
	hunkHasAddition := false
	hunkHasDeletion := false
	hunkContextTargets := make([]addedLine, 0)
	finishHunk := func() {
		if hunkHasDeletion && !hunkHasAddition {
			result.DeletionContextTargets = append(
				result.DeletionContextTargets,
				hunkContextTargets...,
			)
		}
		hunkHasAddition = false
		hunkHasDeletion = false
		hunkContextTargets = hunkContextTargets[:0]
	}

	for index, line := range lines {
		if err := checkContext(ctx); err != nil {
			return parsedPatch{}, err
		}
		patchLine := uint32(index + 1)

		if inHunk {
			switch {
			case line == noNewlineMarker:
				if !allowNoNewlineMarker {
					return parsedPatch{}, fmt.Errorf(
						"line %d has a no-newline marker without a preceding hunk body line",
						patchLine,
					)
				}
				allowNoNewlineMarker = false
				continue
			case oldRemaining == 0 && newRemaining == 0:
				if isHunkBodyLine(line) {
					return parsedPatch{}, fmt.Errorf(
						"line %d exceeds the declared counts for hunk at line %d",
						patchLine,
						hunkStartLine,
					)
				}
				finishHunk()
				inHunk = false
				allowNoNewlineMarker = false
			default:
				if strings.HasPrefix(line, "diff --git ") {
					return parsedPatch{}, incompleteHunkError(
						hunkStartLine,
						patchLine,
						"a file boundary",
						oldRemaining,
						newRemaining,
					)
				}
				if strings.HasPrefix(line, "@@") {
					return parsedPatch{}, incompleteHunkError(
						hunkStartLine,
						patchLine,
						"a new hunk",
						oldRemaining,
						newRemaining,
					)
				}
				switch {
				case strings.HasPrefix(line, "+"):
					if newRemaining == 0 {
						return parsedPatch{}, fmt.Errorf(
							"line %d exceeds the declared new-line count for hunk at line %d",
							patchLine,
							hunkStartLine,
						)
					}
					if newLine > math.MaxUint32 {
						return parsedPatch{}, fmt.Errorf(
							"line %d advances beyond the supported new-file line range",
							patchLine,
						)
					}
					if !skipCurrent {
						result.Added = append(result.Added, addedLine{
							Path: currentPath, NewLine: uint32(newLine), PatchLine: patchLine,
							Text: strings.TrimPrefix(line, "+"),
						})
					}
					hunkHasAddition = true
					newLine++
					newRemaining--
				case strings.HasPrefix(line, "-"):
					if oldRemaining == 0 {
						return parsedPatch{}, fmt.Errorf(
							"line %d exceeds the declared old-line count for hunk at line %d",
							patchLine,
							hunkStartLine,
						)
					}
					hunkHasDeletion = true
					oldRemaining--
				case strings.HasPrefix(line, " "):
					if oldRemaining == 0 || newRemaining == 0 {
						return parsedPatch{}, fmt.Errorf(
							"line %d exceeds the declared context-line count for hunk at line %d",
							patchLine,
							hunkStartLine,
						)
					}
					if newLine > math.MaxUint32 {
						return parsedPatch{}, fmt.Errorf(
							"line %d advances beyond the supported new-file line range",
							patchLine,
						)
					}
					if !skipCurrent {
						hunkContextTargets = append(hunkContextTargets, addedLine{
							Path: currentPath, NewLine: uint32(newLine), PatchLine: patchLine,
							Text: strings.TrimPrefix(line, " "),
						})
					}
					newLine++
					oldRemaining--
					newRemaining--
				default:
					return parsedPatch{}, fmt.Errorf(
						"line %d is invalid inside hunk at line %d",
						patchLine,
						hunkStartLine,
					)
				}
				allowNoNewlineMarker = true
				continue
			}
		}

		switch {
		case strings.HasPrefix(line, "diff --git "):
			sawDiff = true
			currentPath = ""
			oldPath = ""
			result.Files = append(result.Files, PatchFileSummary{})
			currentFile = len(result.Files) - 1
			if candidate, ok := parseSimpleDiffTargetPath(line); ok {
				result.Files[currentFile].Path = candidate
				result.Files[currentFile].PathKnown = true
			}
			inHunk = false
			skipCurrent = false
		case strings.HasPrefix(line, "--- "):
			rawPath := strings.TrimPrefix(line, "--- ")
			if candidate, ok := parsePatchSidePath(rawPath, "a/"); ok {
				oldPath = candidate
			}
		case strings.HasPrefix(line, "+++ "):
			rawPath := strings.TrimPrefix(line, "+++ ")
			if rawPath == "/dev/null" {
				currentPath = oldPath
				skipCurrent = currentPath == ""
				if currentFile >= 0 && currentPath != "" {
					result.Files[currentFile].Path = currentPath
					result.Files[currentFile].PathKnown = true
					sawFile = true
				}
				continue
			}
			candidate, ok := parsePatchSidePath(rawPath, "b/")
			if !ok {
				currentPath = ""
				skipCurrent = true
				continue
			}
			currentPath = candidate
			if currentFile >= 0 {
				result.Files[currentFile].Path = currentPath
				result.Files[currentFile].PathKnown = true
			}
			skipCurrent = false
			sawFile = true
			inHunk = false
		case strings.HasPrefix(line, "rename to ") ||
			strings.HasPrefix(line, "copy to "):
			prefix := "rename to "
			if strings.HasPrefix(line, "copy to ") {
				prefix = "copy to "
			}
			candidate, ok := decodeGitPath(strings.TrimPrefix(line, prefix))
			if ok {
				currentPath = candidate
				if currentFile >= 0 {
					result.Files[currentFile].Path = candidate
					result.Files[currentFile].PathKnown = true
				}
			}
		case strings.HasPrefix(line, "@@"):
			if currentPath == "" && !skipCurrent {
				return parsedPatch{}, fmt.Errorf("line %d starts a hunk without a target file", patchLine)
			}
			matches := hunkHeader.FindStringSubmatch(line)
			if matches == nil {
				return parsedPatch{}, fmt.Errorf("line %d has an invalid hunk header", patchLine)
			}
			oldStart, err := parseHunkNumber(matches[1], "old-file start", patchLine)
			if err != nil {
				return parsedPatch{}, err
			}
			oldCount, err := parseHunkCount(matches[2], "old-file count", patchLine)
			if err != nil {
				return parsedPatch{}, err
			}
			newStart, err := parseHunkNumber(matches[3], "new-file start", patchLine)
			if err != nil {
				return parsedPatch{}, err
			}
			newCount, err := parseHunkCount(matches[4], "new-file count", patchLine)
			if err != nil {
				return parsedPatch{}, err
			}
			if oldCount == 0 && newCount == 0 {
				return parsedPatch{}, fmt.Errorf("line %d declares an empty hunk", patchLine)
			}
			if err := validateHunkRange("old-file", oldStart, oldCount, patchLine); err != nil {
				return parsedPatch{}, err
			}
			if err := validateHunkRange("new-file", newStart, newCount, patchLine); err != nil {
				return parsedPatch{}, err
			}
			newLine = newStart
			oldRemaining = oldCount
			newRemaining = newCount
			hunkStartLine = patchLine
			inHunk = true
			allowNoNewlineMarker = false
			hunkHasAddition = false
			hunkHasDeletion = false
			hunkContextTargets = hunkContextTargets[:0]
			if currentFile < 0 {
				return parsedPatch{}, fmt.Errorf("line %d starts a hunk before a file diff", patchLine)
			}
			result.Files[currentFile].Hunks++
			if !skipCurrent {
				sawHunk = true
			}
		case isHunkBodyLine(line):
			return parsedPatch{}, fmt.Errorf("line %d has hunk body content outside a hunk", patchLine)
		case line == noNewlineMarker:
			return parsedPatch{}, fmt.Errorf("line %d has a no-newline marker outside a hunk", patchLine)
		}
	}
	if inHunk && (oldRemaining != 0 || newRemaining != 0) {
		return parsedPatch{}, fmt.Errorf(
			"hunk at line %d reaches EOF before its declared counts are consumed "+
				"(old remaining %d, new remaining %d)",
			hunkStartLine,
			oldRemaining,
			newRemaining,
		)
	}
	if inHunk {
		finishHunk()
	}
	if !sawDiff {
		return parsedPatch{}, fmt.Errorf("patch has no canonical file diff")
	}
	if sawFile && !sawHunk {
		return parsedPatch{}, fmt.Errorf("patch has no unified diff hunk")
	}
	return result, nil
}

func parseHunkCount(raw string, name string, patchLine uint32) (uint64, error) {
	if raw == "" {
		return 1, nil
	}
	return parseHunkNumber(raw, name, patchLine)
}

func parseHunkNumber(raw string, name string, patchLine uint32) (uint64, error) {
	parsed, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("line %d has an invalid %s: %w", patchLine, name, err)
	}
	return parsed, nil
}

func validateHunkRange(name string, start uint64, count uint64, patchLine uint32) error {
	if count == 0 {
		return nil
	}
	if start == 0 {
		return fmt.Errorf("line %d has a zero %s start with a non-zero count", patchLine, name)
	}
	if start+count-1 > math.MaxUint32 {
		return fmt.Errorf("line %d has an overflowing %s range", patchLine, name)
	}
	return nil
}

func incompleteHunkError(
	hunkLine uint32,
	boundaryLine uint32,
	boundary string,
	oldRemaining uint64,
	newRemaining uint64,
) error {
	return fmt.Errorf(
		"line %d starts %s before hunk at line %d consumes its declared counts "+
			"(old remaining %d, new remaining %d)",
		boundaryLine,
		boundary,
		hunkLine,
		oldRemaining,
		newRemaining,
	)
}

func isHunkBodyLine(line string) bool {
	return strings.HasPrefix(line, "+") ||
		strings.HasPrefix(line, "-") ||
		strings.HasPrefix(line, " ")
}

func parseSimpleDiffTargetPath(line string) (string, bool) {
	const prefix = "diff --git "
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	remainder := strings.TrimPrefix(line, prefix)

	// Git C-quotes paths containing quotes, control bytes, or non-portable
	// bytes. The second side is authoritative for the reviewed target.
	if boundary := strings.LastIndex(remainder, ` "b/`); boundary >= 0 {
		if candidate, rest, ok := consumeQuotedGitPath(remainder[boundary+1:]); ok &&
			rest == "" {
			return parseDecodedPatchSidePath(candidate, "b/")
		}
	}
	if strings.HasPrefix(remainder, `"`) {
		_, rest, ok := consumeQuotedGitPath(remainder)
		if !ok || !strings.HasPrefix(rest, " ") {
			return "", false
		}
		return parsePatchSidePath(strings.TrimPrefix(rest, " "), "b/")
	}

	// Git does not quote ordinary spaces. Avoid guessing the separator: for a
	// non-rename file, the only valid boundary is the one where the a/ and b/
	// path bodies are identical. Rename/copy metadata later supplies the new
	// path when the two sides legitimately differ.
	for searchFrom := 0; searchFrom < len(remainder); {
		offset := strings.Index(remainder[searchFrom:], " b/")
		if offset < 0 {
			break
		}
		boundary := searchFrom + offset
		oldCandidate, oldOK := parsePatchSidePath(remainder[:boundary], "a/")
		newCandidate, newOK := parsePatchSidePath(remainder[boundary+1:], "b/")
		if oldOK && newOK && oldCandidate == newCandidate {
			return newCandidate, true
		}
		searchFrom = boundary + len(" b/")
	}
	return "", false
}

func parsePatchSidePath(rawPath string, prefix string) (string, bool) {
	decoded, ok := decodeGitPath(rawPath)
	if !ok {
		return "", false
	}
	return parseDecodedPatchSidePath(decoded, prefix)
}

func parseDecodedPatchSidePath(decoded string, prefix string) (string, bool) {
	if !strings.HasPrefix(decoded, prefix) {
		return "", false
	}
	candidate := strings.TrimPrefix(decoded, prefix)
	if err := validateRepositoryPath("patch path", candidate); err != nil {
		return "", false
	}
	return candidate, true
}

func decodeGitPath(rawPath string) (string, bool) {
	if rawPath == "" || strings.ContainsRune(rawPath, '\t') {
		return "", false
	}
	if !strings.HasPrefix(rawPath, `"`) {
		if err := validateRepositoryPath("patch path", rawPath); err != nil {
			return "", false
		}
		return rawPath, true
	}
	decoded, rest, ok := consumeQuotedGitPath(rawPath)
	if !ok || rest != "" {
		return "", false
	}
	if err := validateRepositoryPath("patch path", decoded); err != nil {
		return "", false
	}
	return decoded, true
}

func consumeQuotedGitPath(value string) (string, string, bool) {
	if !strings.HasPrefix(value, `"`) {
		return "", value, false
	}
	escaped := false
	for index := 1; index < len(value); index++ {
		switch value[index] {
		case '\\':
			escaped = !escaped
		case '"':
			if escaped {
				escaped = false
				continue
			}
			decoded, err := strconv.Unquote(value[:index+1])
			if err != nil {
				return "", value, false
			}
			return decoded, value[index+1:], true
		default:
			escaped = false
		}
	}
	return "", value, false
}
