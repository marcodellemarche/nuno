// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
	"github.com/marcodellemarche/nuno/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// The templates and the assets are embedded, so the binary serves the UI with
// no files on disk and the container works with no outbound network
// (ADR-0006).
var templateFuncs = template.FuncMap{
	"iec":      core.FormatIEC,
	"quota":    quotaText,
	"barWidth": barWidth,
	"title":    core.TitleCase,
}

// Every page is the shared layout plus one content file. They are separate
// template sets because each content file defines "content", and one set holds
// one template of a given name.
func mustPage(name string) *template.Template {
	return template.Must(template.New("layout.html").Funcs(templateFuncs).
		ParseFS(templateFS, "templates/layout.html", "templates/"+name))
}

var (
	quotasTemplate   = mustPage("quotas.html")
	tiersTemplate    = mustPage("tiers.html")
	servicesTemplate = mustPage("services.html")
	accountsTemplate = mustPage("accounts.html")
	activityTemplate = mustPage("activity.html")

	keyTemplate = template.Must(template.New("key.html").Funcs(templateFuncs).
			ParseFS(templateFS, "templates/key.html"))
)

func staticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(sub))
	// The assets are embedded and their URL never changes between builds, so a
	// cached copy would outlive the binary that served it. Revalidate every
	// time: it is a small stylesheet and a small script.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}

// layout is what every page shares: the tab bar, the flash messages and the
// warnings. Each page embeds it and adds its own data.
type layout struct {
	Version  string
	Now      time.Time
	Tab      string
	Slug     string
	Warnings []string

	// Editable is false when no Actor is wired, which makes the pages
	// read-only rather than offering buttons that cannot work (FR-55).
	Editable bool
	OK       string
	Error    string
}

func newLayout(opts Options, r *http.Request, tab string) layout {
	return layout{
		Version:  opts.Version,
		Now:      time.Now().UTC(),
		Tab:      tab,
		Slug:     strings.ToLower(tab),
		Editable: opts.Actor != nil,
		OK:       r.URL.Query().Get("ok"),
		Error:    r.URL.Query().Get("err"),
	}
}

// render degrades instead of failing when Nuno is misconfigured: a missing
// credential shows as a warning and the page still lists what it knows
// (FR-55).
func render(w http.ResponseWriter, opts Options, page *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := page.ExecuteTemplate(w, "layout.html", data); err != nil {
		opts.Log.Error("page: render", "error", err)
	}
}

// providerRows is the one read every page makes, because the warnings belong
// to the whole surface rather than to the Services page alone.
func providerRows(ctx context.Context, opts Options, l *layout) []store.ProviderRow {
	providers, err := opts.Store.ListProviders(ctx)
	if err != nil {
		opts.Log.Error("page: read providers", "error", err)
		l.Warnings = append(l.Warnings, "Providers could not be read: "+err.Error())
	}
	l.Warnings = append(l.Warnings, warningsFor(providers, opts)...)
	return providers
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
	if opts.AdminPassword.Empty() && strings.TrimSpace(opts.TrustedProxy) == "" {
		warnings = append(warnings,
			"No admin password is set, so this page is only as protected as whatever sits in front of it.")
	}
	return warnings
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

// fillFor picks the bar's colour from how full it is, leaving anything whose
// numbers mean nothing in the muted shade rather than green.
func fillFor(percent int, status string) string {
	switch {
	case status != core.StatusOK && status != core.StatusStale:
		return "fill-bad"
	case percent >= 90:
		return "fill-warn"
	}
	return "fill-ok"
}
