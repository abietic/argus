package gitadapter

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxGitPathBytes = 1 << 20

type nameStatusCollector struct {
	maxFiles int

	token      []byte
	wantPaths  int
	status     ChangeStatus
	similarity *int
	paths      []gitPath

	total    int
	files    []FileChange
	parseErr error
}

func newNameStatusCollector(maxFiles int) *nameStatusCollector {
	return &nameStatusCollector{
		maxFiles: maxFiles,
		files:    make([]FileChange, 0, min(maxFiles, 256)),
	}
}

func (collector *nameStatusCollector) Write(data []byte) (int, error) {
	originalLength := len(data)
	if collector.parseErr != nil {
		return originalLength, nil
	}
	for len(data) > 0 {
		index := bytes.IndexByte(data, 0)
		if index < 0 {
			collector.appendToken(data)
			return originalLength, nil
		}
		collector.appendToken(data[:index])
		if collector.parseErr != nil {
			return originalLength, nil
		}
		collector.consumeToken(collector.token)
		collector.token = collector.token[:0]
		if collector.parseErr != nil {
			return originalLength, nil
		}
		data = data[index+1:]
	}
	return originalLength, nil
}

func (collector *nameStatusCollector) appendToken(data []byte) {
	if len(collector.token)+len(data) > maxGitPathBytes {
		collector.parseErr = fmt.Errorf("git name-status token exceeds %d bytes", maxGitPathBytes)
		collector.token = nil
		return
	}
	collector.token = append(collector.token, data...)
}

func (collector *nameStatusCollector) consumeToken(token []byte) {
	if collector.wantPaths == 0 {
		status, similarity, paths, err := parseNameStatus(string(token))
		if err != nil {
			collector.parseErr = err
			return
		}
		collector.status = status
		collector.similarity = similarity
		collector.wantPaths = paths
		collector.paths = collector.paths[:0]
		return
	}
	if len(token) == 0 {
		collector.parseErr = fmt.Errorf("git path is empty")
		return
	}
	collector.paths = append(collector.paths, newGitPath(token))
	if len(collector.paths) != collector.wantPaths {
		return
	}

	change := FileChange{
		Status:            collector.status,
		SimilarityPercent: collector.similarity,
		Included:          true,
		SkippedReasons:    []ReasonCode{},
	}
	if collector.wantPaths == 2 {
		change.OldPath = collector.paths[0].value
		change.OldPathSHA256 = collector.paths[0].sha256
		change.Path = collector.paths[1].value
		change.PathSHA256 = collector.paths[1].sha256
	} else {
		change.Path = collector.paths[0].value
		change.PathSHA256 = collector.paths[0].sha256
	}
	portable := collector.paths[len(collector.paths)-1].portable
	if collector.wantPaths == 2 {
		portable = portable && collector.paths[0].portable
	}
	if portable {
		change.Language = DetectLanguage(change.Path)
	} else {
		change.Included = false
		change.SkippedReasons = append(change.SkippedReasons, ReasonUnsafeRepositoryPath)
		change.Language = "unknown"
	}
	collector.total++
	if len(collector.files) < collector.maxFiles {
		collector.files = append(collector.files, change)
	}
	collector.wantPaths = 0
	collector.paths = collector.paths[:0]
	collector.similarity = nil
}

type gitPath struct {
	value    string
	sha256   string
	portable bool
}

func newGitPath(raw []byte) gitPath {
	value := ""
	if utf8.Valid(raw) {
		value = string(raw)
	}
	return gitPath{
		value:    value,
		sha256:   sha256Hex(raw),
		portable: isPortableRepositoryPath(value),
	}
}

