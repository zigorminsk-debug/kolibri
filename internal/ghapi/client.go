// Package ghapi is a small GitHub REST client covering what the pipeline
// needs: repositories, atomic commits through the Git Data API, and
// GitHub Actions runs, logs and artifacts.
package ghapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

const defaultBaseURL = "https://api.github.com"

// Client is a GitHub API client authenticated with a personal access token.
type Client struct {
	Token string
	// BaseURL overrides https://api.github.com (tests, GitHub Enterprise).
	BaseURL string
	HTTP    *http.Client
}

// APIError is an error answer from GitHub.
type APIError struct {
	Status  int
	Message string
	// Path is the API path that failed, for diagnostics.
	Path string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	switch e.Status {
	case http.StatusUnauthorized:
		return "GitHub не принял токен (401): проверьте его в настройках — возможно, он истёк или отозван"
	case http.StatusForbidden:
		if strings.Contains(strings.ToLower(msg), "rate limit") {
			return "GitHub временно ограничил запросы (rate limit). Подождите несколько минут и повторите"
		}
		return fmt.Sprintf("GitHub запретил действие (403): %s. Проверьте права токена", msg)
	}
	return fmt.Sprintf("GitHub ответил ошибкой %d: %s", e.Status, msg)
}

// IsNotFound reports whether err is a 404 from GitHub.
func IsNotFound(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Status == http.StatusNotFound
}

// IsEmptyRepo reports whether err is GitHub's "Git Repository is empty" answer.
func IsEmptyRepo(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Status == http.StatusConflict &&
		strings.Contains(strings.ToLower(api.Message), "empty")
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return defaultBaseURL
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "kolibri-forge")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func friendly(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var netErr *net.OpError
	var dnsErr *net.DNSError
	if errors.As(err, &netErr) || errors.As(err, &dnsErr) {
		return fmt.Errorf("нет связи с GitHub — проверьте подключение к интернету (%w)", err)
	}
	return err
}

func apiError(resp *http.Response, path string) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &APIError{Status: resp.StatusCode, Path: path}
	var payload struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Field   string `json:"field"`
		} `json:"errors"`
	}
	if json.Unmarshal(data, &payload) == nil {
		e.Message = payload.Message
		for _, sub := range payload.Errors {
			detail := sub.Message
			if detail == "" {
				detail = strings.TrimSpace(sub.Field + " " + sub.Code)
			}
			if detail != "" {
				e.Message += "; " + detail
			}
		}
	}
	return e
}

// do performs a JSON request and decodes the answer into out (when not nil).
func (c *Client) do(ctx context.Context, method, path string, body, out any) (http.Header, error) {
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, friendly(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.Header, apiError(resp, path)
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.Header, fmt.Errorf("ответ GitHub не разобран (%s): %w", path, err)
		}
	}
	return resp.Header, nil
}

// download streams a (possibly redirected) binary answer into w, refusing
// more than limit bytes. Go drops the Authorization header when a redirect
// leaves api.github.com, which is what the signed storage URLs expect.
func (c *Client) download(ctx context.Context, path string, w io.Writer, limit int64) error {
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return friendly(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp, path)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return friendly(err)
	}
	if n > limit {
		return fmt.Errorf("ответ GitHub больше допустимого (%d МБ)", limit>>20)
	}
	return nil
}

func repoPath(owner, repo string) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}

// User is the account a token belongs to.
type User struct {
	Login string `json:"login"`
	// Scopes are the OAuth scopes of a classic token. Fine-grained tokens
	// report none: their permissions cannot be read through the API.
	Scopes []string `json:"-"`
	// Classic is true when GitHub reported scopes for the token.
	Classic bool `json:"-"`
}

// HasScope reports whether a classic token carries the scope.
func (u User) HasScope(scope string) bool {
	for _, s := range u.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// CurrentUser returns the account the token belongs to.
func (c *Client) CurrentUser(ctx context.Context) (User, error) {
	var u User
	hdr, err := c.do(ctx, http.MethodGet, "/user", nil, &u)
	if err != nil {
		return u, err
	}
	if _, ok := hdr["X-Oauth-Scopes"]; ok {
		u.Classic = true
		for _, s := range strings.Split(hdr.Get("X-Oauth-Scopes"), ",") {
			if s = strings.TrimSpace(s); s != "" {
				u.Scopes = append(u.Scopes, s)
			}
		}
	}
	return u, nil
}

// Repo is a repository.
type Repo struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	HTMLURL       string `json:"html_url"`
	Archived      bool   `json:"archived"`
	Permissions   struct {
		Push bool `json:"push"`
	} `json:"permissions"`
}

// GetRepo fetches a repository; IsNotFound(err) when it does not exist
// (or the token cannot see it).
func (c *Client) GetRepo(ctx context.Context, owner, repo string) (Repo, error) {
	var r Repo
	_, err := c.do(ctx, http.MethodGet, repoPath(owner, repo), nil, &r)
	return r, err
}

// CreateRepo creates a repository with an initial commit, under the token's
// own account when owner == login and under the organisation otherwise.
func (c *Client) CreateRepo(ctx context.Context, login, owner, name, description string, private bool) (Repo, error) {
	body := map[string]any{
		"name":        name,
		"description": description,
		"private":     private,
		"auto_init":   true,
	}
	path := "/user/repos"
	if !strings.EqualFold(owner, login) {
		path = "/orgs/" + url.PathEscape(owner) + "/repos"
	}
	var r Repo
	_, err := c.do(ctx, http.MethodPost, path, body, &r)
	return r, err
}

// Head describes the tip of a branch.
type Head struct {
	Commit string
	Tree   string
}

// BranchHead returns the tip commit and its tree for a branch.
// IsNotFound(err) when the branch is missing, IsEmptyRepo(err) when the
// repository has no commits at all.
func (c *Client) BranchHead(ctx context.Context, owner, repo, branch string) (Head, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if _, err := c.do(ctx, http.MethodGet, repoPath(owner, repo)+"/git/ref/heads/"+escapeRef(branch), nil, &ref); err != nil {
		return Head{}, err
	}
	var commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if _, err := c.do(ctx, http.MethodGet, repoPath(owner, repo)+"/git/commits/"+ref.Object.SHA, nil, &commit); err != nil {
		return Head{}, err
	}
	return Head{Commit: ref.Object.SHA, Tree: commit.Tree.SHA}, nil
}

func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// CreateBranch points a new branch at an existing commit.
func (c *Client) CreateBranch(ctx context.Context, owner, repo, branch, sha string) error {
	body := map[string]string{"ref": "refs/heads/" + branch, "sha": sha}
	_, err := c.do(ctx, http.MethodPost, repoPath(owner, repo)+"/git/refs", body, nil)
	return err
}

// InitEmptyRepo makes the first commit in a repository that has none, by
// creating a README through the Contents API (the Git Data API refuses
// empty repositories).
func (c *Client) InitEmptyRepo(ctx context.Context, owner, repo, branch, title string) error {
	body := map[string]string{
		"message": "Initial commit",
		"content": encodeBase64([]byte("# " + title + "\n")),
	}
	if branch != "" {
		body["branch"] = branch
	}
	_, err := c.do(ctx, http.MethodPut, repoPath(owner, repo)+"/contents/README.md", body, nil)
	return err
}

// Archive downloads the repository snapshot at ref as a zip file.
func (c *Client) Archive(ctx context.Context, owner, repo, ref string, w io.Writer, limit int64) error {
	return c.download(ctx, repoPath(owner, repo)+"/zipball/"+escapeRef(ref), w, limit)
}
