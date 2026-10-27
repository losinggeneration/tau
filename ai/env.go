package ai // import "github.com/losinggeneration/tau/ai"

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func init() {
	// Register transport for API key detection
}

// TelemetryEnabled returns true if telemetry is enabled.
// It checks the TAU_TELEMETRY environment variable:
// - "1", "true", "yes" (case-insensitive) -> enabled
// - "0", "false", "no" (case-insensitive) -> disabled
// - Empty or unset -> enabled (default)
func TelemetryEnabled() bool {
	val := os.Getenv("TAU_TELEMETRY")
	if val == "" {
		return true // Default to enabled
	}
	switch strings.ToLower(val) {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	default:
		return true // Default to enabled for unknown values
	}
}

// HasVertexADC checks the configured or default ADC path on each discovery.
func HasVertexADC() bool {
	return hasVertexADCWithEnv(os.Getenv)
}

func hasVertexADCWithEnv(lookup func(string) string) bool {
	if path := lookup("GOOGLE_APPLICATION_CREDENTIALS"); path != "" {
		return fileExists(path)
	}
	return fileExists(filepath.Join(userHomeDir(), ".config", "gcloud", "application_default_credentials.json"))
}

// HasVertexProject checks if the required Google Cloud project is set.
// Checks GOOGLE_CLOUD_PROJECT or GCLOUD_PROJECT.
func HasVertexProject() bool {
	return os.Getenv("GOOGLE_CLOUD_PROJECT") != "" || os.Getenv("GCLOUD_PROJECT") != ""
}

// HasVertexLocation checks if the required Google Cloud location is set.
func HasVertexLocation() bool {
	return os.Getenv("GOOGLE_CLOUD_LOCATION") != ""
}

// HasBedrockCredentials checks if AWS credentials are available from any source.
// This checks all AWS credential sources: explicit credentials, profiles,
// ECS task roles, IRSA, and Bedrock bearer tokens.
func HasBedrockCredentials() bool {
	// Check explicit credentials
	if os.Getenv("AWS_ACCESS_KEY_ID") != "" && os.Getenv("AWS_SECRET_ACCESS_KEY") != "" {
		return true
	}
	// Check for profile (credentials come from shared AWS config chain)
	if os.Getenv("AWS_PROFILE") != "" {
		return true
	}
	// Check Bedrock bearer token
	if os.Getenv("AWS_BEARER_TOKEN_BEDROCK") != "" {
		return true
	}
	// Check ECS task role credentials
	if os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") != "" {
		return true
	}
	if os.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI") != "" {
		return true
	}
	// Check IRSA (IAM Roles for Service Accounts)
	if os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE") != "" {
		return true
	}
	return false
}

// EnvTransport is a transport that uses environment variables for authentication.
type EnvTransport struct{}

// transportKey is used to store/retrieve transport from context.
type transportKey struct{}

// WithTransportContext adds a custom transport to the context.
func WithTransportContext(ctx context.Context, t *Transport) context.Context {
	return context.WithValue(ctx, transportKey{}, t)
}

// TransportFromContext retrieves a transport from the context.
func TransportFromContext(ctx context.Context) (*Transport, bool) {
	t, ok := ctx.Value(transportKey{}).(*Transport)

	return t, ok
}

// HTTPClient returns an HTTP client configured for the given provider.
func HTTPClient(provider string) *http.Client {
	return &http.Client{}
}

// fileExists checks if a file exists and is not a directory.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// userHomeDir returns the user's home directory.
func userHomeDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}

// envVarNames returns the environment variables consulted for the given
// provider. For plain API-key providers this is the single entry from
// EnvVarRegistry. Special-cased providers (Anthropic OAuth precedence,
// Copilot/Codex token chain, Google Vertex ADC, Amazon Bedrock credential
// sources, Cloudflare AI Gateway account/gateway) return the full ordered
// list consulted during resolution.
func envVarNames(provider string) []string {
	// Special-cased providers first — their resolution logic consults
	// more than the single registry entry.
	switch provider {
	case ProviderAnthropic:
		return []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_API_KEY"}
	case ProviderGitHubCopilot, ProviderOpenAICodex:
		return []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"}
	case ProviderGoogleVertex:
		return []string{
			"GOOGLE_CLOUD_API_KEY",
			"GOOGLE_APPLICATION_CREDENTIALS",
			"GOOGLE_CLOUD_PROJECT",
			"GCLOUD_PROJECT",
			"GOOGLE_CLOUD_LOCATION",
		}
	case ProviderAmazonBedrock:
		return []string{
			"AWS_PROFILE",
			"AWS_ACCESS_KEY_ID",
			"AWS_SECRET_ACCESS_KEY",
			"AWS_BEARER_TOKEN_BEDROCK",
			"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
			"AWS_CONTAINER_CREDENTIALS_FULL_URI",
			"AWS_WEB_IDENTITY_TOKEN_FILE",
		}
	case ProviderCloudflareAIGateway:
		return []string{"CLOUDFLARE_API_KEY", "CLOUDFLARE_ACCOUNT_ID", "CLOUDFLARE_GATEWAY_ID"}
	}
	if v, ok := EnvVarRegistry[provider]; ok {
		return []string{v}
	}
	return nil
}

