# Security Policy

## Reporting a vulnerability

Please report suspected vulnerabilities privately via GitHub's
[**Report a vulnerability**](https://github.com/kgns/gws-identitycenter-sync/security/advisories/new)
(Security → Advisories). Do not open a public issue for a security report.

Include reproduction steps and the affected version/commit. You'll get an acknowledgement,
and a fix and disclosure will be coordinated with you.

## What this tool can do in your account

Deploying this application grants its Lambda **write and delete** access to your IAM
Identity Center directory via the Identity Store API:

- `identitystore:CreateUser`, `UpdateUser`, `DeleteUser`
- `identitystore:CreateGroup`, `UpdateGroup`, `DeleteGroup`
- `identitystore:CreateGroupMembership`, `DeleteGroupMembership`

plus read access to the Google service-account secret in Secrets Manager and the S3 state
bucket. Review [template.yaml](template.yaml) and [IMPLEMENTATION.md](IMPLEMENTATION.md)
before deploying into a production organization, and note:

- **Destructive operations are gated.** Group/user deletion only happens within
  `MANAGED_GROUP_PREFIX`, and user deletion additionally requires `PRUNE_USERS=true`. With
  the defaults, nothing is deleted.
- **Dry run first.** `DRY_RUN` defaults to `true`; review one run's logs before going live.
- **Single writer.** Do not run this alongside SCIM provisioning against the same
  directory — concurrent writers cause drift.

## Supported versions

Fixes are released against the latest published version. Older versions are not patched.
