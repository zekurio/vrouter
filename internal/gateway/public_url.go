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
	invalid := errors.New("VROUTER_PUBLIC_URL must be an absolute http or https origin without credentials, a path, query, or fragment")
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" || strings.ContainsAny(value, "?#") {
		return "", invalid
	}
	if port := u.Port(); port != "" {
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return "", invalid
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return "", invalid
	}
	return u.Scheme + "://" + u.Host, nil
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
