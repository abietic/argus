// Package githubadapter projects Argus publication requests to GitHub pull
// requests. Secrets and permission authority are injected and never persisted.
package githubadapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/abietic/argus/internal/publication"
)

const (
	ProviderID       = "github"
	apiVersion       = "2022-11-28"
	defaultAPIBase   = "https://api.github.com"
	maxResponseBytes = int64(4 << 20)
	maxLookupPages   = 30
)

type Credential struct {
	Token       string
	PrincipalID string
	Revision    string
}

func (credential Credential) String() string {
	return fmt.Sprintf(
		"Credential{Token:[REDACTED] PrincipalID:%q Revision:%q}",
		credential.PrincipalID, credential.Revision,
	)
}

func (credential Credential) GoString() string { return credential.String() }

type CredentialSource interface {
	Resolve(context.Context, string) (Credential, error)
}

type PermissionRequest struct {
	Provider                   string
	RepositoryID               string
	ChangeKind                 string
	ChangeID                   string
	Channel                    string
	PrincipalID                string
	CredentialRevision         string
	ExpectedPermissionRevision string
}

type PermissionDecision struct {
	Granted  bool
	Revision string
}

// PermissionBroker is mandatory because GitHub API credentials alone do not
// prove Argus tenant/workspace authorization or an approved publication role.
type PermissionBroker interface {
	Check(context.Context, PermissionRequest) (PermissionDecision, error)
}

type Adapter struct {
	client      *http.Client
	baseURL     *url.URL
	credentials CredentialSource
	permissions PermissionBroker
	now         func() time.Time
}

func New(
	client *http.Client,
	credentials CredentialSource,
	permissions PermissionBroker,
	now func() time.Time,
) (*Adapter, error) {
	return newAdapter(client, defaultAPIBase, credentials, permissions, now)
}

