package sync

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/kgns/gws-identitycenter-sync/internal/model"
	"github.com/kgns/gws-identitycenter-sync/internal/state"
)

// fakeTarget records mutations and serves a fixed current state.
type fakeTarget struct {
	current model.Directory

	createdUsers, deletedUsers, updatedUsers, renamedUsers []string
	createdGroups, deletedGroups, renamedGroups            []string
	added, removed                                         []string // "group/user"
}

func (f *fakeTarget) Fetch(context.Context) (model.Directory, error) { return f.current, nil }
func (f *fakeTarget) CreateUser(_ context.Context, u model.User) (string, error) {
	f.createdUsers = append(f.createdUsers, u.Key())
	return "uid-" + u.Key(), nil
}
func (f *fakeTarget) UpdateUser(_ context.Context, _ string, u model.User) error {
	f.updatedUsers = append(f.updatedUsers, u.Key())
	return nil
}
func (f *fakeTarget) RenameUser(_ context.Context, _ string, u model.User) error {
	f.renamedUsers = append(f.renamedUsers, u.Key())
	return nil
}
func (f *fakeTarget) DeleteUser(_ context.Context, _, email string) error {
	f.deletedUsers = append(f.deletedUsers, email)
	return nil
}
func (f *fakeTarget) CreateGroup(_ context.Context, g model.Group) (string, error) {
	f.createdGroups = append(f.createdGroups, g.Key())
	return "gid-" + g.Key(), nil
}
func (f *fakeTarget) RenameGroup(_ context.Context, _, name string) error {
	f.renamedGroups = append(f.renamedGroups, name)
	return nil
}
func (f *fakeTarget) DeleteGroup(_ context.Context, _, name string) error {
	f.deletedGroups = append(f.deletedGroups, name)
	return nil
}
func (f *fakeTarget) AddMember(_ context.Context, _, _, groupName, email string) error {
	f.added = append(f.added, groupName+"/"+email)
	return nil
}
func (f *fakeTarget) RemoveMember(_ context.Context, _, _, groupName, email string) error {
	f.removed = append(f.removed, groupName+"/"+email)
	return nil
}

type fakeDesired struct{ dir model.Directory }

func (f fakeDesired) Fetch(context.Context) (model.Directory, error) { return f.dir, nil }

// fakeStore serves a preset state and captures what gets saved.
type fakeStore struct {
	prev  state.State
	saved state.State
}

func (f *fakeStore) Load(context.Context) (state.State, error) { return f.prev, nil }
func (f *fakeStore) Save(_ context.Context, s state.State) error {
	f.saved = s
	return nil
}

func newLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func dirWith(users []model.User, groups []model.Group) model.Directory {
	d := model.NewDirectory()
	for _, u := range users {
		d.Users[u.Key()] = u
	}
	for _, g := range groups {
		d.Groups[g.Key()] = g
	}
	return d
}

func TestProvisionFromEmpty(t *testing.T) {
	desired := fakeDesired{dir: dirWith(
		[]model.User{{PrimaryEmail: "alice@x.com", GivenName: "Alice"}},
		[]model.Group{{DisplayName: "iam-admins", MemberKeys: []string{"alice@x.com"}}},
	)}
	tgt := &fakeTarget{current: model.NewDirectory()}

	st, err := NewEngine(desired, tgt, nil, "iam-", false, false, newLogger()).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.UsersCreated != 1 || st.GroupsCreated != 1 || st.MembershipsAdded != 1 {
		t.Fatalf("unexpected stats: %+v", st)
	}
	if len(tgt.added) != 1 || tgt.added[0] != "iam-admins/alice@x.com" {
		t.Fatalf("membership not added correctly: %v", tgt.added)
	}
}

