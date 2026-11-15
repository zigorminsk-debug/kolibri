package ghapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RepoSummary is one entry of the account's repository list.
type RepoSummary struct {
	FullName      string    `json:"full_name"`
	Description   string    `json:"description"`
	Private       bool      `json:"private"`
	Fork          bool      `json:"fork"`
	Archived      bool      `json:"archived"`
	Language      string    `json:"language"`
	DefaultBranch string    `json:"default_branch"`
	PushedAt      time.Time `json:"pushed_at"`
	Permissions   struct {
		Push bool `json:"push"`
	} `json:"permissions"`
}

// Writable reports whether the token may commit to the repository.
func (r RepoSummary) Writable() bool { return r.Permissions.Push && !r.Archived }

const (
	repoPageSize = 100
	// maxRepoPages bounds the listing; accounts with more repositories get
	// the most recently pushed ones.
	maxRepoPages = 10
)

// ListRepos returns the repositories the token can see — the user's own,
// those shared with them and those of their organisations — most recently
// pushed first. more is true when the account has more than were fetched.
func (c *Client) ListRepos(ctx context.Context) (repos []RepoSummary, more bool, err error) {
	for page := 1; page <= maxRepoPages; page++ {
		var batch []RepoSummary
		path := fmt.Sprintf("/user/repos?per_page=%d&page=%d&sort=pushed&affiliation=owner,collaborator,organization_member", repoPageSize, page)
		if _, err := c.do(ctx, http.MethodGet, path, nil, &batch); err != nil {
			return nil, false, err
		}
		repos = append(repos, batch...)
		if len(batch) < repoPageSize {
			return repos, false, nil
		}
	}
	return repos, true, nil
}

// IsAuthError reports whether GitHub refused the request because of the
// token itself: missing, expired, revoked, or lacking the permission.
func IsAuthError(err error) bool {
	var api *APIError
	if !errors.As(err, &api) {
		return false
	}
	if api.Status == http.StatusForbidden {
		// A rate limit is also a 403, but a new token does not cure it.
		return !strings.Contains(strings.ToLower(api.Message), "rate limit")
	}
	return api.Status == http.StatusUnauthorized
}

// TokenURL is the GitHub page that creates a classic token with the scopes
// the app needs already ticked.
func TokenURL() string {
	q := url.Values{"scopes": {"repo,workflow"}, "description": {"Kolibri Forge"}}
	return "https://github.com/settings/tokens/new?" + q.Encode()
}
