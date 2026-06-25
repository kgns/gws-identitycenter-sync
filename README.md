# gws-identitycenter-sync

[![CI](https://github.com/kgns/gws-identitycenter-sync/actions/workflows/ci.yml/badge.svg)](https://github.com/kgns/gws-identitycenter-sync/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/kgns/gws-identitycenter-sync)](https://goreportcard.com/report/github.com/kgns/gws-identitycenter-sync)

Sync Google Workspace groups and their members into **AWS IAM Identity Center** using
the **Identity Store API** (`identitystore:*`) over normal IAM/SigV4 auth.

**Why not SCIM / ssosync / idp-scim-sync?** Those write over the SCIM endpoint, which
needs a bearer token that AWS hard-expires after 1 year and that **cannot be created
via any API** — only the console. That forces an annual manual rotation. This tool uses
no SCIM endpoint and **no bearer token**, so there is nothing to rotate. The only
long-lived credential is the Google service-account key, which you control.

The trade-off (no `externalId`, match-by-natural-key, and known limitations) is
documented in **[IMPLEMENTATION.md](IMPLEMENTATION.md)** — read it before deploying.

## How it works

Reconciliation each run: read desired state from Google (groups matching one or more
filters + their ACTIVE USER members) and current state from Identity Center, then
converge — create/update/rename users, create/rename groups, reconcile memberships, and
(optionally, scoped by a prefix) delete orphaned groups / prune users.

Each user is synced with name (given/family/formatted), display name, primary email,
and the extended attributes Google provides and the Identity Store API accepts: title,
preferred language, phone numbers, and addresses. Users missing a field AWS requires
(email / given name / family name) are skipped with a warning. See
[IMPLEMENTATION.md](IMPLEMENTATION.md#user-attributes-mapped-to-identity-center) for the
full mapping and the attributes that have no Identity Store home.

Stateless by default — matching is by natural key (email / display name). Set
`STATE_BUCKET` to enable an optional S3 join table so renames survive in place (see
**Rename-survival** below and [IMPLEMENTATION.md](IMPLEMENTATION.md)).

## Prerequisites

1. **Google service account** with domain-wide delegation, granted these read-only
   scopes in the Workspace Admin console:
   - `https://www.googleapis.com/auth/admin.directory.user.readonly`
   - `https://www.googleapis.com/auth/admin.directory.group.readonly`
   - `https://www.googleapis.com/auth/admin.directory.group.member.readonly`
2. A Workspace **admin email** for the service account to impersonate.
3. The IAM Identity Center **identity store id** (`d-xxxxxxxxxx`).

## Configuration (env vars)

| Var | Required | Notes |
|---|---|---|
| `IDENTITY_STORE_ID` | no | `d-xxxxxxxxxx`. Empty = auto-discover via `sso:ListInstances`; set explicitly if the account has more than one Identity Center instance |
| `GOOGLE_ADMIN_EMAIL` | yes | impersonated admin (or `GOOGLE_ADMIN_EMAIL_SECRET`) |
| `GOOGLE_CREDENTIALS` / `GOOGLE_CREDENTIALS_FILE` / `GOOGLE_CREDENTIALS_SECRET` | one of | SA JSON inline / path / Secrets Manager id |
| `GOOGLE_GROUPS_QUERY` | no | Directory API group search, e.g. `name:iam-*`. Multiple filters comma- or newline-separated (OR-combined). Empty = all groups |
| `GOOGLE_CUSTOMER_ID` | no | default `my_customer` |
| `MANAGED_GROUP_PREFIX` | no | gates deletes/prune, e.g. `iam-`. Empty = never delete/prune. Must align with `GOOGLE_GROUPS_QUERY` — the sync errors on startup if a non-empty prefix matches no synced or existing group |
| `PRUNE_USERS` | no | `true` to delete users dropped from managed groups (needs prefix). Leave `false` for the initial cutover; turn on in steady state |
| `DRY_RUN` | no | `true` logs intended changes without applying. Start `true` on a new deployment, then flip to `false` once a dry run looks right |
| `STATE_BUCKET` | no | S3 bucket for the join-table state — enables rename-survival. Empty = stateless |
| `STATE_KEY` | no | state object key (default `state.json`) |
| `LOG_LEVEL` | no | `debug` / `info` / `warn` / `error` (default `info`) |
| `LOG_FORMAT` | no | `json` / `text` (default `json`) |
| `AWS_REGION` | yes | set automatically on Lambda |

**Query ↔ prefix coherence:** `GOOGLE_GROUPS_QUERY` selects which groups are *synced*;
`MANAGED_GROUP_PREFIX` selects which of those are eligible for *delete/prune*. They are
independent settings, so a divergence (e.g. query `name:iam-*` but prefix `aws-`) leaves
pruning silently inert. To catch that, the sync **errors on startup** when a non-empty
prefix matches neither a synced group nor an existing Identity Center group. Keep the
prefix consistent with the groups your query returns.

**Rename-survival:** with `STATE_BUCKET` set, a Google email change (or group rename) is
applied as an in-place update that keeps the Identity Store id and any direct account
assignments. Without it, the tool is stateless and an email change is a delete+recreate
(group access self-heals; direct assignments are lost). See
[IMPLEMENTATION.md](IMPLEMENTATION.md#recovering-rename-survival-with-an-optional-state-file-state_bucket).

## Deploy from the Serverless Application Repository

The easiest way to run this in your own account — no clone or build needed.

[![Deploy from SAR](https://img.shields.io/badge/Deploy_from-Serverless_Application_Repository-FF9900)](https://console.aws.amazon.com/lambda/home#/create/app?applicationId=arn:aws:serverlessrepo:us-east-1:359519776451:applications/gws-identitycenter-sync)

1. Click the button above — or search for **`gws-identitycenter-sync`** in the
   [Serverless Application Repository](https://serverlessrepo.aws.amazon.com/applications)
   (tick *Show apps that create custom IAM roles or resource policies*). Deploy it in the
   region where your IAM Identity Center is enabled.
2. Provide the deployment parameters — at minimum `GoogleAdminEmail` and
   `GoogleServiceAccountJSON` (`IdentityStoreId` is auto-discovered; see
   [Configuration](#configuration-env-vars)). `DryRun` defaults to `true`.
3. Deploy. The stack creates the Lambda and its schedule, the Secrets Manager secret, and
   the state bucket.

> **Blast radius.** Deploying grants the Lambda write/delete on your Identity Store (users,
> groups, memberships). Destructive operations are gated (`MANAGED_GROUP_PREFIX` +
> `PRUNE_USERS`) and `DryRun` defaults on, but review [template.yaml](template.yaml) and
> [SECURITY.md](SECURITY.md) first, and don't run it alongside SCIM provisioning.

## Build, test, deploy

```bash
make test            # unit tests (reconcile engine, no AWS/Google needed)
make build           # local compile

# deploy as a scheduled Lambda (provided.al2023, arm64)
sam build
sam deploy --guided  # prompts for IdentityStoreId, GoogleAdminEmail, GoogleServiceAccountJSON, ...
```

`DryRun` defaults to `true`, so a fresh deployment only logs intended changes. Review one
scheduled run's logs, confirm the plan looks right (especially ~0 user creates if you are
adopting an existing directory), then redeploy with `DryRun=false` to go live.

Run locally / one-shot (uses your ambient AWS creds + Google SA file):

```bash
DRY_RUN=true \
IDENTITY_STORE_ID=d-xxxxxxxxxx \
GOOGLE_ADMIN_EMAIL=admin@yourco.com \
GOOGLE_CREDENTIALS_FILE=./sa.json \
GOOGLE_GROUPS_QUERY='name:iam-*' MANAGED_GROUP_PREFIX='iam-' \
AWS_REGION=eu-central-1 \
go run ./cmd/sync
```

## IAM (Lambda execution role)

Granted by `template.yaml`: the `identitystore:*` read/write actions listed in
[IMPLEMENTATION.md](IMPLEMENTATION.md#aws-iam-required-lambda-execution-role) plus
`secretsmanager:GetSecretValue` on the Google-credentials secret only. **No SCIM
token.**
