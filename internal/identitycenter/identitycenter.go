// Package identitycenter is the AWS write/read backend: it reads and mutates users,
// groups, and memberships in IAM Identity Center via the Identity Store API
// (identitystore:*) using normal IAM/SigV4 auth. No SCIM endpoint, no bearer token.
package identitycenter

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/identitystore"
	"github.com/aws/aws-sdk-go-v2/service/identitystore/document"
	"github.com/aws/aws-sdk-go-v2/service/identitystore/types"

	"github.com/kgns/gws-identitycenter-sync/internal/model"
)

// API is the subset of the Identity Store client we use (interface for testability).
type API interface {
	ListUsers(context.Context, *identitystore.ListUsersInput, ...func(*identitystore.Options)) (*identitystore.ListUsersOutput, error)
	ListGroups(context.Context, *identitystore.ListGroupsInput, ...func(*identitystore.Options)) (*identitystore.ListGroupsOutput, error)
	ListGroupMemberships(context.Context, *identitystore.ListGroupMembershipsInput, ...func(*identitystore.Options)) (*identitystore.ListGroupMembershipsOutput, error)
	GetGroupMembershipId(context.Context, *identitystore.GetGroupMembershipIdInput, ...func(*identitystore.Options)) (*identitystore.GetGroupMembershipIdOutput, error)
	CreateUser(context.Context, *identitystore.CreateUserInput, ...func(*identitystore.Options)) (*identitystore.CreateUserOutput, error)
	UpdateUser(context.Context, *identitystore.UpdateUserInput, ...func(*identitystore.Options)) (*identitystore.UpdateUserOutput, error)
	DeleteUser(context.Context, *identitystore.DeleteUserInput, ...func(*identitystore.Options)) (*identitystore.DeleteUserOutput, error)
	CreateGroup(context.Context, *identitystore.CreateGroupInput, ...func(*identitystore.Options)) (*identitystore.CreateGroupOutput, error)
	UpdateGroup(context.Context, *identitystore.UpdateGroupInput, ...func(*identitystore.Options)) (*identitystore.UpdateGroupOutput, error)
	DeleteGroup(context.Context, *identitystore.DeleteGroupInput, ...func(*identitystore.Options)) (*identitystore.DeleteGroupOutput, error)
	CreateGroupMembership(context.Context, *identitystore.CreateGroupMembershipInput, ...func(*identitystore.Options)) (*identitystore.CreateGroupMembershipOutput, error)
	DeleteGroupMembership(context.Context, *identitystore.DeleteGroupMembershipInput, ...func(*identitystore.Options)) (*identitystore.DeleteGroupMembershipOutput, error)
}

type Client struct {
	api     API
	storeID string
	dryRun  bool
	log     *slog.Logger
}

func New(api API, storeID string, dryRun bool, log *slog.Logger) *Client {
	return &Client{api: api, storeID: storeID, dryRun: dryRun, log: log}
}

// Fetch reads the full current state of Identity Center into a model.Directory.
func (c *Client) Fetch(ctx context.Context) (model.Directory, error) {
	dir := model.NewDirectory()

	// Users — also build UserId -> userKey for membership resolution.
	byUserID := map[string]string{}
	up := identitystore.NewListUsersPaginator(c.api, &identitystore.ListUsersInput{IdentityStoreId: &c.storeID})
	for up.HasMorePages() {
		page, err := up.NextPage(ctx)
		if err != nil {
			return dir, fmt.Errorf("ListUsers: %w", err)
		}
		for _, u := range page.Users {
			m := userFromIC(u)
			if m.Key() == "" {
				continue
			}
			dir.Users[m.Key()] = m
			byUserID[aws.ToString(u.UserId)] = m.Key()
		}
	}

	// Groups.
	gp := identitystore.NewListGroupsPaginator(c.api, &identitystore.ListGroupsInput{IdentityStoreId: &c.storeID})
	for gp.HasMorePages() {
		page, err := gp.NextPage(ctx)
		if err != nil {
			return dir, fmt.Errorf("ListGroups: %w", err)
		}
		for _, g := range page.Groups {
			grp := model.Group{
				DisplayName: aws.ToString(g.DisplayName),
				Description: aws.ToString(g.Description),
				ICGroupID:   aws.ToString(g.GroupId),
			}
			if grp.Key() == "" {
				continue
			}
			dir.Groups[grp.Key()] = grp
		}
	}

	// Memberships per group.
	for key, g := range dir.Groups {
		mp := identitystore.NewListGroupMembershipsPaginator(c.api, &identitystore.ListGroupMembershipsInput{
			IdentityStoreId: &c.storeID,
			GroupId:         &g.ICGroupID,
		})
		for mp.HasMorePages() {
			page, err := mp.NextPage(ctx)
			if err != nil {
				return dir, fmt.Errorf("ListGroupMemberships(%s): %w", g.DisplayName, err)
			}
			for _, mem := range page.GroupMemberships {
				if userKey, ok := byUserID[memberUserID(mem.MemberId)]; ok {
					g.MemberKeys = append(g.MemberKeys, userKey)
				}
			}
		}
		dir.Groups[key] = g
	}

	return dir, nil
}

