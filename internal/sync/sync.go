// Package sync is the reconciliation engine: read desired state (Google) and current
// state (Identity Center), then converge current -> desired.
//
// Matching: if a state store (join table) is provided, users/groups are matched by
// Google's immutable id first, so an email / display-name change becomes an in-place
// rename (preserving the Identity Store id and any direct assignments) instead of
// delete+recreate. Without state — or for ids not yet in it — matching falls back to
// the natural key (email / display name).
//
// Ordering is load-bearing:
//  1. create/update/rename users (so memberships can reference them)
//  2. create/rename groups
//  3. reconcile memberships (add missing, remove extra)
//  4. destructive deletes last (groups, then optionally prune users)
package sync

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/kgns/gws-identitycenter-sync/internal/model"
	"github.com/kgns/gws-identitycenter-sync/internal/state"
)

// DesiredSource yields the desired directory (Google Workspace).
type DesiredSource interface {
	Fetch(ctx context.Context) (model.Directory, error)
}

// Target is the system being converged (IAM Identity Center).
type Target interface {
	Fetch(ctx context.Context) (model.Directory, error)
	CreateUser(ctx context.Context, u model.User) (userID string, err error)
	UpdateUser(ctx context.Context, userID string, u model.User) error
	RenameUser(ctx context.Context, userID string, u model.User) error
	DeleteUser(ctx context.Context, userID, email string) error
	CreateGroup(ctx context.Context, g model.Group) (groupID string, err error)
	RenameGroup(ctx context.Context, groupID, newName string) error
	DeleteGroup(ctx context.Context, groupID, name string) error
	AddMember(ctx context.Context, groupID, userID, groupName, email string) error
	RemoveMember(ctx context.Context, groupID, userID, groupName, email string) error
}

type Engine struct {
	desired DesiredSource
	target  Target
	store   state.Store
	prefix  string // case-folded ManagedGroupPrefix
	prune   bool
	dryRun  bool
	log     *slog.Logger
}

func NewEngine(desired DesiredSource, target Target, store state.Store, managedGroupPrefix string, prune, dryRun bool, log *slog.Logger) *Engine {
	if store == nil {
		store = state.Noop{}
	}
	return &Engine{
		desired: desired,
		target:  target,
		store:   store,
		prefix:  strings.ToLower(strings.TrimSpace(managedGroupPrefix)),
		prune:   prune,
		dryRun:  dryRun,
		log:     log,
	}
}

type Stats struct {
	UsersCreated, UsersUpdated, UsersRenamed, UsersDeleted int
	GroupsCreated, GroupsRenamed, GroupsDeleted            int
	MembershipsAdded, MembershipsRemoved                   int
	Errors                                                 int
}

