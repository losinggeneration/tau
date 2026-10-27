package ai

import (
	"net/url"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/http/httpproxy"
)

func unsetProxyVariable(t *testing.T, key string) {
	t.Helper()
	value, present := os.LookupEnv(key)
	require.NoError(t, os.Unsetenv(key))
	t.Cleanup(func() {
		if present {
			require.NoError(t, os.Setenv(key, value))
		} else {
			require.NoError(t, os.Unsetenv(key))
		}
	})
}

func TestApplyHTTPProxySettings(t *testing.T) {
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "NO_PROXY", "no_proxy", "REQUEST_METHOD"} {
		unsetProxyVariable(t, key)
	}
	settings := &Settings{HTTPProxy: "  http://proxy.invalid:8080  "}
	require.NoError(t, settings.ApplyHTTPProxySettings())
	for _, scheme := range []string{"http", "https"} {
		target, err := url.Parse(scheme + "://example.com")
		require.NoError(t, err)
		proxy, err := httpproxy.FromEnvironment().ProxyFunc()(target)
		require.NoError(t, err)
		require.Equal(t, "http://proxy.invalid:8080", proxy.String())
	}
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "http://explicit.invalid")
	require.NoError(t, settings.ApplyHTTPProxySettings())
	require.Empty(t, os.Getenv("HTTP_PROXY"))
	require.Equal(t, "http://explicit.invalid", os.Getenv("HTTPS_PROXY"))
}

func TestApplyHTTPProxySettingsRejectsInvalidWithoutMutation(t *testing.T) {
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY"} {
		unsetProxyVariable(t, key)
	}
	for _, proxy := range []string{"not-a-url", "ftp://host", "http://", "http://proxy/#fragment", "http://user:secret@%"} {
		require.Error(t, (&Settings{HTTPProxy: proxy}).ApplyHTTPProxySettings())
		_, exists := os.LookupEnv("HTTP_PROXY")
		require.False(t, exists)
		_, exists = os.LookupEnv("HTTPS_PROXY")
		require.False(t, exists)
	}
	require.NoError(t, (*Settings)(nil).ApplyHTTPProxySettings())
	require.NoError(t, (&Settings{}).ApplyHTTPProxySettings())
}
