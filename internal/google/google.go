// Package google reads the desired state (managed groups + their user members) from
// the Google Workspace Directory API, using a service account with domain-wide
// delegation impersonating an admin. Read-only scopes only.
package google

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"golang.org/x/oauth2/google"
	admin "google.golang.org/api/admin/directory/v1"
	"google.golang.org/api/option"

	"github.com/kgns/gws-identitycenter-sync/internal/model"
)

type Client struct {
	svc        *admin.Service
	customerID string
	queries    []string // group-search filters, OR-combined. Empty/nil = all groups.
	log        *slog.Logger
}

// New builds a Directory API client from a service-account JSON key, impersonating
// adminEmail via domain-wide delegation. groupsQueries are OR-combined Directory API
// group-search filters (one List call each); empty/nil means "all groups".
func New(ctx context.Context, credentialsJSON []byte, adminEmail, customerID string, groupsQueries []string, log *slog.Logger) (*Client, error) {
	cfg, err := google.JWTConfigFromJSON(credentialsJSON,
		admin.AdminDirectoryUserReadonlyScope,
		admin.AdminDirectoryGroupReadonlyScope,
		admin.AdminDirectoryGroupMemberReadonlyScope,
	)
	if err != nil {
		return nil, fmt.Errorf("parsing Google credentials: %w", err)
	}
	cfg.Subject = adminEmail

	svc, err := admin.NewService(ctx, option.WithHTTPClient(cfg.Client(ctx)))
	if err != nil {
		return nil, fmt.Errorf("creating Directory service: %w", err)
	}
	if customerID == "" {
		customerID = "my_customer"
	}
	return &Client{svc: svc, customerID: customerID, queries: groupsQueries, log: log}, nil
}

// Fetch returns the desired Directory: groups matching the configured query filters and
// the ACTIVE USER members of those groups (with their profiles).
//
// Multiple filters are OR-combined — one Groups.List call per filter, results unioned —
// matching idp-scim-sync's repeatable --gws-groups-filter. Groups are deduped by their
// case-folded display name (the natural key); a collision across filters is skipped with
// a warning.
func (c *Client) Fetch(ctx context.Context) (model.Directory, error) {
	dir := model.NewDirectory()
	profiles := map[string]model.User{} // cache: user key -> profile
	skipped := map[string]bool{}        // user keys that failed the required-field guard
	seen := map[string]bool{}           // group keys already added (dedup across filters)

	queries := c.queries
	if len(queries) == 0 {
		queries = []string{""} // no filter => all groups
	}

	for _, q := range queries {
		call := c.svc.Groups.List().Customer(c.customerID).MaxResults(200)
		if q != "" {
			call = call.Query(q)
		}
		err := call.Pages(ctx, func(page *admin.Groups) error {
			for _, g := range page.Groups {
				grp := model.Group{GoogleID: g.Id, DisplayName: g.Name, Description: g.Description}
				if grp.Key() == "" {
					c.log.Warn("skipping group with empty name", "id", g.Id, "email", g.Email)
					continue
				}
				if seen[grp.Key()] {
					c.log.Warn("skipping duplicate group (same display name matched by another filter)", "name", g.Name, "email", g.Email)
					continue
				}
				members, err := c.fetchMembers(ctx, g.Email, profiles, skipped, dir)
				if err != nil {
					return err
				}
				grp.MemberKeys = members
				dir.Groups[grp.Key()] = grp
				seen[grp.Key()] = true
			}
			return nil
		})
		if err != nil {
			return dir, fmt.Errorf("listing groups (filter %q): %w", q, err)
		}
	}
	return dir, nil
}

