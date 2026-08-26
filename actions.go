package main

// actions.go — bot-executed CI actions (trigger, release) with SERVER-SET
// attribution. The theme's READ token never gains write power: actions use a
// separate GITLAB_ACTION_TOKEN (a group bot token distributed via the
// ThemedApp env secret — never committed to git; unset = actions hidden).
//
// Attribution: the backend stamps the acting identity itself — the signed-in
// konstruct user. Clients cannot supply or influence it (an earlier design
// offered an "acting as" picker; that was an impersonation vector with no
// real weight, so it's gone). Until konstruct SSO forwards a per-user
// identity into the theme, the actor is the configured session identity
// (ACTION_ACTOR, default "kbot") — coarse but honest. The identity rides
// every run as the INITIATED_BY job variable, which the CI templates stamp
// into the commits the jobs create. Seam: when konstruct forwards a signed
// per-user identity, only actorFor() changes.
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
	"strings"
)

// actionJobs maps an action to the one manual job it may play.
var actionJobs = map[string]string{
	"trigger": "trigger:manual",
	"release": "release",
}

type actions struct {
	gl    *glClient // write-scoped bot client; nil = actions disabled
	topo  topology
	actor string // server-set attribution for every action
}

func newActions(read *glClient, topo topology, groups []string) *actions {
	var wr *glClient
	if tok := os.Getenv("GITLAB_ACTION_TOKEN"); tok != "" {
		wr = newGLClient(read.base, tok)
	}
	actor := os.Getenv("ACTION_ACTOR")
	if actor == "" {
		// The konstruct session identity. Internal SSO isn't per-user yet, so
		// everyone shares the platform identity — coarse today, honest always.
		actor = "kbot"
	}
	return &actions{gl: wr, topo: topo, actor: actor}
}

func (x *actions) enabled() bool { return x.gl != nil }

// actorFor resolves who this action is attributed to. Server-side only — the
// request never carries identity. This is the konstruct-SSO seam: a verified
// per-user identity forwarded by the shell would be read here.
func (x *actions) actorFor(r *http.Request) string { return x.actor }

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
		"actions": []string{"trigger", "release"},
		"actor":   x.actorFor(r),
	})
}

type runReq struct {
	Project string `json:"project"`
	Action  string `json:"action"`
	Confirm string `json:"confirm"`
}

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
	jobName, ok := actionJobs[req.Action]
	if !ok {
		writeErr(w, 400, "unknown action")
		return
	}
	if !x.allowed(req.Project) {
		writeErr(w, 400, "project is not part of the metaphor topology")
		return
	}
	actor := x.actorFor(r)
	short := req.Project[strings.LastIndex(req.Project, "/")+1:]
	if req.Action == "release" && req.Confirm != short {
		writeErr(w, 400, fmt.Sprintf("confirmation mismatch: type %q to release", short))
		return
	}

	// Uncached reads: an action must see reality, not a 45s-old cache.
	ctx := context.Background()
	lp, err := x.gl.latestPipeline(ctx, req.Project)
	if err != nil {
		writeErr(w, 502, "latest pipeline: "+err.Error())
		return
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
	// One greppable audit line per action, in the platform runtime logs.
	log.Printf("ACTION %s project=%s job=%s(%d) pipeline=%d actor=%s remote=%s",
		req.Action, req.Project, jobName, target.ID, lp.ID, actor, r.RemoteAddr)
	writeJSON(w, map[string]any{
		"ok": true, "action": req.Action, "actor": actor,
		"job_url": played.WebURL, "pipeline_url": lp.WebURL,
	})
}
