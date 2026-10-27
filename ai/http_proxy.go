package ai

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// ApplyHTTPProxySettings fills absent proxy variables for process-wide HTTP clients.
// Call during bootstrap, before transports cache their environment proxy policy.
func (s *Settings) ApplyHTTPProxySettings() error {
	if s == nil {
		return nil
	}
	proxy := strings.TrimSpace(s.HTTPProxy)
	if proxy == "" {
		return nil
	}
	parsed, err := url.Parse(proxy)
	if err != nil || parsed.Hostname() == "" || parsed.Fragment != "" {
		return fmt.Errorf("invalid httpProxy URL")
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("unsupported httpProxy scheme")
	}
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY"} {
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, proxy); err != nil {
			return fmt.Errorf("set %s: %w", key, err)
		}
	}
	return nil
}
