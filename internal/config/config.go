// Package config loads runtime configuration from environment variables (Lambda)
// or the equivalent flags (CLI). Google credentials may be supplied inline, from a
// file, or — most commonly on Lambda — from a Secrets Manager secret resolved by the
// caller after the AWS config is available.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	// Google credential source (exactly one of JSON/File/Secret must yield content).
	GoogleCredentialsJSON   []byte
	GoogleCredentialsSecret string // Secrets Manager id; resolved by caller if JSON empty

	// Admin user to impersonate for domain-wide delegation.
	GoogleAdminEmail       string
	GoogleAdminEmailSecret string // optional Secrets Manager id

	GoogleGroupsQueries []string // Directory API group-search filters, OR-combined. Empty = all groups.
	GoogleCustomerID    string   // default "my_customer"

	IdentityStoreID string // d-xxxxxxxxxx; empty => auto-discover via sso:ListInstances
	Region          string // AWS region

	// Optional S3-backed state (join table googleId -> Identity Store id). Enables
	// rename-survival. Empty StateBucket => stateless natural-key matching.
	StateBucket string
	StateKey    string // default "state.json"

	// ManagedGroupPrefix scopes destructive operations: only IC groups whose
	// display name (case-folded) starts with this prefix are eligible for deletion,
	// and only their members are eligible for user pruning. Empty => never delete
	// groups or prune users (safe default). e.g. "iam-".
	ManagedGroupPrefix string

	DryRun     bool // log intended changes without applying
	PruneUsers bool // delete IC users no longer desired (only within managed groups; needs ManagedGroupPrefix)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envBool(k string) bool {
	b, _ := strconv.ParseBool(os.Getenv(k))
	return b
}

// splitQueries parses GOOGLE_GROUPS_QUERY into individual Directory API group filters.
// Filters may be separated by newlines or commas; surrounding whitespace is trimmed and
// blanks dropped. An empty/blank value yields nil — meaning "all groups". (A single
// Google filter uses spaces, e.g. "name:iam-* email:iam-*", so comma/newline are safe
// separators between filters.)
func splitQueries(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == ',' })
	var out []string
	for _, f := range fields {
		if t := strings.TrimSpace(f); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// LoadFromEnv reads configuration from the process environment. GoogleCredentialsJSON
// is populated from GOOGLE_CREDENTIALS (raw) or GOOGLE_CREDENTIALS_FILE; if only
// GOOGLE_CREDENTIALS_SECRET is set, GoogleCredentialsSecret is returned for the caller
// to resolve via Secrets Manager.
func LoadFromEnv() (Config, error) {
	c := Config{
		GoogleCredentialsSecret: os.Getenv("GOOGLE_CREDENTIALS_SECRET"),
		GoogleAdminEmail:        os.Getenv("GOOGLE_ADMIN_EMAIL"),
		GoogleAdminEmailSecret:  os.Getenv("GOOGLE_ADMIN_EMAIL_SECRET"),
		GoogleGroupsQueries:     splitQueries(os.Getenv("GOOGLE_GROUPS_QUERY")),
		GoogleCustomerID:        env("GOOGLE_CUSTOMER_ID", "my_customer"),
		IdentityStoreID:         os.Getenv("IDENTITY_STORE_ID"),
		Region:                  env("AWS_REGION", os.Getenv("AWS_DEFAULT_REGION")),
		StateBucket:             os.Getenv("STATE_BUCKET"),
		StateKey:                env("STATE_KEY", "state.json"),
		ManagedGroupPrefix:      os.Getenv("MANAGED_GROUP_PREFIX"),
		DryRun:                  envBool("DRY_RUN"),
		PruneUsers:              envBool("PRUNE_USERS"),
	}

	if raw := os.Getenv("GOOGLE_CREDENTIALS"); raw != "" {
		c.GoogleCredentialsJSON = []byte(raw)
	} else if path := os.Getenv("GOOGLE_CREDENTIALS_FILE"); path != "" {
		b, err := os.ReadFile(path) // #nosec G304 G703 -- operator-supplied config path, not untrusted input
		if err != nil {
			return c, fmt.Errorf("reading GOOGLE_CREDENTIALS_FILE: %w", err)
		}
		c.GoogleCredentialsJSON = b
	}

	return c, nil
}

// Validate checks that the configuration is internally consistent. Call it after the
// caller has resolved any Secrets Manager references.
func (c Config) Validate() error {
	var missing []string
	if c.Region == "" {
		missing = append(missing, "AWS_REGION")
	}
	if len(c.GoogleCredentialsJSON) == 0 {
		missing = append(missing, "GOOGLE_CREDENTIALS / GOOGLE_CREDENTIALS_FILE / GOOGLE_CREDENTIALS_SECRET")
	}
	if c.GoogleAdminEmail == "" {
		missing = append(missing, "GOOGLE_ADMIN_EMAIL")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}
	return nil
}
