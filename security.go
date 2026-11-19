package main

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// appContentSecurityPolicy locks the app shell down to its own scripts and
// styles. The UI loads nothing from other origins and never needs inline code.
const appContentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; " +
	"media-src 'self'; frame-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; " +
	"frame-ancestors 'self'"

// secure wraps the app with the checks every request has to pass before it is
// routed, and with the security headers every response carries.
//
// The app has no accounts and is meant to be reachable from the whole network,
// so the main remaining threat is a web page in someone's browser talking to
// it on their behalf: cross-site form posts (CSRF) and DNS rebinding, where an
// attacker's hostname is pointed at the machine running Local Share.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		h.Set("Content-Security-Policy", appContentSecurityPolicy)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}

		if !hostAllowed(r.Host, s.allowedHosts) {
			http.Error(w, "host not allowed; set LOCAL_SHARE_ALLOWED_HOSTS to permit it", http.StatusForbidden)
			return
		}
		if !isSameOriginRequest(r) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed reports whether a request's Host header names this machine in a
// way an attacker's DNS record cannot: an IP address, localhost, a single-label
// or well-known local name. Anything else (a public-looking domain) has to be
// listed explicitly. "*" disables the check; "*.example.com" matches subdomains.
func hostAllowed(hostport string, extra []string) bool {
	host := strings.ToLower(strings.TrimSuffix(stripPort(hostport), "."))
	if host == "" {
		return false
	}
	for _, allowed := range extra {
		allowed = strings.ToLower(strings.TrimSpace(allowed))
		switch {
		case allowed == "":
		case allowed == "*":
			return true
		case strings.HasPrefix(allowed, "*."):
			if strings.HasSuffix(host, allowed[1:]) {
				return true
			}
		case allowed == host:
			return true
		}
	}

	if net.ParseIP(host) != nil || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range []string{".local", ".lan", ".home.arpa", ".localdomain"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

func stripPort(hostport string) string {
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return host
	}
	return strings.Trim(hostport, "[]")
}

// isSameOriginRequest rejects state-changing requests that a browser marks as
// coming from another site. Clients that send neither Origin nor
// Sec-Fetch-Site (curl, scripts) are not browsers acting for a third party and
// are let through.
func isSameOriginRequest(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}

	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}

	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, r.Host)
}
