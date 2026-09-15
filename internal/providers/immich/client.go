// SPDX-License-Identifier: AGPL-3.0-or-later

package immich

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/marcodellemarche/nuno/internal/core"
)

const providerType = "immich"

const maxBody = 32 << 20

type client struct {
	base   *url.URL
	apiKey core.Secret
	http   *http.Client
}

func newClient(baseURL string, apiKey core.Secret, timeout time.Duration) (*client, error) {
	base, err := url.Parse(strings.TrimSuffix(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("URL: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("URL must be http or https, got %q", core.RedactURL(baseURL))
	}
	if base.Host == "" {
		return nil, fmt.Errorf("URL has no host: %q", core.RedactURL(baseURL))
	}
	return &client{base: base, apiKey: apiKey, http: &http.Client{Timeout: timeout}}, nil
}

func (c *client) get(ctx context.Context, path string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, path, nil)
}

func (c *client) put(ctx context.Context, path string, body any) (json.RawMessage, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPut, path, encoded)
}

func (c *client) do(ctx context.Context, method, path string, body []byte) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.JoinPath(path).String(), reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", c.apiKey.Reveal())
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &core.UnreachableError{Provider: providerType, Err: err}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, &core.UnreachableError{Provider: providerType, Err: err}
	}

	op := method + " " + path
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, &core.ThrottledError{Provider: providerType, RetryAfter: retryAfter(resp)}
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		// A key without adminUser.read or adminUser.update lands here, which
		// is a scope problem and not a wrong key.
		return nil, &core.AuthError{
			Provider: providerType,
			Status:   resp.StatusCode,
			Message:  message(raw) + " (check the key's adminUser.read and adminUser.update scopes)",
		}
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, &core.ProviderError{
			Provider: providerType, Op: op,
			Status: resp.StatusCode, Message: message(raw),
		}
	}
	return raw, nil
}

// message pulls Immich's error text out of the body without echoing an
// arbitrary payload into a log line.
func message(raw []byte) string {
	var body struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err == nil {
		switch {
		case body.Message != "":
			return body.Message
		case body.Error != "":
			return body.Error
		}
	}
	// A body that is not Immich's JSON is something else answering, often a
	// proxy's HTML error page. Collapsing it keeps one log line one line.
	const limit = 160
	text := strings.Join(strings.Fields(string(raw)), " ")
	if text == "" {
		return "no message"
	}
	if len(text) > limit {
		return text[:limit] + "..."
	}
	return text
}

func retryAfter(resp *http.Response) time.Duration {
	value := resp.Header.Get("Retry-After")
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}
