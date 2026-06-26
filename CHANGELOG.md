# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). The version is the deployment
contract (template parameters / env vars); a breaking change to it bumps the major.

## [Unreleased]

## [1.0.1] - 2026-06-26

### Fixed

- Grant `s3:ListBucket` on the state bucket. Without it, a first run (no `state.json` yet)
  failed with `403 AccessDenied` instead of reading an empty state — S3 returns 403 rather
  than 404 for a missing object when the caller cannot list the bucket.

## [1.0.0] - 2026-06-26

### Added

- Reconcile Google Workspace groups and their active members into AWS IAM Identity Center
  over the Identity Store API — no SCIM endpoint and no bearer token to rotate.
- Create / update / rename users and groups, reconcile memberships, and (gated) delete
  orphaned groups and prune users.
- Optional S3-backed state file (`STATE_BUCKET`) for in-place rename survival; stateless
  natural-key matching otherwise.
- Startup coherence guard: errors when `MANAGED_GROUP_PREFIX` can never match a synced or
  existing group.
- `DRY_RUN` (default on) and double-gated destructive operations
  (`MANAGED_GROUP_PREFIX` + `PRUNE_USERS`).
- SAM deployment as a scheduled Lambda; published to the AWS Serverless Application
  Repository.

[Unreleased]: https://github.com/kgns/gws-identitycenter-sync/compare/v1.0.1...HEAD
[1.0.1]: https://github.com/kgns/gws-identitycenter-sync/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/kgns/gws-identitycenter-sync/releases/tag/v1.0.0
