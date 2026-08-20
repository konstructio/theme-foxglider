# THEME
version: v2
theme: foxglider
capabilities: []

Foxglider is an informational dashboard, not a build surface: it uses no
theme-rpc operations. All data comes from its own backend's read-only
GitLab proxy (`GITLAB_HOST` / `GITLAB_TOKEN` / `GITLAB_GROUPS` env).
Direct-open and platform-iframe render identically.
