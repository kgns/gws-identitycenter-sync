# Publishing to the Serverless Application Repository

This repository includes a [`release`](.github/workflows/release.yml) workflow that builds,
tests, and publishes the application to the AWS Serverless Application Repository (SAR) as
a public application, then cuts a GitHub Release. To publish your own copy, complete the
one-time setup below and push a semantic-version tag.

Throughout, replace the placeholders: `<account-id>`, `<region>` (the workflow defaults to
`us-east-1`), `<artifacts-bucket>`, `<owner>/<repo>`, and the application `Name` / `Author`
declared in the `AWS::ServerlessRepo::Application` metadata of [template.yaml](template.yaml).

## One-time AWS setup

### 1. Artifacts bucket

SAR serves the packaged Lambda from S3, so the bucket must allow the SAR service to read it.

```bash
aws s3 mb s3://<artifacts-bucket> --region <region>
```

Attach this bucket policy:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ServerlessRepoRead",
      "Effect": "Allow",
      "Principal": {
        "Service": "serverlessrepo.amazonaws.com"
      },
      "Action": "s3:GetObject",
      "Resource": "arn:aws:s3:::<artifacts-bucket>/*",
      "Condition": {
        "StringEquals": {
          "aws:SourceAccount": "<account-id>"
        }
      }
    }
  ]
}
```

### 2. GitHub OIDC provider and publish role

If the account has no GitHub OIDC provider yet:

```bash
aws iam create-open-id-connect-provider \
  --url https://token.actions.githubusercontent.com \
  --client-id-list sts.amazonaws.com
```

Create a role for the workflow to assume. Trust policy — scoped to tags of the publishing
repository (add a branch `sub` to also dispatch the workflow manually from a branch):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Federated": "arn:aws:iam::<account-id>:oidc-provider/token.actions.githubusercontent.com"
      },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "token.actions.githubusercontent.com:aud": "sts.amazonaws.com"
        },
        "StringLike": {
          "token.actions.githubusercontent.com:sub": "repo:<owner>/<repo>:ref:refs/tags/v*"
        }
      }
    }
  ]
}
```

Permission policy (scope the bucket ARN to the artifacts bucket):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "Artifacts",
      "Effect": "Allow",
      "Action": [
        "s3:PutObject",
        "s3:GetObject"
      ],
      "Resource": "arn:aws:s3:::<artifacts-bucket>/*"
    },
    {
      "Sid": "PublishToSar",
      "Effect": "Allow",
      "Action": [
        "serverlessrepo:CreateApplication",
        "serverlessrepo:CreateApplicationVersion",
        "serverlessrepo:UpdateApplication",
        "serverlessrepo:GetApplication",
        "serverlessrepo:ListApplications",
        "serverlessrepo:GetApplicationPolicy",
        "serverlessrepo:PutApplicationPolicy"
      ],
      "Resource": "*"
    }
  ]
}
```

### 3. Repository variables

Settings → Secrets and variables → Actions → **Variables** (none are secret):

| Variable | Value |
|---|---|
| `AWS_PUBLISH_ROLE_ARN` | ARN of the role from step 2 |
| `SAR_ARTIFACTS_BUCKET` | the bucket from step 1 |
| `AWS_REGION` | optional; defaults to `us-east-1` |

## Reserving the application ARN up front (optional)

`sam publish` creates the application automatically on its first run. To know the
application ARN before the first release — for example, to fill in a deploy link — create
the application with no versions first. The ARN is stable for the application's lifetime.

```bash
aws serverlessrepo create-application \
  --region <region> \
  --name <app-name> \
  --author "<author>" \
  --description "<description>" \
  --spdx-license-id Apache-2.0 \
  --query ApplicationId --output text
```

The returned `ApplicationId` has the form
`arn:aws:serverlessrepo:<region>:<account-id>:applications/<app-name>`.

## Cutting a release

1. Update [CHANGELOG.md](CHANGELOG.md).
2. Tag and push — the tag becomes the published `SemanticVersion`:

   ```bash
   git tag v1.0.0
   git push origin v1.0.0
   ```

3. The workflow publishes the version to SAR (creating the application on the first run)
   and makes it public. It can also be triggered from the Actions tab with an explicit
   version.
```