# Okta Groups + GitHub Teams Sync Agent

<!-- TOC -->

- [Overview](#overview)
- [Quick Usage](#quick-usage)
- [Deployment (GitHub Actions)](#deployment-github-actions)
- [Troubleshooting](#troubleshooting)
- [Prerequisites (One-Time Setup)](#prerequisites-one-time-setup)
  - [GitHub Enterprise Cloud](#github-enterprise-cloud)
  - [Okta Admin Console](#okta-admin-console)
- [Configuration](#configuration)
- [Local Development](#local-development)
- [License](#license)

<!-- /TOC -->

## Overview

`kc-okta-github-team-sync` is a Go app that syncs Okta groups to GitHub teams across multiple GitHub organizations. It is intended for GitHub Enterprise Cloud (GHEC) deployments that use GitHub Team Sync with Okta.

GitHub Team Sync keeps GitHub team membership aligned with Okta group membership, but it does not create new GitHub teams or automatically map newly created Okta groups to those teams. This app fills that gap:

- It runs as a scheduled agent, usually from GitHub Actions.
- It watches Okta groups whose names start with `OKTA_TEAM_SYNC_GROUP_PREFIX`.
- For each matching Okta group, it creates or reuses the corresponding GitHub team in each target organization.
- It maps the GitHub team back to the Okta group so Team Sync can manage membership.

Group names can optionally include a permission suffix. For example:
- with `OKTA_TEAM_SYNC_GROUP_PREFIX=github-team-`, `github-team-platform__read` creates or maps the `github-team-platform` team and sets its GitHub team permission to `pull`. 
- Supported suffix separators are `__` and `:`
  - For example: `github-team-platform__maintain` or `github-team-platform:admin`.

Supported permission suffixes are `admin`, `maintain`, `triage`, `write`, `member`, `read`, `viewer`, and `read-only`.

## Quick Usage

```shell
git clone https://github.com/CaseyLabs/kc-okta-github-team-sync
cd kc-okta-github-team-sync

cp env.template .env
# Edit .env with your Okta, GitHub, organization, and group prefix settings.

# Ensure Docker is running for all make targets.
make build
make dryrun
make run
```

`make dryrun` uses `DRY_RUN=true` and reports what would change without creating teams, updating mappings, or advancing the cursor.

## Deployment (GitHub Actions)

The repository includes an example workflow at:
- `examples/github-actions/okta-github-team-sync.yml`
- Copy it to `.github/workflows/okta-github-team-sync.yml`
- Then adjust the schedule and environment values for your organization

## Troubleshooting

- **Authentication failures** - Reconfirm values in `.env` or GitHub Actions secrets. 
- **Okta 403/429 responses** - Ensure the service principal is assigned to an administrator role spanning the tenant. 
- **Resetting state** - Remove the cursor file (`state/okta_cursor.json`) and optionally invalidate the Actions cache.
- **Isolating Okta client tests** - Run `docker compose run --rm --no-deps -T dev go test ./internal/okta -run TestFetchSystemLogDelta -count=1` to focus on the system log pagination test.

## Prerequisites (One-Time Setup)

Use this section to bootstrap a new GHEC + Okta deployment. Adapt names and URLs to your environment.

### GitHub Enterprise Cloud

Log into the GitHub web console as a service account or admin user with organization permissions.

Verify *Team Sync* is enabled for each GitHub organization:

- In each GitHub organization, go to `Settings -> Authentication Security -> Team synchronization`.
- If Team Sync is not enabled:
  - Log into the Okta Admin console with a service account.
    - Permissions needed: *Manage Users, Scope: Users resource - All users*.
    - Go to `Security -> API -> Tokens`.
    - Click *Create Token*.
    - Copy the token. This is your `SSWS Token`.
  - Log into GitHub using the same service account.
    - Go to `Organizations -> [Target Org] -> Settings`.
    - Go to *Authentication Security*.
    - Scroll to *Team Synchronization*.
    - Click *Enable for Okta*.
    - Paste the `SSWS Token`.
    - Set the URL to `https://<your-okta-domain>.okta.com`.

Create a dedicated GitHub App at the Enterprise level:

- In the Enterprise, go to `Settings -> GitHub Apps -> New GitHub App`.
- App Name: `Okta-Team-Sync`.
- Homepage URL: `https://github.com/<your-org>/<your-repo>`.
- Disable `Webhook -> Active`.
- Permissions:
  - `Organization permissions -> Members: Read & Write`.
  - `Organization permissions -> Administration: Read & Write`.
- Leave all other settings at defaults.
- Click *Create GitHub App*.
- Click *Generate a private key*.
- Save the private key and set `OKTA_TEAM_SYNC_GH_PRIVATE_KEY` to its contents.
- Copy the `App ID` and set `OKTA_TEAM_SYNC_GH_APP_ID` to that value.
- Click *Install App*.
- Install the app in each GitHub organization.

After installing the app into each organization, collect the installation IDs:

- In each GitHub organization, go to `Settings -> GitHub Apps -> Configure`.
- The installation settings URL includes the installation ID, for example `https://github.com/organizations/<ORG-name>/settings/installations/<INSTALLATION_ID>`.
- Populate `OKTA_TEAM_SYNC_GH_INSTALLATION_IDS` with comma- or newline-separated `org-slug:installation_id` entries. Organization slugs are case-insensitive.
- Optionally set `OKTA_TEAM_SYNC_GH_INSTALLATION_ID` when the GitHub App is installed once across all organizations or when you want a default fallback for organizations not listed in `OKTA_TEAM_SYNC_GH_INSTALLATION_IDS`.
- Mirror these values into GitHub Actions secrets so workflow runs resolve the correct installation per organization.

### Okta Admin Console

Log into the Okta Admin web console.

Go to *Applications -> Applications*:

- Click *Create App Integration*.
- Select *API Services* and click *Next*.
- App integration name: `GithubTeamSync`.

In the *General* tab:

- Copy the *Client ID* and save it as `OKTA_TEAM_SYNC_OKTA_CLIENT_ID`.
- Click `Client Credentials -> Edit`.
- Change *Client authentication* to *Public key / Private key*.
- Click *Add key*.
- Click *Generate new key*.
- Copy the `PEM` version of the private key and save it as `OKTA_TEAM_SYNC_OKTA_PRIVATE_KEY`.
- Copy the `KID` (Key ID) and save it as `OKTA_TEAM_SYNC_OKTA_KEY_ID`.
- Click *Done*.
- Click *Save*.

Go to `Security -> API -> Authorization Servers -> default`:

- Click `Access Policies -> Add Policy`.
  - Name: `GithubTeamSync-Policy`.
  - Description: `GithubTeamSync`.
  - Assign To: `GithubTeamSync`.
  - Click *Create Policy*.
- Click *Add rule*.
  - Rule Name: `GithubTeamSync-Rule`.
  - Leave defaults in place and click *Create Rule*.

Return to `Applications -> Applications -> GithubTeamSync`:

- In the *General* tab, disable *Require Demonstrating Proof of Possession (DPoP)* if it is enabled.
- In the *Okta API Scopes* tab, grant `okta.groups.read`, `okta.logs.read`, and `okta.apps.manage`.
- In the *Admin Roles* tab, add `Read-only Administrator`.

Then go to `Security -> Administrators`:

- Click *Add Administrator*.
- Select `GithubTeamSync`.
- Role: `Read-only Administrator`.
- Click *Save Changes*.

## Configuration

All runtime configuration is provided through environment variables. The repository ships with `env.template` as the local starting point.

| Variable                             | Required      | Description                                                                                          |
| ------------------------------------ | ------------- | ---------------------------------------------------------------------------------------------------- |
| `OKTA_TEAM_SYNC_OKTA_BASE_URL`       | yes           | Base URL of the Okta tenant, e.g. `https://example.okta.com`.                                       |
| `OKTA_TEAM_SYNC_OKTA_CLIENT_ID`      | yes           | Client ID for the API Service app using `private_key_jwt`.                                           |
| `OKTA_TEAM_SYNC_OKTA_PRIVATE_KEY`    | yes           | PEM-encoded private key for the service app (use literal newlines or `\n`).                          |
| `OKTA_TEAM_SYNC_OKTA_KEY_ID`         | conditionally | Key ID (`kid`) assigned when generating the Okta private key; required for Okta to accept the client assertion. |
| `OKTA_GITHUB_APP_IDS`                | conditionally | Comma- or newline-separated Okta application IDs for the GitHub Enterprise Cloud SCIM integration(s). Required only to auto-assign groups to Okta apps. |
| `OKTA_GITHUB_APP_ID`                 | conditionally | Backward-compatible single app ID; equivalent to a single-element `OKTA_GITHUB_APP_IDS`.             |
| `OKTA_TEAM_SYNC_OKTA_TOKEN_URL`      | no            | Token endpoint override; defaults to `${OKTA_TEAM_SYNC_OKTA_BASE_URL}/oauth2/v1/token`.              |
| `OKTA_TEAM_SYNC_OKTA_SCOPES`         | no            | Space-delimited scopes; defaults to `okta.groups.read okta.logs.read okta.apps.manage`.              |
| `OKTA_TEAM_SYNC_GH_APP_ID`           | conditionally | GitHub App identifier (numeric). Required when `OKTA_TEAM_SYNC_GITHUB_TOKEN` is empty.               |
| `OKTA_TEAM_SYNC_GH_INSTALLATION_ID`  | conditionally | Default installation ID for the GitHub App. Used when an organization is not listed in `OKTA_TEAM_SYNC_GH_INSTALLATION_IDS`. |
| `OKTA_TEAM_SYNC_GH_INSTALLATION_IDS` | conditionally | Comma- or newline-separated list of `org-slug:installation_id` pairs. Required for per-organization GitHub App installations and can provide the org list. |
| `OKTA_TEAM_SYNC_GH_PRIVATE_KEY`      | conditionally | PEM-encoded GitHub App private key associated with `OKTA_TEAM_SYNC_GH_APP_ID`.                       |
| `OKTA_TEAM_SYNC_GITHUB_TOKEN`        | conditionally | Fine-grained PAT fallback. Provide either PAT or GitHub App credentials.                             |
| `GITHUB_API_URL`                     | no            | Override for GitHub API base URL (leave blank for GitHub.com).                                       |
| `ORG_LIST`                           | yes*          | Comma- or newline-separated list of organization slugs to reconcile.                                 |
| `OKTA_TEAM_SYNC_ORG_LIST`            | yes*          | Alternate name for `ORG_LIST`.                                                                       |
| `OKTA_TEAM_SYNC_GROUP_PREFIX`        | yes           | Okta group name prefix to monitor, e.g. `github-team-`. No code default is provided.                 |
| `LOOKBACK_MINUTES`                   | no            | Minutes to backfill when no cursor is stored (default `15`).                                         |
| `STATE_PATH`                         | no            | Path to the persisted Okta cursor (default `state/okta_cursor.json`).                                |
| `MAX_WORKERS`                        | no            | Concurrency for processing candidate groups (default `2`).                                           |
| `DRY_RUN`                            | no            | Set to `true`/`false` to toggle dry-run behavior (default `false`).                                  |

- `ORG_LIST` or `OKTA_TEAM_SYNC_ORG_LIST` is required unless the app can derive organizations from `OKTA_TEAM_SYNC_GH_INSTALLATION_IDS`. 
- Set an org list explicitly when using PAT-based auth or when the orgs to reconcile differ from the GitHub App installation map. 
- In GitHub Actions, the sample stores this as the variable `OKTA_TEAM_SYNC_ORG_LIST` and maps it to `ORG_LIST`.

## Local Development

1. Install Docker with Docker Compose.
2. Copy the template: `cp env.template .env` and populate required values.
3. Build the binary: `make build` (outputs `bin/sync`).
4. Run a dry-run: `make dryrun`.
5. Run the sync: `make run`.
6. Execute unit tests: `make test` (`go test ./...`).
7. Validate Okta credentials: `make test-okta`; set `ASSERTION_ONLY=false` to exchange the assertion for an access token.
8. Diagnose Okta/GitHub connectivity and mappings: `make diagnose` (set `DIAG_FLAGS=--http-debug` for verbose logs).

- Cursor state is written to `state/okta_cursor.json` by default. 

## License

This project is free for personal use. Commercial usage requires a license. See [`LICENSE.md`](LICENSE.md) for more details.
