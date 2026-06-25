# Design: Google Workspace → IAM Identity Center sync via the Identity Store API

## Why this exists

`ssosync` (and the related `idp-scim-sync`) write into IAM Identity Center over the
**SCIM** endpoint, which requires a **bearer token that AWS hard-expires after 1 year and
cannot be minted via any API** — only the console. That forces an annual manual
rotation.

This tool writes over the **Identity Store API** (`identitystore:*`) using normal
IAM/SigV4 auth. **There is no bearer token, so there is nothing to rotate.** The only
long-lived credential is the Google service-account key, which *you* control and can
rotate on your own schedule.

## The one hard trade-off: no `externalId`

SCIM lets you stamp the IdP's stable user id into AWS as `externalId` and match on it
forever. The Identity Store `CreateUser`/`CreateGroup` APIs **have no `externalId`
field**. So we cannot persist Google's immutable id on the AWS side.

**Consequence — matching is by natural key:**

| Entity | Match key (both sides) | AWS attribute |
|---|---|---|
| User | primary email (case-folded) | `userName` |
| Group | display name (case-folded) | `displayName` |
| Membership | (group key, user key) | `MemberId` union → `UserId` |

### Recovering rename-survival with an optional state file (`STATE_BUCKET`)

Because AWS can't hold `externalId`, we optionally keep the `googleId → Identity Store
id` mapping ourselves, in an S3 **state file** (`internal/state`) — the join table that
externalId would otherwise have given us:

```
users:  googleUserId  -> { icUserId, email }
groups: googleGroupId -> { icGroupId, displayName }
```

With it, matching is **state-first**: look up by Google's immutable id, fall back to
natural key for anything not yet mapped (first run, or state lost). So a Google **email
change** is recognised as the *same* user (same `googleId → icUserId`) and applied as an
**in-place `UpdateUser` rename** — the `UserId` and any direct account assignments
survive. Group display-name changes are handled the same way via `UpdateGroup`. The
engine rewrites its in-memory view of current state after a rename so membership
reconciliation produces **no spurious churn** for the renamed principal.

`RenameUser` surfaces an error rather than silently falling back to delete+recreate if
the API rejects a `userName` change — that protects direct assignments either way.

**The state file is an optimization layer, not a source of truth:** lose it and the
next run rebuilds by natural-key adoption — degraded (email changes become
delete+recreate again), never broken. Enable S3 versioning. **Without `STATE_BUCKET`
set, the tool is fully stateless** (natural-key matching only), and an email change is a
delete+recreate: group access self-heals next sync, but direct assignments to the old
`UserId` are lost. Pick based on whether you use direct (non-group) assignments.

## Scope of "managed" objects (safety)

To avoid touching unrelated principals (e.g. break-glass admins created directly in
IC), the tool manages only:

- **Groups** returned by the Google group search `GOOGLE_GROUPS_QUERY` — one or more
  comma/newline-separated filters, OR-combined (one `Groups.List` call each, results
  unioned, then deduped by display-name key), like idp-scim-sync's repeatable
  `--gws-groups-filter`; empty = all groups — and
- **Users** who are USER-type members of those groups.

On the IC side, a group is considered "ours" only if its `displayName` matches a
desired group's key. We never delete a group we didn't expect.

## User attributes mapped to Identity Center

For each ACTIVE member we fetch the full Google profile (`Users.Get`) and map these into
the Identity Store user (`internal/google` reads, `internal/identitycenter` writes):

| Identity Store attribute | Google source | Notes |
|---|---|---|
| `userName` | `primaryEmail` | also the match key |
| `name.givenName` / `name.familyName` | `name.givenName` / `name.familyName` | **required** (see guard) |
| `name.formatted` / `displayName` | `name.fullName`, else `Given Family`, else email | one value, computed once (`model.User.EffectiveDisplayName`) |
| `emails` | `primaryEmail` | single primary `work` email |
| `title` | primary `organizations[].title` (else first non-empty) | |
| `preferredLanguage` | preferred `languages[].languageCode` (else first) | |
| `phoneNumbers` | `phones[]` (`value`/`type`/`primary`; `custom` → `customType`) | |
| `addresses` | `addresses[]` (street/locality/region/postal/country/formatted/type/primary) | |

**Required-field guard.** A user missing `primaryEmail`, `givenName`, or `familyName` is
**skipped with a warning** and dropped from its groups' member lists — matching
idp-scim-sync's `buildUser` guard. AWS Identity Center requires these on `CreateUser`, so
this prevents a downstream rejection rather than letting one user fail the run.

**Replace-only, never clear.** On update we emit an `UpdateUser` operation for an optional
attribute **only when the desired value is present**. We never send an empty/clearing op,
because (a) the API may reject an empty value and (b) an unclearable attribute would flip
"changed" on every run and never converge. Change detection (`model.UserChanged`) is
symmetric with this — an empty desired value is not treated as a change. **Trade-off:** an
attribute *removed* in Google is not cleared in Identity Center (it goes stale until set
to a new value or cleared manually).

### Attributes deliberately not mapped (and why)

- **`userType`** — idp-scim-sync sets it from the Google `Kind` field, which is the
  constant string `admin#directory#user` (not a meaningful user type). We leave `userType`
  unset rather than write that noise to every user.
