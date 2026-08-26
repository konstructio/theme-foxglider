package main

// actions.go — bot-executed CI actions (re-run, release) with the acting user
// carried as a trace. The theme's READ token never gains write power: actions
// use a separate GITLAB_ACTION_TOKEN (a group bot token distributed via the
// ThemedApp env secret — never committed to git; unset = actions hidden).
//
// Identity is self-declared v1: the operator picks who they are from the
// group roster and the choice rides the run as an INITIATED_BY job variable,
// which the CI templates stamp into the commits the job creates (author on
// the SHA + Initiated-by trailer). The seam upgrades cleanly to a
// konstruct-forwarded verified identity later — only the source of
// `acting_as` changes, nothing else.
//
// Mistake-proofing: only two job names are playable, only on projects in the
// metaphor topology, only when GitLab says the job is in "manual" state; a
// release additionally requires typing the repo's short name. Every run
// writes one greppable audit line into the runtime logs.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const ttlMembers = 5 * time.Minute

// actionJobs maps an action to the one manual job it may play.
var actionJobs = map[string]string{
	"trigger": "trigger:manual",
	"release": "release",
}

type actions struct {
	gl    *glClient // write-scoped bot client; nil = actions disabled
	read  *glClient // read client (roster)
	c     *cache
	topo  topology
	group string
}

func newActions(read *glClient, topo topology, groups []string) *actions {
	group := "civo/metaphor"
	if len(groups) > 0 {
		group = groups[0]
	}
	var wr *glClient
	if tok := os.Getenv("GITLAB_ACTION_TOKEN"); tok != "" {
		wr = newGLClient(read.base, tok)
	}
	return &actions{gl: wr, read: read, c: newCache(), topo: topo, group: group}
}

func (x *actions) enabled() bool { return x.gl != nil }

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

// status tells the UI whether actions render, and offers the acting-as roster.
func (x *actions) status(w http.ResponseWriter, r *http.Request) {
	if !x.enabled() {
		writeJSON(w, map[string]any{"enabled": false})
		return
	}
	type person struct {
		Username string `json:"username"`
		Name     string `json:"name"`
	}
	var roster []person
	if v, err := x.c.do("members:"+x.group, ttlMembers, func() (any, error) {
		return x.read.members(context.Background(), x.group)
	}); err == nil {
		for _, m := range v.([]glMember) {
			// Developer+ humans only: bots can't be "acting as" anyone.
			if m.AccessLevel >= 30 && !strings.HasPrefix(m.Username, "group_") &&
				!strings.Contains(strings.ToLower(m.Username), "bot") {
				roster = append(roster, person{m.Username, m.Name})
			}
		}
		sort.Slice(roster, func(i, j int) bool { return roster[i].Username < roster[j].Username })
	}
	writeJSON(w, map[string]any{"enabled": true, "actions": []string{"trigger", "release"}, "members": roster})
}

type runReq struct {
	Project  string `json:"project"`
	Action   string `json:"action"`
	ActingAs string `json:"acting_as"`
	Confirm  string `json:"confirm"`
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
	if req.ActingAs == "" {
		writeErr(w, 400, "acting_as required — pick who you are")
		return
	}
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
		// Re-run degrades gracefully: when the latest pipeline has no playable
		// trigger job (config-error pipelines have zero jobs), start a fresh
		// pipeline on the same ref instead. Same delivery outcome, no new
		// commit — the trace rides as a pipeline variable.
		if req.Action == "trigger" {
			pl, err := x.gl.createPipeline(ctx, req.Project, lp.Ref, map[string]string{
				"INITIATED_BY": req.ActingAs,
			})
			if err != nil {
				writeErr(w, 502, "new pipeline: "+err.Error())
				return
			}
			log.Printf("ACTION %s project=%s mode=fresh-pipeline pipeline=%d acting_as=%s remote=%s",
				req.Action, req.Project, pl.ID, req.ActingAs, r.RemoteAddr)
			writeJSON(w, map[string]any{
				"ok": true, "action": req.Action, "acting_as": req.ActingAs,
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
		"INITIATED_BY": req.ActingAs,
	})
	if err != nil {
		writeErr(w, 502, "play: "+err.Error())
		return
	}
	// One greppable audit line per action, in the platform runtime logs.
	log.Printf("ACTION %s project=%s job=%s(%d) pipeline=%d acting_as=%s remote=%s",
		req.Action, req.Project, jobName, target.ID, lp.ID, req.ActingAs, r.RemoteAddr)
	writeJSON(w, map[string]any{
		"ok": true, "action": req.Action, "acting_as": req.ActingAs,
		"job_url": played.WebURL, "pipeline_url": lp.WebURL,
	})
}
