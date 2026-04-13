# Okta Groups + GitHub Teams Sync Agent

<!-- TOC -->

- [Overview](#overview)
- [Quick Usage](#quick-usage)
- [Deployment (GitHub Actions)](#deployment-github-actions)
- [Troubleshooting](#troubleshooting)
- [Prerequisites (1-Time Setup)](#prerequisites-1-time-setup)
  - [GitHub Enterprise Cloud](#github-enterprise-cloud)
  - [Okta Admin Console](#okta-admin-console)
- [Configuration](#configuration)
- [Local Development](#local-development)
- [License](#license)

<!-- /TOC -->

## Overview

`kc-okta-github-team-sync` is a Golang app that syncs Okta Groups to GitHub Teams across multiple GitHub Orgs. This project is intended to be used for a GitHub Enterprise Cloud (GHEC) deployment.

**Original Issue:**

- GitHub Team Sync is a built-in GitHub feature, which syncs the *membership* of Okta Groups to GitHub Teams.
- However, GitHub Team Sync cannot *create* new GitHub Teams when a new Okta Group is created.
- Nor can it automatically map that new Okta Group to the GitHub Team.
- Hence, GitHub Teams must be manually created and mapped.

**Solution:**

`kc-okta-github-team-sync` solves this limitation:

- `okta-team-sync` acts as an agent that runs on a scheduled GitHub Actions workflow.
- The agent uses the Okta API to check for new groups starting with **`<OKTA_GROUP_PREFIX>-*`**
- If the agent finds a new group (such as `<OKTA_GROUP_PREFIX>-myNewTeam-read`), the agent will create a new GitHub Team with the same name across all GitHub Orgs, and automatically map those teams back to the Okta Group.

## Quick Usage

In a Terminal:

```shell
git clone https://github.com/CaseyLabs/kc-okta-github-team-sync
cd kc-okta-github-team-sync

cp env.template .env
# Edit .env with your settings

# Ensure Docker is running for all build/test/run commands.

# Build the sync binary to bin/sync
make build

# Run app in dry-run mode
make dryrun

# Run the actual sync
make run
```

## Deployment (GitHub Actions)

The repository includes an example workflow at `examples/github-actions/okta-github-team-sync.yml`. Copy it to `.github/workflows/okta-github-team-sync.yml`, then adjust the schedule and environment variables for your organization. The sample uses Go 1.25.6, caches the Okta cursor, and caps concurrency to one in-flight run.

Important inputs (configure as repo secrets or variables):

- Required secrets: `OKTA_TEAM_SYNC_OKTA_BASE_URL`, `OKTA_TEAM_SYNC_OKTA_CLIENT_ID`, `OKTA_TEAM_SYNC_OKTA_PRIVATE_KEY`, `OKTA_TEAM_SYNC_OKTA_KEY_ID`, `OKTA_TEAM_SYNC_GH_PRIVATE_KEY`, and either `OKTA_TEAM_SYNC_GH_APP_ID` with installation IDs or `OKTA_TEAM_SYNC_GITHUB_TOKEN`.
- Optional secrets: `OKTA_TEAM_SYNC_OKTA_TOKEN_URL`, `OKTA_TEAM_SYNC_OKTA_SCOPES`, `OKTA_TEAM_SYNC_GH_INSTALLATION_ID`, `OKTA_TEAM_SYNC_GH_INSTALLATION_IDS`, `OKTA_GITHUB_APP_ID`, `OKTA_GITHUB_APP_IDS`, `OKTA_TEAM_SYNC_GITHUB_TOKEN`.
- Optional vars: `ORG_LIST` (overrides deriving orgs from installation IDs), `GITHUB_API_URL`, `OKTA_TEAM_SYNC_GROUP_PREFIX` (defaults to `Okta-Team-` in the workflow env).

## Troubleshooting

- **Authentication failures** – Reconfirm values in `.env` or GitHub Actions secrets. GitHub App payloads require numeric `OKTA_TEAM_SYNC_GH_APP_ID` and either `OKTA_TEAM_SYNC_GH_INSTALLATION_IDS` or `OKTA_TEAM_SYNC_GH_INSTALLATION_ID`; the PEM may need literal newline characters. If `OKTA_TEAM_SYNC_GITHUB_TOKEN` is set, the app will ignore GitHub App env vars and use the PAT (ensure it includes `read:org` plus team administration scopes).
- **Okta 403/429 responses** – Ensure the service principal is assigned to an administrator role spanning the entire tenant. Rate limit responses are retried with backoff, but persistent failures keep the group in the queue for the next run.
- **Resetting state** – Remove the cursor file (`state/okta_cursor.json`) and optionally invalidate the Actions cache to force a backfill run equal to `LOOKBACK_MINUTES`.
- **Isolating Okta client tests** – Run `docker compose run --rm --no-deps -T dev go test ./internal/okta -run TestFetchSystemLogDelta -count=1` to focus on the system log pagination test.

## Prerequisites (1-Time Setup)

*Use this section to bootstrap a new GHEC + Okta deployment; adapt names and URLs to your environment.*

### GitHub Enterprise Cloud

Log into the GitHub web console as a service account or admin user with org permissions.

Verify *Team Sync* is enabled for each GitHub Org:

- In each GitHub Organizaiton: go to `Settings → Authentication Security → Team synchronization`
- If Team Sync is not enabled:
  - Log into the Okta Admin console with a service account.
    - Permissions needed: *Manage Users, Scope: Users resource - All users*
    - Left sidebar: Go to `Security → API → Tokens tab`
    - Click “Create Token”
    - Copy the token, this will be your `SSWS Token`
  - Log into GitHub using the same service account.
    - Go to *Organizations --> \[Target Org] --> Settings*
    - Left sidebar: *Authentication Security*
    - Scroll down to "Team Synchronization"
    - Click "Enable for Okta"
      - Paste the `SSWS Token` you copied from Okta
      - URL setting: `https://<your-okta-domain>.okta.com`

Create a dedicated GitHub App at the Enterprise level:

- In the Enterprise: go to `Settings → GitHub Apps → New GitHub App`
- App Name: `Okta-Team-Sync`
- Hompage URL: `https://github.com/<your-org>/<your-repo>`
- Uncheck/disable: `Webhook → Active`
- Permissions:
  - `Organization permissions → Administration: Read & Write`
  - `Organization Permissions → Members: Read & Write`
- Leave all other settings at defaults
- Click *Create Github App*
- Click *Generate a private key*

Save and download the private key

Set the value of `OKTA_TEAM_SYNC_GH_PRIVATE_KEY` to the value contained in the private key

- Copy the `App ID`
  - In your `.env` file, set the value of `OKTA_TEAM_SYNC_GH_APP_ID` to the App ID
- Left-column: click *Install App*
- Install the app in each GitHub Org

After you have installed the app into each Org, you will need to get the `installation_id` for each app installation:

- In each GitHub Organizaiton: go to `Settings → GitHub Apps → Configure`
- The URL for the installation settings page includes the installation ID. For example:
  - [`https://github.com/organizations/<ORG-name>/settings/installations/<INSTALLATION_ID>`](https://github.com/organizations/<ORG-name>/settings/installations/<INSTALLATION_ID>)
- Copy the `installation_id` for each app for later use.
- Populate `OKTA_TEAM_SYNC_GH_INSTALLATION_IDS` in your `.env` with comma-separated `org-slug:installation_id` entries (case insensitive). This drives per-organization routing for the agent and its GitHub App access.
- Optionally set `OKTA_TEAM_SYNC_GH_INSTALLATION_ID` when the GitHub App is installed once across all orgs or when you want a default fallback for organizations not listed in `OKTA_TEAM_SYNC_GH_INSTALLATION_IDS`. Any organization not matched falls back to this value or, if unset, to `OKTA_TEAM_SYNC_GITHUB_TOKEN`.
- Mirror the same value into the GitHub Actions secret `OKTA_TEAM_SYNC_GH_INSTALLATION_IDS` so CI/CD runs resolve the correct installation per org.

### Okta Admin Console

Log into the Okta Admin web console.

Go to *Applications → Applications:*

- Click *Create App Integration*.
- Select *API Services* and click *Next*.
- App integration name: `GithubTeamSync`.

In the "General" tab:

- Copy the *Client ID*
  - Save it as this Github Actions Secret: `OKTA_TEAM_SYNC_OKTA_CLIENT_ID`
- Click on *Client Credentials → Edit*
- Change "Client authentication" to *Public key / Private key*
- Click the *Add key* button
- Click *Generate new key*
- Copy the `PEM` version of the private key, and save it to this Github Actions Secret:
  - `OKTA_TEAM_SYNC_OKTA_PRIVATE_KEY`
- Copy the `KID` (Key ID), and save it to this GitHub Secret:
  - `OKTA_TEAM_SYNC_OKTA_KEY_ID`
- Click *Done*
- Click *Save*

Go to *Security → API → Authorization Servers → default*:

- Click on *Access Policies → Add Policy*
  - *Name:* `GithubTeamSync-Policy`
  - *Description:* `GithubTeamSync`
  - *Assign To:* `GithubTeamSync`
  - Click *Create Policy*
- Click *Add rule*
  - *Rule Name:* `GithubTeamSync-Rule`
  - Leave all defaults, and click *Create Rule*

Return to *Applications → Applications → GithubTeamSync*.

- Click on the *General* tab
  - General Settings: click "Edit"
  - Uncheck/disable "Require Demonstrating Proof of Possession (DPoP)"
  - Click *Save*
- Click on the *Okta API Scopes* tab
  - Grant `okta.groups.read`, `okta.logs.read`, and `okta.apps.manage` so the sync can list groups, read system log events, and assign groups to org-specific GitHub SAML apps.
- Click on the *Admin Roles* tab
  - Click *Edit Assignments*
  - Add: `Role: Read-only Administrator`
  - Click *Save*

Left sidebar: Click *Security → Administrators*

- Click *Add Administrator*
- *Select Admin:* `GithubTeamSync`
- *Role*: `Read-only Administrator`
- Click *Save Changes*

## Configuration

All runtime configuration is provided through environment variables (the repository ships with `env.template` documenting each one).

| Variable                             | Required      | Description                                                                                          |
| ------------------------------------ | ------------- | ---------------------------------------------------------------------------------------------------- |
| `OKTA_TEAM_SYNC_OKTA_BASE_URL`       | yes           | Base URL of the Okta tenant, e.g. `https://example.okta.com`.                                       |
| `OKTA_TEAM_SYNC_OKTA_CLIENT_ID`      | yes           | Client ID for the API Service app using `private_key_jwt`.                                           |
| `OKTA_TEAM_SYNC_OKTA_PRIVATE_KEY`    | yes           | PEM-encoded private key for the service app (use literal newlines or `\n`).                          |
| `OKTA_TEAM_SYNC_OKTA_KEY_ID`         | conditionally | Key ID (`kid`) assigned when generating the Okta private key; required for Okta to accept the client assertion. |
| `OKTA_GITHUB_APP_IDS`                | conditionally | Comma- or newline-separated Okta application IDs for the GitHub Enterprise Cloud SCIM integration(s). Required to auto-assign groups (one entry per org app). |
| `OKTA_GITHUB_APP_ID`                 | conditionally | Backward-compatible single app ID; equivalent to a single-element `OKTA_GITHUB_APP_IDS`.             |
| `OKTA_TEAM_SYNC_OKTA_TOKEN_URL`      | no            | Token endpoint override; defaults to `${OKTA_TEAM_SYNC_OKTA_BASE_URL}/oauth2/v1/token`.              |
| `OKTA_TEAM_SYNC_OKTA_SCOPES`         | no            | Space-delimited scopes; defaults to `okta.groups.read okta.logs.read okta.apps.manage`.              |
| `OKTA_TEAM_SYNC_GH_APP_ID`           | conditionally | GitHub App identifier (numeric). Required when `OKTA_TEAM_SYNC_GITHUB_TOKEN` is empty.               |
| `OKTA_TEAM_SYNC_GH_INSTALLATION_ID`  | conditionally | Default installation ID for the GitHub App (used when an organization is not listed in `OKTA_TEAM_SYNC_GH_INSTALLATION_IDS`). |
| `OKTA_TEAM_SYNC_GH_INSTALLATION_IDS` | no            | Optional comma-separated list of `org-slug:installation_id` pairs for per-organization installations. |
| `OKTA_TEAM_SYNC_GH_PRIVATE_KEY`      | conditionally | PEM-encoded GitHub App private key associated with `OKTA_TEAM_SYNC_GH_APP_ID`.                       |
| `OKTA_TEAM_SYNC_GITHUB_TOKEN`        | conditionally | Fine-grained PAT fallback. Provide either PAT or GitHub App credentials.                             |
| `GITHUB_API_URL`                     | no            | Override for GitHub API base URL (leave blank for GitHub.com).                                       |
| `ORG_LIST`                           | yes*          | Comma- or newline-separated list of organization slugs to reconcile (*if omitted, derived from `OKTA_TEAM_SYNC_GH_INSTALLATION_IDS`). |
| `OKTA_TEAM_SYNC_GROUP_PREFIX`        | yes           | Okta group name prefix to monitor (e.g. `Okta-Team-`).                                               |
| `LOOKBACK_MINUTES`                   | no            | Minutes to backfill when no cursor is stored (default `15`).                                         |
| `STATE_PATH`                         | no            | Path to the persisted Okta cursor (default `state/okta_cursor.json`).                                |
| `MAX_WORKERS`                        | no            | Concurrency for processing candidate groups (default `2`).                                           |
| `DRY_RUN`                            | no            | Set to `true`/`false` to toggle dry-run behavior (default `false`).                                  |

If `ORG_LIST` is unset, the app will derive the organization list from the keys in `OKTA_TEAM_SYNC_GH_INSTALLATION_IDS` (comma- or newline-separated). Set `ORG_LIST` explicitly when you want to reconcile orgs that differ from the installed GitHub App list or when using PAT-based auth. In GitHub Actions, you can store the list as a variable (for example `OKTA_TEAM_SYNC_ORG_LIST`) and map it to `ORG_LIST` in the workflow env.

## Local Development

1. Install Docker with Docker Compose (the container uses Go 1.25.6).
2. Copy the template: `cp env.template .env` and populate required secrets (the real `.env` stays untracked).
3. Build the binary: `make build` (outputs `bin/sync`).
4. Run end-to-end with environment loaded: `make run`. Use `make dryrun` for a non-mutating rehearsal (`DRY_RUN=true`).
5. Execute unit tests: `make test` (`go test ./...`).
6. Validate Okta credentials: `make test-okta`; set `ASSERTION_ONLY=false` to exchange the assertion for an access token.
7. Diagnose Okta↔GitHub connectivity and mappings: `make diagnose` (set `DIAG_FLAGS=--http-debug` for verbose logs).

Cursor state is written to `state/okta_cursor.json` by default. Remove the file (or override `STATE_PATH`) to force a wider backfill window. Persist the cursor (for example via GitHub Actions cache) so only new events are processed; if reconciliation fails, the previous cursor is preserved automatically.

## License

This project is free for personal use. Commerical usage requires a license. See [`LICENSE.md`](LICENSE.md) for more details.
