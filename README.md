# theme-metaphor

The **delivery-view theme** for the Metaphor org — a Konstruct `Theme` that
renders a read-only dashboard of GitLab build metadata: every project in every
group your token can see, with pipeline timelines, per-pipeline stage/job
gantts, and a cross-project activity feed. Every element deep-links to the
corresponding GitLab page. Borrows the Konstruct theme architecture (embedded
static frontend + tiny Go server) without using any theme-rpc operations.

> **Seed.** This repo is forked from `civo/konstruct/theme-foxglider` and
> currently ships foxglider's read-only pipeline dashboard, re-identified for
> Metaphor and org-pinned to `civo/metaphor`. The full delivery-view build —
> releases/tags/package-registry data plus **guarded pipeline-trigger buttons**
> — is tracked in [`civo/metaphor/metaphor#37`](https://git.civo.com/civo/metaphor/metaphor/-/issues/37).

## Endpoints

| endpoint | returns |
|----------|---------|
| `GET /api/overview` | groups → projects, each with its 20 newest pipelines |
| `GET /api/projects/{id}/pipelines` | recent pipelines for one project |
| `GET /api/pipelines/{pid}/{plid}` | pipeline detail: stages + jobs with timings |
| `GET /api/activity?hours=24` | merged newest-first events (pipelines, pushes, MRs, issues, comments) |

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
