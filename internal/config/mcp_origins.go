package config

import (
	"errors"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

const (
	MaxMCPAllowedOrigins    = 8
	MaxMCPOriginConfigBytes = 2048
)

var errMCPOrigin = errors.New("must contain exact HTTPS origins (loopback HTTP is allowed only in development)")

// NormalizeMCPOrigin normalizes an operator-owned origin, accepting a single
// trailing root slash for BASE_URL. It never uses request Host or proxy headers.
// Browser Origin headers additionally reject any path at the HTTP boundary.
func NormalizeMCPOrigin(raw string, production bool) (string, error) {
	if raw == "" || len(raw) > MaxMCPOriginConfigBytes || raw != strings.TrimSpace(raw) || strings.ContainsAny(raw, "?#") {
		return "", errMCPOrigin
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return "", errMCPOrigin
	}
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname())
	if scheme != "https" && scheme != "http" || host == "" || strings.HasSuffix(u.Host, ":") {
		return "", errMCPOrigin
	}
	ip, ipErr := netip.ParseAddr(host)
	if ipErr == nil && ip.Zone() == "" {
		if strings.HasPrefix(u.Host, "[") != ip.Is6() {
			return "", errMCPOrigin
		}
		host = ip.String()
	} else {
		// Wildcards, URL escapes, zones, separators, and ambiguous host spellings
		// are not origins. International names must use their ASCII DNS form.
		if len(host) > 253 || strings.ContainsAny(u.Host, "[]") {
			return "", errMCPOrigin
		}
		for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return "", errMCPOrigin
			}
			for _, ch := range label {
				if ch != '-' && !(ch >= 'a' && ch <= 'z') && !(ch >= '0' && ch <= '9') {
					return "", errMCPOrigin
				}
			}
		}
	}
	if scheme == "http" && (production || host != "localhost" && (!ip.IsValid() || !ip.Unmap().IsLoopback())) {
		return "", errMCPOrigin
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", errMCPOrigin
		}
		port = strconv.Itoa(n)
		if scheme == "https" && port == "443" || scheme == "http" && port == "80" {
			port = ""
		}
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host, nil
}

func parseMCPAllowedOrigins(raw string, production bool) ([]string, error) {
	if len(raw) > MaxMCPOriginConfigBytes {
		return nil, errors.New("must be at most 2048 bytes and contain at most 8 origins")
	}
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) > MaxMCPAllowedOrigins {
		return nil, errors.New("must be at most 2048 bytes and contain at most 8 origins")
	}
	origins := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		origin, err := NormalizeMCPOrigin(strings.TrimSpace(part), production)
		if err != nil || seen[origin] {
			return nil, errMCPOrigin
		}
		seen[origin] = true
		origins = append(origins, origin)
	}
	return origins, nil
}
