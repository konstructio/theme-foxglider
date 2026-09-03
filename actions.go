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
	// dropBranches invalidates the branches cache for a project after a
	// mutation (branch deleted), so the next Branches render is honest.
	dropBranches func(project string)
	// noteDeleted tombstones a deleted branch so renders skip it while
	// GitLab's branch list catches up (it can lag the DELETE by seconds).
	noteDeleted func(project, branch string)
	// dropMRs invalidates the cached MR lookup for a branch after a merge so
	// the feature view reflects it immediately.
	dropMRs func(project, branch string)
	// clientFor resolves a delivery target's own host/credential client.
	clientFor func(deliverySpec) *glClient
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
		"enabled":       true,
		"actions":       []string{"trigger", "release", "deliver", "feature", "delete", "merge-mr", "hotfix", "hotfix-join", "retire", "catchup"},
		"actor":         x.actorFor(r),
		"branch_policy": x.topo.BranchPolicy,
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
	// MRIID: merge-mr — the carrying MR to merge from the feature view.
	MRIID int `json:"mr_iid"`
	// Env: deliver — which delivery target (defaults to the first).
	Env string `json:"env"`
	// Tag: hotfix — the published umbrella tag to branch a hotfix from.
	Tag string `json:"tag"`
}

// reVersionArg keeps typed version overrides to sane semver-ish shapes.
var reVersionArg = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+-]{0,63}$`)

// clientIP prefers the first X-Forwarded-For hop (the true client, appended
// by the gateway) over RemoteAddr, which behind Envoy is only the last hop.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	return r.RemoteAddr
}

// branchBuilding reports whether a pipeline is still in flight on a branch.
// Destroying a branch mid-build orphans the pipeline: its later-stage jobs
// fail at get_sources ("couldn't find remote ref"). A lookup error fails OPEN
// (returns false) — a convenience guard must not wedge the action when GitLab
// is briefly unreachable; the confirm handshake is still the real gate.
func (x *actions) branchBuilding(ctx context.Context, project, branch string) bool {
	pls, err := x.gl.recentPipelines(ctx, project, branch, "", 5)
	if err != nil {
		return false
	}
	for _, p := range pls {
		switch p.Status {
		case "running", "pending", "created", "waiting_for_resource":
			return true
		}
	}
	return false
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
	jobName, isJob := actionJobs[req.Action]
	if !isJob && req.Action != "deliver" && req.Action != "feature" && req.Action != "delete" &&
		req.Action != "merge-mr" && req.Action != "hotfix" && req.Action != "hotfix-join" && req.Action != "retire" &&
		req.Action != "catchup" {
		writeErr(w, 400, "unknown action")
		return
	}
	actor := x.actorFor(r)

	// hotfix: cut a hotfix branch across every repo a published umbrella tag
	// pins. It carries no single project (it resolves them from the tag and
	// validates each against the topology internally), so it runs BEFORE the
	// single-project gate.
	if req.Action == "hotfix" {
		x.runHotfix(w, r, req, actor)
		return
	}

	if !x.allowed(req.Project) {
		writeErr(w, 400, "project is not part of the metaphor topology")
		return
	}
	short := req.Project[strings.LastIndex(req.Project, "/")+1:]
	if (req.Action == "release" || req.Action == "deliver" || req.Action == "merge-mr") && req.Confirm != short {
		writeErr(w, 400, fmt.Sprintf("confirmation mismatch: %q required", short))
		return
	}

	// hotfix-join: create (or join) a hotfix branch on ONE allowed repo — the
	// org's permitted lane, so no branch-policy check and no confirm handshake.
	if req.Action == "hotfix-join" {
		branch := strings.TrimSpace(req.Branch)
		if !strings.HasPrefix(branch, "hotfix") || !reBranch.MatchString(branch) {
			writeErr(w, 400, "hotfix branch names only (e.g. hotfix-0.9)")
			return
		}
		ctx := context.Background()
		joined := false
		webURL := ""
		if b, err := x.gl.createBranch(ctx, req.Project, branch, "main"); err != nil {
			if strings.Contains(err.Error(), "already exists") {
				joined = true
			} else {
				writeErr(w, 502, "branch on "+req.Project+": "+err.Error())
				return
			}
		} else {
			webURL = b.WebURL
		}
		if x.dropBranches != nil {
			x.dropBranches(req.Project)
		}
		if x.markHot != nil {
			x.markHot(req.Project)
		}
		log.Printf("ACTION hotfix-join project=%s branch=%s joined=%v actor=%s remote=%s",
			req.Project, branch, joined, actor, clientIP(r))
		writeJSON(w, map[string]any{
			"ok": true, "action": "hotfix-join", "actor": actor, "branch": branch,
			"joined": joined, "web_url": webURL,
			"checkout": fmt.Sprintf("git fetch origin %s && git checkout %s", branch, branch),
		})
		return
	}

	// catchup: merge main INTO a feature branch that fell behind — only when
	// GitLab's mergeability engine says it merges clean. A merge, never a
	// rebase: nobody's local clone gets its history rewritten. Mechanics: a
	// short-lived MR main→branch asks GitLab the safety question; "mergeable"
	// → accept it; anything else → close it and report why. Hotfix branches
	// are excluded on purpose — they pin old versions and SHOULD stay behind.
	if req.Action == "catchup" {
		branch := strings.TrimSpace(req.Branch)
		// epic- allowlist, same as merge-mr and retire: the UI only offers
		// catch-up on feature rows, but a direct POST must not be able to
		// point the write-scoped bot at deploy/staging/anyone's branch.
		if !strings.HasPrefix(branch, "epic-") || !reBranch.MatchString(branch) {
			writeErr(w, 400, "catch-up applies to feature (epic-*) branches only")
			return
		}
		ctx := context.Background()
		// live re-check: the card's ↓behind may be a cached view
		behind, _, err := x.gl.compareBranch(ctx, req.Project, branch, "main")
		if err != nil {
			writeErr(w, 502, "compare failed: "+err.Error())
			return
		}
		if behind == 0 {
			writeErr(w, 409, "branch is already current with main")
			return
		}
		// removeSource=false — the SOURCE here is main; asking GitLab to
		// delete it on merge would be an outage gated only by branch
		// protection. The probe MR must never carry that flag.
		mr, err := x.gl.createMR(ctx, req.Project, "main", branch,
			fmt.Sprintf("catch up %s with main", branch),
			fmt.Sprintf("Automated catch-up: merge main into `%s` (↓%d behind).\n\nInitiated-by: @%s", branch, behind, actor), false)
		if err != nil {
			writeErr(w, 502, "catch-up MR on "+req.Project+": "+err.Error())
			return
		}
		// GitLab computes mergeability async — poll briefly until it settles
		status := mr.DetailedMergeStatus
		for i := 0; i < 10 && (status == "" || status == "checking" || status == "unchecked" || status == "preparing"); i++ {
			time.Sleep(700 * time.Millisecond)
			if m, err := x.gl.mr(ctx, req.Project, mr.IID); err == nil {
				status = m.DetailedMergeStatus
			}
		}
		if status != "mergeable" {
			_ = x.gl.closeMR(ctx, req.Project, mr.IID)
			reason := "mergeability is " + status
			if status == "conflict" || status == "broken_status" {
				reason = "main conflicts with this branch — resolve manually"
			} else if status == "" || status == "checking" || status == "unchecked" || status == "preparing" {
				reason = "GitLab did not settle mergeability in time — try again"
			}
			log.Printf("ACTION catchup project=%s branch=%s behind=%d status=%q refused actor=%s remote=%s",
				req.Project, branch, behind, status, actor, clientIP(r))
			writeErr(w, 409, "not safe to catch up: "+reason)
			return
		}
		merged, err := x.gl.mergeMR(ctx, req.Project, mr.IID)
		if err != nil {
			_ = x.gl.closeMR(ctx, req.Project, mr.IID)
			writeErr(w, 502, "merge failed: "+err.Error())
			return
		}
		if x.dropBranches != nil {
			x.dropBranches(req.Project)
		}
		if x.markHot != nil {
			x.markHot(req.Project)
		}
		log.Printf("ACTION catchup project=%s branch=%s behind=%d mr=%d actor=%s remote=%s",
			req.Project, branch, behind, mr.IID, actor, clientIP(r))
		writeJSON(w, map[string]any{
			"ok": true, "action": "catchup", "actor": actor, "branch": branch,
			"behind": behind, "mr": mr.IID, "web_url": merged.WebURL,
		})
		return
	}

	// feature: start (or join) an epic feature branch — created on the micro
	// AND mirrored on the macro repo, so the feature umbrella line exists from
	// the first moment. Creating is idempotent: an existing branch means the
	// app is JOINING a feature already in flight.
	if req.Action == "feature" {
		if x.topo.BranchPolicy == "hotfix-only" {
			writeErr(w, 400, "this org takes app-repo changes on hotfix- branches only — feature branches are disabled")
			return
		}
		branch := strings.TrimSpace(req.Branch)
		if !reBranch.MatchString(branch) {
			writeErr(w, 400, "branch name must be lowercase letters, digits, dots, dashes (e.g. epic-20-pink)")
			return
		}
		ctx := context.Background()
		// macro (charts) feature: the umbrella feature line only — one branch,
		// no micro, no draft MR. Idempotent: an existing branch is a join.
		if req.Project == x.topo.MacroProj {
			created := map[string]bool{}
			urls := map[string]string{}
			for _, proj := range []string{x.topo.MacroProj} {
				b, err := x.gl.createBranch(ctx, proj, branch, "main")
				if err != nil {
					if strings.Contains(err.Error(), "already exists") {
						created[proj] = false
						continue
					}
					writeErr(w, 502, "branch on "+proj+": "+err.Error())
					return
				}
				created[proj] = true
				urls[proj] = b.WebURL
			}
			if x.markHot != nil {
				x.markHot(x.topo.MacroProj)
			}
			log.Printf("ACTION feature project=%s branch=%s epic=%d charts_created=%v actor=%s remote=%s",
				x.topo.MacroProj, branch, req.EpicIID, created[x.topo.MacroProj], actor, clientIP(r))
			writeJSON(w, map[string]any{
				"ok": true, "action": "feature", "actor": actor, "branch": branch,
				"charts_created": created[x.topo.MacroProj], "urls": urls,
				"checkout": fmt.Sprintf("git fetch origin %s && git checkout %s", branch, branch),
			})
			return
		}
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
		// draft the carrying MR on the service repo (idempotent: reuse an
		// existing one if the branch already had it) — the feature is
		// mergeable from minute one.
		var mr *glMR
		if list, err := x.gl.mrsBySource(ctx, req.Project, branch); err == nil && len(list) > 0 {
			mr = &list[0]
		} else {
			desc := "Feature branch drafted by foxglider.\n\nInitiated-by: @" + actor
			if req.EpicIID > 0 {
				desc += fmt.Sprintf("\n\nRelated to &%d", req.EpicIID)
			}
			if m, err := x.gl.createMR(ctx, req.Project, branch, "main", "Draft: "+branch, desc, true); err == nil {
				mr = &m
			} else {
				log.Printf("ACTION feature draft-mr failed project=%s branch=%s err=%v", req.Project, branch, err)
			}
		}
		log.Printf("ACTION feature project=%s branch=%s epic=%d micro_created=%v charts_created=%v mr=%v actor=%s remote=%s",
			req.Project, branch, req.EpicIID, created[req.Project], created[x.topo.MacroProj], mr != nil, actor, clientIP(r))
		resp := map[string]any{
			"ok": true, "action": "feature", "actor": actor, "branch": branch,
			"micro_created": created[req.Project], "charts_created": created[x.topo.MacroProj],
			"urls": urls,
			// copy-paste for the engineer's terminal, straight from the modal
			"checkout": fmt.Sprintf("git fetch origin %s && git checkout %s", branch, branch),
		}
		if mr != nil {
			resp["mr"] = map[string]any{"iid": mr.IID, "web_url": mr.WebURL}
		}
		writeJSON(w, resp)
		return
	}

	// merge-mr: merge a feature's carrying MR into main — the user's act, by
	// design (the worker parks at CI green and never merges). Only MRs whose
	// source is an epic-* branch are mergeable from here; the epic close
	// (Done) follows via the merge observer once every carrying MR is in.
	if req.Action == "merge-mr" {
		if req.MRIID <= 0 {
			writeErr(w, 400, "mr_iid required")
			return
		}
		ctx := context.Background()
		m, err := x.gl.mr(ctx, req.Project, req.MRIID)
		if err != nil {
			writeErr(w, 502, "MR lookup failed: "+err.Error())
			return
		}
		if !strings.HasPrefix(m.SourceBranch, "epic-") {
			writeErr(w, 400, "only feature (epic-*) MRs merge from here")
			return
		}
		if m.State != "opened" {
			writeErr(w, 409, "MR is "+m.State+", not open")
			return
		}
		if x.branchBuilding(ctx, req.Project, m.SourceBranch) {
			writeErr(w, 409, "a pipeline is still running on "+m.SourceBranch+" — merging deletes the branch and would strand it; let CI finish first")
			return
		}
		merged, err := x.gl.mergeMR(ctx, req.Project, req.MRIID)
		if err != nil {
			writeErr(w, 502, "merge failed: "+err.Error())
			return
		}
		if x.dropMRs != nil {
			x.dropMRs(req.Project, m.SourceBranch)
		}
		if x.markHot != nil {
			x.markHot(req.Project)
			x.markHot(x.topo.MacroProj)
		}
		log.Printf("ACTION merge-mr project=%s mr=%d branch=%s actor=%s state=%s remote=%s",
			req.Project, req.MRIID, m.SourceBranch, actor, merged.State, clientIP(r))
		writeJSON(w, map[string]any{"ok": true, "action": "merge-mr", "actor": actor,
			"mr": req.MRIID, "state": merged.State, "web_url": m.WebURL})
		return
	}

	// delete: remove a hotfix branch. Fully-merged branches delete freely —
	// every commit is reachable from main, only the pointer goes. Branches
	// ahead of main require the confirm handshake carrying the branch name,
	// and merge state is re-checked HERE at delete time — the UI's view can
	// be a minute stale, and stranding unmerged work deserves a live answer.
	if req.Action == "delete" {
		br := strings.TrimSpace(req.Ref)
		if !strings.HasPrefix(br, "hotfix") || !reBranch.MatchString(br) {
			writeErr(w, 400, "only hotfix branches can be deleted from here")
			return
		}
		ctx := context.Background()
		ahead, _, err := x.gl.compareBranch(ctx, req.Project, "main", br)
		if err != nil {
			writeErr(w, 502, "couldn't verify merge state: "+err.Error())
			return
		}
		if ahead > 0 && req.Confirm != br {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(409)
			json.NewEncoder(w).Encode(map[string]any{
				"error": "unmerged", "ahead": ahead,
				"message": fmt.Sprintf("%s carries %d commits not in main", br, ahead),
			})
			return
		}
		if x.branchBuilding(ctx, req.Project, br) {
			writeErr(w, 409, "a pipeline is still running on "+br+" — let it finish before deleting")
			return
		}
		if err := x.gl.deleteBranch(ctx, req.Project, br); err != nil {
			writeErr(w, 502, "delete failed: "+err.Error())
			return
		}
		if x.noteDeleted != nil {
			x.noteDeleted(req.Project, br)
		}
		if x.dropBranches != nil {
			x.dropBranches(req.Project)
		}
		log.Printf("ACTION delete project=%s branch=%s ahead=%d actor=%s remote=%s",
			req.Project, br, ahead, actor, clientIP(r))
		writeJSON(w, map[string]any{"ok": true, "action": "delete", "actor": actor,
			"branch": br, "ahead": ahead})
		return
	}

	// retire: remove a feature from the org in one motion — delete its epic-*
	// branch on every repo that has it (services + the charts twin) and close
	// the epic. Refused while any carrying MR is still open: retiring must
	// never orphan unmerged work. Countdown-armed; confirm = the branch name.
	if req.Action == "retire" {
		br := strings.TrimSpace(req.Branch)
		if !strings.HasPrefix(br, "epic-") || !reBranch.MatchString(br) {
			writeErr(w, 400, "only epic- feature branches retire from here")
			return
		}
		if req.Confirm != br {
			writeErr(w, 400, fmt.Sprintf("confirmation mismatch: %q required", br))
			return
		}
		ctx := context.Background()
		projects := make([]string, 0, len(x.topo.Services)+1)
		for _, s := range x.topo.Services {
			projects = append(projects, s.Project)
		}
		if x.topo.hasMacro() {
			projects = append(projects, x.topo.MacroProj)
		}
		// an in-flight pipeline must PREVENT the retire — deleting a branch
		// mid-build strands its jobs (they fail at get_sources). Refuse and
		// name the repos still building; let them finish, then retire.
		var building []string
		for _, proj := range projects {
			if x.branchBuilding(ctx, proj, br) {
				building = append(building, proj[strings.LastIndex(proj, "/")+1:])
			}
		}
		if len(building) > 0 {
			writeErr(w, 409, "pipelines are still running on "+br+" — let them finish before retiring: "+strings.Join(building, ", "))
			return
		}
		var open []string
		for _, proj := range projects {
			if list, err := x.gl.mrsBySource(ctx, proj, br); err == nil {
				for _, m := range list {
					if m.State == "opened" {
						open = append(open, fmt.Sprintf("%s!%d", proj[strings.LastIndex(proj, "/")+1:], m.IID))
					}
				}
			}
		}
		if len(open) > 0 {
			writeErr(w, 409, "carrying MRs are still open — merge or close them first: "+strings.Join(open, ", "))
			return
		}
		type retireResult struct {
			Project string `json:"project"`
			Status  string `json:"status"` // deleted | absent | error
		}
		results := make([]retireResult, 0, len(projects))
		deleted := 0
		for _, proj := range projects {
			err := x.gl.deleteBranch(ctx, proj, br)
			switch {
			case err == nil:
				deleted++
				results = append(results, retireResult{proj, "deleted"})
				if x.noteDeleted != nil {
					x.noteDeleted(proj, br)
				}
				if x.dropBranches != nil {
					x.dropBranches(proj)
				}
			case strings.Contains(err.Error(), "404"):
				results = append(results, retireResult{proj, "absent"})
			default:
				results = append(results, retireResult{proj, "error"})
			}
		}
		epicClosed := false
		if m := reEpicBranch.FindStringSubmatch(br); m != nil {
			if iid, err := strconv.Atoi(m[1]); err == nil {
				if x.gl.epicUpdate(ctx, x.group(), iid, "", "", "close") == nil {
					epicClosed = true
				}
			}
		}
		log.Printf("ACTION retire branch=%s deleted=%d epic_closed=%v actor=%s remote=%s",
			br, deleted, epicClosed, actor, clientIP(r))
		writeJSON(w, map[string]any{"ok": true, "action": "retire", "actor": actor,
			"branch": br, "deleted": deleted, "results": results, "epic_closed": epicClosed})
		return
	}

	// deliver: run the tag pipeline for the newest published RC — GitLab's
	// canonical "deliver this version" front door. deploy:tag-dev then writes
	// the dev bump MR, which a background watcher auto-merges (dev
	// auto-delivers by policy; staging/prod stay human).
	if req.Action == "deliver" {
		// resolve the delivery TARGET: umbrella-level by default, or a
		// service's own target (single-app delivery) when the project is a
		// service carrying targets.
		var target *deliverySpec
		var forApp string
		if req.Project == x.topo.MacroProj {
			for i := range x.topo.Delivery {
				if req.Env == "" || x.topo.Delivery[i].Env == req.Env {
					target = &x.topo.Delivery[i]
					break
				}
			}
		} else {
			for si := range x.topo.Services {
				if x.topo.Services[si].Project != req.Project {
					continue
				}
				for i := range x.topo.Services[si].Delivery {
					if req.Env == "" || x.topo.Services[si].Delivery[i].Env == req.Env {
						target = &x.topo.Services[si].Delivery[i]
						forApp = x.topo.Services[si].Name
						break
					}
				}
			}
		}
		if target == nil {
			writeErr(w, 400, "no delivery target matches that project/env")
			return
		}
		ctx := context.Background()

		// non-tag-pipeline targets: the deliver IS a gitops edit — bump the
		// targetRevision in the target repo with the target's own credential,
		// as an MR (platform repos keep their approvers) or a direct commit.
		if target.Write == "mr" || target.Write == "commit" {
			ver := strings.TrimSpace(req.Version)
			if ver == "" || !reVersionArg.MatchString(ver) {
				writeErr(w, 400, "deliver to this target needs an explicit published version")
				return
			}
			tc := x.clientFor
			var c *glClient
			if tc != nil {
				c = tc(*target)
			}
			if c == nil {
				writeErr(w, 503, "this target's credentials aren't configured yet ("+target.TokenEnv+")")
				return
			}
			br := target.Branch
			if br == "" {
				br = "main"
			}
			raw, err := c.fileRaw(ctx, target.Project, target.App, br)
			if err != nil {
				writeErr(w, 502, "target file read: "+err.Error())
				return
			}
			cur := targetRevision(raw)
			if cur == ver {
				writeErr(w, 409, "the target already asks for "+ver)
				return
			}
			updated := reTargetRev.ReplaceAllString(raw, "targetRevision: "+ver)
			name := x.topo.MacroName
			if forApp != "" {
				name = forApp
			}
			wb := fmt.Sprintf("foxglider-deliver-%s-%s", target.Env, strings.ReplaceAll(ver, ".", "-"))
			msg := fmt.Sprintf("chore: deliver %s %s to %s\n\nWas %s.\n\nInitiated-by: @%s", name, ver, target.Env, cur, actor)
			if target.Write == "commit" {
				wb = br // straight onto the target branch
			}
			if err := c.commitFile(ctx, target.Project, wb, br, target.App, updated, msg); err != nil {
				writeErr(w, 502, "bump commit: "+err.Error())
				return
			}
			resp := map[string]any{"ok": true, "action": "deliver", "actor": actor,
				"version": ver, "env": target.Env, "mode": target.Write, "was": cur}
			if target.Write == "mr" {
				m, err := c.createMR(ctx, target.Project, wb, br,
					fmt.Sprintf("chore: deliver %s %s to %s", name, ver, target.Env),
					fmt.Sprintf("Requested from foxglider.\n\nWas `%s`.\n\nInitiated-by: @%s", cur, actor), true)
				if err != nil {
					writeErr(w, 502, "bump MR: "+err.Error())
					return
				}
				resp["mr"] = m.IID
				resp["mr_url"] = m.WebURL
				log.Printf("ACTION deliver mode=mr env=%s version=%s mr=%d actor=%s remote=%s",
					target.Env, ver, m.IID, actor, clientIP(r))
			} else {
				log.Printf("ACTION deliver mode=commit env=%s version=%s actor=%s remote=%s",
					target.Env, ver, actor, clientIP(r))
			}
			if x.markHot != nil {
				x.markHot(target.Project)
			}
			writeJSON(w, resp)
			return
		}

		if req.Project != x.topo.MacroProj {
			writeErr(w, 400, "tag-pipeline delivery applies to the umbrella only")
			return
		}
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
			req.Project, tag, pl.ID, actor, clientIP(r))
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
	if req.Action == "trigger" && req.Ref != "" && x.topo.BranchPolicy == "hotfix-only" && strings.HasPrefix(strings.ToLower(req.Ref), "epic-") {
		writeErr(w, 400, "this org takes app-repo changes on hotfix- branches only")
		return
	}
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
			req.Project, req.Ref, pl.ID, actor, clientIP(r))
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
				req.Action, req.Project, pl.ID, actor, clientIP(r))
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
		req.Action, req.Project, jobName, target.ID, lp.ID, actor, clientIP(r))
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
					// a FEATURE landing in dev flips its epic to In Review —
					// the delivery IS the review request (John's lifecycle)
					if m := reEpicRef.FindStringSubmatch(version); m != nil {
						if iid, err := strconv.Atoi(m[1]); err == nil && iid > 0 {
							if err := x.gl.epicUpdate(ctx, x.group(), iid,
								"status::In Review", "status::In Progress,status::Ready for Queue", ""); err == nil {
								log.Printf("LIFECYCLE epic=%d delivered→In Review version=%s actor=%s", iid, version, actor)
							} else {
								log.Printf("LIFECYCLE epic=%d In Review flip failed: %v", iid, err)
							}
						}
					}
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

// hotfixRepoPlan is one repo's resolved plan for a hotfix-from-version cut:
// where to branch from (a commit sha or a tag), or why it's skipped.
type hotfixRepoPlan struct {
	Name    string
	Project string
	Pin     string
	Ref     string
	Kind    string // "commit" | "tag" | "skip"
	Reason  string
}

// resolveHotfixRepos plans a hotfix cut from a published umbrella tag: for each
// service the tag pins, the ref to branch from — the pinned commit when it's a
// sha-rc that still exists, else a service tag carrying the pin, else a skip
// with a human reason. The macro repo itself is appended, branching from the
// tag. A commit-existence check that fails ON THE WIRE aborts the whole plan:
// a GitLab outage must never be mis-reported as "commit absent → skipped".
func (x *actions) resolveHotfixRepos(ctx context.Context, tag string) ([]hotfixRepoPlan, error) {
	raw, err := x.gl.fileRaw(ctx, x.topo.MacroProj, x.topo.MacroFile, tag)
	if err != nil {
		return nil, fmt.Errorf("read umbrella %s@%s: %w", x.topo.MacroFile, tag, err)
	}
	pins := macroDeps(raw)
	plans := make([]hotfixRepoPlan, 0, len(x.topo.Services)+1)
	for _, s := range x.topo.Services {
		pin := pins[s.Name]
		if pin == "" {
			// a topology service the umbrella doesn't pin must still show —
			// an invisible row reads as "covered" when it isn't
			plans = append(plans, hotfixRepoPlan{
				Name: s.Name, Project: s.Project,
				Kind: "skip", Reason: "not declared in this umbrella",
			})
			continue
		}
		plan := hotfixRepoPlan{Name: s.Name, Project: s.Project, Pin: pin}
		if sha := rcSHA(pin); sha != "" {
			ok, err := x.gl.commitExists(ctx, s.Project, sha)
			if err != nil {
				return nil, fmt.Errorf("commit check %s %s: %w", s.Project, sha, err)
			}
			if ok {
				plan.Kind, plan.Ref = "commit", sha
				plans = append(plans, plan)
				continue
			}
		}
		if tref := x.tagForPin(ctx, s.Project, pin); tref != "" {
			plan.Kind, plan.Ref = "tag", tref
		} else if psha := x.pipeForPin(ctx, s.Project, pin); psha != "" {
			// counter pins (rc.N) carry no sha in the version — the publish
			// pipeline's stamped name does
			plan.Kind, plan.Ref = "commit", psha
		} else {
			plan.Kind, plan.Reason = "skip", "no commit, tag, or publish pipeline for "+pin
		}
		plans = append(plans, plan)
	}
	plans = append(plans, hotfixRepoPlan{
		Name: shortName(x.topo.MacroProj), Project: x.topo.MacroProj,
		Pin: strings.TrimPrefix(tag, x.topo.MacroTag), Ref: tag, Kind: "tag",
	})
	return plans, nil
}

// pipeForPin resolves a counter pin (X.Y.Z-rc.N) to the commit that published
// it via the publish pipeline's stamped name ("[<rc-version> | <branch>]" —
// both the konstruct-gitops .publish-chart and metaphor's helm-publish write
// it). Bounded search of recent pipelines; the durable fallback (chart
// appVersion in the OCI registry) is deliberately not consulted — the theme
// speaks only GitLab, and older pins age out of reach honestly.
func (x *actions) pipeForPin(ctx context.Context, project, pin string) string {
	pls, err := x.gl.recentPipelines(ctx, project, "", "", 100)
	if err != nil {
		return ""
	}
	needle := "[" + pin + " "
	for _, p := range pls {
		if strings.HasPrefix(p.Name, needle) && p.SHA != "" {
			if len(p.SHA) > 8 {
				return p.SHA[:8]
			}
			return p.SHA
		}
	}
	return ""
}

// tagForPin finds a service tag matching a pin exactly or as v<pin>.
func (x *actions) tagForPin(ctx context.Context, project, pin string) string {
	tags, err := x.gl.tags(ctx, project, 100)
	if err != nil {
		return ""
	}
	for _, t := range tags {
		if t.Name == pin || t.Name == "v"+pin {
			return t.Name
		}
	}
	return ""
}

// hotfixPreview answers "what would a hotfix-from-version cut touch": the plan
// per repo, creating nothing. GET /api/actions/hotfix-preview?tag=…
func (x *actions) hotfixPreview(w http.ResponseWriter, r *http.Request) {
	if !x.enabled() {
		writeErr(w, 503, "actions not configured")
		return
	}
	tag := strings.TrimSpace(r.URL.Query().Get("tag"))
	if tag == "" || !strings.HasPrefix(tag, x.topo.MacroTag) || len(tag) > 128 {
		writeErr(w, 400, "tag must be a published umbrella tag")
		return
	}
	plans, err := x.resolveHotfixRepos(context.Background(), tag)
	if err != nil {
		writeErr(w, 502, "resolve: "+err.Error())
		return
	}
	type repoOut struct {
		Name    string `json:"name"`
		Project string `json:"project"`
		Pin     string `json:"pin"`
		Ref     string `json:"ref,omitempty"`
		Kind    string `json:"kind"`
		Reason  string `json:"reason,omitempty"`
	}
	repos := make([]repoOut, 0, len(plans))
	for _, p := range plans {
		repos = append(repos, repoOut{p.Name, p.Project, p.Pin, p.Ref, p.Kind, p.Reason})
	}
	writeJSON(w, map[string]any{
		"tag": tag, "version": strings.TrimPrefix(tag, x.topo.MacroTag), "repos": repos,
	})
}

// runHotfix cuts the hotfix branch across every repo the tag pins (skipping the
// unresolved). branch must be hotfix-<...>; confirm must be the macro repo's
// short name (the deliver/delete handshake). No branch-policy check — hotfixes
// ARE the allowed lane. One audit line carries the created/joined/skipped tally.
func (x *actions) runHotfix(w http.ResponseWriter, r *http.Request, req runReq, actor string) {
	tag := strings.TrimSpace(req.Tag)
	if tag == "" || !strings.HasPrefix(tag, x.topo.MacroTag) || len(tag) > 128 {
		writeErr(w, 400, "tag must be a published umbrella tag")
		return
	}
	branch := strings.TrimSpace(req.Branch)
	if !strings.HasPrefix(branch, "hotfix-") || !reBranch.MatchString(branch) {
		writeErr(w, 400, "hotfix branch name must look like hotfix-0.9")
		return
	}
	confirm := shortName(x.topo.MacroProj)
	if req.Confirm != confirm {
		writeErr(w, 400, fmt.Sprintf("confirmation mismatch: %q required", confirm))
		return
	}
	ctx := context.Background()
	plans, err := x.resolveHotfixRepos(ctx, tag)
	if err != nil {
		writeErr(w, 502, "resolve: "+err.Error())
		return
	}
	type result struct {
		Project string `json:"project"`
		Status  string `json:"status"` // created | joined | skipped
		Ref     string `json:"ref,omitempty"`
		Reason  string `json:"reason,omitempty"`
		WebURL  string `json:"web_url,omitempty"`
	}
	results := make([]result, 0, len(plans))
	created, joined, skipped := 0, 0, 0
	for _, p := range plans {
		if p.Kind == "skip" {
			skipped++
			results = append(results, result{Project: p.Project, Status: "skipped", Reason: p.Reason})
			continue
		}
		b, err := x.gl.createBranch(ctx, p.Project, branch, p.Ref)
		switch {
		case err == nil:
			created++
			results = append(results, result{Project: p.Project, Status: "created", Ref: p.Ref, WebURL: b.WebURL})
		case strings.Contains(err.Error(), "already exists"):
			joined++
			results = append(results, result{Project: p.Project, Status: "joined", Ref: p.Ref})
		default:
			skipped++
			results = append(results, result{Project: p.Project, Status: "skipped", Ref: p.Ref, Reason: err.Error()})
			continue
		}
		if x.dropBranches != nil {
			x.dropBranches(p.Project)
		}
		if x.markHot != nil {
			x.markHot(p.Project)
		}
	}
	log.Printf("ACTION hotfix tag=%s branch=%s created=%d joined=%d skipped=%d actor=%s remote=%s",
		tag, branch, created, joined, skipped, actor, clientIP(r))
	writeJSON(w, map[string]any{
		"ok": true, "action": "hotfix", "actor": actor, "branch": branch, "tag": tag,
		"results":  results,
		"checkout": fmt.Sprintf("git fetch origin %s && git checkout %s", branch, branch),
	})
}
