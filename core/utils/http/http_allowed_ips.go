package http

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/pkg/errors"
	"go.uber.org/multierr"

	"github.com/smartcontractkit/chainlink/core/logger"
)

var privateIPBlocks []*net.IPNet
var dbURL string

func init() {
	for _, cidr := range []string{
		"127.0.0.0/8",    // IPv4 loopback
		"10.0.0.0/8",     // RFC1918
		"172.16.0.0/12",  // RFC1918
		"192.168.0.0/16", // RFC1918
		"169.254.0.0/16", // RFC3927 link-local
		"::1/128",        // IPv6 loopback
		"fe80::/10",      // IPv6 link-local
		"fc00::/7",       // IPv6 unique local addr
	} {
		_, block, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(fmt.Errorf("parse error on %q: %v", cidr, err))
		}
		privateIPBlocks = append(privateIPBlocks, block)
	}
	dbURL = os.Getenv("DATABASE_URL")
}

func isRestrictedIP(ip net.IP, cfg httpClientConfig, lggr logger.Logger) bool {
	if !ip.IsGlobalUnicast() ||
		ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.Equal(net.IPv4bcast) ||
		ip.Equal(net.IPv4allsys) ||
		ip.Equal(net.IPv4allrouter) ||
		ip.Equal(net.IPv4zero) ||
		ip.IsMulticast() {
		return true
	}

	for _, block := range privateIPBlocks {
		if block.Contains(ip) {
			return true
		}
	}

	blacklisted, err := isBlacklistedIP(ip, cfg)
	if err != nil {
		lggr.Errorw("Failed to check IP blacklist status, this IP will be blocked", "err", err, "ip", ip)
		return true
	}

	return blacklisted
}

func isBlacklistedIP(ip net.IP, cfg httpClientConfig) (bool, error) {
	dbURL := cfg.DatabaseURL()
	if dbURL.String() == "" {
		return false, nil
	}
	ips, err := net.LookupIP(dbURL.String())
	if err != nil {
		return true, errors.Wrapf(err, "failed to lookup IP for DB URL")
	}
	for _, dbIP := range ips {
		if dbIP.Equal(ip) {
			return true, nil
		}
	}
	return false, nil
}

var ErrDisallowedIP = errors.New("disallowed IP")

// makeRestrictedDialContext returns a dialcontext function using the given arguments
func makeRestrictedDialContext(cfg httpClientConfig, lggr logger.Logger) func(context.Context, string, string) (net.Conn, error) {
	// restrictedDialContext wraps the Dialer such that after successful connection,
	// we check the IP.
	// If the resolved IP is restricted, close the connection and return an error.
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		con, err := (&net.Dialer{
			// Defaults from GoLang standard http package
			// https://golang.org/pkg/net/http/#RoundTripper
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext(ctx, network, address)
		if err == nil {
			// If a connection could be established, ensure it's not local or private
			a, _ := con.RemoteAddr().(*net.TCPAddr)

			if isRestrictedIP(a.IP, cfg, lggr) {
				return nil, multierr.Combine(
					errors.Wrapf(ErrDisallowedIP, "disallowed IP %s. Connections to local/private and multicast networks are disabled by default for security reasons", a.IP.String()),
					con.Close())
			}
		}
		return con, err
	}
}
