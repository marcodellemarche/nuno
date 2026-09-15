// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"html/template"
	"net"
	"net/http"
	"strings"

	"github.com/marcodellemarche/nuno/internal/core"
)

// meTemplate is a second, tiny template set: the page is rendered inside a
// dashboard card in an iframe, so it carries no navigation and no admin
// surface.
var meTemplate = template.Must(template.New("me.html").Funcs(template.FuncMap{
	"title": core.TitleCase,
}).ParseFS(templateFS, "templates/me.html"))

// meHandler renders one person's own quota. The identity comes from the
// forward-auth header the proxy sets, and is believed only from the configured
// proxy network: without that check, any container sharing the network could
// claim to be somebody else and read their quota.
//
// It is not behind the admin password, because the person reading it is not an
// admin. The proxy is what authenticates them.
func meHandler(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !fromTrustedProxy(r, opts.TrustedProxy) {
			http.Error(w, "this page is served through the proxy that authenticates you", http.StatusForbidden)
			return
		}
		uid := strings.TrimSpace(r.Header.Get("Remote-User"))
		if uid == "" {
			http.Error(w, "no authenticated user in the request", http.StatusUnauthorized)
			return
		}

		response, err := buildUsage(r.Context(), opts, true)
		if err != nil {
			opts.Log.Error("me: build usage", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		var found *core.UsageUser
		for i := range response.Users {
			if response.Users[i].User == uid {
				found = &response.Users[i]
				break
			}
		}
		if found == nil {
			http.Error(w, "no quota for "+uid, http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := meTemplate.ExecuteTemplate(w, "me.html", found); err != nil {
			opts.Log.Error("me: render", "error", err)
		}
	}
}

// fromTrustedProxy reports whether the request arrived from the configured
// network. An empty setting trusts nothing, so a header is never believed by
// accident.
func fromTrustedProxy(r *http.Request, trusted string) bool {
	if strings.TrimSpace(trusted) == "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, part := range strings.Split(trusted, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, network, err := net.ParseCIDR(part); err == nil {
			if network.Contains(ip) {
				return true
			}
			continue
		}
		if single := net.ParseIP(part); single != nil && single.Equal(ip) {
			return true
		}
	}
	return false
}