func envVarNameForProvider(provider string) string {
	if envVar := EnvVarForProvider(provider); envVar != "" {
		return envVar
	}
	names := envVarNames(provider)
	if len(names) > 0 {
		return strings.Join(names, " or ")
	}
	return "API_KEY"
}

// tooling to enumerate all env vars that may carry credentials; for
// authoritative per-provider resolution, use GetEnvAPIKey.
var EnvAPIKeys = buildEnvAPIKeys()

func buildEnvAPIKeys() map[string]string {
	m := make(map[string]string)
	// Forward registry: provider -> env var. For shared env vars, the first
	// provider encountered wins; this is a representative mapping, not a
	// disambiguator.
	for _, provider := range registryProviderOrder() {
		envVar := EnvVarRegistry[provider]
		if _, exists := m[envVar]; !exists {
			m[envVar] = provider
		}
	}
	// Special-case providers' additional env vars.
	m["ANTHROPIC_AUTH_TOKEN"] = ProviderAnthropic
	m["ANTHROPIC_OAUTH_TOKEN"] = ProviderAnthropic
	m["ANTHROPIC_API_KEY"] = ProviderAnthropic
	m["COPILOT_GITHUB_TOKEN"] = ProviderGitHubCopilot
	m["GH_TOKEN"] = ProviderGitHubCopilot
	m["GITHUB_TOKEN"] = ProviderGitHubCopilot
	// AWS Bedrock credential sources
	m["AWS_PROFILE"] = ProviderAmazonBedrock
	m["AWS_ACCESS_KEY_ID"] = ProviderAmazonBedrock
	m["AWS_SECRET_ACCESS_KEY"] = ProviderAmazonBedrock
	m["AWS_BEARER_TOKEN_BEDROCK"] = ProviderAmazonBedrock
	m["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"] = ProviderAmazonBedrock
	m["AWS_CONTAINER_CREDENTIALS_FULL_URI"] = ProviderAmazonBedrock
	m["AWS_WEB_IDENTITY_TOKEN_FILE"] = ProviderAmazonBedrock
	// Google Vertex ADC supplementary variables
	m["GOOGLE_APPLICATION_CREDENTIALS"] = ProviderGoogleVertex
	m["GOOGLE_CLOUD_PROJECT"] = ProviderGoogleVertex
	m["GCLOUD_PROJECT"] = ProviderGoogleVertex
	m["GOOGLE_CLOUD_LOCATION"] = ProviderGoogleVertex
	// Cloudflare AI Gateway supplementary variables
	m["CLOUDFLARE_ACCOUNT_ID"] = ProviderCloudflareAIGateway
	m["CLOUDFLARE_GATEWAY_ID"] = ProviderCloudflareAIGateway
	return m
}