func (e *Engine) Run(ctx context.Context) (Stats, error) {
	var st Stats

	desired, err := e.desired.Fetch(ctx)
	if err != nil {
		return st, fmt.Errorf("fetch desired (Google): %w", err)
	}
	current, err := e.target.Fetch(ctx)
	if err != nil {
		return st, fmt.Errorf("fetch current (Identity Center): %w", err)
	}
	prev, err := e.store.Load(ctx)
	if err != nil {
		return st, fmt.Errorf("load state: %w", err)
	}
	next := state.New()
	e.log.Info("fetched state",
		"desired_users", len(desired.Users), "desired_groups", len(desired.Groups),
		"current_users", len(current.Users), "current_groups", len(current.Groups),
		"state_users", len(prev.Users), "state_groups", len(prev.Groups))

	// Coherence guard: MANAGED_GROUP_PREFIX scopes every delete/prune. If it is set but
	// matches neither a synced group nor an existing IC group it can never scope an
	// operation — a misconfiguration (prefix not aligned with GOOGLE_GROUPS_QUERY), not an
	// intentional teardown (which still leaves matching IC groups to delete). Fail before
	// mutating anything. The len(desired.Groups) guard exempts a run that syncs nothing.
	if e.prefix != "" && len(desired.Groups) > 0 {
		var desiredManaged, currentManaged int
		for key := range desired.Groups {
			if e.managed(key) {
				desiredManaged++
			}
		}
		for key := range current.Groups {
			if e.managed(key) {
				currentManaged++
			}
		}
		if desiredManaged == 0 && currentManaged == 0 {
			return st, fmt.Errorf("MANAGED_GROUP_PREFIX %q matches none of the %d synced groups or any Identity Center group: it cannot scope deletes/prune — align it with GOOGLE_GROUPS_QUERY", e.prefix, len(desired.Groups))
		}
	}

	record := func(err error) {
		if err != nil {
			st.Errors++
			e.log.Error("operation failed", "err", err)
		}
	}

	// Reverse indexes for state-based matching by stable Identity Store id.
	currentUserByID := indexUsersByICID(current)
	currentGroupByID := indexGroupsByICID(current)

	// 1. Users: match (state-first), then create / rename / update.
	userID := map[string]string{} // desired userKey -> ICUserID
	for key, du := range desired.Users {
		matchKey, ok := e.matchUserKey(du, key, prev, current, currentUserByID)
		if !ok {
			id, err := e.target.CreateUser(ctx, du)
			if err != nil {
				record(err)
				continue
			}
			userID[key] = id
			st.UsersCreated++
			recordUser(next, du, id)
			continue
		}

		cu := current.Users[matchKey]
		userID[key] = cu.ICUserID
		recordUser(next, du, cu.ICUserID)

		if matchKey != key { // email changed -> in-place rename
			if err := e.target.RenameUser(ctx, cu.ICUserID, du); err != nil {
				record(err)
				continue
			}
			st.UsersRenamed++
			applyUserRename(current, matchKey, key, du)
		} else if model.UserChanged(du, cu) {
			if err := e.target.UpdateUser(ctx, cu.ICUserID, du); err != nil {
				record(err)
			} else {
				st.UsersUpdated++
			}
		}
	}

	// 2. Groups: match (state-first), then create / rename.
	groupID := map[string]string{} // desired groupKey -> ICGroupID
	for key, dg := range desired.Groups {
		matchKey, ok := e.matchGroupKey(dg, key, prev, current, currentGroupByID)
		if !ok {
			id, err := e.target.CreateGroup(ctx, dg)
			if err != nil {
				record(err)
				continue
			}
			groupID[key] = id
			st.GroupsCreated++
			recordGroup(next, dg, id)
			continue
		}

		cg := current.Groups[matchKey]
		groupID[key] = cg.ICGroupID
		recordGroup(next, dg, cg.ICGroupID)

		if matchKey != key { // display name changed -> in-place rename
			if err := e.target.RenameGroup(ctx, cg.ICGroupID, dg.DisplayName); err != nil {
				record(err)
				continue
			}
			st.GroupsRenamed++
			applyGroupRename(current, matchKey, key, dg)
		}
	}

	// 3. Memberships per desired group (current is now rename-consistent).
	for key, dg := range desired.Groups {
		want := toSet(dg.MemberKeys)
		have := toSet(current.Groups[key].MemberKeys) // empty for newly-created groups
		for uk := range want {
			if have[uk] {
				continue
			}
			uid := userID[uk]
			if uid == "" {
				e.log.Warn("skip membership add: user has no Identity Store id (create failed or dry-run)",
					"group", dg.DisplayName, "user", uk)
				continue
			}
			if err := e.target.AddMember(ctx, groupID[key], uid, dg.DisplayName, uk); err != nil {
				record(err)
			} else {
				st.MembershipsAdded++
			}
		}
		for uk := range have {
			if want[uk] {
				continue
			}
			if err := e.target.RemoveMember(ctx, groupID[key], current.Users[uk].ICUserID, dg.DisplayName, uk); err != nil {
				record(err)
			} else {
				st.MembershipsRemoved++
			}
		}
	}

	// 4. Destructive deletes — only within the managed prefix.
	if e.prefix == "" {
		e.log.Info("MANAGED_GROUP_PREFIX unset: skipping group deletion and user pruning")
	} else {
		e.deletes(ctx, desired, current, &st, record)
	}

	if !e.dryRun {
		if err := e.store.Save(ctx, next); err != nil {
			e.log.Error("failed to save state (next run degrades to natural-key matching)", "err", err)
		}
	}

	return st, nil
}

