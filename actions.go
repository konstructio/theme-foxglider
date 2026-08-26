package main

// actions.go — bot-executed CI actions (trigger, release) with SERVER-SET
// attribution. The theme's READ token never gains write power: actions use a
// separate GITLAB_ACTION_TOKEN (a group bot token distributed via the
// ThemedApp env secret — never committed to git; unset = actions hidden).
//
// Attribution: the acting identity is COMMUNICATED by konstruct, never
// chosen in the theme (an earlier "acting as" picker was an impersonation
// vector with no real weight, so it's gone) and never hardcoded. The
// konstruct shell posts the signed-in user into the iframe; the frontend
// relays it as the X-Konstruct-Actor header. Today that session resolves to
// kbot (internal SSO is not per-user yet) — commits say kbot because
// konstruct SAID kbot, not because anything assumes it. With no
// communicated identity the fallback is ACTION_ACTOR, then the neutral
// "konstruct". The identity rides every run as the INITIATED_BY job
// variable, which the CI templates stamp into the commits the jobs create.
//
// Mistake-proofing: only two job names are playable, only on projects in the
// metaphor topology, only when GitLab says the job is playable; a release
// additionally requires typing the repo's short name. Every run writes one
// greppable audit line into the runtime logs.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// actionJobs maps an action to the one manual job it may play. "deliver" is
// not job-based: it runs the tag pipeline for the newest published RC.
var actionJobs = map[string]string{
	"trigger": "trigger:manual",
	"release": "release",
}

type actions struct {
	gl    *glClient // write-scoped bot client; nil = actions disabled
	topo  topology
	actor string // fallback attribution when no identity is communicated

	// markHot flips the project onto the fast cache lane after a successful
	// action, so the delivery tiles pick up the new commit/SHA quickly.
	markHot func(project string)
}

func newActions(read *glClient, topo topology, groups []string) *actions {
	var wr *glClient
	if tok := os.Getenv("GITLAB_ACTION_TOKEN"); tok != "" {
		wr = newGLClient(read.base, tok)
	}
	actor := os.Getenv("ACTION_ACTOR")
	if actor == "" {
		actor = "konstruct" // neutral: no identity was communicated
	}
	return &actions{gl: wr, topo: topo, actor: actor}
}

func (x *actions) enabled() bool { return x.gl != nil }

// reBranch: epic-shaped or sane free-form — the UI nudges epic-N-slug, the
// engineer may type anything reasonable.
var reBranch = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{1,60}$`)

// epicsList feeds the feature modal's picker.
func (x *actions) epicsList(w http.ResponseWriter, r *http.Request) {
	if !x.enabled() {
		writeJSON(w, []any{})
		return
	}
	group := x.group()
	eps, err := x.gl.epics(context.Background(), group)
	if err != nil {
		writeJSON(w, []any{})
		return
	}
	writeJSON(w, eps)
}

// featuresList = epic-* branches on the macro repo: the join list.
func (x *actions) featuresList(w http.ResponseWriter, r *http.Request) {
	if !x.enabled() {
		writeJSON(w, []any{})
		return
	}
	brs, err := x.gl.branches(context.Background(), x.topo.MacroProj, "epic-")
	if err != nil {
		writeJSON(w, []any{})
		return
	}
	out := []map[string]any{}
	for _, b := range brs {
		if strings.HasPrefix(b.Name, "epic-") {
			out = append(out, map[string]any{"name": b.Name, "web_url": b.WebURL})
		}
	}
	writeJSON(w, out)
}

// group derives the org group from the macro project path.
func (x *actions) group() string {
	p := x.topo.MacroProj
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return p
}

// reActor keeps communicated identities to sane username shapes.
var reActor = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// actorFor resolves who this action is attributed to: the identity the
// konstruct shell communicated for this session (relayed by the frontend as
// X-Konstruct-Actor), else the configured fallback. The value is a relayed
// session fact, not a user choice — the UI offers no way to set it, and
// malformed values fall back rather than pass through.
func (x *actions) actorFor(r *http.Request) string {
	if h := r.Header.Get("X-Konstruct-Actor"); h != "" && reActor.MatchString(h) {
		return h
	}
	return x.actor
}

// allowed restricts actions to the metaphor topology — never arbitrary projects.
func (x *actions) allowed(project string) bool {
	if project == x.topo.MacroProj {
		return true
	}
	for _, s := range x.topo.Services {
		if s.Project == project {
			return true
		}
	}
	return false
}

// status tells the UI whether actions render and who they run as.
func (x *actions) status(w http.ResponseWriter, r *http.Request) {
	if !x.enabled() {
		writeJSON(w, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, map[string]any{
		"enabled": true,
		"actions": []string{"trigger", "release", "deliver", "feature"},
		"actor":   x.actorFor(r),
	})
}

type runReq struct {
	Project string `json:"project"`
	Action  string `json:"action"`
	Confirm string `json:"confirm"`
	// Version: deliver-only override — any PUBLISHED version (upgrade or
	// rollback). Empty means the newest RC tag.
	Version string `json:"version"`
	// Branch + EpicIID: feature action — the branch to start/join, and the
	// epic it came from (trace only; naming is the UI's job).
	Branch  string `json:"branch"`
	EpicIID int    `json:"epic_iid"`
	// Ref: trigger/release on a specific branch (epic-*, hotfix/*) instead of
	// the default branch — branch CI from the Branches page.
	Ref string `json:"ref"`
}

// reVersionArg keeps typed version overrides to sane semver-ish shapes.
var reVersionArg = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+-]{0,63}$`)

