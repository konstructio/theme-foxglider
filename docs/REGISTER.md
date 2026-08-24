# Registering this theme in the Metaphor org

`theme-metaphor` becomes a live, org-pinned themed app once a Konstruct
`Theme` CR exists in the **`metaphor`** namespace pointing at this repo.
The theme-controller then spawns a `ThemedApp`, kpack-builds it, and serves it
in a credential-less, org-pinned iframe.

> **Recommendation:** register against **`civo/metaphor/theme-metaphor`** (this
> repo), not `civo/konstruct/theme-foxglider`. The metaphor org's git account
> (`metaphor-civo-metaphor`, a group token scoped to `civo/metaphor`, id 1642)
> can read this repo but **cannot** read the konstruct-group foxglider repo.

## Option A — Theme CR (recommended; needs cluster access to the metaphor ns)

```yaml
apiVersion: konstruct.civo.com/v1alpha1
kind: Theme
metadata:
  name: metaphor
  namespace: metaphor
spec:
  display_name: Metaphor
  repo_url: https://git.civo.com/civo/metaphor/theme-metaphor
  repo_name: civo/metaphor/theme-metaphor
  branch: main
  # zone_ref is optional — the theme-controller auto-creates/resolves the
  # `themes` zone in the namespace (as it did for the konstruct org).
```

```sh
kubectl apply -f docs/register-theme.yaml   # (save the block above)
kubectl -n metaphor get themes.konstruct.civo.com metaphor -w
# Ready when: status.phase == Live and status.url_ready == true
```

## Option B — konstruct-api endpoint (needs a metaphor-org session/API key)

```sh
curl -X POST "https://<konstruct-api-host>/theme/themes/metaphor" \
  -H "Authorization: Bearer <METAPHOR_ORG_API_KEY>" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "metaphor",
    "display_name": "Metaphor",
    "repo_url": "https://git.civo.com/civo/metaphor/theme-metaphor",
    "repo_name": "civo/metaphor/theme-metaphor",
    "branch": "main"
  }'
```

The Konstruct dashboard's "Add theme" action (used while signed in to the
metaphor org) calls this same endpoint.

## Prerequisites / notes

- **metaphor-org write credential (John):** kbot's MCP/API key is `konstruct`-org
  scoped and read-only for themes, so it cannot register into the `metaphor`
  namespace. Use cluster kubeconfig (Option A) or a metaphor-org session (Option B).
- **GitLab polling secret:** the running theme reads GitLab via
  `GITLAB_HOST` / `GITLAB_TOKEN` / `GITLAB_GROUPS` (defaults to `civo/metaphor`).
  A `read_api`-scoped token must be provided to the themed app (foxglider used a
  `foxglider-gitlab` secret). Confirm the metaphor themed-app deployment gets an
  equivalent secret, or the dashboard shows "Not connected to GitLab".
- **Cross-org visibility gap:** `konstruct-api ListThemes` is scoped to the
  caller's org namespace, so this metaphor theme is only visible to metaphor-org
  users (tracked upstream as konstruct-api#74).