func newAdapter(
	client *http.Client,
	base string,
	credentials CredentialSource,
	permissions PermissionBroker,
	now func() time.Time,
) (*Adapter, error) {
	if credentials == nil || permissions == nil {
		return nil, fmt.Errorf("GitHub credential source and permission broker are required")
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Host == "" || (parsed.Scheme != "https" && !isLoopbackHTTP(parsed)) {
		return nil, fmt.Errorf("GitHub API base must be HTTPS or loopback HTTP")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if now == nil {
		now = time.Now
	}
	return &Adapter{
		client: &clientCopy, baseURL: parsed, credentials: credentials,
		permissions: permissions, now: now,
	}, nil
}

func (adapter *Adapter) Revalidate(
	ctx context.Context,
	request publication.Request,
) (publication.Revalidation, error) {
	if err := adapter.validateRequest(request); err != nil {
		return publication.Revalidation{}, err
	}
	credential, decision, err := adapter.authorize(
		ctx, request.RepositoryID, request.ChangeKind, request.ChangeID,
		request.Channel, request.ExpectedPermissionRevision,
	)
	if err != nil {
		return publication.Revalidation{}, err
	}
	_ = credential
	checkedAt := adapter.now().UTC()
	observation := publication.Revalidation{
		SchemaVersion:       publication.RevalidationSchemaVersion,
		PermissionGranted:   decision.Granted,
		PermissionRevision:  decision.Revision,
		ConfigBundleSHA256:  request.ConfigBundleRef.SHA256,
		FindingSourceSHA256: request.FindingSourceRef.SHA256,
		FindingCurrent:      true, CheckedAt: checkedAt,
	}
	if !decision.Granted || decision.Revision != request.ExpectedPermissionRevision {
		observation.ProviderBaseRevision = request.BaseRevision
		observation.ProviderHeadRevision = request.ExpectedHeadRevision
		return observation, nil
	}
	pull, err := adapter.getPullRequest(ctx, credential, request.RepositoryID, request.ChangeID)
	if err != nil {
		return publication.Revalidation{}, err
	}
	observation.ProviderBaseRevision = pull.Base.SHA
	observation.ProviderHeadRevision = pull.Head.SHA
	if pull.State != "open" || pull.Base.SHA != request.BaseRevision ||
		pull.Head.SHA != request.ExpectedHeadRevision {
		return observation, nil
	}
	if request.Channel == "pull_request_summary" {
		observation.AnchorCurrent = true
		return observation, nil
	}
	observation.AnchorCurrent, err = adapter.anchorCurrent(
		ctx, credential, request.RepositoryID, request.ChangeID, request.Anchor,
	)
	if err != nil {
		return publication.Revalidation{}, err
	}
	return observation, nil
}

func (adapter *Adapter) Publish(
	ctx context.Context,
	request publication.ProviderRequest,
) (publication.ProviderResult, error) {
	if err := adapter.validateProviderRequest(request); err != nil {
		return publication.ProviderResult{}, err
	}
	credential, decision, err := adapter.authorize(
		ctx, request.RepositoryID, request.ChangeKind, request.ChangeID,
		request.Channel, request.PermissionRevision,
	)
	if err != nil {
		return publication.ProviderResult{}, err
	}
	if !decision.Granted || decision.Revision != request.PermissionRevision {
		return adapter.rejected(request, "permission_denied"), nil
	}
	pull, err := adapter.getPullRequest(ctx, credential, request.RepositoryID, request.ChangeID)
	if err != nil {
		return publication.ProviderResult{}, err
	}
	if pull.State != "open" || pull.Base.SHA != request.BaseRevision ||
		pull.Head.SHA != request.HeadRevision {
		return adapter.rejected(request, "provider_head_moved"), nil
	}
	body := request.Message + "\n\n" + publicationMarker(request.IdempotencyKey)
	var endpoint string
	var payload any
	switch request.Channel {
	case "pull_request_inline":
		endpoint = pullCommentsPath(request.RepositoryID, request.ChangeID)
		payload = inlineCommentPayload(request, body)
	case "pull_request_summary":
		endpoint = issueCommentsPath(request.RepositoryID, request.ChangeID)
		payload = map[string]string{"body": body}
	default:
		return publication.ProviderResult{}, fmt.Errorf("unsupported GitHub channel %q", request.Channel)
	}
	var response commentResponse
	status, err := adapter.doJSON(ctx, credential, http.MethodPost, endpoint, payload, &response)
	if err != nil {
		return publication.ProviderResult{}, err
	}
	if status != http.StatusCreated {
		if status == http.StatusUnauthorized || status == http.StatusForbidden ||
			status == http.StatusNotFound || status == http.StatusConflict ||
			status == http.StatusUnprocessableEntity {
			return adapter.rejected(request, githubReason(status)), nil
		}
		return publication.ProviderResult{}, fmt.Errorf("GitHub create comment returned HTTP %d", status)
	}
	if response.ID <= 0 {
		return publication.ProviderResult{}, fmt.Errorf("GitHub create comment omitted id")
	}
	id := strconv.FormatInt(response.ID, 10)
	return publication.ProviderResult{
		SchemaVersion:     publication.ProviderResultSchema,
		Status:            publication.ProviderResultPublished,
		IdempotencyKey:    request.IdempotencyKey,
		ProviderRequestID: id, CommentID: id, ObservedAt: adapter.now().UTC(),
	}, nil
}

func (adapter *Adapter) Lookup(
	ctx context.Context,
	request publication.ProviderRequest,
) (publication.ProviderResult, error) {
	if err := adapter.validateProviderRequest(request); err != nil {
		return publication.ProviderResult{}, err
	}
	credential, err := adapter.credentials.Resolve(ctx, request.RepositoryID)
	if err != nil {
		return publication.ProviderResult{}, fmt.Errorf("resolve GitHub credential: %w", err)
	}
	if err := credential.Validate(); err != nil {
		return publication.ProviderResult{}, err
	}
	endpoint := pullCommentsPath(request.RepositoryID, request.ChangeID)
	if request.Channel == "pull_request_summary" {
		endpoint = issueCommentsPath(request.RepositoryID, request.ChangeID)
	}
	marker := publicationMarker(request.IdempotencyKey)
	for page := 1; page <= maxLookupPages; page++ {
		var comments []commentResponse
		status, err := adapter.doJSON(ctx, credential, http.MethodGet,
			endpoint+"?per_page=100&page="+strconv.Itoa(page), nil, &comments)
		if err != nil {
			return publication.ProviderResult{}, err
		}
		if status != http.StatusOK {
			return publication.ProviderResult{}, fmt.Errorf("GitHub list comments returned HTTP %d", status)
		}
		for _, comment := range comments {
			if comment.ID > 0 && strings.Contains(comment.Body, marker) {
				id := strconv.FormatInt(comment.ID, 10)
				return publication.ProviderResult{
					SchemaVersion:     publication.ProviderResultSchema,
					Status:            publication.ProviderResultPublished,
					IdempotencyKey:    request.IdempotencyKey,
					ProviderRequestID: id, CommentID: id, ObservedAt: adapter.now().UTC(),
				}, nil
			}
		}
		if len(comments) < 100 {
			break
		}
	}
	return publication.ProviderResult{
		SchemaVersion:  publication.ProviderResultSchema,
		Status:         publication.ProviderResultNotFound,
		IdempotencyKey: request.IdempotencyKey,
		ObservedAt:     adapter.now().UTC(),
	}, nil
}

func (adapter *Adapter) authorize(
	ctx context.Context,
	repositoryID string,
	changeKind string,
	changeID string,
	channel string,
	expectedRevision string,
) (Credential, PermissionDecision, error) {
	credential, err := adapter.credentials.Resolve(ctx, repositoryID)
	if err != nil {
		return Credential{}, PermissionDecision{}, fmt.Errorf("resolve GitHub credential: %w", err)
	}
	if err := credential.Validate(); err != nil {
		return Credential{}, PermissionDecision{}, err
	}
	decision, err := adapter.permissions.Check(ctx, PermissionRequest{
		Provider: ProviderID, RepositoryID: repositoryID,
		ChangeKind: changeKind, ChangeID: changeID, Channel: channel,
		PrincipalID: credential.PrincipalID, CredentialRevision: credential.Revision,
		ExpectedPermissionRevision: expectedRevision,
	})
	if err != nil {
		return Credential{}, PermissionDecision{}, fmt.Errorf("check GitHub publication permission: %w", err)
	}
	if decision.Revision == "" || strings.ContainsAny(decision.Revision, "\r\n\x00") {
		return Credential{}, PermissionDecision{}, fmt.Errorf("permission broker returned invalid revision")
	}
	return credential, decision, nil
}

func (credential Credential) Validate() error {
	if credential.Token == "" || strings.ContainsAny(credential.Token, "\r\n\x00") {
		return fmt.Errorf("GitHub credential token is invalid")
	}
	if credential.PrincipalID == "" || credential.Revision == "" ||
		strings.ContainsAny(credential.PrincipalID+credential.Revision, "\r\n\x00") {
		return fmt.Errorf("GitHub credential identity is invalid")
	}
	return nil
}

func (adapter *Adapter) validateRequest(request publication.Request) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if request.Provider != ProviderID {
		return fmt.Errorf("GitHub adapter rejects provider %q", request.Provider)
	}
	return validateDestination(request.RepositoryID, request.ChangeKind, request.ChangeID)
}

func (adapter *Adapter) validateProviderRequest(request publication.ProviderRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if request.Provider != ProviderID {
		return fmt.Errorf("GitHub adapter rejects provider %q", request.Provider)
	}
	return validateDestination(request.RepositoryID, request.ChangeKind, request.ChangeID)
}

func validateDestination(repositoryID, changeKind, changeID string) error {
	parts := strings.Split(repositoryID, "/")
	if len(parts) != 2 || !validGitHubName(parts[0]) || !validGitHubName(parts[1]) {
		return fmt.Errorf("GitHub repository_id must be owner/name")
	}
	if changeKind != "pull_request" {
		return fmt.Errorf("GitHub change_kind must be pull_request")
	}
	number, err := strconv.ParseUint(changeID, 10, 64)
	if err != nil || number == 0 || strconv.FormatUint(number, 10) != changeID {
		return fmt.Errorf("GitHub change_id must be a canonical positive pull request number")
	}
	return nil
}

func validGitHubName(value string) bool {
	if value == "" || len(value) > 100 || strings.HasPrefix(value, ".") ||
		strings.HasSuffix(value, ".") || strings.Contains(value, "..") {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

type pullResponse struct {
	State string `json:"state"`
	Base  struct {
		SHA string `json:"sha"`
	} `json:"base"`
	Head struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

type changedFile struct {
	Filename string `json:"filename"`
	Patch    string `json:"patch"`
}

type commentResponse struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

func (adapter *Adapter) getPullRequest(
	ctx context.Context,
	credential Credential,
	repositoryID string,
	changeID string,
) (pullResponse, error) {
	var pull pullResponse
	status, err := adapter.doJSON(ctx, credential, http.MethodGet,
		pullPath(repositoryID, changeID), nil, &pull)
	if err != nil {
		return pullResponse{}, err
	}
	if status != http.StatusOK {
		return pullResponse{}, fmt.Errorf("GitHub get pull request returned HTTP %d", status)
	}
	if pull.Base.SHA == "" || pull.Head.SHA == "" || pull.State == "" {
		return pullResponse{}, fmt.Errorf("GitHub pull request response is incomplete")
	}
	return pull, nil
}

func (adapter *Adapter) anchorCurrent(
	ctx context.Context,
	credential Credential,
	repositoryID string,
	changeID string,
	anchor publication.StableAnchor,
) (bool, error) {
	endpoint := pullFilesPath(repositoryID, changeID)
	for page := 1; page <= maxLookupPages; page++ {
		var files []changedFile
		status, err := adapter.doJSON(ctx, credential, http.MethodGet,
			endpoint+"?per_page=100&page="+strconv.Itoa(page), nil, &files)
		if err != nil {
			return false, err
		}
		if status != http.StatusOK {
			return false, fmt.Errorf("GitHub list pull request files returned HTTP %d", status)
		}
		for _, file := range files {
			if file.Filename == anchor.Path {
				return patchContainsHeadRange(file.Patch, anchor.StartLine, anchor.EndLine), nil
			}
		}
		if len(files) < 100 {
			break
		}
	}
	return false, nil
}

func patchContainsHeadRange(patch string, start, end uint32) bool {
	if patch == "" || start == 0 || end < start {
		return false
	}
	present := make(map[uint32]struct{}, end-start+1)
	var headLine uint64
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "@@ ") {
			plus := strings.Index(line, " +")
			if plus < 0 {
				headLine = 0
				continue
			}
			field := strings.Fields(line[plus+1:])
			if len(field) == 0 || !strings.HasPrefix(field[0], "+") {
				headLine = 0
				continue
			}
			value := strings.TrimPrefix(strings.SplitN(field[0], ",", 2)[0], "+")
			headLine, _ = strconv.ParseUint(value, 10, 32)
			continue
		}
		if headLine == 0 || line == "" {
			continue
		}
		switch line[0] {
		case ' ', '+':
			if headLine >= uint64(start) && headLine <= uint64(end) {
				present[uint32(headLine)] = struct{}{}
			}
			headLine++
		case '-':
		case '\\':
		default:
			headLine = 0
		}
	}
	return uint64(len(present)) == uint64(end-start)+1
}

func inlineCommentPayload(request publication.ProviderRequest, body string) map[string]any {
	payload := map[string]any{
		"body": body, "commit_id": request.HeadRevision,
		"path": request.Anchor.Path, "line": request.Anchor.EndLine, "side": "RIGHT",
	}
	if request.Anchor.StartLine < request.Anchor.EndLine {
		payload["start_line"] = request.Anchor.StartLine
		payload["start_side"] = "RIGHT"
	}
	return payload
}

func (adapter *Adapter) doJSON(
	ctx context.Context,
	credential Credential,
	method string,
	endpoint string,
	payload any,
	target any,
) (int, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return 0, fmt.Errorf("marshal GitHub request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	reference, err := adapter.baseURL.Parse(endpoint)
	if err != nil || reference.Host != adapter.baseURL.Host || reference.Scheme != adapter.baseURL.Scheme {
		return 0, fmt.Errorf("resolve GitHub endpoint")
	}
	request, err := http.NewRequestWithContext(ctx, method, reference.String(), body)
	if err != nil {
		return 0, fmt.Errorf("create GitHub request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+credential.Token)
	request.Header.Set("X-GitHub-Api-Version", apiVersion)
	request.Header.Set("User-Agent", "argus-code-review")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := adapter.client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("GitHub request outcome unknown: %w", err)
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maxResponseBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return 0, fmt.Errorf("read GitHub response: %w", err)
	}
	if int64(len(data)) > maxResponseBytes {
		return 0, fmt.Errorf("GitHub response exceeds %d bytes", maxResponseBytes)
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 && target != nil {
		// GitHub responses contain many fields, so decode only the selected
		// projection instead of applying Argus strict-decoder semantics.
		if err := json.Unmarshal(data, target); err != nil {
			return 0, fmt.Errorf("decode GitHub response: %w", err)
		}
	}
	return response.StatusCode, nil
}

func (adapter *Adapter) rejected(
	request publication.ProviderRequest,
	reason string,
) publication.ProviderResult {
	return publication.ProviderResult{
		SchemaVersion:  publication.ProviderResultSchema,
		Status:         publication.ProviderResultRejected,
		IdempotencyKey: request.IdempotencyKey,
		ReasonCode:     reason, ObservedAt: adapter.now().UTC(),
	}
}

func publicationMarker(idempotencyKey string) string {
	digest := sha256.Sum256([]byte(idempotencyKey))
	return "<!-- argus-publication:" + hex.EncodeToString(digest[:]) + " -->"
}

func githubReason(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "github_unauthorized"
	case http.StatusForbidden:
		return "github_forbidden"
	case http.StatusNotFound:
		return "github_target_not_found"
	case http.StatusConflict:
		return "github_conflict"
	default:
		return "github_validation_failed"
	}
}

func pullPath(repositoryID, changeID string) string {
	return "/repos/" + repositoryID + "/pulls/" + changeID
}

func pullFilesPath(repositoryID, changeID string) string {
	return pullPath(repositoryID, changeID) + "/files"
}

func pullCommentsPath(repositoryID, changeID string) string {
	return pullPath(repositoryID, changeID) + "/comments"
}

func issueCommentsPath(repositoryID, changeID string) string {
	return "/repos/" + repositoryID + "/issues/" + changeID + "/comments"
}

func isLoopbackHTTP(value *url.URL) bool {
	if value.Scheme != "http" {
		return false
	}
	host := value.Hostname()
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

var _ publication.Provider = (*Adapter)(nil)