func (e *Engine) deletes(ctx context.Context, desired, current model.Directory, st *Stats, record func(error)) {
	managedUserKeys := map[string]struct{}{}
	for key, cg := range current.Groups {
		if !e.managed(key) {
			continue
		}
		for _, uk := range cg.MemberKeys {
			managedUserKeys[uk] = struct{}{}
		}
		if _, want := desired.Groups[key]; want {
			continue
		}
		// Group removed from Google: drop its memberships, then delete it.
		for _, uk := range cg.MemberKeys {
			if err := e.target.RemoveMember(ctx, cg.ICGroupID, current.Users[uk].ICUserID, cg.DisplayName, uk); err != nil {
				record(err)
			} else {
				st.MembershipsRemoved++
			}
		}
		if err := e.target.DeleteGroup(ctx, cg.ICGroupID, cg.DisplayName); err != nil {
			record(err)
		} else {
			st.GroupsDeleted++
		}
	}

	if !e.prune {
		return
	}
	for uk := range managedUserKeys {
		if _, ok := desired.Users[uk]; ok {
			continue
		}
		cu := current.Users[uk]
		if err := e.target.DeleteUser(ctx, cu.ICUserID, cu.PrimaryEmail); err != nil {
			record(err)
		} else {
			st.UsersDeleted++
		}
	}
}

// matchUserKey returns the *current* key (email) of the IC user this desired user maps
// to: by Google id via state first, then by natural key. ok=false => create.
func (e *Engine) matchUserKey(du model.User, key string, prev state.State, current model.Directory, byICID map[string]string) (string, bool) {
	if du.GoogleID != "" {
		if rec, ok := prev.Users[du.GoogleID]; ok {
			if curKey, ok := byICID[rec.ICUserID]; ok {
				return curKey, true
			}
		}
	}
	if _, ok := current.Users[key]; ok {
		return key, true
	}
	return "", false
}

func (e *Engine) matchGroupKey(dg model.Group, key string, prev state.State, current model.Directory, byICID map[string]string) (string, bool) {
	if dg.GoogleID != "" {
		if rec, ok := prev.Groups[dg.GoogleID]; ok {
			if curKey, ok := byICID[rec.ICGroupID]; ok {
				return curKey, true
			}
		}
	}
	if _, ok := current.Groups[key]; ok {
		return key, true
	}
	return "", false
}

func (e *Engine) managed(groupKey string) bool { return strings.HasPrefix(groupKey, e.prefix) }

// applyUserRename rewrites the in-memory current state so downstream membership
// reconciliation sees the new key (no spurious churn for the renamed user).
func applyUserRename(current model.Directory, oldKey, newKey string, du model.User) {
	nu := current.Users[oldKey]
	nu.PrimaryEmail = du.PrimaryEmail
	nu.GivenName, nu.FamilyName, nu.DisplayName = du.GivenName, du.FamilyName, du.DisplayName
	delete(current.Users, oldKey)
	current.Users[newKey] = nu
	for gk, g := range current.Groups {
		for i, mk := range g.MemberKeys {
			if mk == oldKey {
				g.MemberKeys[i] = newKey
			}
		}
		current.Groups[gk] = g
	}
}

func applyGroupRename(current model.Directory, oldKey, newKey string, dg model.Group) {
	cg := current.Groups[oldKey]
	cg.DisplayName = dg.DisplayName
	delete(current.Groups, oldKey)
	current.Groups[newKey] = cg
}

func indexUsersByICID(d model.Directory) map[string]string {
	m := make(map[string]string, len(d.Users))
	for key, u := range d.Users {
		if u.ICUserID != "" {
			m[u.ICUserID] = key
		}
	}
	return m
}

func indexGroupsByICID(d model.Directory) map[string]string {
	m := make(map[string]string, len(d.Groups))
	for key, g := range d.Groups {
		if g.ICGroupID != "" {
			m[g.ICGroupID] = key
		}
	}
	return m
}

func recordUser(s state.State, du model.User, icUserID string) {
	if du.GoogleID == "" || icUserID == "" {
		return
	}
	s.Users[du.GoogleID] = state.UserRecord{ICUserID: icUserID, Email: du.PrimaryEmail}
}

func recordGroup(s state.State, dg model.Group, icGroupID string) {
	if dg.GoogleID == "" || icGroupID == "" {
		return
	}
	s.Groups[dg.GoogleID] = state.GroupRecord{ICGroupID: icGroupID, DisplayName: dg.DisplayName}
}

func toSet(keys []string) map[string]bool {
	s := make(map[string]bool, len(keys))
	for _, k := range keys {
		s[k] = true
	}
	return s
}
