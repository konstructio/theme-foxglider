package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type glClient struct {
	base, token string
	hc          *http.Client
}

func newGLClient(base, token string) *glClient {
	// git.civo.com can be very slow to answer; a tight timeout just turns a slow
	// page into a broken one. Give calls a long leash — the cache means we pay
	// this cost rarely, and detached fetch contexts let slow calls finish warming
	// the cache even after the browser/ingress has given up.
	return &glClient{base: base, token: token, hc: &http.Client{Timeout: 120 * time.Second}}
}

type glProject struct {
	ID                int    `json:"id"`
	Name              string `json:"name"`
	PathWithNamespace string `json:"path_with_namespace"`
	WebURL            string `json:"web_url"`
	DefaultBranch     string `json:"default_branch"`
	Namespace         struct {
		FullPath string `json:"full_path"`
	} `json:"namespace"`
}

type glPipeline struct {
	ID        int       `json:"id"`
	Status    string    `json:"status"`
	Ref       string    `json:"ref"`
	SHA       string    `json:"sha"`
	WebURL    string    `json:"web_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type glPipelineDetail struct {
	glPipeline
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Duration   float64    `json:"duration"`
}

type glJob struct {
	ID         int        `json:"id"`
	Name       string     `json:"name"`
	Stage      string     `json:"stage"`
	Status     string     `json:"status"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Duration   float64    `json:"duration"`
	WebURL     string     `json:"web_url"`
}

type glEvent struct {
	ActionName  string    `json:"action_name"`
	TargetType  string    `json:"target_type"`
	TargetIID   int       `json:"target_iid"`
	TargetID    int       `json:"target_id"`
	CreatedAt   time.Time `json:"created_at"`
	TargetTitle string    `json:"target_title"`
	Author      struct {
		Username string `json:"username"`
	} `json:"author"`
	PushData *struct {
		Ref         string `json:"ref"`
		CommitTitle string `json:"commit_title"`
		CommitCount int    `json:"commit_count"`
	} `json:"push_data"`
	Note *struct {
		NoteableType string `json:"noteable_type"`
		NoteableIID  int    `json:"noteable_iid"`
	} `json:"note"`
}

// get fetches one page; returns the x-next-page value ("" when done).
func (c *glClient) get(ctx context.Context, path string, q url.Values, into any) (string, error) {
	u := c.base + "/api/v4" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	res, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", fmt.Errorf("gitlab %s: %s", path, res.Status)
	}
	if err := json.NewDecoder(res.Body).Decode(into); err != nil {
		return "", fmt.Errorf("gitlab %s: decode: %w", path, err)
	}
	return res.Header.Get("x-next-page"), nil
}

func (c *glClient) projects(ctx context.Context) ([]glProject, error) {
	var all []glProject
	page := "1"
	for page != "" {
		q := url.Values{"per_page": {"100"}, "page": {page}, "archived": {"false"}, "simple": {"true"}}
		var batch []glProject
		next, err := c.get(ctx, "/projects", q, &batch)
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		page = next
	}
	return all, nil
}

func (c *glClient) pipelines(ctx context.Context, projectID, limit int) ([]glPipeline, error) {
	var out []glPipeline
	q := url.Values{"per_page": {fmt.Sprint(limit)}}
	_, err := c.get(ctx, fmt.Sprintf("/projects/%d/pipelines", projectID), q, &out)
	return out, err
}

func (c *glClient) pipeline(ctx context.Context, projectID, id int) (glPipelineDetail, error) {
	var out glPipelineDetail
	_, err := c.get(ctx, fmt.Sprintf("/projects/%d/pipelines/%d", projectID, id), nil, &out)
	return out, err
}

func (c *glClient) jobs(ctx context.Context, projectID, pipelineID int) ([]glJob, error) {
	var out []glJob
	q := url.Values{"per_page": {"100"}, "include_retried": {"false"}}
	_, err := c.get(ctx, fmt.Sprintf("/projects/%d/pipelines/%d/jobs", projectID, pipelineID), q, &out)
	return out, err
}

func (c *glClient) events(ctx context.Context, projectID, limit int) ([]glEvent, error) {
	var out []glEvent
	q := url.Values{"per_page": {fmt.Sprint(limit)}}
	_, err := c.get(ctx, fmt.Sprintf("/projects/%d/events", projectID), q, &out)
	return out, err
}

// --- delivery-ecosystem access (projects addressed by URL-encoded path) ---

type glTag struct {
	Name string `json:"name"`
}

// getRaw fetches one non-JSON endpoint and returns the body as a string.
func (c *glClient) getRaw(ctx context.Context, path string, q url.Values) (string, error) {
	u := c.base + "/api/v4" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	res, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", fmt.Errorf("gitlab %s: %s", path, res.Status)
	}
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return "", fmt.Errorf("gitlab %s: read: %w", path, err)
	}
	return string(b), nil
}

// fileRaw returns the raw contents of a repo file on the given ref.
func (c *glClient) fileRaw(ctx context.Context, projectPath, filePath, ref string) (string, error) {
	p := fmt.Sprintf("/projects/%s/repository/files/%s/raw", url.QueryEscape(projectPath), url.QueryEscape(filePath))
	return c.getRaw(ctx, p, url.Values{"ref": {ref}})
}

