// Command sync reconciles Google Workspace groups/members into AWS IAM Identity
// Center via the Identity Store API. It runs once as a CLI, or as a Lambda when
// AWS_LAMBDA_FUNCTION_NAME is set (e.g. on a 15-minute EventBridge schedule).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/identitystore"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/ssoadmin"

	"github.com/kgns/gws-identitycenter-sync/internal/config"
	"github.com/kgns/gws-identitycenter-sync/internal/google"
	"github.com/kgns/gws-identitycenter-sync/internal/identitycenter"
	"github.com/kgns/gws-identitycenter-sync/internal/state"
	syncengine "github.com/kgns/gws-identitycenter-sync/internal/sync"
)

func main() {
	log := newLogger()

	if os.Getenv("AWS_LAMBDA_FUNCTION_NAME") != "" {
		lambda.Start(func(ctx context.Context) error { return run(ctx, log) })
		return
	}
	if err := run(context.Background(), log); err != nil {
		log.Error("sync failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		return err
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return fmt.Errorf("loading AWS config: %w", err)
	}

	// Resolve Google credentials / admin email from Secrets Manager if not inline.
	sm := secretsmanager.NewFromConfig(awsCfg)
	if len(cfg.GoogleCredentialsJSON) == 0 && cfg.GoogleCredentialsSecret != "" {
		s, err := secretString(ctx, sm, cfg.GoogleCredentialsSecret)
		if err != nil {
			return fmt.Errorf("resolving GOOGLE_CREDENTIALS_SECRET: %w", err)
		}
		cfg.GoogleCredentialsJSON = []byte(s)
	}
	if cfg.GoogleAdminEmail == "" && cfg.GoogleAdminEmailSecret != "" {
		s, err := secretString(ctx, sm, cfg.GoogleAdminEmailSecret)
		if err != nil {
			return fmt.Errorf("resolving GOOGLE_ADMIN_EMAIL_SECRET: %w", err)
		}
		cfg.GoogleAdminEmail = strings.TrimSpace(s)
	}

	if err := cfg.Validate(); err != nil {
		return err
	}

	// IDENTITY_STORE_ID is optional: auto-discover it from the account's Identity Center
	// instance when not supplied.
	if cfg.IdentityStoreID == "" {
		id, err := resolveIdentityStoreID(ctx, ssoadmin.NewFromConfig(awsCfg))
		if err != nil {
			return err
		}
		cfg.IdentityStoreID = id
		log.Info("auto-discovered identity store id", "identity_store_id", id)
	}

	if cfg.DryRun {
		log.Info("DRY RUN: no changes will be applied")
	}

	gws, err := google.New(ctx, cfg.GoogleCredentialsJSON, cfg.GoogleAdminEmail, cfg.GoogleCustomerID, cfg.GoogleGroupsQueries, log)
	if err != nil {
		return err
	}
	ic := identitycenter.New(identitystore.NewFromConfig(awsCfg), cfg.IdentityStoreID, cfg.DryRun, log)

	var store state.Store = state.Noop{}
	if cfg.StateBucket != "" {
		store = state.NewS3Store(s3.NewFromConfig(awsCfg), cfg.StateBucket, cfg.StateKey)
		log.Info("state-backed matching enabled", "bucket", cfg.StateBucket, "key", cfg.StateKey)
	}

	engine := syncengine.NewEngine(gws, ic, store, cfg.ManagedGroupPrefix, cfg.PruneUsers, cfg.DryRun, log)
	stats, err := engine.Run(ctx)
	if err != nil {
		return err
	}
	log.Info("sync complete",
		"users_created", stats.UsersCreated, "users_updated", stats.UsersUpdated,
		"users_renamed", stats.UsersRenamed, "users_deleted", stats.UsersDeleted,
		"groups_created", stats.GroupsCreated, "groups_renamed", stats.GroupsRenamed, "groups_deleted", stats.GroupsDeleted,
		"memberships_added", stats.MembershipsAdded, "memberships_removed", stats.MembershipsRemoved,
		"errors", stats.Errors)

	if stats.Errors > 0 {
		return fmt.Errorf("sync completed with %d errors", stats.Errors)
	}
	return nil
}

func secretString(ctx context.Context, sm *secretsmanager.Client, id string) (string, error) {
	out, err := sm.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: &id})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.SecretString), nil
}

// resolveIdentityStoreID looks up the identity store id from the account's IAM Identity
// Center instance. It fails on zero instances (Identity Center not enabled in this
// account/region) or more than one (org plus account-level instances) — the latter must
// be disambiguated by setting IDENTITY_STORE_ID explicitly.
func resolveIdentityStoreID(ctx context.Context, c *ssoadmin.Client) (string, error) {
	out, err := c.ListInstances(ctx, &ssoadmin.ListInstancesInput{})
	if err != nil {
		return "", fmt.Errorf("auto-discovering identity store id (set IDENTITY_STORE_ID to skip): %w", err)
	}
	switch len(out.Instances) {
	case 0:
		return "", fmt.Errorf("no IAM Identity Center instance found in this account/region; enable Identity Center or set IDENTITY_STORE_ID")
	case 1:
		return aws.ToString(out.Instances[0].IdentityStoreId), nil
	default:
		return "", fmt.Errorf("found %d Identity Center instances; set IDENTITY_STORE_ID explicitly", len(out.Instances))
	}
}

// newLogger builds the slog logger from LOG_LEVEL (debug|info|warn|error, default info)
// and LOG_FORMAT (json|text, default json).
func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	if strings.EqualFold(os.Getenv("LOG_FORMAT"), "text") {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}
