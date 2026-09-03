# theme-foxglider

**Foxglider** — the delivery-view theme for the Metaphor org — a Konstruct `Theme` that
renders a read-only dashboard of GitLab build metadata: every project in every
group your token can see, with pipeline timelines, per-pipeline stage/job
gantts, and a cross-project activity feed. Every element deep-links to the
corresponding GitLab page. Borrows the Konstruct theme architecture (embedded
static frontend + tiny Go server) without using any theme-rpc operations.

> **Grew from a seed.** Forked from `civo/konstruct/theme-foxglider`, org-pinned
> to `civo/metaphor`. On top of foxglider's read-only pipeline dashboard it now
> adds the **Delivery** view — the metaphor supply chain (microcharts → the
> `metaphor-macro` umbrella → what's delivered to dev-33) with drift detection,
> which is the data-plane half of
> [`civo/metaphor/metaphor#37`](https://git.civo.com/civo/metaphor/metaphor/-/issues/37).
> Still ahead in #37: **guarded pipeline-trigger buttons** (releases/promotions
> from the UI, with preconditions).

## Views

- **Delivery** (default) — the supply chain: each service's base→bundled
  microchart version, the umbrella's published RC, and the delivered version per
  environment with an up-to-date / *N behind* drift badge.
- **Fleet** — per-project pipeline timelines across the org.
- **Activity** — merged, filterable event stream.

## Endpoints

| endpoint | returns |
|----------|---------|
| `GET /api/ecosystem` | the metaphor supply chain: services (base + bundled chart versions + latest pipeline), the `metaphor-macro` umbrella (base + published RC tag), and per-environment delivered version with drift |
| `GET /api/meta` | theme build version + connection scope (unguarded, so the version badge always renders) |
| `GET /api/overview` | groups → projects, each with its 20 newest pipelines |
| `GET /api/projects/{id}/pipelines` | recent pipelines for one project |
| `GET /api/pipelines/{pid}/{plid}` | pipeline detail: stages + jobs with timings |
| `GET /api/activity?hours=24` | merged newest-first events (pipelines, pushes, MRs, issues, comments) |

The ecosystem topology (which repos are services, the umbrella, and the delivery
targets) lives in `defaultTopology()` in `eco.go`.

All GitLab access is GET-only. List-row `duration_s` approximates
`updated_at − created_at`; exact durations come from the detail endpoint.

## Config

| var | default | meaning |
|-----|---------|---------|
| `GITLAB_HOST` |  `https://gitlab.com` | GitLab base URL |
| `GITLAB_TOKEN` | *(required)* | `read_api`-scoped PAT; never sent to the browser |
| `GITLAB_GROUPS` | `civo/metaphor` | comma-separated group paths to scope; override to broaden |
| `PORT` | `8080` | listen port |

Without a token the API answers `503 {"error": …}` and the UI shows an
explicit "Not connected to GitLab" state — there is no sample data.

## Local dev

```sh
go run ./cmd/fakegitlab                                   # canned GitLab on :9911
GITLAB_HOST=http://localhost:9911 GITLAB_TOKEN=dev go run .
```

## Tests

```sh
go test -race ./...
```