// jobsByPath lists a pipeline's jobs for a project addressed by path — used by
// the live stage-progress on running microcharts.
func (c *glClient) jobsByPath(ctx context.Context, projectPath string, pipelineID int) ([]glJob, error) {
	var out []glJob
	p := fmt.Sprintf("/projects/%s/pipelines/%d/jobs", url.QueryEscape(projectPath), pipelineID)
	_, err := c.get(ctx, p, url.Values{"per_page": {"100"}, "include_retried": {"false"}}, &out)
	return out, err
}

// tags lists the newest tags for a project addressed by path.
func (c *glClient) tags(ctx context.Context, projectPath string, limit int) ([]glTag, error) {
	var out []glTag
	p := fmt.Sprintf("/projects/%s/repository/tags", url.QueryEscape(projectPath))
	_, err := c.get(ctx, p, url.Values{"per_page": {fmt.Sprint(limit)}, "order_by": {"updated"}}, &out)
	return out, err
}

// glLatestPipeline is /pipelines/latest — a single pipeline plus the user who
// ran it (the person who pushed the change that triggered the build).
type glLatestPipeline struct {
	ID         int        `json:"id"`
	Status     string     `json:"status"`
	Ref        string     `json:"ref"`
	SHA        string     `json:"sha"`
	WebURL     string     `json:"web_url"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at"`
	User       *struct {
		Name      string `json:"name"`
		Username  string `json:"username"`
		AvatarURL string `json:"avatar_url"`
		WebURL    string `json:"web_url"`
	} `json:"user"`
}

// latestPipeline returns the newest pipeline on the default branch for a project
// addressed by path, including the triggering user (zero value + error when the
// project has never run one).
func (c *glClient) latestPipeline(ctx context.Context, projectPath string) (glLatestPipeline, error) {
	var out glLatestPipeline
	p := fmt.Sprintf("/projects/%s/pipelines/latest", url.QueryEscape(projectPath))
	_, err := c.get(ctx, p, nil, &out)
	return out, err
}

// --- commit + per-SHA pipeline story (rich delivery tiles) ---

type glCommit struct {
	ID           string    `json:"id"`
	ShortID      string    `json:"short_id"`
	Title        string    `json:"title"`
	WebURL       string    `json:"web_url"`
	AuthorName   string    `json:"author_name"`
	AuthoredDate time.Time `json:"authored_date"`
}

// commitInfo returns one commit's headline for a project addressed by path.
func (c *glClient) commitInfo(ctx context.Context, projectPath, sha string) (glCommit, error) {
	var out glCommit
	p := fmt.Sprintf("/projects/%s/repository/commits/%s", url.QueryEscape(projectPath), url.PathEscape(sha))
	_, err := c.get(ctx, p, nil, &out)
	return out, err
}

// glSHAPipeline is a pipeline in a ?sha= listing; Source separates the branch
// run from tag/trigger runs of the same commit.
type glSHAPipeline struct {
	ID        int       `json:"id"`
	Status    string    `json:"status"`
	Ref       string    `json:"ref"`
	Source    string    `json:"source"`
	WebURL    string    `json:"web_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// pipelinesForSHA lists every pipeline that ran on one commit — the complete
// build story of a SHA (branch run, RC tag run, re-runs).
func (c *glClient) pipelinesForSHA(ctx context.Context, projectPath, sha string, limit int) ([]glSHAPipeline, error) {
	var out []glSHAPipeline
	p := fmt.Sprintf("/projects/%s/pipelines", url.QueryEscape(projectPath))
	_, err := c.get(ctx, p, url.Values{"sha": {sha}, "per_page": {fmt.Sprint(limit)}}, &out)
	return out, err
}

// --- write surface (bot-executed CI actions; see actions.go) ---

// postJSON sends an authenticated POST and decodes the response. Non-2xx maps
// to an error carrying GitLab's message so the UI can show the real reason.
func (c *glClient) postJSON(ctx context.Context, path string, body, into any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v4"+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 300))
		return fmt.Errorf("gitlab %s: %s: %s", path, res.Status, strings.TrimSpace(string(b)))
	}
	if into != nil {
		return json.NewDecoder(res.Body).Decode(into)
	}
	return nil
}

type glPlayedJob struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	WebURL   string `json:"web_url"`
	Pipeline struct {
		ID     int    `json:"id"`
		WebURL string `json:"web_url"`
	} `json:"pipeline"`
}

// playJob starts a manual job, passing job-level variables (the acting-as trace).
func (c *glClient) playJob(ctx context.Context, projectPath string, jobID int, vars map[string]string) (glPlayedJob, error) {
	attrs := make([]map[string]string, 0, len(vars))
	for k, v := range vars {
		attrs = append(attrs, map[string]string{"key": k, "value": v})
	}
	var out glPlayedJob
	p := fmt.Sprintf("/projects/%s/jobs/%d/play", url.QueryEscape(projectPath), jobID)
	err := c.postJSON(ctx, p, map[string]any{"job_variables_attributes": attrs}, &out)
	return out, err
}

type glMember struct {
	Username    string `json:"username"`
	Name        string `json:"name"`
	AccessLevel int    `json:"access_level"`
}

// members lists a group's effective membership (direct + inherited) — the
// roster the acting-as picker offers.
func (c *glClient) members(ctx context.Context, groupPath string) ([]glMember, error) {
	var out []glMember
	p := fmt.Sprintf("/groups/%s/members/all", url.QueryEscape(groupPath))
	_, err := c.get(ctx, p, url.Values{"per_page": {"100"}}, &out)
	return out, err
}