func TestRemoveMembershipAndPrune(t *testing.T) {
	current := dirWith(
		[]model.User{
			{PrimaryEmail: "alice@x.com", ICUserID: "u-alice"},
			{PrimaryEmail: "bob@x.com", ICUserID: "u-bob"},
		},
		[]model.Group{{DisplayName: "iam-admins", ICGroupID: "g1", MemberKeys: []string{"alice@x.com", "bob@x.com"}}},
	)
	desired := fakeDesired{dir: dirWith(
		[]model.User{{PrimaryEmail: "alice@x.com"}},
		[]model.Group{{DisplayName: "iam-admins", MemberKeys: []string{"alice@x.com"}}},
	)}
	tgt := &fakeTarget{current: current}

	st, err := NewEngine(desired, tgt, nil, "iam-", true, false, newLogger()).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.MembershipsRemoved != 1 || tgt.removed[0] != "iam-admins/bob@x.com" {
		t.Fatalf("bob membership not removed: %+v %v", st, tgt.removed)
	}
	if st.UsersDeleted != 1 || tgt.deletedUsers[0] != "bob@x.com" {
		t.Fatalf("bob not pruned: %+v %v", st, tgt.deletedUsers)
	}
	if st.UsersCreated != 0 || st.GroupsCreated != 0 {
		t.Fatalf("unexpected creates: %+v", st)
	}
}

func TestDeleteOrphanGroup(t *testing.T) {
	current := dirWith(
		[]model.User{{PrimaryEmail: "carol@x.com", ICUserID: "u-carol"}},
		[]model.Group{{DisplayName: "iam-old", ICGroupID: "g2", MemberKeys: []string{"carol@x.com"}}},
	)
	tgt := &fakeTarget{current: current}

	st, err := NewEngine(fakeDesired{dir: model.NewDirectory()}, tgt, nil, "iam-", false, false, newLogger()).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.GroupsDeleted != 1 || tgt.deletedGroups[0] != "iam-old" {
		t.Fatalf("orphan group not deleted: %+v %v", st, tgt.deletedGroups)
	}
	if st.MembershipsRemoved != 1 {
		t.Fatalf("orphan group membership not removed first: %+v", st)
	}
	if st.UsersDeleted != 0 { // prune disabled
		t.Fatalf("user should not be pruned when prune=false: %+v", st)
	}
}

func TestPrefixUnsetSkipsDeletes(t *testing.T) {
	current := dirWith(nil, []model.Group{{DisplayName: "iam-old", ICGroupID: "g2"}})
	tgt := &fakeTarget{current: current}

	st, err := NewEngine(fakeDesired{dir: model.NewDirectory()}, tgt, nil, "", true, false, newLogger()).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.GroupsDeleted != 0 || len(tgt.deletedGroups) != 0 {
		t.Fatalf("no deletes expected without prefix: %+v", st)
	}
}

// The crux of the state feature: an email change for a user mapped in state becomes an
// in-place rename (preserving the IC UserId) — NOT delete+recreate — and causes no
// membership churn.
func TestStateRenameNoChurn(t *testing.T) {
	current := dirWith(
		[]model.User{{PrimaryEmail: "old@x.com", ICUserID: "u1"}},
		[]model.Group{{DisplayName: "iam-team", ICGroupID: "g1", MemberKeys: []string{"old@x.com"}}},
	)
	desired := fakeDesired{dir: dirWith(
		[]model.User{{GoogleID: "G1", PrimaryEmail: "new@x.com", GivenName: "N"}},
		[]model.Group{{GoogleID: "GG1", DisplayName: "iam-team", MemberKeys: []string{"new@x.com"}}},
	)}
	store := &fakeStore{prev: state.State{
		Users:  map[string]state.UserRecord{"G1": {ICUserID: "u1", Email: "old@x.com"}},
		Groups: map[string]state.GroupRecord{"GG1": {ICGroupID: "g1", DisplayName: "iam-team"}},
	}}
	tgt := &fakeTarget{current: current}

	st, err := NewEngine(desired, tgt, store, "iam-", false, false, newLogger()).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.UsersRenamed != 1 || len(tgt.renamedUsers) != 1 || tgt.renamedUsers[0] != "new@x.com" {
		t.Fatalf("expected in-place rename to new@x.com: %+v renamed=%v", st, tgt.renamedUsers)
	}
	if st.UsersCreated != 0 || len(tgt.createdUsers) != 0 || st.UsersDeleted != 0 {
		t.Fatalf("rename must not create or delete the user: %+v", st)
	}
	if st.MembershipsAdded != 0 || st.MembershipsRemoved != 0 {
		t.Fatalf("rename must not churn memberships: %+v added=%v removed=%v", st, tgt.added, tgt.removed)
	}
	if rec := store.saved.Users["G1"]; rec.ICUserID != "u1" || rec.Email != "new@x.com" {
		t.Fatalf("saved state should map G1->u1 with new email: %+v", store.saved.Users)
	}
}
