package ghapi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Run is a GitHub Actions workflow run.
type Run struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`     // queued, in_progress, completed, ...
	Conclusion string    `json:"conclusion"` // success, failure, cancelled, ... (when completed)
	HTMLURL    string    `json:"html_url"`
	RunNumber  int       `json:"run_number"`
	HeadSHA    string    `json:"head_sha"`
	Event      string    `json:"event"`
	CreatedAt  time.Time `json:"created_at"`
}

// Done reports whether the run has finished.
func (r Run) Done() bool { return r.Status == "completed" }

// OK reports whether a finished run counts as a good build.
func (r Run) OK() bool {
	return r.Done() && (r.Conclusion == "success" || r.Conclusion == "skipped" || r.Conclusion == "neutral")
}

// RunsForCommit lists the workflow runs GitHub started for a commit.
func (c *Client) RunsForCommit(ctx context.Context, owner, repo, sha string) ([]Run, error) {
	var payload struct {
		Runs []Run `json:"workflow_runs"`
	}
	path := repoPath(owner, repo) + "/actions/runs?per_page=50&head_sha=" + url.QueryEscape(sha)
	_, err := c.do(ctx, http.MethodGet, path, nil, &payload)
	return payload.Runs, err
}

// Step is one step of a job.
type Step struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Number     int    `json:"number"`
}

// Job is one job of a workflow run.
type Job struct {
	ID          int64     `json:"id"`
	RunID       int64     `json:"run_id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	HTMLURL     string    `json:"html_url"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	Steps       []Step    `json:"steps"`
}

// Failed reports whether the job finished unsuccessfully.
func (j Job) Failed() bool {
	return j.Status == "completed" && (j.Conclusion == "failure" || j.Conclusion == "timed_out" || j.Conclusion == "startup_failure")
}

// FailedStep returns the name of the first failed step, "" when none is known.
func (j Job) FailedStep() string {
	for _, s := range j.Steps {
		if s.Conclusion == "failure" || s.Conclusion == "timed_out" {
			return s.Name
		}
	}
	return ""
}

// CurrentStep returns the step that is running now, "" when none.
func (j Job) CurrentStep() string {
	for _, s := range j.Steps {
		if s.Status == "in_progress" {
			return s.Name
		}
	}
	return ""
}

// Jobs lists the jobs of a run (latest attempt).
func (c *Client) Jobs(ctx context.Context, owner, repo string, runID int64) ([]Job, error) {
	var payload struct {
		Jobs []Job `json:"jobs"`
	}
	path := fmt.Sprintf("%s/actions/runs/%d/jobs?per_page=100", repoPath(owner, repo), runID)
	_, err := c.do(ctx, http.MethodGet, path, nil, &payload)
	return payload.Jobs, err
}

// JobLog downloads the plain-text log of a job.
func (c *Client) JobLog(ctx context.Context, owner, repo string, jobID int64) (string, error) {
	var buf bytes.Buffer
	path := fmt.Sprintf("%s/actions/jobs/%d/logs", repoPath(owner, repo), jobID)
	if err := c.download(ctx, path, &buf, 32<<20); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// Annotation is an error or warning a job attached to the commit.
type Annotation struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	Level     string `json:"annotation_level"` // notice, warning, failure
	Message   string `json:"message"`
	Title     string `json:"title"`
}

// Annotations lists what a job reported (a job's ID is its check run ID).
func (c *Client) Annotations(ctx context.Context, owner, repo string, jobID int64) ([]Annotation, error) {
	var out []Annotation
	path := fmt.Sprintf("%s/check-runs/%d/annotations?per_page=50", repoPath(owner, repo), jobID)
	_, err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// Artifact is a build output stored by a run.
type Artifact struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Size    int64  `json:"size_in_bytes"`
	Expired bool   `json:"expired"`
}

// Artifacts lists the artifacts of a run.
func (c *Client) Artifacts(ctx context.Context, owner, repo string, runID int64) ([]Artifact, error) {
	var payload struct {
		Artifacts []Artifact `json:"artifacts"`
	}
	path := fmt.Sprintf("%s/actions/runs/%d/artifacts?per_page=100", repoPath(owner, repo), runID)
	_, err := c.do(ctx, http.MethodGet, path, nil, &payload)
	return payload.Artifacts, err
}

// DownloadArtifact streams an artifact's zip archive into w.
func (c *Client) DownloadArtifact(ctx context.Context, owner, repo string, id int64, w io.Writer, limit int64) error {
	path := fmt.Sprintf("%s/actions/artifacts/%d/zip", repoPath(owner, repo), id)
	return c.download(ctx, path, w, limit)
}

// Release is a published GitHub release.
type Release struct {
	TagName     string         `json:"tag_name"`
	Name        string         `json:"name"`
	HTMLURL     string         `json:"html_url"`
	Body        string         `json:"body"`
	Draft       bool           `json:"draft"`
	Prerelease  bool           `json:"prerelease"`
	PublishedAt time.Time      `json:"published_at"`
	Assets      []ReleaseAsset `json:"assets"`
}

// ReleaseAsset is a file attached to a release.
type ReleaseAsset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"browser_download_url"`
}

// Releases lists the latest releases of a repository, newest first. A
// repository without releases gives an empty list; a public repository
// needs no token.
func (c *Client) Releases(ctx context.Context, owner, repo string, limit int) ([]Release, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	var out []Release
	_, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/releases?per_page=%d", repoPath(owner, repo), limit), nil, &out)
	return out, err
}

// ReleaseByTag fetches a release; IsNotFound(err) when there is none.
func (c *Client) ReleaseByTag(ctx context.Context, owner, repo, tag string) (Release, error) {
	var r Release
	_, err := c.do(ctx, http.MethodGet, repoPath(owner, repo)+"/releases/tags/"+url.PathEscape(tag), nil, &r)
	return r, err
}