// --- mutations -------------------------------------------------------------------

// CreateUser creates u and returns its new Identity Store UserId.
func (c *Client) CreateUser(ctx context.Context, u model.User) (string, error) {
	c.log.Info("create user", "email", u.PrimaryEmail, "dryRun", c.dryRun)
	if c.dryRun {
		return "", nil
	}
	in := &identitystore.CreateUserInput{
		IdentityStoreId: &c.storeID,
		UserName:        aws.String(u.PrimaryEmail),
		DisplayName:     aws.String(u.EffectiveDisplayName()),
		Name: &types.Name{
			GivenName:  aws.String(u.GivenName),
			FamilyName: aws.String(u.FamilyName),
			Formatted:  aws.String(u.EffectiveDisplayName()),
		},
		Emails: []types.Email{{Value: aws.String(u.PrimaryEmail), Primary: true, Type: aws.String("work")}},
	}
	if u.Title != "" {
		in.Title = aws.String(u.Title)
	}
	if u.PreferredLanguage != "" {
		in.PreferredLanguage = aws.String(u.PreferredLanguage)
	}
	if ph := toICPhones(u.PhoneNumbers); len(ph) > 0 {
		in.PhoneNumbers = ph
	}
	if ad := toICAddresses(u.Addresses); len(ad) > 0 {
		in.Addresses = ad
	}
	out, err := c.api.CreateUser(ctx, in)
	if err != nil {
		return "", fmt.Errorf("CreateUser(%s): %w", u.PrimaryEmail, err)
	}
	return aws.ToString(out.UserId), nil
}

// UpdateUser pushes changed display/name attributes for an existing user.
func (c *Client) UpdateUser(ctx context.Context, userID string, u model.User) error {
	c.log.Info("update user", "email", u.PrimaryEmail, "dryRun", c.dryRun)
	if c.dryRun {
		return nil
	}
	return c.updateUser(ctx, userID, attrOps(u, false))
}

// RenameUser changes a user's userName (login = email) in place, preserving the
// Identity Store UserId and any direct account assignments — the whole point of the
// state file. NOTE: if the API rejects a userName change, this surfaces the error
// rather than silently delete+recreate (which would drop direct assignments). The
// emails list is intentionally left as-is; we key on userName, so it stays consistent.
func (c *Client) RenameUser(ctx context.Context, userID string, u model.User) error {
	c.log.Warn("rename user (email change)", "newEmail", u.PrimaryEmail, "icUserId", userID, "dryRun", c.dryRun)
	if c.dryRun {
		return nil
	}
	return c.updateUser(ctx, userID, attrOps(u, true))
}

func (c *Client) updateUser(ctx context.Context, userID string, ops []types.AttributeOperation) error {
	_, err := c.api.UpdateUser(ctx, &identitystore.UpdateUserInput{
		IdentityStoreId: &c.storeID,
		UserId:          &userID,
		Operations:      ops,
	})
	if err != nil {
		return fmt.Errorf("UpdateUser(%s): %w", userID, err)
	}
	return nil
}

func (c *Client) DeleteUser(ctx context.Context, userID, email string) error {
	c.log.Warn("delete user", "email", email, "dryRun", c.dryRun)
	if c.dryRun {
		return nil
	}
	_, err := c.api.DeleteUser(ctx, &identitystore.DeleteUserInput{IdentityStoreId: &c.storeID, UserId: &userID})
	if err != nil {
		return fmt.Errorf("DeleteUser(%s): %w", email, err)
	}
	return nil
}

// CreateGroup creates g and returns its new Identity Store GroupId.
func (c *Client) CreateGroup(ctx context.Context, g model.Group) (string, error) {
	c.log.Info("create group", "name", g.DisplayName, "dryRun", c.dryRun)
	if c.dryRun {
		return "", nil
	}
	in := &identitystore.CreateGroupInput{
		IdentityStoreId: &c.storeID,
		DisplayName:     aws.String(g.DisplayName),
	}
	if g.Description != "" {
		in.Description = aws.String(g.Description)
	}
	out, err := c.api.CreateGroup(ctx, in)
	if err != nil {
		return "", fmt.Errorf("CreateGroup(%s): %w", g.DisplayName, err)
	}
	return aws.ToString(out.GroupId), nil
}

