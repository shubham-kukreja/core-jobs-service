package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// probe GETs a loopback health URL and fails unless it answers 200. The
// runtime image has no shell or curl, so container healthchecks call this.
// Only loopback hosts are allowed: it is a probe, not an HTTP client.
func probe(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return fmt.Errorf("probe: %q must be an http:// URL", raw)
	}
	ip := net.ParseIP(u.Hostname())
	if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("probe: %q is not a loopback address", u.Hostname())
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil) //nolint:gosec // G704: host checked above to be loopback only.
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: loopback-only URL, see above.
	if err != nil {
		return fmt.Errorf("probe: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("probe: %s answered %d", u.Redacted(), resp.StatusCode)
	}
	return nil
}
