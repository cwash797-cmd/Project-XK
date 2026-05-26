package router

import (
	"testing"
)

func TestRouteExactIP(t *testing.T) {
	r := New(nil, nil, nil)
	_ = r.SetRules([]Rule{
		{Match: "192.168.1.5", ClientID: "alice"},
	})

	clientID, ok := r.Route("192.168.1.5", 80)
	if !ok || clientID != "alice" {
		t.Errorf("exact IP: got (%q, %v), want (alice, true)", clientID, ok)
	}

	_, ok = r.Route("192.168.1.6", 80)
	if ok {
		t.Error("non-matching IP should not route")
	}
}

func TestRouteCIDR(t *testing.T) {
	r := New(nil, nil, nil)
	_ = r.SetRules([]Rule{
		{Match: "10.0.0.0/8", ClientID: "bob"},
	})

	for _, ip := range []string{"10.0.0.1", "10.255.255.255", "10.1.2.3"} {
		clientID, ok := r.Route(ip, 443)
		if !ok || clientID != "bob" {
			t.Errorf("CIDR match %s: got (%q, %v), want (bob, true)", ip, clientID, ok)
		}
	}

	_, ok := r.Route("192.168.0.1", 443)
	if ok {
		t.Error("192.168.0.1 should not match 10.0.0.0/8")
	}
}

func TestRouteSuffixGlob(t *testing.T) {
	r := New(nil, nil, nil)
	_ = r.SetRules([]Rule{
		{Match: "*.internal.corp", ClientID: "carol"},
	})

	for _, host := range []string{"api.internal.corp", "db.internal.corp", "svc.api.internal.corp"} {
		clientID, ok := r.Route(host, 80)
		if !ok || clientID != "carol" {
			t.Errorf("suffix match %s: got (%q, %v), want (carol, true)", host, clientID, ok)
		}
	}

	_, ok := r.Route("internal.corp", 80)
	if ok {
		t.Error("bare domain should not match *.internal.corp")
	}

	_, ok = r.Route("evil.other.corp", 80)
	if ok {
		t.Error("evil.other.corp should not match *.internal.corp")
	}
}

func TestRouteExactHost(t *testing.T) {
	r := New(nil, nil, nil)
	_ = r.SetRules([]Rule{
		{Match: "example.com", ClientID: "dave"},
	})

	clientID, ok := r.Route("example.com", 443)
	if !ok || clientID != "dave" {
		t.Errorf("exact host: got (%q, %v), want (dave, true)", clientID, ok)
	}

	clientID, ok = r.Route("EXAMPLE.COM", 443) // case-insensitive
	if !ok || clientID != "dave" {
		t.Errorf("case-insensitive: got (%q, %v), want (dave, true)", clientID, ok)
	}
}

func TestRouteWildcard(t *testing.T) {
	r := New(nil, nil, nil)
	_ = r.SetRules([]Rule{
		{Match: "*", ClientID: "wildcard"},
	})

	for _, host := range []string{"anything.com", "1.2.3.4", "foo.bar.baz"} {
		clientID, ok := r.Route(host, 80)
		if !ok || clientID != "wildcard" {
			t.Errorf("wildcard %s: got (%q, %v), want (wildcard, true)", host, clientID, ok)
		}
	}
}

func TestRouteDefault(t *testing.T) {
	r := New(nil, nil, nil)
	_ = r.SetRules([]Rule{
		{Match: "10.0.0.0/8", ClientID: "specific"},
	})
	r.SetDefaultClient("fallback")

	// Specific rule should win
	clientID, ok := r.Route("10.1.2.3", 80)
	if !ok || clientID != "specific" {
		t.Errorf("specific rule: got (%q, %v)", clientID, ok)
	}

	// No match → fallback
	clientID, ok = r.Route("8.8.8.8", 80)
	if !ok || clientID != "fallback" {
		t.Errorf("default: got (%q, %v), want (fallback, true)", clientID, ok)
	}
}

func TestRouteNoMatch(t *testing.T) {
	r := New(nil, nil, nil)
	_ = r.SetRules([]Rule{
		{Match: "10.0.0.0/8", ClientID: "internal"},
	})

	_, ok := r.Route("8.8.8.8", 80)
	if ok {
		t.Error("no default set: should return false when no rule matches")
	}
}

func TestRoutePriority(t *testing.T) {
	// Earlier rules take priority
	r := New(nil, nil, nil)
	_ = r.SetRules([]Rule{
		{Match: "10.0.0.1", ClientID: "specific-ip"},
		{Match: "10.0.0.0/8", ClientID: "subnet"},
	})

	// Exact IP should match the first rule
	clientID, ok := r.Route("10.0.0.1", 80)
	if !ok || clientID != "specific-ip" {
		t.Errorf("priority: exact IP should win over CIDR, got (%q, %v)", clientID, ok)
	}

	// Other IP in CIDR should use subnet rule
	clientID, ok = r.Route("10.0.0.2", 80)
	if !ok || clientID != "subnet" {
		t.Errorf("CIDR fallback: got (%q, %v), want (subnet, true)", clientID, ok)
	}
}

func TestCompileInvalidCIDR(t *testing.T) {
	r := New(nil, nil, nil)
	err := r.SetRules([]Rule{
		{Match: "not-a-cidr/99", ClientID: "x"},
	})
	if err == nil {
		t.Error("should return error for invalid CIDR")
	}
}

func TestTableSummary(t *testing.T) {
	r := New(nil, nil, nil)
	_ = r.SetRules([]Rule{
		{Match: "10.0.0.0/8", ClientID: "alice"},
		{Match: "*.example.com", ClientID: "bob"},
	})
	r.SetDefaultClient("fallback")

	summary := r.TableSummary()
	if len(summary) != 3 { // 2 rules + 1 default
		t.Errorf("expected 3 entries, got %d", len(summary))
	}
	last := summary[len(summary)-1]
	if !last.IsDefault || last.ClientID != "fallback" {
		t.Errorf("last entry should be default fallback, got %+v", last)
	}
}
