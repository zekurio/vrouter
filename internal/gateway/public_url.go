package gateway

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func parsePublicURL(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	u, err := url.Parse(value)
	if err != nil || !publicOrigin(u, value) {
		return "", errors.New("VROUTER_PUBLIC_URL must be an absolute http or https origin without credentials, a path, query, or fragment")
	}
	return u.Scheme + "://" + u.Host, nil
}

// publicOrigin reports whether u, parsed from raw, is a bare http(s) origin
// with a valid port, if any.
func publicOrigin(u *url.URL, raw string) bool {
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" || strings.ContainsAny(raw, "?#") {
		return false
	}
	port := u.Port()
	if port == "" {
		return !strings.HasSuffix(u.Host, ":")
	}
	p, err := strconv.Atoi(port)
	return err == nil && p >= 1 && p <= 65535
}

func (s *server) appURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL
	}
	// Management authorization has already checked Origin against Host.
	if origin := r.Header.Get("Origin"); origin != "" {
		return origin
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	// Forwarded headers are not trusted. Hosted deployments set PublicURL.
	return scheme + "://" + r.Host
}