// RenameGroup changes a group's displayName in place, preserving its GroupId.
func (c *Client) RenameGroup(ctx context.Context, groupID, newName string) error {
	c.log.Warn("rename group", "newName", newName, "icGroupId", groupID, "dryRun", c.dryRun)
	if c.dryRun {
		return nil
	}
	_, err := c.api.UpdateGroup(ctx, &identitystore.UpdateGroupInput{
		IdentityStoreId: &c.storeID,
		GroupId:         &groupID,
		Operations: []types.AttributeOperation{
			{AttributePath: aws.String("displayName"), AttributeValue: document.NewLazyDocument(newName)},
		},
	})
	if err != nil {
		return fmt.Errorf("UpdateGroup(%s): %w", groupID, err)
	}
	return nil
}

func (c *Client) DeleteGroup(ctx context.Context, groupID, name string) error {
	c.log.Warn("delete group", "name", name, "dryRun", c.dryRun)
	if c.dryRun {
		return nil
	}
	_, err := c.api.DeleteGroup(ctx, &identitystore.DeleteGroupInput{IdentityStoreId: &c.storeID, GroupId: &groupID})
	if err != nil {
		return fmt.Errorf("DeleteGroup(%s): %w", name, err)
	}
	return nil
}

func (c *Client) AddMember(ctx context.Context, groupID, userID, groupName, email string) error {
	c.log.Info("add membership", "group", groupName, "email", email, "dryRun", c.dryRun)
	if c.dryRun {
		return nil
	}
	_, err := c.api.CreateGroupMembership(ctx, &identitystore.CreateGroupMembershipInput{
		IdentityStoreId: &c.storeID,
		GroupId:         &groupID,
		MemberId:        &types.MemberIdMemberUserId{Value: userID},
	})
	if err != nil {
		return fmt.Errorf("CreateGroupMembership(%s/%s): %w", groupName, email, err)
	}
	return nil
}

// RemoveMember resolves the membership id for (groupID, userID) via the API, then
// deletes it. Keying on the stable Identity Store ids (not email) keeps removals
// correct across renames.
func (c *Client) RemoveMember(ctx context.Context, groupID, userID, groupName, email string) error {
	c.log.Info("remove membership", "group", groupName, "email", email, "dryRun", c.dryRun)
	if c.dryRun {
		return nil
	}
	got, err := c.api.GetGroupMembershipId(ctx, &identitystore.GetGroupMembershipIdInput{
		IdentityStoreId: &c.storeID,
		GroupId:         &groupID,
		MemberId:        &types.MemberIdMemberUserId{Value: userID},
	})
	if err != nil {
		return fmt.Errorf("GetGroupMembershipId(%s/%s): %w", groupName, email, err)
	}
	_, err = c.api.DeleteGroupMembership(ctx, &identitystore.DeleteGroupMembershipInput{
		IdentityStoreId: &c.storeID,
		MembershipId:    got.MembershipId,
	})
	if err != nil {
		return fmt.Errorf("DeleteGroupMembership(%s/%s): %w", groupName, email, err)
	}
	return nil
}

// --- helpers ---------------------------------------------------------------------

// attrOps builds the UpdateUser operations. With rename=true it also replaces userName.
//
// Optional extended attributes are emitted only when the desired value is present: we
// never send a clearing op (empty value / nil AttributeValue), which avoids
// ValidationExceptions and the update churn that an unclearable attribute would cause.
// The trade-off is that an attribute removed in Google is not cleared in Identity Center.
func attrOps(u model.User, rename bool) []types.AttributeOperation {
	ops := []types.AttributeOperation{
		{AttributePath: aws.String("displayName"), AttributeValue: document.NewLazyDocument(u.EffectiveDisplayName())},
		{AttributePath: aws.String("name.givenName"), AttributeValue: document.NewLazyDocument(u.GivenName)},
		{AttributePath: aws.String("name.familyName"), AttributeValue: document.NewLazyDocument(u.FamilyName)},
		{AttributePath: aws.String("name.formatted"), AttributeValue: document.NewLazyDocument(u.EffectiveDisplayName())},
	}
	if u.Title != "" {
		ops = append(ops, types.AttributeOperation{AttributePath: aws.String("title"), AttributeValue: document.NewLazyDocument(u.Title)})
	}
	if u.PreferredLanguage != "" {
		ops = append(ops, types.AttributeOperation{AttributePath: aws.String("preferredLanguage"), AttributeValue: document.NewLazyDocument(u.PreferredLanguage)})
	}
	if ph := phoneDocs(u.PhoneNumbers); len(ph) > 0 {
		ops = append(ops, types.AttributeOperation{AttributePath: aws.String("phoneNumbers"), AttributeValue: document.NewLazyDocument(ph)})
	}
	if ad := addressDocs(u.Addresses); len(ad) > 0 {
		ops = append(ops, types.AttributeOperation{AttributePath: aws.String("addresses"), AttributeValue: document.NewLazyDocument(ad)})
	}
	if rename {
		ops = append(ops, types.AttributeOperation{
			AttributePath: aws.String("userName"), AttributeValue: document.NewLazyDocument(u.PrimaryEmail),
		})
	}
	return ops
}