// registryProviderOrder returns provider IDs in a stable, sorted order so
// the reverse-map construction is deterministic.
func registryProviderOrder() []string {
	ids := make([]string, 0, len(EnvVarRegistry))
	for id := range EnvVarRegistry {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// GetEnvAPIKey returns the API key for a provider by consulting the
// environment. For plain API-key providers, it reads the single registered
// env var from EnvVarRegistry. For special-cased providers, it follows the
// documented precedence chain.
//
// Returns empty string when no credential is configured in the environment.
// Returns "<authenticated>" for providers whose environment configuration
// is sufficient to authenticate without an explicit key (Vertex ADC, any
// Bedrock source).
//
// Per the package's precedence rules, an explicit request API key supplied
// at call time wins over this environment-fallback helper; callers should
// always check an explicit key first.
func GetEnvAPIKey(provider string) string {
	return getEnvAPIKey(provider, os.Getenv)
}

func getEnvAPIKey(provider string, lookup func(string) string) string {
	// Special-cased providers consult more than the single registry entry.
	switch provider {
	case ProviderAnthropic:
		// OAuth token takes priority over the regular API key.
		if key := lookup("ANTHROPIC_OAUTH_TOKEN"); key != "" {
			return key
		}
		return lookup("ANTHROPIC_API_KEY")

	case ProviderGitHubCopilot, ProviderOpenAICodex:
		if key := lookup("COPILOT_GITHUB_TOKEN"); key != "" {
			return key
		}
		if key := lookup("GH_TOKEN"); key != "" {
			return key
		}
		return lookup("GITHUB_TOKEN")

	case ProviderGoogleVertex:
		// GOOGLE_CLOUD_API_KEY takes priority.
		if key := lookup("GOOGLE_CLOUD_API_KEY"); key != "" {
			return key
		}
		// ADC auth requires credentials + project + location.
		if hasVertexADCWithEnv(lookup) && (lookup("GOOGLE_CLOUD_PROJECT") != "" || lookup("GCLOUD_PROJECT") != "") && lookup("GOOGLE_CLOUD_LOCATION") != "" {
			return "<authenticated>"
		}
		return ""

	case ProviderAmazonBedrock:
		// Multiple credential sources: profile, IAM keys, bearer token,
		// ECS task role, IRSA. Any of them authenticates the client.
		if lookup("AWS_PROFILE") != "" {
			return "<authenticated>"
		}
		if lookup("AWS_ACCESS_KEY_ID") != "" && lookup("AWS_SECRET_ACCESS_KEY") != "" {
			return "<authenticated>"
		}
		if lookup("AWS_BEARER_TOKEN_BEDROCK") != "" {
			return "<authenticated>"
		}
		if lookup("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") != "" {
			return "<authenticated>"
		}
		if lookup("AWS_CONTAINER_CREDENTIALS_FULL_URI") != "" {
			return "<authenticated>"
		}
		if lookup("AWS_WEB_IDENTITY_TOKEN_FILE") != "" {
			return "<authenticated>"
		}
		return ""

	case ProviderHuggingFace:
		// HF_TOKEN is the primary var; HUGGINGFACE_HUB_TOKEN is a widely-used
		// alias that the public helper also recognizes.
		if key := lookup("HF_TOKEN"); key != "" {
			return key
		}
		return lookup("HUGGINGFACE_HUB_TOKEN")
	}

	// Plain API-key providers: read the single entry from EnvVarRegistry.
	if envVar, ok := EnvVarRegistry[provider]; ok {
		return lookup(envVar)
	}
	return ""
}

// getEnv is exported for internal use by providers.
// ShareViewerURL returns the share viewer URL override from TAU_SHARE_VIEWER_URL.
// Returns empty string if not set (defaults to https://pi.dev/session/).
func ShareViewerURL() string {
	return os.Getenv("TAU_SHARE_VIEWER_URL")
}

// GetEnvHeaders returns provider-owned authentication headers. Bearer tokens
// are kept separate from API keys to avoid activating OAuth request shaping.
func GetEnvHeaders(provider string) map[string]string {
	return getEnvHeaders(provider, os.Getenv)
}

func getEnvHeaders(provider string, lookup func(string) string) map[string]string {
	if provider == ProviderAnthropic {
		if token := lookup("ANTHROPIC_AUTH_TOKEN"); token != "" {
			return map[string]string{"Authorization": "Bearer " + token}
		}
	}
	return nil
}

// HasEnvCredentials reports environment authentication without resolving stored
// credentials or performing network calls.
func HasEnvCredentials(provider string) bool {
	return ProviderEnvHasCredentials(provider, nil)
}

// ProviderEnvAPIKey resolves a key from scoped variables before process values.
func ProviderEnvAPIKey(provider string, env map[string]string) string {
	return (&options{providerEnv: env}).envAPIKey(provider)
}

// ProviderEnvHeaders resolves header-owned authentication from scoped variables.
func ProviderEnvHeaders(provider string, env map[string]string) map[string]string {
	return (&options{providerEnv: env}).envHeaders(provider)
}

// ProviderEnvHasCredentials reports scoped authentication without exchanging tokens.
func ProviderEnvHasCredentials(provider string, env map[string]string) bool {
	o := &options{providerEnv: env}
	return len(o.envHeaders(provider)) != 0 || o.envAPIKey(provider) != "" || (provider == ProviderAnthropic && o.providerEnvValue("ANTHROPIC_FEDERATION_RULE_ID") != "" && o.providerEnvValue("ANTHROPIC_ORGANIZATION_ID") != "" && o.providerEnvValue("ANTHROPIC_IDENTITY_TOKEN_FILE") != "")
}
