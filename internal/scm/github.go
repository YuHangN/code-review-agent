package scm

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/YuHangN/code-review-agent/internal/domain"
)

const githubAPIVersion = "2026-03-10"

const (
	maxGitHubResponseBytes = int64(32 << 20)
	maxRepositoryFileBytes = int64(1 << 20)
)

// GitHubAdapter 通过 GitHub REST API 拉取固定 SHA 的 PR 变更。
type GitHubAdapter struct {
	client  *http.Client
	apiBase *url.URL
	token   string
}

// NewGitHubAdapter 创建 GitHub Adapter；apiBaseURL 可替换为测试服务器地址。
func NewGitHubAdapter(client *http.Client, apiBaseURL, token string) (*GitHubAdapter, error) {
	if client == nil {
		client = http.DefaultClient
	}
	base, err := url.Parse(apiBaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("parse GitHub API base URL")
	}
	return &GitHubAdapter{client: client, apiBase: base, token: token}, nil
}

// ParseGitHubPullRequestURL 从标准 GitHub PR URL 中提取仓库和编号。
func ParseGitHubPullRequestURL(rawURL string) (PullRequestRef, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, "github.com") {
		return PullRequestRef{}, fmt.Errorf("unsupported GitHub pull request URL")
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" || parts[2] != "pull" {
		return PullRequestRef{}, fmt.Errorf("invalid GitHub pull request URL")
	}
	number, err := strconv.Atoi(parts[3])
	if err != nil || number <= 0 {
		return PullRequestRef{}, fmt.Errorf("invalid GitHub pull request number")
	}
	return PullRequestRef{Owner: parts[0], Repository: parts[1], Number: number}, nil
}

// Fetch 先读取 PR 的 base/head SHA，再用这对 SHA 获取不可变的 compare diff。
func (a *GitHubAdapter) Fetch(ctx context.Context, ref PullRequestRef) (domain.ChangeSnapshot, error) {
	metadataBody, err := a.get(ctx, fmt.Sprintf("/repos/%s/%s/pulls/%d", url.PathEscape(ref.Owner), url.PathEscape(ref.Repository), ref.Number), "application/vnd.github+json")
	if err != nil {
		return domain.ChangeSnapshot{}, fmt.Errorf("get pull request metadata: %w", err)
	}
	var metadata struct {
		Base struct {
			SHA string `json:"sha"`
		} `json:"base"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(metadataBody, &metadata); err != nil {
		return domain.ChangeSnapshot{}, fmt.Errorf("decode pull request metadata: %w", err)
	}
	if metadata.Base.SHA == "" || metadata.Head.SHA == "" {
		return domain.ChangeSnapshot{}, fmt.Errorf("pull request metadata is missing base or head SHA")
	}

	compare := url.PathEscape(metadata.Base.SHA + "..." + metadata.Head.SHA)
	diff, err := a.get(ctx, fmt.Sprintf("/repos/%s/%s/compare/%s", url.PathEscape(ref.Owner), url.PathEscape(ref.Repository), compare), "application/vnd.github.diff")
	if err != nil {
		return domain.ChangeSnapshot{}, fmt.Errorf("get pinned compare diff: %w", err)
	}
	diffHash := sha256.Sum256(diff)
	return domain.ChangeSnapshot{
		BaseSHA:    metadata.Base.SHA,
		HeadSHA:    metadata.Head.SHA,
		Diff:       string(diff),
		DiffSHA256: fmt.Sprintf("%x", diffHash),
	}, nil
}

// ReadFile 只读取调用方明确给出的 commit SHA，不跟随 PR 后续更新。
func (a *GitHubAdapter) ReadFile(ctx context.Context, ref PullRequestRef, sha, filePath string) ([]byte, error) {
	if ref.Owner == "" || ref.Repository == "" || strings.TrimSpace(sha) == "" || !safeRepositoryPath(filePath) {
		return nil, fmt.Errorf("unsafe repository file path")
	}
	escapedPath := escapeRepositoryPath(filePath)
	return a.getWithQueryLimit(
		ctx,
		fmt.Sprintf("/repos/%s/%s/contents/%s", url.PathEscape(ref.Owner), url.PathEscape(ref.Repository), escapedPath),
		"application/vnd.github.raw+json",
		url.Values{"ref": []string{sha}},
		maxRepositoryFileBytes,
	)
}

func safeRepositoryPath(filePath string) bool {
	if filePath == "" || strings.HasPrefix(filePath, "/") || path.Clean(filePath) != filePath {
		return false
	}
	for _, segment := range strings.Split(filePath, "/") {
		if segment == "" || segment == "." || segment == ".." || segment == ".git" {
			return false
		}
	}
	return true
}

func escapeRepositoryPath(filePath string) string {
	segments := strings.Split(filePath, "/")
	for index, segment := range segments {
		segments[index] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

func (a *GitHubAdapter) get(ctx context.Context, path, accept string) ([]byte, error) {
	return a.getWithQuery(ctx, path, accept, nil)
}

func (a *GitHubAdapter) getWithQuery(ctx context.Context, requestPath, accept string, query url.Values) ([]byte, error) {
	return a.getWithQueryLimit(ctx, requestPath, accept, query, maxGitHubResponseBytes)
}

func (a *GitHubAdapter) getWithQueryLimit(ctx context.Context, requestPath, accept string, query url.Values, maxBytes int64) ([]byte, error) {
	target := *a.apiBase
	target.Path = strings.TrimRight(target.Path, "/") + requestPath
	target.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	if a.token != "" {
		request.Header.Set("Authorization", "Bearer "+a.token)
	}

	response, err := a.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("GitHub API response body exceeds limit")
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned HTTP %d", response.StatusCode)
	}
	return body, nil
}
