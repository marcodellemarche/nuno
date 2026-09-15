// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
	"github.com/marcodellemarche/nuno/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// The templates and the stylesheet are embedded, so the binary serves the UI
// with no files on disk and the container works with no outbound network
// (ADR-0006).
var pageTemplate = template.Must(template.New("page").Funcs(template.FuncMap{
	"iec":      core.FormatIEC,
	"quota":    quotaText,
	"barWidth": barWidth,
}).ParseFS(templateFS, "templates/*.html"))

func staticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	return http.FileServer(http.FS(sub))
}

// pageData is everything the page renders. It is assembled here rather than in
// the template, so the template stays a layout.
type pageData struct {
	Version   string
	Now       time.Time
	Usage     core.UsageResponse
	Providers []providerView
	Issues    []issueView
	Warnings  []string
}

type providerView struct {
	Name        string
	Type        string
	Version     string
	Reachable   bool
	InSupported bool
	WriteAccess string
	LastObserve *time.Time
	LastError   string
	Degraded    string
}

type issueView struct {
	Provider string
	Kind     string
	Person   string
	Account  string
	Detail   string
}

// pageHandler renders the aggregate page. It degrades instead of failing when
// Nuno is misconfigured: a missing credential shows as a warning and the page
// still lists what it knows (FR-55).
func pageHandler(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := pageData{Version: opts.Version, Now: time.Now().UTC()}

		usage, err := buildUsage(r.Context(), opts)
		if err != nil {
			opts.Log.Error("page: build usage", "error", err)
			data.Warnings = append(data.Warnings, "Usage could not be read from the database: "+err.Error())
		}
		data.Usage = usage

		providers, err := opts.Store.ListProviders(r.Context())
		if err != nil {
			opts.Log.Error("page: read providers", "error", err)
			data.Warnings = append(data.Warnings, "Providers could not be read: "+err.Error())
		}
		for _, p := range providers {
			data.Providers = append(data.Providers, providerView{
				Name:        p.Name,
				Type:        p.Type,
				Version:     p.Version,
				Reachable:   p.Reachable,
				InSupported: p.InSupported,
				WriteAccess: p.WriteAccess.String(),
				LastObserve: p.LastObserveAt,
				LastError:   p.LastError,
				Degraded:    p.DegradedReason,
			})
		}
		data.Warnings = append(data.Warnings, warningsFor(providers, opts)...)

		users, err := opts.Store.ListUsers(r.Context())
		if err == nil {
			data.Issues = issueViews(r.Context(), opts, providers, users)
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := pageTemplate.ExecuteTemplate(w, "page.html", data); err != nil {
			opts.Log.Error("page: render", "error", err)
		}
	}
}

func warningsFor(providers []store.ProviderRow, opts Options) []string {
	var warnings []string
	if len(providers) == 0 {
		warnings = append(warnings,
			"No providers are configured. Set NUNO_PROVIDER_<NAME>_TYPE and _URL, then run nuno observe.")
	}
	for _, p := range providers {
		switch {
		case p.LastObserveAt == nil:
			warnings = append(warnings, p.Name+" has never been observed. Run nuno observe.")
		case !p.LastObserveOK:
			warnings = append(warnings, p.Name+" could not be read: "+p.LastError)
		}
		if p.Degraded() {
			warnings = append(warnings, p.Name+" is degraded and will not be written to: "+p.DegradedReason)
		}
	}
	if opts.AdminPassword.Empty() {
		warnings = append(warnings,
			"No admin password is set, so this page is only as protected as whatever sits in front of it.")
	}
	return warnings
}

func issueViews(ctx context.Context, opts Options, providers []store.ProviderRow, users []core.User) []issueView {
	issues, err := opts.Store.ListLinkIssues(ctx)
	if err != nil {
		opts.Log.Error("page: read link issues", "error", err)
		return nil
	}
	providerNames := map[int64]string{}
	for _, p := range providers {
		providerNames[p.ID] = p.Name
	}
	userNames := map[int64]string{}
	for _, u := range users {
		userNames[u.ID] = u.UID
	}

	views := make([]issueView, 0, len(issues))
	for _, issue := range issues {
		person := ""
		if issue.UserID != nil {
			person = userNames[*issue.UserID]
		}
		views = append(views, issueView{
			Provider: providerNames[issue.ProviderID],
			Kind:     string(issue.Kind),
			Person:   person,
			Account:  issue.ExternalID,
			Detail:   issue.Detail,
		})
	}
	return views
}

// quotaText renders a ceiling for a human, distinguishing the states a number
// cannot express.
func quotaText(bytes *int64, status string) string {
	switch status {
	case core.StatusUnknown:
		return "unknown"
	case core.StatusUnavailable:
		return "unavailable"
	}
	if bytes == nil {
		return "unlimited"
	}
	return core.FormatIEC(*bytes)
}

// barWidth is clamped, because a quota below current usage is a real state and
// the bar must not overflow its track.
func barWidth(percent *float64) int {
	if percent == nil {
		return 0
	}
	switch {
	case *percent < 0:
		return 0
	case *percent > 100:
		return 100
	}
	return int(*percent)
}
