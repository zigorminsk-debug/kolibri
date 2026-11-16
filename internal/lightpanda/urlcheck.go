package lightpanda

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Special IPv4 and IPv6 blocks that netip.Addr.IsPrivate / IsLoopback do not
// cover on their own: carrier-grade NAT, benchmark networks and reserved space.
var reservedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:db8::/32"),
}

// blockURLPatterns is passed to Lightpanda's --block-urls flag so that page
// redirects, scripts and subresources cannot reach local services either.
const blockURLPatterns = "*://localhost*,*://127.*,*://0.0.0.0*,*://[::1]*,*://[::]*," +
	"*://169.254.*,*://10.*,*://192.168.*," +
	"*://172.16.*,*://172.17.*,*://172.18.*,*://172.19.*," +
	"*://172.20.*,*://172.21.*,*://172.22.*,*://172.23.*," +
	"*://172.24.*,*://172.25.*,*://172.26.*,*://172.27.*," +
	"*://172.28.*,*://172.29.*,*://172.30.*,*://172.31.*," +
	"*://*.localhost/*,*://*.local/*,*://*.internal/*"

// CheckURL verifies that raw is a public http/https address safe for a model
// to open. Loopback, private, link-local and metadata addresses are refused,
// so neither a confused model nor a hostile page can poke at the user's
// machine, local colibri server or office router.
//
// allowLoopback lifts the ban on 127.0.0.0/8, ::1 and localhost for tests.
// skipDNS skips hostname resolution (used when traffic goes through a proxy).
func CheckURL(ctx context.Context, raw string, allowLoopback, skipDNS bool) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("не указан URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("некорректный URL %q: %w", raw, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("схема %q запрещена: разрешены только http и https", u.Scheme)
	}
	if u.User != nil {
		return nil, errors.New("учётные данные в URL запрещены")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return nil, fmt.Errorf("в URL %q нет имени хоста", raw)
	}
	if isLocalName(host) {
		if allowLoopback && (host == "localhost" || strings.HasSuffix(host, ".localhost")) {
			return u, nil
		}
		return nil, fmt.Errorf("доступ к локальному адресу %s заблокирован", host)
	}
	// Refuse integer or octal IP spellings like http://2130706433 or http://0x7f000001.
	if looksNumericHost(host) {
		addr, perr := netip.ParseAddr(host)
		if perr != nil {
			return nil, fmt.Errorf("нестандартная запись IP-адреса %s заблокирована", host)
		}
		if blockedAddr(addr, allowLoopback) {
			return nil, fmt.Errorf("доступ к внутреннему адресу %s заблокирован", host)
		}
		return u, nil
	}
	if addr, perr := netip.ParseAddr(host); perr == nil {
		if blockedAddr(addr, allowLoopback) {
			return nil, fmt.Errorf("доступ к внутреннему адресу %s заблокирован", host)
		}
		return u, nil
	}
	if skipDNS {
		return u, nil
	}
	dnsCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(dnsCtx, "ip", host)
	if err != nil {
		// Unresolvable domains fail naturally in Lightpanda; only a parent
		// cancellation stops the check here.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return u, nil
	}
	for _, addr := range addrs {
		if blockedAddr(addr, allowLoopback) {
			return nil, fmt.Errorf("адрес %s указывает на внутреннюю сеть (%s) — доступ заблокирован", host, addr.Unmap())
		}
	}
	return u, nil
}

func isLocalName(host string) bool {
	if host == "localhost" || host == "broadcasthost" || host == "ip6-localhost" || host == "ip6-loopback" {
		return true
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".home.arpa", ".lan"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// looksNumericHost reports hosts made only of digits, dots or a 0x hex form,
// which curl/browsers may parse as an IPv4 address even when netip.ParseAddr
// rejects them (octal 0177.0.0.1, single integer 2130706433, short 127.1).
func looksNumericHost(host string) bool {
	if strings.HasPrefix(host, "0x") {
		_, err := strconv.ParseUint(host[2:], 16, 64)
		return err == nil
	}
	parts := strings.Split(host, ".")
	for _, p := range parts {
		if p == "" {
			return false
		}
		if strings.HasPrefix(p, "0x") {
			if _, err := strconv.ParseUint(p[2:], 16, 32); err != nil {
				return false
			}
			continue
		}
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				return false
			}
		}
	}
	// Strict decimal quad with no leading zeros goes through netip.ParseAddr;
	// octal parts like 0177 have a leading zero and are rejected.
	if len(parts) == 4 {
		for _, p := range parts {
			if len(p) > 1 && p[0] == '0' {
				return true
			}
		}
	}
	return true
}

func blockedAddr(addr netip.Addr, allowLoopback bool) bool {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return true
	}
	if addr.IsLoopback() {
		return !allowLoopback
	}
	if addr.IsPrivate() || addr.IsUnspecified() || addr.IsMulticast() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
		return true
	}
	for _, p := range reservedPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
