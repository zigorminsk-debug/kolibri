package ghapi

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

func encodeBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// TreeEntry is one path of a repository tree.
type TreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"` // blob, tree or commit (submodule)
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
}

// Tree lists every path under a tree. truncated is true when the repository
// is too large for GitHub to list in one answer.
func (c *Client) Tree(ctx context.Context, owner, repo, treeSHA string) (entries []TreeEntry, truncated bool, err error) {
	var payload struct {
		Tree      []TreeEntry `json:"tree"`
		Truncated bool        `json:"truncated"`
	}
	_, err = c.do(ctx, http.MethodGet, repoPath(owner, repo)+"/git/trees/"+treeSHA+"?recursive=1", nil, &payload)
	return payload.Tree, payload.Truncated, err
}

// Change is one file to write or delete in a commit.
type Change struct {
	Path    string
	Content []byte
	Delete  bool
	// Mode is the Git file mode; "" means a regular file (100644).
	Mode string
}

// ErrBranchMoved means someone else pushed to the branch while the commit
// was being prepared.
var ErrBranchMoved = errors.New("ветка изменилась на GitHub, пока готовился коммит — запустите задачу ещё раз")

// Commit writes all changes as a single commit on top of head and moves the
// branch to it. Nothing becomes visible in the branch unless every step
// succeeds. It returns the new tip of the branch.
func (c *Client) Commit(ctx context.Context, owner, repo, branch string, head Head, message string, changes []Change) (Head, error) {
	base := repoPath(owner, repo)

	type treeItem struct {
		Path string  `json:"path"`
		Mode string  `json:"mode"`
		Type string  `json:"type"`
		SHA  *string `json:"sha"`
	}
	items := make([]treeItem, 0, len(changes))
	for _, ch := range changes {
		mode := ch.Mode
		if mode == "" {
			mode = "100644"
		}
		if ch.Delete {
			items = append(items, treeItem{Path: ch.Path, Mode: mode, Type: "blob", SHA: nil})
			continue
		}
		var blob struct {
			SHA string `json:"sha"`
		}
		body := map[string]string{"content": encodeBase64(ch.Content), "encoding": "base64"}
		if _, err := c.do(ctx, http.MethodPost, base+"/git/blobs", body, &blob); err != nil {
			return Head{}, explainWrite(err, ch.Path)
		}
		sha := blob.SHA
		items = append(items, treeItem{Path: ch.Path, Mode: mode, Type: "blob", SHA: &sha})
	}

	var tree struct {
		SHA string `json:"sha"`
	}
	if _, err := c.do(ctx, http.MethodPost, base+"/git/trees", map[string]any{"base_tree": head.Tree, "tree": items}, &tree); err != nil {
		return Head{}, explainWrite(err, workflowPath(changes))
	}

	var commit struct {
		SHA string `json:"sha"`
	}
	commitBody := map[string]any{"message": message, "tree": tree.SHA, "parents": []string{head.Commit}}
	if _, err := c.do(ctx, http.MethodPost, base+"/git/commits", commitBody, &commit); err != nil {
		return Head{}, explainWrite(err, "")
	}

	refBody := map[string]any{"sha": commit.SHA, "force": false}
	if _, err := c.do(ctx, http.MethodPatch, base+"/git/refs/heads/"+escapeRef(branch), refBody, nil); err != nil {
		var api *APIError
		if errors.As(err, &api) && api.Status == http.StatusUnprocessableEntity &&
			strings.Contains(strings.ToLower(api.Message), "fast forward") {
			return Head{}, ErrBranchMoved
		}
		return Head{}, explainWrite(err, workflowPath(changes))
	}
	return Head{Commit: commit.SHA, Tree: tree.SHA}, nil
}

func workflowPath(changes []Change) string {
	for _, ch := range changes {
		if strings.HasPrefix(ch.Path, ".github/workflows/") {
			return ch.Path
		}
	}
	return ""
}

// WriteError explains a refused write in terms of token permissions.
type WriteError struct {
	Err error
	// Workflow is true when the refusal is most likely about writing
	// .github/workflows, which needs a separate permission.
	Workflow bool
}

func (e *WriteError) Unwrap() error { return e.Err }

func (e *WriteError) Error() string {
	if e.Workflow {
		return "GitHub не дал записать файл сборки в .github/workflows. Токену нужно право на workflow: " +
			"для classic-токена — области repo и workflow, для fine-grained — Contents и Workflows с доступом Read and write" + e.detail()
	}
	return "GitHub не дал записать в репозиторий. Токену нужно право записи: " +
		"для classic-токена — область repo, для fine-grained — Contents: Read and write для этого репозитория" + e.detail()
}

// detail appends GitHub's own words, which name the exact missing permission.
func (e *WriteError) detail() string {
	var api *APIError
	if errors.As(e.Err, &api) && api.Message != "" {
		return fmt.Sprintf(" (ответ GitHub: %d %s)", api.Status, api.Message)
	}
	return ""
}

// explainWrite wraps permission failures of write calls. GitHub answers 404
// (not 403) to writes the token may not perform, and names the workflow
// scope in the message when that is what is missing.
func explainWrite(err error, workflowFile string) error {
	var api *APIError
	if !errors.As(err, &api) {
		return err
	}
	msg := strings.ToLower(api.Message)
	if strings.Contains(msg, "workflow") {
		return &WriteError{Err: err, Workflow: true}
	}
	if api.Status == http.StatusNotFound || api.Status == http.StatusForbidden {
		return &WriteError{Err: err, Workflow: workflowFile != "" && api.Status == http.StatusNotFound && strings.Contains(api.Path, "/git/refs")}
	}
	return err
}