func (x *actions) run(w http.ResponseWriter, r *http.Request) {
	if !x.enabled() {
		writeErr(w, 503, "actions not configured (GITLAB_ACTION_TOKEN unset)")
		return
	}
	var req runReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad request body")
		return
	}
	jobName, isJob := actionJobs[req.Action]
	if !isJob && req.Action != "deliver" && req.Action != "feature" {
		writeErr(w, 400, "unknown action")
		return
	}
	if !x.allowed(req.Project) {
		writeErr(w, 400, "project is not part of the metaphor topology")
		return
	}
	actor := x.actorFor(r)
	short := req.Project[strings.LastIndex(req.Project, "/")+1:]
	if (req.Action == "release" || req.Action == "deliver") && req.Confirm != short {
		writeErr(w, 400, fmt.Sprintf("confirmation mismatch: %q required", short))
		return
	}

	// feature: start (or join) an epic feature branch — created on the micro
	// AND mirrored on the macro repo, so the feature umbrella line exists from
	// the first moment. Creating is idempotent: an existing branch means the
	// app is JOINING a feature already in flight.
	if req.Action == "feature" {
		if req.Project == x.topo.MacroProj {
			writeErr(w, 400, "start features from a service card — the macro branch follows automatically")
			return
		}
		branch := strings.TrimSpace(req.Branch)
		if !reBranch.MatchString(branch) {
			writeErr(w, 400, "branch name must be lowercase letters, digits, dots, dashes (e.g. epic-20-pink)")
			return
		}
		ctx := context.Background()
		created := map[string]bool{}
		urls := map[string]string{}
		for _, proj := range []string{req.Project, x.topo.MacroProj} {
			b, err := x.gl.createBranch(ctx, proj, branch, "main")
			if err != nil {
				if strings.Contains(err.Error(), "already exists") {
					created[proj] = false // joining
					continue
				}
				writeErr(w, 502, "branch on "+proj+": "+err.Error())
				return
			}
			created[proj] = true
			urls[proj] = b.WebURL
		}
		if x.markHot != nil {
			x.markHot(req.Project)
			x.markHot(x.topo.MacroProj)
		}
		log.Printf("ACTION feature project=%s branch=%s epic=%d micro_created=%v charts_created=%v actor=%s remote=%s",
			req.Project, branch, req.EpicIID, created[req.Project], created[x.topo.MacroProj], actor, r.RemoteAddr)
		writeJSON(w, map[string]any{
			"ok": true, "action": "feature", "actor": actor, "branch": branch,
			"micro_created": created[req.Project], "charts_created": created[x.topo.MacroProj],
			"urls": urls,
		})
		return
	}

	// deliver: run the tag pipeline for the newest published RC — GitLab's
	// canonical "deliver this version" front door. deploy:tag-dev then writes
	// the dev bump MR, which a background watcher auto-merges (dev
	// auto-delivers by policy; staging/prod stay human).
	if req.Action == "deliver" {
		if req.Project != x.topo.MacroProj {
			writeErr(w, 400, "deliver applies to the umbrella only")
			return
		}
		ctx := context.Background()
		tags, err := x.gl.tags(ctx, x.topo.MacroProj, 100)
		if err != nil {
			writeErr(w, 502, "tags: "+err.Error())
			return
		}
		var tag string
		if want := strings.TrimSpace(req.Version); want != "" {
			// explicit version (upgrade or rollback): it must be a real
			// published tag — typos never reach the delivery pipeline.
			if !reVersionArg.MatchString(want) {
				writeErr(w, 400, "that doesn't look like a version")
				return
			}
			candidate := x.topo.MacroTag + want
			for _, t := range tags {
				if t.Name == candidate {
					tag = candidate
					break
				}
			}
			if tag == "" {
				writeErr(w, 400, fmt.Sprintf("no published tag %s — check the version", candidate))
				return
			}
		} else {
			tag = newestTag(tags, x.topo.MacroTag)
		}
		if tag == "" {
			writeErr(w, 409, "no published RC tag to deliver yet")
			return
		}
		version := strings.TrimPrefix(tag, x.topo.MacroTag)
		pl, err := x.gl.createPipeline(ctx, x.topo.MacroProj, tag, map[string]string{
			"INITIATED_BY": actor,
		})
		if err != nil {
			writeErr(w, 502, "tag pipeline: "+err.Error())
			return
		}
		if x.markHot != nil {
			x.markHot(x.topo.MacroProj)
			if len(x.topo.Delivery) > 0 {
				x.markHot(x.topo.Delivery[0].Project)
			}
		}
		go x.mergeDevBump(version, actor)
		log.Printf("ACTION deliver project=%s tag=%s pipeline=%d actor=%s remote=%s",
			req.Project, tag, pl.ID, actor, r.RemoteAddr)
		writeJSON(w, map[string]any{
			"ok": true, "action": "deliver", "actor": actor,
			"version": version, "tag": tag,
			"job_url": pl.WebURL, "pipeline_url": pl.WebURL,
		})
		return
	}

	// Uncached reads: an action must see reality, not a 45s-old cache.
	ctx := context.Background()

	// Branch-scoped trigger: run the branch's CI on its tip — for epic
	// branches that's the whole publish chain (a fresh -<branch>.N version).
	if req.Action == "trigger" && req.Ref != "" {
		if !reBranch.MatchString(strings.ToLower(req.Ref)) {
			writeErr(w, 400, "that doesn't look like a branch name")
			return
		}
		pl, err := x.gl.createPipeline(ctx, req.Project, req.Ref, map[string]string{
			"INITIATED_BY": actor,
		})
		if err != nil {
			writeErr(w, 502, "pipeline on "+req.Ref+": "+err.Error())
			return
		}
		if x.markHot != nil {
			x.markHot(req.Project)
		}
		log.Printf("ACTION trigger project=%s ref=%s pipeline=%d actor=%s remote=%s",
			req.Project, req.Ref, pl.ID, actor, r.RemoteAddr)
		writeJSON(w, map[string]any{
			"ok": true, "action": "trigger", "actor": actor, "ref": req.Ref,
			"job_url": pl.WebURL, "pipeline_url": pl.WebURL,
		})
		return
	}

	// Branch-scoped release: play the release job on the branch's newest
	// pipeline (hotfix releases live on hotfix branches).
	var lp glLatestPipeline
	var err error
	if req.Action == "release" && req.Ref != "" {
		recent, rerr := x.gl.recentPipelines(ctx, req.Project, req.Ref, "", 1)
		if rerr != nil || len(recent) == 0 {
			writeErr(w, 409, "no pipeline on "+req.Ref+" yet — trigger it first")
			return
		}
		full, ferr := x.gl.pipelineByPath(ctx, req.Project, recent[0].ID)
		if ferr != nil {
			writeErr(w, 502, "pipeline: "+ferr.Error())
			return
		}
		lp = full
	} else {
		lp, err = x.gl.latestPipeline(ctx, req.Project)
		if err != nil {
			writeErr(w, 502, "latest pipeline: "+err.Error())
			return
		}
	}
	jobs, err := x.gl.jobsByPath(ctx, req.Project, lp.ID)
	if err != nil {
		writeErr(w, 502, "jobs: "+err.Error())
		return
	}
	var target *glJob
	for i := range jobs {
		if jobs[i].Name == jobName {
			target = &jobs[i]
			break
		}
	}
	if target == nil || target.Status != "manual" {
		// Trigger degrades gracefully: when the latest pipeline has no playable
		// trigger job (config-error pipelines have zero jobs), start a fresh
		// pipeline on the same ref instead. Same delivery outcome, no new
		// commit — the trace rides as a pipeline variable.
		if req.Action == "trigger" {
			pl, err := x.gl.createPipeline(ctx, req.Project, lp.Ref, map[string]string{
				"INITIATED_BY": actor,
			})
			if err != nil {
				writeErr(w, 502, "new pipeline: "+err.Error())
				return
			}
			if x.markHot != nil {
				x.markHot(req.Project)
			}
			log.Printf("ACTION %s project=%s mode=fresh-pipeline pipeline=%d actor=%s remote=%s",
				req.Action, req.Project, pl.ID, actor, r.RemoteAddr)
			writeJSON(w, map[string]any{
				"ok": true, "action": req.Action, "actor": actor,
				"job_url": pl.WebURL, "pipeline_url": pl.WebURL,
			})
			return
		}
		if target == nil {
			writeErr(w, 409, fmt.Sprintf("job %q not present on pipeline #%d", jobName, lp.ID))
			return
		}
		writeErr(w, 409, fmt.Sprintf("job %q is %s — not playable right now", jobName, target.Status))
		return
	}

	played, err := x.gl.playJob(ctx, req.Project, target.ID, map[string]string{
		"INITIATED_BY": actor,
	})
	if err != nil {
		writeErr(w, 502, "play: "+err.Error())
		return
	}
	if x.markHot != nil {
		x.markHot(req.Project)
	}
	// One greppable audit line per action, in the platform runtime logs.
	log.Printf("ACTION %s project=%s job=%s(%d) pipeline=%d actor=%s remote=%s",
		req.Action, req.Project, jobName, target.ID, lp.ID, actor, r.RemoteAddr)
	writeJSON(w, map[string]any{
		"ok": true, "action": req.Action, "actor": actor,
		"job_url": played.WebURL, "pipeline_url": lp.WebURL,
	})
}

