package gateway

import (
	"errors"
	"net/netip"
	"sort"
	"strings"
)

// Network modes for the egress proxy. See docs/gateway.md, section 5.
const (
	ModeOff   = "off"   // every proxy request is refused
	ModeAllow = "allow" // only hosts on the allowlist
	ModeOn    = "on"    // the public internet
)

// ValidMode reports whether m is one of the three network modes.
func ValidMode(m string) bool {
	return m == ModeOff || m == ModeAllow || m == ModeOn
}

// blockedNames are refused by name, before any lookup, so the log gives a
// clear reason. Their addresses would be refused anyway.
var blockedNames = map[string]bool{
	"localhost":                  true,
	"host.docker.internal":       true,
	"gateway.docker.internal":    true,
	"kubernetes.docker.internal": true,
	"metadata.google.internal":   true,
}

// blockedPrefixes are address ranges the proxy never connects to, in any
// mode: anything that isn't the public internet. Loopback, private, link-local
// (which includes the 169.254.169.254 cloud metadata address), multicast and
// unspecified addresses are checked separately in blockedAddr.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT, also used by some VPNs
	netip.MustParsePrefix("192.0.0.0/24"),    // protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, and the broadcast address
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64, which can carry any IPv4 address
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local NAT64
	netip.MustParsePrefix("100::/64"),        // discard
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4, which can carry any IPv4 address
}

// blockedAddr reports whether the proxy must refuse to connect to a.
func blockedAddr(a netip.Addr) bool {
	a = a.Unmap() // ::ffff:10.0.0.1 is 10.0.0.1
	if !a.IsValid() || a.Zone() != "" {
		return true
	}
	if a.IsLoopback() || a.IsPrivate() || a.IsUnspecified() ||
		a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// normalizeHost lowercases a host name and checks that it is a plain DNS
// name or an IP address. It refuses anything else, so odd spellings can't
// slip past the allowlist.
func normalizeHost(h string) (string, error) {
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	if h == "" || len(h) > 253 {
		return "", errors.New("missing or too long host name")
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return h, nil
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("not a valid host name")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", errors.New("not a valid host name")
			}
		}
	}
	return h, nil
}

// allowlist holds approved hosts. "example.com" matches only that name.
// "*.example.com" matches names under it, such as api.example.com, but not
// example.com itself, so each is approved on purpose.
type allowlist struct {
	exact  map[string]bool
	suffix []string // ".example.com"
}

// newAllowlist checks and stores entries. IP addresses are refused: the
// allowlist is for names, and local addresses stay blocked whatever it says.
func newAllowlist(entries []string) (*allowlist, error) {
	l := &allowlist{exact: map[string]bool{}}
	for _, e := range entries {
		wild := strings.HasPrefix(e, "*.")
		name, err := normalizeHost(strings.TrimPrefix(e, "*."))
		if err != nil {
			return nil, errors.New("allowlist entry " + quote(e) + ": " + err.Error())
		}
		if _, err := netip.ParseAddr(name); err == nil {
			return nil, errors.New("allowlist entry " + quote(e) + ": use a host name, not an address")
		}
		if !strings.Contains(name, ".") {
			return nil, errors.New("allowlist entry " + quote(e) + ": needs at least one dot")
		}
		if wild {
			l.suffix = append(l.suffix, "."+name)
		} else {
			l.exact[name] = true
		}
	}
	return l, nil
}

func (l *allowlist) allows(host string) bool {
	if l.exact[host] {
		return true
	}
	for _, s := range l.suffix {
		if strings.HasSuffix(host, s) {
			return true
		}
	}
	return false
}

// entries returns the list in the form it was given, sorted.
func (l *allowlist) entries() []string {
	out := make([]string, 0, len(l.exact)+len(l.suffix))
	for h := range l.exact {
		out = append(out, h)
	}
	for _, s := range l.suffix {
		out = append(out, "*"+s)
	}
	sort.Strings(out)
	return out
}

func quote(s string) string { return "\"" + s + "\"" }