- **Enterprise data** (`employeeNumber`, `department`, `costCenter`, `division`,
  `organization`, `manager`) — the Identity Store API exposes an
  `aws:identitystore:enterprise` slot via the `Extensions` map on `CreateUser`/`UpdateUser`,
  but it is **not implemented** here: mapping it needs non-trivial assembly of the Google
  side (`organizations[]` plus `relations[]` for manager). A possible future addition.

## Reconciliation algorithm (`internal/sync`)

```
desired := google.Fetch()          // managed groups + member user profiles
current := identitycenter.Fetch()  // all IC users, groups, memberships
prev    := state.Load()            // join table (empty if no STATE_BUCKET / first run)

# users first (so memberships can reference them)
for u in desired.Users:
    match u to a current user: by googleId via prev (state), else by email
    if no match:                      create user  -> get UserId
    else if email changed:            rename user  (UpdateUser userName; keeps UserId)
    else if attrs changed:            update user
    record googleId -> {UserId, email} into next state

# groups
for g in desired.Groups:
    match g to a current group: by googleId via prev (state), else by displayName
    if no match:                      create group -> get GroupId
    else if displayName changed:      rename group (UpdateGroup displayName; keeps GroupId)
    record googleId -> {GroupId, displayName} into next state

# memberships (per desired group; current is rename-consistent here)
for g in desired.Groups:
    add    = desired members - current members  -> CreateGroupMembership
    remove = current members - desired members  -> DeleteGroupMembership

# destructive deletes last, only within MANAGED_GROUP_PREFIX
for g in current.Groups (managed) not in desired:   remove memberships, then delete group
for u in managed users not in desired (if PruneUsers): delete user

state.Save(next)   # unless dry-run; a save failure only degrades the next run to natural-key matching
```

Order matters: **create users/groups before adding memberships; remove memberships
before deleting groups/users.** Deletes run last. Without `STATE_BUCKET`, `prev`/`next`
are empty and matching is purely by natural key (an email change becomes
delete+recreate, not a rename).

`DRY_RUN=true` logs every intended call without executing it.

### Membership reading (matches idp-scim-sync)

Group members are read with `Members.list(IncludeDerivedMembership=true)` and filtered
to `member.Status == "ACTIVE"`:

- **Derived membership** returns *indirect* members — nested-group members (flattened
  to USER entries) and auto/dynamic memberships like an org-wide "everyone" group.
  Without it, an auto-applied group reports ~no direct members and reconciliation would
  strip every member. The GROUP container entries are skipped (members come through
  flattened).
- **`member.Status == "ACTIVE"`** is the authoritative per-membership signal and
  excludes suspended, archived, and otherwise-inactive members. (We use this instead of
  a separate `Users.get().suspended` check, which misses archived users.)

### Notes / known limitations

- **No "active" flag.** The Identity Store API has no per-user enabled/disabled bit
  via Create/Update. Inactive Google users are simply not provisioned (excluded by the
  ACTIVE-member filter); if one was already in IC and goes inactive, its memberships are
  removed (and, with `PRUNE_USERS=true`, the user deleted).
- **Rate limits.** Identity Store APIs have modest TPS quotas; for large directories
  add client-side throttling/backoff (the SDK already retries throttles).

## Before you enable this

- **Run a single writer.** Mutating the Identity Store API while SCIM is also
  provisioning the same directory causes drift — they are an **either/or**, not additive.
  Disable automatic provisioning (SCIM) in IAM Identity Center before enabling this tool.
- **Verify the first run adopts, not recreates.** If the directory already contains
  users, this tool matches them by `userName` (email) and should *adopt* them rather than
  create duplicates. Do a `DRY_RUN=true` run first and confirm it plans ~0 user creates
  and only reconciles memberships. If it plans to recreate everyone, stop: the `userName`s
  don't line up.

## AWS IAM required (Lambda execution role)

No SCIM secret. The role needs:

```
identitystore:ListUsers, ListGroups, ListGroupMemberships, GetGroupMembershipId,
identitystore:CreateUser, UpdateUser, DeleteUser,
identitystore:CreateGroup, UpdateGroup, DeleteGroup,
identitystore:CreateGroupMembership, DeleteGroupMembership
  (resource: *  — Identity Store does not support resource-level scoping here)
sso:ListInstances                (auto-discover the identity store id; skipped when IDENTITY_STORE_ID is set)
secretsmanager:GetSecretValue   (only the Google credential secret ARN)
s3:GetObject, s3:PutObject       (only the state bucket, when STATE_BUCKET is set)
```
