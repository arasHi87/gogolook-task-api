package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/spf13/cobra"

	"github.com/arasHi87/gogolook-task-api/internal/config"
)

// newHealthcheckCommand probes the admin listener's readiness endpoint and
// exits 0 or 1.
//
// It exists because the runtime image is distroless: there is no shell and no
// curl, so a container HEALTHCHECK has nothing to call. Shipping the probe as a
// subcommand of the same binary keeps the image minimal and means the probe
// reads the same configuration as the process it is probing — including a
// non-default --admin.addr.
func newHealthcheckCommand() *cobra.Command {
	var (
		endpoint string
		timeout  time.Duration
	)

	cmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "Probe the admin listener and exit 0 (ready) or 1 (not ready)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := config.Load(config.Options{
				File:  config.FilePath(cmd.Flags()),
				Flags: cmd.Flags(),
			})
			if err != nil {
				return err
			}

			target, err := probeURL(res.Config.Admin.Addr, endpoint)
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			if err != nil {
				return err
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("probe %s: %w", target, err)
			}
			defer resp.Body.Close() //nolint:errcheck // probe response body is discarded

			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("probe %s: status %d", target, resp.StatusCode)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "ok")
			return err
		},
	}

	cmd.Flags().StringVar(&endpoint, "endpoint", "/readyz", "admin endpoint to probe")
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Second, "probe timeout")
	return cmd
}

// probeURL turns a listen address into something dialable. ":9090" and
// "0.0.0.0:9090" both mean "every interface" to a listener but neither is a
// valid destination, so they become localhost here.
func probeURL(addr, endpoint string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("admin.addr %q: %w", addr, err)
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: endpoint}
	return u.String(), nil
}
