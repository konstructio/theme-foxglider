package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

type glClient struct {
	base, token string
	hc          *http.Client
}

func newGLClient(base, token string) *glClient {
	return &glClient{base: base, token: token, hc: &http.Client{Timeout: 15 * time.Second}}
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

// tags lists the newest tags for a project addressed by path.
func (c *glClient) tags(ctx context.Context, projectPath string, limit int) ([]glTag, error) {
	var out []glTag
	p := fmt.Sprintf("/projects/%s/repository/tags", url.QueryEscape(projectPath))
	_, err := c.get(ctx, p, url.Values{"per_page": {fmt.Sprint(limit)}, "order_by": {"updated"}}, &out)
	return out, err
}

// latestPipeline returns the newest pipeline for a project addressed by path
// (zero value, no error, when the project has never run one).
func (c *glClient) latestPipeline(ctx context.Context, projectPath string) (glPipeline, error) {
	var out []glPipeline
	p := fmt.Sprintf("/projects/%s/pipelines", url.QueryEscape(projectPath))
	if _, err := c.get(ctx, p, url.Values{"per_page": {"1"}}, &out); err != nil {
		return glPipeline{}, err
	}
	if len(out) == 0 {
		return glPipeline{}, nil
	}
	return out[0], nil
}
