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
	ID                int       `json:"id"`
	Name              string    `json:"name"`
	PathWithNamespace string    `json:"path_with_namespace"`
	WebURL            string    `json:"web_url"`
	DefaultBranch     string    `json:"default_branch"`
	LastActivityAt    time.Time `json:"last_activity_at"`
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
	Name   string `json:"name"`
	Commit struct {
		CreatedAt *time.Time `json:"created_at"`
	} `json:"commit"`
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

// tagsSearch lists newest tags whose name matches search. GitLab honours ^term
// (starts-with) and term$ (ends-with) anchors. Used to pull an umbrella's own
// version line out of a charts monorepo where every subchart shares the tag
// prefix — a server-side filter beats paging the whole tag list.
func (c *glClient) tagsSearch(ctx context.Context, projectPath, search string, limit int) ([]glTag, error) {
	var out []glTag
	p := fmt.Sprintf("/projects/%s/repository/tags", url.QueryEscape(projectPath))
	_, err := c.get(ctx, p, url.Values{"per_page": {fmt.Sprint(limit)}, "order_by": {"updated"}, "search": {search}}, &out)
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

// glRef is a branch/tag a commit is contained in — commitRefs' element.
type glRef struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// commitRefs lists the branches that contain a commit — the provenance lookup
// for sha-suffixed RC pins (which line a build came from).
func (c *glClient) commitRefs(ctx context.Context, projectPath, sha string) ([]glRef, error) {
	var out []glRef
	p := fmt.Sprintf("/projects/%s/repository/commits/%s/refs", url.QueryEscape(projectPath), url.PathEscape(sha))
	_, err := c.get(ctx, p, url.Values{"type": {"branch"}, "per_page": {"20"}}, &out)
	return out, err
}

// commitExists reports whether a commit is present in a project. An explicit
// 404 is a definite "no" (nil error); any other non-200 is a real failure the
// caller must NOT read as absence (a GitLab outage is not a missing commit).
func (c *glClient) commitExists(ctx context.Context, projectPath, sha string) (bool, error) {
	u := c.base + "/api/v4" + fmt.Sprintf("/projects/%s/repository/commits/%s",
		url.QueryEscape(projectPath), url.PathEscape(sha))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	res, err := c.hc.Do(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("gitlab commit %s: %s", sha, res.Status)
	}
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

// pipelineByPath fetches one pipeline (with its user) for a path-addressed
// project — used to upgrade a fallback list entry to full card data.
func (c *glClient) pipelineByPath(ctx context.Context, projectPath string, id int) (glLatestPipeline, error) {
	var out glLatestPipeline
	p := fmt.Sprintf("/projects/%s/pipelines/%d", url.QueryEscape(projectPath), id)
	_, err := c.get(ctx, p, nil, &out)
	return out, err
}

// recentPipelines lists pipelines for a path-addressed project filtered by
// ref/status — used to find the previous successful run for progress baselines.
func (c *glClient) recentPipelines(ctx context.Context, projectPath, ref, status string, limit int) ([]glSHAPipeline, error) {
	var out []glSHAPipeline
	q := url.Values{"per_page": {fmt.Sprint(limit)}}
	if ref != "" {
		q.Set("ref", ref)
	}
	if status != "" {
		q.Set("status", status)
	}
	p := fmt.Sprintf("/projects/%s/pipelines", url.QueryEscape(projectPath))
	_, err := c.get(ctx, p, q, &out)
	return out, err
}

// createPipeline starts a fresh pipeline on a ref with pipeline-level
// variables — the re-run fallback when the latest pipeline has no playable
// trigger job (e.g. it was a config-error pipeline with zero jobs).
func (c *glClient) createPipeline(ctx context.Context, projectPath, ref string, vars map[string]string) (glPipeline, error) {
	vv := make([]map[string]string, 0, len(vars))
	for k, v := range vars {
		vv = append(vv, map[string]string{"key": k, "value": v})
	}
	var out glPipeline
	p := fmt.Sprintf("/projects/%s/pipeline", url.QueryEscape(projectPath))
	err := c.postJSON(ctx, p, map[string]any{"ref": ref, "variables": vv}, &out)
	return out, err
}

// --- merge-request surface (the deliver action merges the dev bump MR) ---

type glMR struct {
	IID            int        `json:"iid"`
	Title          string     `json:"title"`
	State          string     `json:"state"`
	WebURL         string     `json:"web_url"`
	Draft          bool       `json:"draft"`
	UserNotesCount int        `json:"user_notes_count"`
	MergedAt       *time.Time `json:"merged_at"`
	SourceBranch   string     `json:"source_branch"`
	// Author/MergedBy: who did the PR — surfaced on merged feature cells whose
	// branch (and its committer avatar) is gone after the merge.
	Author   *glUser `json:"author"`
	MergedBy *glUser `json:"merged_by"`
	// DetailedMergeStatus/HasConflicts: GitLab's mergeability verdict — the
	// catch-up action only acts when this says "mergeable".
	DetailedMergeStatus string `json:"detailed_merge_status"`
	HasConflicts        bool   `json:"has_conflicts"`
}

type glUser struct {
	Name      string `json:"name"`
	Username  string `json:"username"`
	AvatarURL string `json:"avatar_url"`
}

// openMRs lists a project's open merge requests, newest first.
func (c *glClient) openMRs(ctx context.Context, projectPath string) ([]glMR, error) {
	var out []glMR
	p := fmt.Sprintf("/projects/%s/merge_requests", url.QueryEscape(projectPath))
	_, err := c.get(ctx, p, url.Values{"state": {"opened"}, "per_page": {"20"}, "order_by": {"created_at"}}, &out)
	return out, err
}

type glGroup struct {
	Name      string `json:"name"`
	FullPath  string `json:"full_path"`
	AvatarURL string `json:"avatar_url"`
	WebURL    string `json:"web_url"`
}

// group fetches a group's public face (name, avatar, url).
func (c *glClient) group(ctx context.Context, groupPath string) (glGroup, error) {
	var out glGroup
	p := fmt.Sprintf("/groups/%s", url.QueryEscape(groupPath))
	_, err := c.get(ctx, p, url.Values{"with_projects": {"false"}}, &out)
	return out, err
}

// fetchBytes GETs an arbitrary URL with the token — the group-avatar proxy
// (private groups serve avatars only to authenticated requests, which the
// browser isn't). Capped at 1MB.
func (c *glClient) fetchBytes(ctx context.Context, rawURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, "", fmt.Errorf("fetch %s: %s", rawURL, res.Status)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, "", err
	}
	return b, res.Header.Get("Content-Type"), nil
}

// commitFile creates a single-file commit on a NEW branch cut from start —
// the mr-mode delivery write (bump targetRevision, open an MR, never merge
// platform repos ourselves).
func (c *glClient) commitFile(ctx context.Context, projectPath, newBranch, startBranch, filePath, content, message string) error {
	body := map[string]any{
		"branch": newBranch, "start_branch": startBranch, "commit_message": message,
		"actions": []map[string]string{{"action": "update", "file_path": filePath, "content": content}},
	}
	p := fmt.Sprintf("/projects/%s/repository/commits", url.QueryEscape(projectPath))
	return c.postJSON(ctx, p, body, nil)
}

// mr fetches one merge request by iid.
func (c *glClient) mr(ctx context.Context, projectPath string, iid int) (glMR, error) {
	var out glMR
	p := fmt.Sprintf("/projects/%s/merge_requests/%d", url.QueryEscape(projectPath), iid)
	_, err := c.get(ctx, p, nil, &out)
	return out, err
}

// mrsBySource lists MRs (any state) whose source is the given branch — the
// feature view's "which MR carries this work" lookup.
func (c *glClient) mrsBySource(ctx context.Context, projectPath, branch string) ([]glMR, error) {
	var out []glMR
	p := fmt.Sprintf("/projects/%s/merge_requests", url.QueryEscape(projectPath))
	_, err := c.get(ctx, p, url.Values{"source_branch": {branch}, "per_page": {"5"}, "order_by": {"created_at"}}, &out)
	return out, err
}

// createMR opens a merge request (used for the auto-drafted feature MR).
// removeSource asks GitLab to delete the SOURCE branch on merge — right for
// feature/scratch sources, catastrophic for the catch-up probe whose source
// is main. The caller must say which it is; there is no safe default for
// both shapes.
func (c *glClient) createMR(ctx context.Context, projectPath, source, target, title, description string, removeSource bool) (glMR, error) {
	var out glMR
	p := fmt.Sprintf("/projects/%s/merge_requests", url.QueryEscape(projectPath))
	err := c.postJSON(ctx, p, map[string]any{
		"source_branch": source, "target_branch": target,
		"title": title, "description": description,
		"remove_source_branch": removeSource,
	}, &out)
	return out, err
}

// mergeMR merges one MR; best-effort approve first (repos differ on rules).
func (c *glClient) mergeMR(ctx context.Context, projectPath string, iid int) (glMR, error) {
	base := fmt.Sprintf("/projects/%s/merge_requests/%d", url.QueryEscape(projectPath), iid)
	_ = c.postJSON(ctx, base+"/approve", nil, nil) // best-effort
	var out glMR
	err := c.putJSON(ctx, base+"/merge", nil, &out)
	return out, err
}

// closeMR closes an MR without merging — cleanup for the short-lived
// catch-up MRs that turn out not to be safely mergeable.
func (c *glClient) closeMR(ctx context.Context, projectPath string, iid int) error {
	var out glMR
	p := fmt.Sprintf("/projects/%s/merge_requests/%d", url.QueryEscape(projectPath), iid)
	return c.putJSON(ctx, p, map[string]string{"state_event": "close"}, &out)
}

// putJSON mirrors postJSON for PUT endpoints.
func (c *glClient) putJSON(ctx context.Context, path string, body, into any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+"/api/v4"+path, rdr)
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

// --- epics + branches (feature-branch workflow) ---

type glEpic struct {
	IID    int    `json:"iid"`
	Title  string `json:"title"`
	State  string `json:"state"`
	WebURL string `json:"web_url"`
}

// epics lists a group's open epics — the feature picker's source.
func (c *glClient) epics(ctx context.Context, groupPath string) ([]glEpic, error) {
	var out []glEpic
	p := fmt.Sprintf("/groups/%s/epics", url.QueryEscape(groupPath))
	_, err := c.get(ctx, p, url.Values{"state": {"opened"}, "per_page": {"50"}, "order_by": {"created_at"}}, &out)
	return out, err
}

type glBranch struct {
	Name   string `json:"name"`
	WebURL string `json:"web_url"`
	Commit *struct {
		ShortID       string    `json:"short_id"`
		Title         string    `json:"title"`
		AuthorName    string    `json:"author_name"`
		AuthorEmail   string    `json:"author_email"`
		CommittedDate time.Time `json:"committed_date"`
	} `json:"commit"`
}

// branches lists a project's branches, optionally filtered by search term.
func (c *glClient) branches(ctx context.Context, projectPath, search string) ([]glBranch, error) {
	var out []glBranch
	q := url.Values{"per_page": {"100"}}
	if search != "" {
		q.Set("search", search)
	}
	p := fmt.Sprintf("/projects/%s/repository/branches", url.QueryEscape(projectPath))
	_, err := c.get(ctx, p, q, &out)
	return out, err
}

// createBranch creates a branch from ref; GitLab 400s when it already exists.
func (c *glClient) createBranch(ctx context.Context, projectPath, branch, ref string) (glBranch, error) {
	var out glBranch
	p := fmt.Sprintf("/projects/%s/repository/branches?branch=%s&ref=%s",
		url.QueryEscape(projectPath), url.QueryEscape(branch), url.QueryEscape(ref))
	err := c.postJSON(ctx, p, nil, &out)
	return out, err
}

// epicUpdate flips an epic's lifecycle labels and (optionally) closes it —
// the delivery-driven status transitions. Empty args are omitted.
func (c *glClient) epicUpdate(ctx context.Context, groupPath string, iid int, addLabels, removeLabels, stateEvent string) error {
	body := map[string]any{}
	if addLabels != "" {
		body["add_labels"] = addLabels
	}
	if removeLabels != "" {
		body["remove_labels"] = removeLabels
	}
	if stateEvent != "" {
		body["state_event"] = stateEvent
	}
	p := fmt.Sprintf("/groups/%s/epics/%d", url.QueryEscape(groupPath), iid)
	return c.putJSON(ctx, p, body, nil)
}

// epicByIID fetches one epic regardless of state (preview resolution).
func (c *glClient) epicByIID(ctx context.Context, groupPath string, iid int) (glEpic, error) {
	var out glEpic
	p := fmt.Sprintf("/groups/%s/epics/%d", url.QueryEscape(groupPath), iid)
	_, err := c.get(ctx, p, nil, &out)
	return out, err
}

// commitsRange lists commits on a ref within a time window (newest first).
// commitsForPath lists the newest commits touching one path on a ref — the
// "when did this file last change" lookup.
func (c *glClient) commitsForPath(ctx context.Context, projectPath, ref, path string, limit int) ([]glCommit, error) {
	var out []glCommit
	q := url.Values{"ref_name": {ref}, "path": {path}, "per_page": {fmt.Sprint(limit)}}
	p := fmt.Sprintf("/projects/%s/repository/commits", url.QueryEscape(projectPath))
	_, err := c.get(ctx, p, q, &out)
	return out, err
}

func (c *glClient) commitsRange(ctx context.Context, projectPath, ref string, since, until time.Time, limit int) ([]glCommit, error) {
	var out []glCommit
	q := url.Values{"ref_name": {ref}, "per_page": {fmt.Sprint(limit)}}
	if !since.IsZero() {
		q.Set("since", since.Format(time.RFC3339))
	}
	if !until.IsZero() {
		q.Set("until", until.Format(time.RFC3339))
	}
	p := fmt.Sprintf("/projects/%s/repository/commits", url.QueryEscape(projectPath))
	_, err := c.get(ctx, p, q, &out)
	return out, err
}

// commitMRs lists the merge requests a commit belongs to (epic detection via
// MR source branches like epic-20-pink).
func (c *glClient) commitMRs(ctx context.Context, projectPath, sha string) ([]struct {
	IID          int    `json:"iid"`
	Title        string `json:"title"`
	SourceBranch string `json:"source_branch"`
	WebURL       string `json:"web_url"`
}, error) {
	var out []struct {
		IID          int    `json:"iid"`
		Title        string `json:"title"`
		SourceBranch string `json:"source_branch"`
		WebURL       string `json:"web_url"`
	}
	p := fmt.Sprintf("/projects/%s/repository/commits/%s/merge_requests", url.QueryEscape(projectPath), url.PathEscape(sha))
	_, err := c.get(ctx, p, nil, &out)
	return out, err
}

type glCompareCommit struct {
	AuthorName  string `json:"author_name"`
	AuthorEmail string `json:"author_email"`
}

// compareBranch returns how many commits `to` carries that `from` lacks —
// the "hotfix not merged back to main" signal — plus the distinct people who
// wrote them, newest first, capped at 5 (the chip avatar stack).
func (c *glClient) compareBranch(ctx context.Context, projectPath, from, to string) (int, []glCompareCommit, error) {
	var out struct {
		Commits []glCompareCommit `json:"commits"`
	}
	p := fmt.Sprintf("/projects/%s/repository/compare", url.QueryEscape(projectPath))
	if _, err := c.get(ctx, p, url.Values{"from": {from}, "to": {to}}, &out); err != nil {
		return 0, nil, err
	}
	// commits arrive oldest→newest; walk backwards so recency wins the cap
	seen := map[string]bool{}
	var people []glCompareCommit
	for i := len(out.Commits) - 1; i >= 0 && len(people) < 5; i-- {
		cm := out.Commits[i]
		key := strings.ToLower(cm.AuthorEmail)
		if key == "" {
			key = strings.ToLower(cm.AuthorName)
		}
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		people = append(people, cm)
	}
	return len(out.Commits), people, nil
}

// avatar resolves a commit email to an avatar URL via GitLab's avatar API
// (falls back to gravatar server-side; "" when nothing resolves).
func (c *glClient) avatar(ctx context.Context, email string) (string, error) {
	var out struct {
		AvatarURL string `json:"avatar_url"`
	}
	_, err := c.get(ctx, "/avatar", url.Values{"email": {email}, "size": {"48"}}, &out)
	return out.AvatarURL, err
}

type glRelease struct {
	TagName    string    `json:"tag_name"`
	Name       string    `json:"name"`
	ReleasedAt time.Time `json:"released_at"`
	Links      struct {
		Self string `json:"self"`
	} `json:"_links"`
}

// latestRelease returns a project's newest release, or nil when none exist.
func (c *glClient) latestRelease(ctx context.Context, projectID int) (*glRelease, error) {
	var out []glRelease
	p := fmt.Sprintf("/projects/%d/releases", projectID)
	if _, err := c.get(ctx, p, url.Values{"per_page": {"1"}}, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

// latestReleaseByPath is latestRelease for path-addressed projects (the
// delivery-ecosystem side of the client).
func (c *glClient) latestReleaseByPath(ctx context.Context, projectPath string) (*glRelease, error) {
	var out []glRelease
	p := fmt.Sprintf("/projects/%s/releases", url.QueryEscape(projectPath))
	if _, err := c.get(ctx, p, url.Values{"per_page": {"1"}}, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

// deleteBranch removes a branch ref. GitLab refuses protected branches (403)
// and 404s absent ones — both surface as errors.
func (c *glClient) deleteBranch(ctx context.Context, projectPath, branch string) error {
	p := fmt.Sprintf("/projects/%s/repository/branches/%s",
		url.QueryEscape(projectPath), url.PathEscape(branch))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.base+"/api/v4"+p, nil)
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	res, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("gitlab %s: %s", p, res.Status)
	}
	return nil
}
