package google

import (
	"io"
	"log/slog"
	"testing"
)

func testClient() *Client { return &Client{log: slog.New(slog.NewTextHandler(io.Discard, nil))} }

// Directory API collection attributes arrive as decoded JSON (interface{} of
// []interface{} / map[string]interface{}); the tests feed that exact shape.

func TestPickTitlePrimaryWins(t *testing.T) {
	orgs := []interface{}{
		map[string]interface{}{"title": "Engineer", "primary": false},
		map[string]interface{}{"title": "Manager", "primary": true},
	}
	if got := testClient().pickTitle(orgs); got != "Manager" {
		t.Fatalf("want Manager, got %q", got)
	}
}

func TestPickTitleFallsBackToFirstNonEmpty(t *testing.T) {
	orgs := []interface{}{
		map[string]interface{}{"title": "", "primary": true},
		map[string]interface{}{"title": "Engineer"},
	}
	if got := testClient().pickTitle(orgs); got != "Engineer" {
		t.Fatalf("want Engineer, got %q", got)
	}
}

func TestPickLanguagePreferred(t *testing.T) {
	langs := []interface{}{
		map[string]interface{}{"languageCode": "fr", "preference": "not_preferred"},
		map[string]interface{}{"languageCode": "en", "preference": "preferred"},
	}
	if got := testClient().pickLanguage(langs); got != "en" {
		t.Fatalf("want en, got %q", got)
	}
}

func TestPhoneNumbersParsedWithCustomTypeAndEmptyDropped(t *testing.T) {
	phones := []interface{}{
		map[string]interface{}{"value": "+1 555", "type": "work", "primary": true},
		map[string]interface{}{"value": "+1 777", "type": "custom", "customType": "satellite"},
		map[string]interface{}{"value": "", "type": "home"}, // dropped: empty value
	}
	got := testClient().phoneNumbers(phones)
	if len(got) != 2 {
		t.Fatalf("want 2 phones, got %d (%+v)", len(got), got)
	}
	if got[0].Type != "work" || !got[0].Primary {
		t.Fatalf("phone[0] wrong: %+v", got[0])
	}
	if got[1].Type != "satellite" { // custom type resolved
		t.Fatalf("custom type not resolved: %+v", got[1])
	}
}

func TestAddressesParsed(t *testing.T) {
	addrs := []interface{}{
		map[string]interface{}{"streetAddress": "1 Main", "locality": "Town", "country": "US", "primary": true},
	}
	got := testClient().addresses(addrs)
	if len(got) != 1 || got[0].StreetAddress != "1 Main" || got[0].Country != "US" || !got[0].Primary {
		t.Fatalf("address parse wrong: %+v", got)
	}
}

func TestNilCollectionsYieldEmpty(t *testing.T) {
	c := testClient()
	if c.phoneNumbers(nil) != nil || c.addresses(nil) != nil || c.pickTitle(nil) != "" || c.pickLanguage(nil) != "" {
		t.Fatal("nil interface should yield empty results")
	}
}
