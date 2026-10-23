package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	airoot "github.com/losinggeneration/tau/ai"
)

// Google Gemini CLI OAuth provider.
type geminiCLIProvider struct {
	clientID     string
	authURL      string
	tokenURL     string
	callbackPort int
}

func NewGeminiCLIProvider() Provider {
	return &geminiCLIProvider{
		clientID:     "686登山56a0o50cfb1o99t56aepqpbkf2q.apps.googleusercontent.com",
		authURL:      "https://accounts.google.com/o/oauth2/v2/auth",
		tokenURL:     "https://oauth2.googleapis.com/token",
		callbackPort: 53694,
	}
}

func (p *geminiCLIProvider) ID() ProviderID {
	return ProviderGeminiCLI
}

func (p *geminiCLIProvider) Name() string {
	return "Google Gemini CLI"
}

func (p *geminiCLIProvider) UsesCallbackServer() bool {
	return true
}

func (p *geminiCLIProvider) ModifyModels(models []*airoot.Model, credentials *Credentials) []*airoot.Model {
	return models
}

func (p *geminiCLIProvider) Login(ctx context.Context, callbacks LoginCallbacks) (creds *Credentials, err error) {
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		return nil, err
	}

	server := newCallbackServer(verifier, p.callbackPort)
	if err := server.Start(); err != nil {
		return nil, fmt.Errorf("start callback server: %w", err)
	}
	defer func() {
		if stopErr := server.Stop(); err == nil && stopErr != nil {
			err = stopErr
		}
	}()

	redirectURI := server.URL()

	authParams := map[string]string{
		"client_id":             p.clientID,
		"response_type":         "code",
		"redirect_uri":          redirectURI,
		"scope":                 "openid email https://www.googleapis.com/auth/cloud-platform",
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		"state":                 verifier,
		"access_type":           "offline",
		"prompt":                "consent",
	}

	authURL := BuildAuthURL(p.authURL, authParams)

	callbacks.OnAuth(authURL, "Complete login in your browser. Paste the final redirect URL here if authenticating on another machine.")

	code, err := waitForAuthorizationCode(ctx, callbacks, server, verifier)
	if err != nil {
		return nil, fmt.Errorf("wait for code: %w", err)
	}

	callbacks.OnProgress("Exchanging authorization code for tokens...")

	creds, err = ExchangeCode(ctx, p.tokenURL, map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     p.clientID,
		"code":          code,
		"redirect_uri":  redirectURI,
		"code_verifier": verifier,
	})
	if err != nil {
		return nil, fmt.Errorf("exchange code: %w", err)
	}

	if creds.ProjectID == "" {
		creds.ProjectID = firstNonEmpty(os.Getenv("GOOGLE_CLOUD_PROJECT"), os.Getenv("GOOGLE_CLOUD_PROJECT_ID"))
	}

	return creds, nil
}

func (p *geminiCLIProvider) RefreshToken(ctx context.Context, credentials *Credentials) (*Credentials, error) {
	if credentials == nil {
		return nil, fmt.Errorf("missing credentials")
	}
	if credentials.ProjectID == "" {
		return nil, fmt.Errorf("google cloud credentials missing projectId")
	}

	refreshed, err := RefreshTokens(ctx, p.tokenURL, map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     p.clientID,
		"refresh_token": credentials.Refresh,
	})
	if err != nil {
		return nil, err
	}
	refreshed.ProjectID = credentials.ProjectID
	if refreshed.Email == "" {
		refreshed.Email = credentials.Email
	}
	return refreshed, nil
}

func (p *geminiCLIProvider) GetAPIKey(credentials *Credentials) string {
	if credentials == nil {
		return ""
	}

	encoded, err := json.Marshal(struct {
		Token     string `json:"token"`
		ProjectID string `json:"projectId"`
	}{
		Token:     credentials.Access,
		ProjectID: credentials.ProjectID,
	})
	if err != nil {
		return ""
	}

	return string(encoded)
}

// TokenInfo represents OAuth token info response.
type TokenInfo struct {
	IssuedTo      string `json:"issued_to"`
	Audience      string `json:"audience"`
	Scope         string `json:"scope"`
	ExpiresIn     int    `json:"expires_in"`
	AccessType    string `json:"access_type"`
	VerifiedEmail bool   `json:"verified_email"`
}

// GetTokenInfo retrieves token information from Google.
func GetTokenInfo(ctx context.Context, accessToken string) (_ *TokenInfo, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://oauth2.googleapis.com/tokeninfo", nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)

	client := &http.Client{Timeout: 10 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := closeBody(resp); err == nil && closeErr != nil {
			err = closeErr
		}
	}()

	var info TokenInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}

	return &info, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
