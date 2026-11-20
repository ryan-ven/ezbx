package gateway

import (
	"net/netip"
	"testing"
)

func TestBlockedAddr(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"10.0.0.1", "172.16.0.1", "172.31.255.255", "192.168.1.1",
		"192.168.65.254", // Docker Desktop's host address
		"169.254.169.254", "fe80::1",
		"100.64.0.1", "0.0.0.0", "0.1.2.3", "::",
		"224.0.0.1", "ff02::1", "255.255.255.255", "240.0.0.1",
		"192.0.2.1", "198.51.100.1", "203.0.113.1", "198.18.0.1",
		"fc00::1", "fd12:3456::1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254",
		"::ffff:100.64.0.1", "::ffff:198.18.0.1", "::ffff:240.0.0.1", // ranges Go doesn't know, written the IPv6 way
		"64:ff9b::a00:1", "2002:a00:1::1", "2001:db8::1",
	}
	public := []string{"1.1.1.1", "8.8.8.8", "104.20.23.154", "172.32.0.1", "100.128.0.1", "2606:4700::1111", "::ffff:1.1.1.1"}

	for _, s := range blocked {
		if !blockedAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range public {
		if blockedAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
	if !blockedAddr(netip.MustParseAddr("fe80::1%eth0")) {
		t.Error("a zoned address should be blocked")
	}
	if !blockedAddr(netip.Addr{}) {
		t.Error("an empty address should be blocked")
	}
}

func TestNormalizeHost(t *testing.T) {
	good := map[string]string{
		"Example.COM":   "example.com",
		"example.com.":  "example.com",
		"a-b.example":   "a-b.example",
		"1.2.3.4":       "1.2.3.4",
		"[2606:4700::]": "2606:4700::",
	}
	for in, want := range good {
		got, err := normalizeHost(in)
		if err != nil || got != want {
			t.Errorf("normalizeHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", ".", "a..b", "-a.com", "a-.com", "ex ample.com", "exa_mple.com", "exämple.com", "a.com/x", "a.com:443", "a.com\x00"} {
		if got, err := normalizeHost(in); err == nil {
			t.Errorf("normalizeHost(%q) = %q, want an error", in, got)
		}
	}
}

func TestAllowlist(t *testing.T) {
	l, err := newAllowlist([]string{"registry.npmjs.org", "*.github.com", "Example.ORG."})
	if err != nil {
		t.Fatal(err)
	}
	yes := []string{"registry.npmjs.org", "api.github.com", "a.b.github.com", "example.org"}
	no := []string{"github.com", "evilgithub.com", "npmjs.org", "registry.npmjs.org.evil.com", "sub.registry.npmjs.org", "github.com.evil.com"}
	for _, h := range yes {
		if !l.allows(h) {
			t.Errorf("%s should be allowed", h)
		}
	}
	for _, h := range no {
		if l.allows(h) {
			t.Errorf("%s should not be allowed", h)
		}
	}
	got := l.entries()
	want := []string{"*.github.com", "example.org", "registry.npmjs.org"}
	if len(got) != len(want) {
		t.Fatalf("entries = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entries = %v, want %v", got, want)
		}
	}

	for _, bad := range []string{"1.2.3.4", "*.10.0.0.1", "localhost", "*", "*.com.", "*.test", "bad host", ""} {
		if _, err := newAllowlist([]string{bad}); err == nil {
			t.Errorf("allowlist entry %q should be refused", bad)
		}
	}
}
