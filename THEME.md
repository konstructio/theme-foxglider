# THEME
version: v2
theme: foxglider
capabilities: []

Foxglider is the delivery-view theme for the metaphor org — an informational
dashboard, not a build surface: it uses no theme-rpc operations. All data comes
from its own backend's read-only GitLab proxy (`GITLAB_HOST` / `GITLAB_TOKEN` /
`GITLAB_GROUPS` env, org-pinned to `civo/metaphor` by default). Direct-open and
platform-iframe render identically.

Seed forked from `civo/konstruct/theme-foxglider`. The full delivery-view build
(releases/tags/package-registry data + guarded pipeline-trigger buttons) is
tracked in civo/metaphor/metaphor#37.
