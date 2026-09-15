// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"fmt"
	"io"
	"log/slog"
	"net/url"
)

const redacted = "[redacted]"

// Secret is a credential. Every way of rendering it is redacted, so the only
// path to the value is Reveal, which makes each such point greppable. See
// NFR-4 and rule 7 in AGENTS.md.
type Secret string

func (s Secret) String() string   { return redacted }
func (s Secret) GoString() string { return redacted }

// Format covers the verbs String does not, including %q and %#v.
func (s Secret) Format(f fmt.State, verb rune) { io.WriteString(f, redacted) }

func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (s Secret) LogValue() slog.Value         { return slog.StringValue(redacted) }

func (s Secret) Reveal() string { return string(s) }
func (s Secret) Empty() bool    { return s == "" }

// RedactURL makes a URL safe to log: the userinfo goes, and so do the query
// values, because a token in a query string is the usual way one travels. An
// unparseable URL is not echoed back, because whatever made it unparseable may
// be a credential.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return redacted
	}
	if u.User != nil {
		u.User = url.User("redacted")
	}
	if u.RawQuery != "" {
		values := u.Query()
		for key := range values {
			values.Set(key, "redacted")
		}
		u.RawQuery = values.Encode()
	}
	return u.String()
}

// RedactURLToHost keeps only the scheme and the host. It is for a URL whose
// path is itself a secret, which is how most webhook endpoints work: the token
// is the path.
func RedactURLToHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return redacted
	}
	return u.Scheme + "://" + u.Host
}
