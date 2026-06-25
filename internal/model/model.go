// Package model holds the IdP-agnostic domain types used by the sync engine.
//
// Unlike a SCIM-based sync, there is no externalId here: the AWS Identity Store
// CreateUser/CreateGroup APIs do not accept one. So the *natural key* is the match
// key on both sides:
//   - users  : PrimaryEmail  (== Identity Store userName)
//   - groups : DisplayName
//
// IDs from each system (GoogleID, ICUserID/ICGroupID) are opaque and only used to
// issue updates/deletes/memberships against that system.
package model

import (
	"fmt"
	"sort"
	"strings"
)

// User is a person to be provisioned into IAM Identity Center.
type User struct {
	GoogleID     string // immutable Google directory id (reference/logging only)
	PrimaryEmail string // natural key; maps to Identity Store userName
	GivenName    string
	FamilyName   string
	DisplayName  string

	// Extended attributes, mapped best-effort from Google and written to Identity
	// Center where the API supports them. On update we replace these only when the
	// desired value is present — we never emit a clearing operation (see
	// internal/identitycenter.attrOps), so an empty desired value is not a change.
	Title             string
	PreferredLanguage string
	PhoneNumbers      []PhoneNumber
	Addresses         []Address

	ICUserID string // Identity Store UserId, set when read from / created in IC
}

// PhoneNumber mirrors the Identity Store (and Google) multi-valued phone attribute.
type PhoneNumber struct {
	Value   string
	Type    string
	Primary bool
}

// Address mirrors the Identity Store (and Google) multi-valued address attribute.
type Address struct {
	Formatted     string
	StreetAddress string
	Locality      string
	Region        string
	PostalCode    string
	Country       string
	Type          string
	Primary       bool
}

// Key returns the case-folded natural key used to match users across systems.
func (u User) Key() string { return strings.ToLower(strings.TrimSpace(u.PrimaryEmail)) }

// EffectiveDisplayName is the display name actually written to Identity Center: the
// Google full name, else "Given Family", else the email. It lives here so the writer
// and change detection agree on the same value (otherwise a user with no Google
// FullName would be flagged changed on every run).
func (u User) EffectiveDisplayName() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	if dn := strings.TrimSpace(u.GivenName + " " + u.FamilyName); dn != "" {
		return dn
	}
	return u.PrimaryEmail
}

// Group is a group to be provisioned into IAM Identity Center.
type Group struct {
	GoogleID    string
	DisplayName string // natural key
	Description string

	ICGroupID  string   // Identity Store GroupId, set when read from / created in IC
	MemberKeys []string // user keys (lower-cased primary emails) of USER members
}

// Key returns the case-folded natural key used to match groups across systems.
func (g Group) Key() string { return strings.ToLower(strings.TrimSpace(g.DisplayName)) }

// Directory is a point-in-time snapshot of one side (Google desired state, or the
// current IAM Identity Center state).
type Directory struct {
	Users  map[string]User  // keyed by User.Key()
	Groups map[string]Group // keyed by Group.Key()
}

// NewDirectory returns an empty, initialised Directory.
func NewDirectory() Directory {
	return Directory{Users: map[string]User{}, Groups: map[string]Group{}}
}

// UserChanged reports whether the desired user's mutable attributes differ from the
// current one (ignores IDs and email, the match key). Optional extended attributes are
// compared only when the desired side provides them: because the writer never clears an
// attribute, an empty desired value is intentionally not treated as a change (otherwise
// it would flip "changed" forever without ever converging).
func UserChanged(desired, current User) bool {
	if desired.GivenName != current.GivenName ||
		desired.FamilyName != current.FamilyName ||
		desired.EffectiveDisplayName() != current.EffectiveDisplayName() {
		return true
	}
	if desired.Title != "" && desired.Title != current.Title {
		return true
	}
	if desired.PreferredLanguage != "" && desired.PreferredLanguage != current.PreferredLanguage {
		return true
	}
	if len(desired.PhoneNumbers) > 0 && phonesKey(desired.PhoneNumbers) != phonesKey(current.PhoneNumbers) {
		return true
	}
	if len(desired.Addresses) > 0 && addressesKey(desired.Addresses) != addressesKey(current.Addresses) {
		return true
	}
	return false
}

// phonesKey / addressesKey produce order-independent canonical strings so reordering by
// either side is not mistaken for a change.
func phonesKey(ps []PhoneNumber) string {
	parts := make([]string, 0, len(ps))
	for _, p := range ps {
		parts = append(parts, fmt.Sprintf("%s|%s|%t", strings.TrimSpace(p.Type), strings.TrimSpace(p.Value), p.Primary))
	}
	sort.Strings(parts)
	return strings.Join(parts, ";")
}

func addressesKey(as []Address) string {
	parts := make([]string, 0, len(as))
	for _, a := range as {
		parts = append(parts, strings.Join([]string{
			strings.TrimSpace(a.Type), strings.TrimSpace(a.Formatted), strings.TrimSpace(a.StreetAddress),
			strings.TrimSpace(a.Locality), strings.TrimSpace(a.Region), strings.TrimSpace(a.PostalCode),
			strings.TrimSpace(a.Country), fmt.Sprintf("%t", a.Primary),
		}, "|"))
	}
	sort.Strings(parts)
	return strings.Join(parts, ";")
}