func userFromIC(u types.User) model.User {
	m := model.User{
		ICUserID:          aws.ToString(u.UserId),
		PrimaryEmail:      aws.ToString(u.UserName),
		DisplayName:       aws.ToString(u.DisplayName),
		Title:             aws.ToString(u.Title),
		PreferredLanguage: aws.ToString(u.PreferredLanguage),
	}
	if u.Name != nil {
		m.GivenName = aws.ToString(u.Name.GivenName)
		m.FamilyName = aws.ToString(u.Name.FamilyName)
	}
	for _, p := range u.PhoneNumbers {
		m.PhoneNumbers = append(m.PhoneNumbers, model.PhoneNumber{
			Value: aws.ToString(p.Value), Type: aws.ToString(p.Type), Primary: p.Primary,
		})
	}
	for _, a := range u.Addresses {
		m.Addresses = append(m.Addresses, model.Address{
			Formatted: aws.ToString(a.Formatted), StreetAddress: aws.ToString(a.StreetAddress),
			Locality: aws.ToString(a.Locality), Region: aws.ToString(a.Region),
			PostalCode: aws.ToString(a.PostalCode), Country: aws.ToString(a.Country),
			Type: aws.ToString(a.Type), Primary: a.Primary,
		})
	}
	if m.PrimaryEmail == "" { // fall back to a primary email if userName is unset
		for _, e := range u.Emails {
			if e.Primary {
				m.PrimaryEmail = aws.ToString(e.Value)
				break
			}
		}
	}
	return m
}

func memberUserID(m types.MemberId) string {
	if uid, ok := m.(*types.MemberIdMemberUserId); ok {
		return uid.Value
	}
	return ""
}

// toICPhones / toICAddresses build the typed values for CreateUser.
func toICPhones(ps []model.PhoneNumber) []types.PhoneNumber {
	out := make([]types.PhoneNumber, 0, len(ps))
	for _, p := range ps {
		ph := types.PhoneNumber{Value: aws.String(p.Value), Primary: p.Primary}
		if p.Type != "" {
			ph.Type = aws.String(p.Type)
		}
		out = append(out, ph)
	}
	return out
}

func toICAddresses(as []model.Address) []types.Address {
	out := make([]types.Address, 0, len(as))
	for _, a := range as {
		ad := types.Address{Primary: a.Primary}
		if a.Formatted != "" {
			ad.Formatted = aws.String(a.Formatted)
		}
		if a.StreetAddress != "" {
			ad.StreetAddress = aws.String(a.StreetAddress)
		}
		if a.Locality != "" {
			ad.Locality = aws.String(a.Locality)
		}
		if a.Region != "" {
			ad.Region = aws.String(a.Region)
		}
		if a.PostalCode != "" {
			ad.PostalCode = aws.String(a.PostalCode)
		}
		if a.Country != "" {
			ad.Country = aws.String(a.Country)
		}
		if a.Type != "" {
			ad.Type = aws.String(a.Type)
		}
		out = append(out, ad)
	}
	return out
}

// phoneDocs / addressDocs build the JSON-document values for the UpdateUser multi-valued
// AttributeOperations (which take a free-form document, not the typed structs).
func phoneDocs(ps []model.PhoneNumber) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(ps))
	for _, p := range ps {
		m := map[string]interface{}{"value": p.Value, "primary": p.Primary}
		if p.Type != "" {
			m["type"] = p.Type
		}
		out = append(out, m)
	}
	return out
}

func addressDocs(as []model.Address) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(as))
	for _, a := range as {
		m := map[string]interface{}{"primary": a.Primary}
		if a.Formatted != "" {
			m["formatted"] = a.Formatted
		}
		if a.StreetAddress != "" {
			m["streetAddress"] = a.StreetAddress
		}
		if a.Locality != "" {
			m["locality"] = a.Locality
		}
		if a.Region != "" {
			m["region"] = a.Region
		}
		if a.PostalCode != "" {
			m["postalCode"] = a.PostalCode
		}
		if a.Country != "" {
			m["country"] = a.Country
		}
		if a.Type != "" {
			m["type"] = a.Type
		}
		out = append(out, m)
	}
	return out
}
