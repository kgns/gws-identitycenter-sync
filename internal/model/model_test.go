package model

import "testing"

func TestEffectiveDisplayName(t *testing.T) {
	cases := []struct {
		name string
		u    User
		want string
	}{
		{"full name wins", User{DisplayName: "Full Name", GivenName: "G"}, "Full Name"},
		{"given+family fallback", User{GivenName: "John", FamilyName: "Doe"}, "John Doe"},
		{"email last resort", User{PrimaryEmail: "a@x.com"}, "a@x.com"},
	}
	for _, c := range cases {
		if got := c.u.EffectiveDisplayName(); got != c.want {
			t.Errorf("%s: want %q, got %q", c.name, c.want, got)
		}
	}
}

func TestUserChangedExtended(t *testing.T) {
	cur := User{
		GivenName: "J", FamilyName: "D", DisplayName: "J D",
		Title:        "Eng",
		PhoneNumbers: []PhoneNumber{{Value: "1", Type: "work"}},
	}

	if UserChanged(cur, cur) {
		t.Fatal("identical users should not be changed")
	}

	d := cur
	d.Title = "Manager"
	if !UserChanged(d, cur) {
		t.Fatal("title change should be detected")
	}

	// Empty desired value must NOT be treated as a change (we never clear attributes).
	d = cur
	d.Title = ""
	if UserChanged(d, cur) {
		t.Fatal("empty desired title must not be a change")
	}

	// Reordered/identical phones must not churn (canonical key is order-independent).
	d = cur
	d.PhoneNumbers = []PhoneNumber{{Value: "1", Type: "work"}}
	if UserChanged(d, cur) {
		t.Fatal("same phones should not be a change")
	}

	d = cur
	d.PhoneNumbers = []PhoneNumber{{Value: "2", Type: "work"}}
	if !UserChanged(d, cur) {
		t.Fatal("phone value change should be detected")
	}
}