// mergeDevBump watches the gitops repo for the dev bump MR this delivery
// produces and merges it — dev auto-delivers; anything not matching this one
// version's bump title is never touched. Gives the deploy bridge ~7 minutes.
func (x *actions) mergeDevBump(version, actor string) {
	if len(x.topo.Delivery) == 0 {
		return
	}
	gitops := x.topo.Delivery[0].Project
	needle := fmt.Sprintf("bump %s to %s", x.topo.MacroName, version)
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	for {
		mrs, err := x.gl.openMRs(ctx, gitops)
		if err == nil {
			for _, mr := range mrs {
				if !strings.Contains(mr.Title, needle) {
					continue
				}
				if merged, err := x.gl.mergeMR(ctx, gitops, mr.IID); err == nil {
					log.Printf("ACTION deliver-merge project=%s mr=%d version=%s actor=%s state=%s",
						gitops, mr.IID, version, actor, merged.State)
					if x.markHot != nil {
						x.markHot(gitops)
					}
					return
				}
				// not mergeable yet (pipeline running etc.) — retry next tick
			}
		}
		select {
		case <-ctx.Done():
			log.Printf("ACTION deliver-merge timeout version=%s (no bump MR merged)", version)
			return
		case <-time.After(10 * time.Second):
		}
	}
}

