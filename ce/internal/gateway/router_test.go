package gateway

import (
	"strings"
	"testing"
)

func TestTableAddConflictFailsFast(t *testing.T) {
	tb := NewTable()
	if err := tb.Add(Route{Prefix: "/v1/admin"}); err != nil {
		t.Fatalf("first add: %v", err)
	}
	err := tb.Add(Route{Prefix: "/v1/admin"})
	if err == nil {
		t.Fatal("duplicate prefix accepted, want fail-fast error")
	}
	if !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("error %q does not mention the conflict", err)
	}
}

func TestTableAddRejectsInvalidPrefix(t *testing.T) {
	for _, p := range []string{"", "/", "v1/admin", "/v1/admin/"} {
		err := NewTable().Add(Route{Prefix: p})
		if err == nil {
			t.Errorf("prefix %q accepted, want error", p)
		}
	}
}

func TestTableMatchLongestPrefixWins(t *testing.T) {
	tb := NewTable()
	if err := tb.Add(Route{Prefix: "/v1/devices", Backend: "http://a"}); err != nil {
		t.Fatal(err)
	}
	if err := tb.Add(Route{Prefix: "/v1/devices/tunnel", Backend: "http://b", WSS: true}); err != nil {
		t.Fatal(err)
	}

	r, ok := tb.Match("/v1/devices/tunnel")
	if !ok || r.Prefix != "/v1/devices/tunnel" || !r.WSS {
		t.Fatalf("Match(/v1/devices/tunnel) = %+v, %v; want the tunnel route", r, ok)
	}
	r, ok = tb.Match("/v1/devices/1/tools")
	if !ok || r.Prefix != "/v1/devices" {
		t.Fatalf("Match(/v1/devices/1/tools) = %+v, %v; want the /v1/devices route", r, ok)
	}
}

func TestTableMatchSegmentAware(t *testing.T) {
	tb := NewTable()
	if err := tb.Add(Route{Prefix: "/v1/admin"}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path string
		want bool
	}{
		{"/v1/admin", true},
		{"/v1/admin/users", true},
		{"/v1/administrator", false},
		{"/v1/adm", false},
		{"/v1/administer", false},
		{"/", false},
		{"/v2/agents", false},
		{"/v1/developer", false},
	}
	for _, c := range cases {
		_, ok := tb.Match(c.path)
		if ok != c.want {
			t.Errorf("Match(%q) matched=%v, want %v", c.path, ok, c.want)
		}
	}
}
