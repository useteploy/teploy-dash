package monitor

import (
	"context"
	"fmt"
	"net"
	"net/http"
)

// DASH-010: monitor targets can otherwise reach loopback, private-network,
// link-local, and cloud-metadata addresses by default — real for the
// intentional "monitor your own fleet over Tailscale" use case, but a
// meaningful hardening gap for any admin who didn't mean to grant that. The
// policy is opt-in per monitor (Monitor.AllowInternal) rather than a global
// switch, and it's enforced at DIAL time (not just when the target string is
// first validated) so a hostname that resolves differently between
// validation and connection — DNS rebinding — can't bypass it: dialTCP below
// resolves once, checks every returned address, and dials the specific
// address it already checked rather than letting net.Dial re-resolve.

// cloudMetadataIPs are well-known cloud provider metadata endpoints. They
// aren't inherently caught by the private/link-local ranges on every cloud
// (AWS/GCP's 169.254.169.254 is link-local and already covered, but listing
// it explicitly documents intent and future-proofs against a provider using
// a non-reserved address).
var cloudMetadataIPs = map[string]bool{
	"169.254.169.254": true, // AWS, GCP, Azure IMDS
	"100.100.100.200": true, // Alibaba Cloud
}

// isBlockedAddr reports whether ip is loopback, unspecified, private,
// link-local, or a known cloud metadata address — the set rejected by
// default unless a monitor explicitly opts in via AllowInternal.
func isBlockedAddr(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if cloudMetadataIPs[ip.String()] {
		return true
	}
	return false
}

// resolveAndFilter resolves host and returns an error identifying the first
// blocked address found, unless allowInternal is set. A hostname that
// resolves to a MIX of public and private addresses is rejected outright —
// letting it through on the public address alone would still let an
// attacker who controls the DNS answer redirect the next check to the
// private one.
func resolveAndFilter(ctx context.Context, host string, allowInternal bool) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if !allowInternal && isBlockedAddr(ip) {
			return nil, fmt.Errorf("target address %s is loopback/private/link-local/metadata — enable \"allow internal\" on this monitor to permit it", ip)
		}
		return []net.IP{ip}, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no addresses found for %s", host)
	}
	ips := make([]net.IP, len(addrs))
	for i, a := range addrs {
		ips[i] = a.IP
		if !allowInternal && isBlockedAddr(a.IP) {
			return nil, fmt.Errorf("target %s resolves to %s (loopback/private/link-local/metadata) — enable \"allow internal\" on this monitor to permit it", host, a.IP)
		}
	}
	return ips, nil
}

// policyDialContext returns a DialContext that resolves once, checks every
// resolved address against the policy, and dials the first validated address
// directly — so the transport can't re-resolve to a different (unchecked)
// address between validation and connection. Used for both the HTTP client's
// Transport.DialContext (which also covers redirects — Go's http.Client
// invokes DialContext again for the redirect target's host) and checkTCP.
func policyDialContext(allowInternal bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := resolveAndFilter(ctx, host, allowInternal)
		if err != nil {
			return nil, err
		}
		// Dial the specific, already-validated address rather than passing
		// the original hostname back to the dialer, which would re-resolve.
		return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
	}
}

// httpClientFor returns an HTTP client whose Transport enforces the network
// policy for this specific monitor (AllowInternal is a per-monitor choice, so
// the client can't be a single shared instance).
func httpClientFor(base *http.Client, allowInternal bool) *http.Client {
	baseTransport, _ := base.Transport.(*http.Transport)
	var transport *http.Transport
	if baseTransport != nil {
		transport = baseTransport.Clone()
	} else {
		transport = &http.Transport{}
	}
	transport.DialContext = policyDialContext(allowInternal)
	return &http.Client{
		Transport:     transport,
		CheckRedirect: base.CheckRedirect,
	}
}