// reEpicRef finds epic ids in commit titles and branch names (epic-20-pink).
var reEpicRef = regexp.MustCompile(`epic-(\d+)`)

// upgradePreview answers "what enters the environment if I upgrade to X":
// the dependency moves between the current desired umbrella and the target,
// each moved service's commits in that window, and any epics those commits
// trace to (via commit titles and MR source branches). Best-effort by
// design — an empty answer is honest, never fabricated.
func (x *actions) upgradePreview(w http.ResponseWriter, r *http.Request) {
	if !x.enabled() {
		writeErr(w, 503, "actions not configured")
		return
	}
	to := strings.TrimSpace(r.URL.Query().Get("to"))
	if to == "" || !reVersionArg.MatchString(to) {
		writeErr(w, 400, "to=<published version> required")
		return
	}
	ctx := context.Background()
	// current desired = targetRevision in the delivery app file
	from := ""
	if len(x.topo.Delivery) > 0 {
		d := x.topo.Delivery[0]
		if raw, err := x.gl.fileRaw(ctx, d.Project, d.App, "main"); err == nil {
			from = targetRevision(raw)
		}
	}
	type change struct {
		Service string     `json:"service"`
		From    string     `json:"from"`
		To      string     `json:"to"`
		Commits []glCommit `json:"commits"`
		Epics   []glEpic   `json:"epics"`
	}
	out := struct {
		From    string   `json:"from"`
		To      string   `json:"to"`
		Changes []change `json:"changes"`
		Epics   []glEpic `json:"epics"`
	}{From: from, To: to, Changes: []change{}, Epics: []glEpic{}}

	if from == "" || from == to {
		writeJSON(w, out)
		return
	}
	fromRaw, err1 := x.gl.fileRaw(ctx, x.topo.MacroProj, x.topo.MacroFile, x.topo.MacroTag+from)
	toRaw, err2 := x.gl.fileRaw(ctx, x.topo.MacroProj, x.topo.MacroFile, x.topo.MacroTag+to)
	if err1 != nil || err2 != nil {
		writeJSON(w, out) // tags missing → nothing to say, honestly
		return
	}
	fromDeps, toDeps := macroDeps(fromRaw), macroDeps(toRaw)
	// window boundaries from the two tags' commit times
	var since, until time.Time
	if c, err := x.gl.commitInfo(ctx, x.topo.MacroProj, x.topo.MacroTag+from); err == nil {
		since = c.AuthoredDate
	}
	if c, err := x.gl.commitInfo(ctx, x.topo.MacroProj, x.topo.MacroTag+to); err == nil {
		until = c.AuthoredDate
	}
	seenEpic := map[int]bool{}
	for _, s := range x.topo.Services {
		f, t := fromDeps[s.Name], toDeps[s.Name]
		if f == t {
			continue
		}
		ch := change{Service: s.Name, From: f, To: t, Commits: []glCommit{}, Epics: []glEpic{}}
		commits, _ := x.gl.commitsRange(ctx, s.Project, "main", since, until, 20)
		epicIDs := map[int]bool{}
		for i, c := range commits {
			ch.Commits = append(ch.Commits, c)
			for _, m := range reEpicRef.FindAllStringSubmatch(c.Title, -1) {
				if n, err := strconv.Atoi(m[1]); err == nil {
					epicIDs[n] = true
				}
			}
			if i >= 8 {
				continue // MR lookups are the expensive part — cap them
			}
			if mrs, err := x.gl.commitMRs(ctx, s.Project, c.ID); err == nil {
				for _, mr := range mrs {
					for _, m := range reEpicRef.FindAllStringSubmatch(mr.SourceBranch+" "+mr.Title, -1) {
						if n, err := strconv.Atoi(m[1]); err == nil {
							epicIDs[n] = true
						}
					}
				}
			}
		}
		for iid := range epicIDs {
			if ep, err := x.gl.epicByIID(ctx, x.group(), iid); err == nil && ep.IID != 0 {
				ch.Epics = append(ch.Epics, ep)
				if !seenEpic[iid] {
					seenEpic[iid] = true
					out.Epics = append(out.Epics, ep)
				}
			}
		}
		out.Changes = append(out.Changes, ch)
	}
	writeJSON(w, out)
}