func isPortableRepositoryPath(value string) bool {
	if value == "" || !utf8.ValidString(value) ||
		strings.ContainsAny(value, `\:`) ||
		value != path.Clean(value) ||
		path.IsAbs(value) ||
		value == "." ||
		value == ".." ||
		strings.HasPrefix(value, "../") {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func (collector *nameStatusCollector) finish() error {
	if collector.parseErr != nil {
		return collector.parseErr
	}
	if len(collector.token) != 0 || collector.wantPaths != 0 {
		return fmt.Errorf("git name-status output ended mid-record")
	}
	return nil
}

func parseNameStatus(raw string) (ChangeStatus, *int, int, error) {
	if raw == "" {
		return "", nil, 0, fmt.Errorf("git name-status contains an empty status")
	}
	switch raw[0] {
	case 'A':
		if len(raw) != 1 {
			break
		}
		return ChangeAdded, nil, 1, nil
	case 'M':
		if len(raw) != 1 {
			break
		}
		return ChangeModified, nil, 1, nil
	case 'D':
		if len(raw) != 1 {
			break
		}
		return ChangeDeleted, nil, 1, nil
	case 'T':
		if len(raw) != 1 {
			break
		}
		return ChangeTypeChange, nil, 1, nil
	case 'U':
		if len(raw) != 1 {
			break
		}
		return ChangeUnmerged, nil, 1, nil
	case 'X', 'B':
		if len(raw) != 1 {
			break
		}
		return ChangeUnknown, nil, 1, nil
	case 'R', 'C':
		score, err := strconv.Atoi(raw[1:])
		if err != nil || score < 0 || score > 100 {
			break
		}
		status := ChangeRenamed
		if raw[0] == 'C' {
			status = ChangeCopied
		}
		return status, &score, 2, nil
	}
	return "", nil, 0, fmt.Errorf("unsupported git name-status value %q", raw)
}

type patchCollector struct {
	maxBytes        int64
	maxTrackedFiles int
	hasher          hash.Hash
	size            int64
	data            []byte
	overflow        bool

	linePrefix  []byte
	currentFile int
	fileHeaders int
	totalHunks  int
	fileHunks   []int
}

var errPatchBudgetExceeded = errors.New("canonical patch exceeds max_patch_bytes")

func newPatchCollector(maxBytes int64, maxTrackedFiles int) *patchCollector {
	return &patchCollector{
		maxBytes:        maxBytes,
		maxTrackedFiles: maxTrackedFiles,
		hasher:          sha256.New(),
		data:            make([]byte, 0, minInt64(maxBytes, 64<<10)),
		currentFile:     -1,
		fileHunks:       make([]int, maxTrackedFiles),
	}
}

func (collector *patchCollector) Write(data []byte) (int, error) {
	originalLength := len(data)
	collector.size += int64(len(data))
	if collector.size > collector.maxBytes {
		collector.overflow = true
		// Never expose a truncated patch prefix. Returning an error stops the
		// untrusted Git stream at the hard boundary instead of hashing it all.
		collector.data = nil
		return 0, errPatchBudgetExceeded
	}
	_, _ = collector.hasher.Write(data)
	collector.data = append(collector.data, data...)
	collector.scanLines(data)
	return originalLength, nil
}

func (collector *patchCollector) scanLines(data []byte) {
	for len(data) > 0 {
		index := bytes.IndexByte(data, '\n')
		segment := data
		if index >= 0 {
			segment = data[:index]
		}
		if len(collector.linePrefix) < 16 {
			remaining := 16 - len(collector.linePrefix)
			if len(segment) < remaining {
				remaining = len(segment)
			}
			collector.linePrefix = append(collector.linePrefix, segment[:remaining]...)
		}
		if index < 0 {
			return
		}
		collector.inspectLine()
		collector.linePrefix = collector.linePrefix[:0]
		data = data[index+1:]
	}
}

func (collector *patchCollector) finish() {
	if len(collector.linePrefix) != 0 {
		collector.inspectLine()
		collector.linePrefix = collector.linePrefix[:0]
	}
}

func (collector *patchCollector) inspectLine() {
	if bytes.HasPrefix(collector.linePrefix, []byte("diff --git ")) {
		collector.currentFile++
		collector.fileHeaders++
		return
	}
	if bytes.HasPrefix(collector.linePrefix, []byte("@@ ")) ||
		bytes.HasPrefix(collector.linePrefix, []byte("@@@ ")) {
		collector.totalHunks++
		if collector.currentFile >= 0 && collector.currentFile < collector.maxTrackedFiles {
			collector.fileHunks[collector.currentFile]++
		}
	}
}

func (collector *patchCollector) sha256() string {
	return fmt.Sprintf("%x", collector.hasher.Sum(nil))
}

type contentCollector struct {
	maxBytes    int64
	hasher      hash.Hash
	size        int64
	data        []byte
	overflow    bool
	containsNUL bool
}

func newContentCollector(maxBytes int64) *contentCollector {
	return &contentCollector{
		maxBytes: maxBytes,
		hasher:   sha256.New(),
		data:     make([]byte, 0, minInt64(maxBytes, 64<<10)),
	}
}

func (collector *contentCollector) Write(data []byte) (int, error) {
	_, _ = collector.hasher.Write(data)
	collector.size += int64(len(data))
	if bytes.IndexByte(data, 0) >= 0 {
		collector.containsNUL = true
	}
	if !collector.overflow {
		if collector.size <= collector.maxBytes {
			collector.data = append(collector.data, data...)
		} else {
			collector.overflow = true
			collector.data = nil
		}
	}
	return len(data), nil
}

func (collector *contentCollector) sha256() string {
	return fmt.Sprintf("%x", collector.hasher.Sum(nil))
}

func DetectLanguage(path string) string {
	base := strings.ToLower(filepath.Base(path))
	switch base {
	case "dockerfile", "containerfile":
		return "dockerfile"
	case "makefile", "gnumakefile":
		return "make"
	case "cmakelists.txt":
		return "cmake"
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".java":
		return "java"
	case ".kt", ".kts":
		return "kotlin"
	case ".py", ".pyi":
		return "python"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".ts", ".tsx", ".mts", ".cts":
		return "typescript"
	case ".rs":
		return "rust"
	case ".c", ".h":
		return "c"
	case ".cc", ".cpp", ".cxx", ".hpp", ".hh", ".hxx":
		return "cpp"
	case ".cs":
		return "csharp"
	case ".rb":
		return "ruby"
	case ".php":
		return "php"
	case ".swift":
		return "swift"
	case ".scala", ".sc":
		return "scala"
	case ".sh", ".bash", ".zsh", ".fish":
		return "shell"
	case ".sql":
		return "sql"
	case ".md", ".mdx":
		return "markdown"
	case ".yaml", ".yml":
		return "yaml"
	case ".json", ".jsonc":
		return "json"
	case ".toml":
		return "toml"
	case ".tf", ".tfvars", ".hcl":
		return "hcl"
	case ".proto":
		return "protobuf"
	case ".xml":
		return "xml"
	case ".html", ".htm":
		return "html"
	case ".css", ".scss", ".sass", ".less":
		return "css"
	case ".vue":
		return "vue"
	case ".svelte":
		return "svelte"
	default:
		return "unknown"
	}
}

func minInt64(left, right int64) int {
	if left < right {
		return int(left)
	}
	return int(right)
}
