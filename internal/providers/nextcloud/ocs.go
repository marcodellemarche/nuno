// SPDX-License-Identifier: AGPL-3.0-or-later

package nextcloud

import (
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

// The provider type, used by the registry and in every error message.
const providerType = "nextcloud"

// maxBody bounds what is read from the instance. A user list is small; an
// unbounded read is a way to be killed by a misconfigured proxy.
const maxBody = 32 << 20

// envelope is the OCS wrapper. Both halves are read: on /ocs/v2.php the HTTP
// status and ocs.meta.statuscode agree, and a disagreement is a provider
// error rather than a value to trust. See ADR-0021 point 1.
type envelope struct {
	OCS struct {
		Meta struct {
			Status     string `json:"status"`
			StatusCode int    `json:"statuscode"`
			Message    string `json:"message"`
		} `json:"meta"`
		Data json.RawMessage `json:"data"`
	} `json:"ocs"`
}

type client struct {
	base     *url.URL
	username string
	password core.Secret
	http     *http.Client
}

func newClient(baseURL string, username string, password core.Secret, timeout time.Duration) (*client, error) {
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
	return &client{
		base:     base,
		username: username,
		password: password,
		http:     &http.Client{Timeout: timeout},
	}, nil
}

// get reads an OCS endpoint.
func (c *client) get(ctx context.Context, path string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, path, nil)
}

// put writes one, with a form body, which is what the provisioning API takes.
func (c *client) put(ctx context.Context, path string, form url.Values) (json.RawMessage, error) {
	return c.do(ctx, http.MethodPut, path, form)
}

func (c *client) do(ctx context.Context, method, path string, form url.Values) (json.RawMessage, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.JoinPath(path).String(), body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.username, c.password.Reveal())
	// Without OCS-APIRequest the request is refused; without Accept the
	// response is XML.
	req.Header.Set("OCS-APIRequest", "true")
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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

	if resp.StatusCode == http.StatusTooManyRequests {
		// 50 writes per 10 minutes on editUser. A throttled run is
		// incomplete, not failed. See ADR-0014 and FR-38c.
		return nil, &core.ThrottledError{Provider: providerType, RetryAfter: retryAfter(resp)}
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, &core.ProviderError{
			Provider: providerType,
			Op:       method + " " + path,
			Status:   resp.StatusCode,
			Message:  "response is not an OCS envelope",
			Err:      err,
		}
	}

	meta := env.OCS.Meta
	if !statusesAgree(resp.StatusCode, meta.StatusCode) {
		return nil, &core.ProviderError{
			Provider: providerType,
			Op:       method + " " + path,
			Status:   resp.StatusCode,
			Message: fmt.Sprintf("HTTP %d disagrees with ocs.meta.statuscode %d (%s): trusting neither",
				resp.StatusCode, meta.StatusCode, meta.Message),
		}
	}
	if !isOK(meta.StatusCode) {
		return nil, statusError(method+" "+path, meta.StatusCode, meta.Message)
	}
	return env.OCS.Data, nil
}

// statusesAgree holds the v2 rule. 100 and 200 both mean ok in the envelope,
// and an ok envelope arrives with HTTP 200.
func statusesAgree(httpStatus, metaStatus int) bool {
	if httpStatus == metaStatus {
		return true
	}
	return httpStatus == http.StatusOK && isOK(metaStatus)
}

func isOK(metaStatus int) bool {
	return metaStatus == 100 || metaStatus == http.StatusOK
}

func statusError(op string, status int, message string) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		// An app password lands here on every write: editUser carries
		// PasswordConfirmationRequired and an app password can never stamp
		// the confirmation. See ADR-0014.
		return &core.AuthError{Provider: providerType, Status: status, Message: message}
	case http.StatusTooManyRequests:
		return &core.ThrottledError{Provider: providerType}
	}
	return &core.ProviderError{Provider: providerType, Op: op, Status: status, Message: message}
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