// fetchMembers returns the user keys of the ACTIVE USER members of a group, and records
// their profiles into dir.Users (deduped via the profiles cache). Users that fail the
// required-field guard (see userProfile) are skipped and excluded from the returned keys
// so they never dangle as a membership of a user we deliberately did not create.
//
// IncludeDerivedMembership(true) is essential: it returns *indirect* members —
// nested-group members (flattened to USER entries) and auto/dynamic memberships such as
// the org-wide "everyone" group. Without it, an auto-applied group reports ~no direct
// members and reconciliation would strip everyone.
//
// We gate on member.Status == "ACTIVE" (the authoritative per-membership signal:
// excludes suspended, archived, and otherwise-inactive members) rather than a separate
// per-user suspended check, which misses archived users.
func (c *Client) fetchMembers(ctx context.Context, groupKey string, profiles map[string]model.User, skipped map[string]bool, dir model.Directory) ([]string, error) {
	var keys []string
	err := c.svc.Members.List(groupKey).IncludeDerivedMembership(true).MaxResults(200).Pages(ctx, func(page *admin.Members) error {
		for _, m := range page.Members {
			// With derived membership, nested-group members appear flattened as USER
			// entries; the GROUP container entries themselves are skipped here.
			if m.Type != "USER" || m.Email == "" {
				continue
			}
			if m.Status != "ACTIVE" {
				c.log.Info("skipping non-active member", "group", groupKey, "email", m.Email, "status", m.Status)
				continue
			}

			key := model.User{PrimaryEmail: m.Email}.Key()
			if skipped[key] {
				continue // already determined this user fails the required-field guard
			}
			if _, ok := profiles[key]; ok {
				keys = append(keys, key) // profile already fetched via another group
				continue
			}

			profile, ok, err := c.userProfile(ctx, m.Email)
			if err != nil {
				return err
			}
			if !ok {
				skipped[key] = true
				continue
			}
			profiles[key] = profile
			dir.Users[key] = profile
			keys = append(keys, key)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing members of %s: %w", groupKey, err)
	}
	return keys, nil
}

// userProfile fetches a Google user and maps it (with extended attributes) to a
// model.User. It returns ok=false (no error) when the user lacks a field AWS Identity
// Center requires on create — primary email, given name, or family name — matching
// idp-scim-sync's buildUser guard so the create cannot be rejected downstream. Such
// users are skipped with a warning.
func (c *Client) userProfile(ctx context.Context, email string) (model.User, bool, error) {
	u, err := c.svc.Users.Get(email).Context(ctx).Do()
	if err != nil {
		return model.User{}, false, fmt.Errorf("get user %s: %w", email, err)
	}

	var given, family, full string
	if u.Name != nil {
		given = strings.TrimSpace(u.Name.GivenName)
		family = strings.TrimSpace(u.Name.FamilyName)
		full = strings.TrimSpace(u.Name.FullName)
	}
	if u.PrimaryEmail == "" || given == "" || family == "" {
		c.log.Warn("skipping user missing a field AWS Identity Center requires (email/givenName/familyName)",
			"email", email, "hasGivenName", given != "", "hasFamilyName", family != "")
		return model.User{}, false, nil
	}

	return model.User{
		GoogleID:          u.Id,
		PrimaryEmail:      u.PrimaryEmail,
		GivenName:         given,
		FamilyName:        family,
		DisplayName:       full,
		Title:             c.pickTitle(u.Organizations),
		PreferredLanguage: c.pickLanguage(u.Languages),
		PhoneNumbers:      c.phoneNumbers(u.Phones),
		Addresses:         c.addresses(u.Addresses),
	}, true, nil
}

// The Directory API returns these collection attributes as untyped JSON (interface{}).
// We re-marshal into the package's typed structs rather than asserting through
// map[string]any — cleaner and less error-prone.

func (c *Client) phoneNumbers(v interface{}) []model.PhoneNumber {
	if v == nil {
		return nil
	}
	var ph []admin.UserPhone
	if err := remarshal(v, &ph); err != nil {
		c.log.Warn("could not parse user phones", "err", err)
		return nil
	}
	var out []model.PhoneNumber
	for _, p := range ph {
		if strings.TrimSpace(p.Value) == "" {
			continue
		}
		out = append(out, model.PhoneNumber{Value: p.Value, Type: resolveType(p.Type, p.CustomType), Primary: p.Primary})
	}
	return out
}

func (c *Client) addresses(v interface{}) []model.Address {
	if v == nil {
		return nil
	}
	var ad []admin.UserAddress
	if err := remarshal(v, &ad); err != nil {
		c.log.Warn("could not parse user addresses", "err", err)
		return nil
	}
	var out []model.Address
	for _, a := range ad {
		out = append(out, model.Address{
			Formatted: a.Formatted, StreetAddress: a.StreetAddress, Locality: a.Locality,
			Region: a.Region, PostalCode: a.PostalCode, Country: a.Country,
			Type: resolveType(a.Type, a.CustomType), Primary: a.Primary,
		})
	}
	return out
}

// pickTitle returns the title of the primary organization, else the first org with a
// non-empty title.
func (c *Client) pickTitle(v interface{}) string {
	if v == nil {
		return ""
	}
	var orgs []admin.UserOrganization
	if err := remarshal(v, &orgs); err != nil {
		c.log.Warn("could not parse user organizations", "err", err)
		return ""
	}
	first := ""
	for _, o := range orgs {
		t := strings.TrimSpace(o.Title)
		if t == "" {
			continue
		}
		if o.Primary {
			return t
		}
		if first == "" {
			first = t
		}
	}
	return first
}

// pickLanguage returns the user's preferred language code, else the first non-empty one.
func (c *Client) pickLanguage(v interface{}) string {
	if v == nil {
		return ""
	}
	var langs []admin.UserLanguage
	if err := remarshal(v, &langs); err != nil {
		c.log.Warn("could not parse user languages", "err", err)
		return ""
	}
	first := ""
	for _, l := range langs {
		code := strings.TrimSpace(l.LanguageCode)
		if code == "" {
			code = strings.TrimSpace(l.CustomLanguage)
		}
		if code == "" {
			continue
		}
		if l.Preference == "preferred" {
			return code
		}
		if first == "" {
			first = code
		}
	}
	return first
}

// resolveType returns the custom type label when Google marks the type as "custom",
// otherwise the standard type.
func resolveType(typ, custom string) string {
	typ = strings.TrimSpace(typ)
	if typ == "custom" && strings.TrimSpace(custom) != "" {
		return strings.TrimSpace(custom)
	}
	return typ
}

// remarshal converts a Directory API interface{} field (already-decoded JSON) into a
// typed destination by round-tripping through JSON.
func remarshal(src, dst interface{}) error {
	b, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}
