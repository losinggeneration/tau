package mcp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/losinggeneration/tau/ai"
	"github.com/losinggeneration/tau/ai/oauth"
)

// signInTimeout bounds a whole sign-in, like upstream's callback wait.
var signInTimeout = 5 * time.Minute

// ErrSignInCancelled reports a sign-in stopped by the user or shutdown.
var ErrSignInCancelled = errors.New("sign-in cancelled")

// SignInPrompt shows the authorization URL and asks for the redirect URL
// when the browser can't reach the loopback callback.
type SignInPrompt struct {
	// ShowURL shows the authorization URL and opens it in a browser.
	ShowURL func(authorizationURL string)
	// RedirectURL asks for the redirect URL from the browser address bar. Its
	// context ends once the callback arrives or the sign-in ends; an empty
	// answer cancels the sign-in. Nil waits for the callback alone.
	RedirectURL func(ctx context.Context) (string, error)
}

// oauthConfig is a server's `oauth` object.
type oauthConfig struct {
	ClientID              string `json:"clientId"`
	ClientSecret          string `json:"clientSecret"`
	CallbackPort          int    `json:"callbackPort"`
	CallbackURL           string `json:"callbackUrl"`
	Scope                 string `json:"scope"`
	ClientName            string `json:"clientName"`
	ClientRegistration    string `json:"clientRegistration"`
	AuthServerMetadataURL string `json:"authServerMetadataUrl"`
}

type authorizationResponse struct {
	result *auth.AuthorizationResult
	err    error
}

// SignIn runs the browser authorization code flow for an OAuth server and
// saves its tokens. It doesn't reconnect the server; see Reconnect.
func (m *Manager) SignIn(ctx context.Context, name string, prompt SignInPrompt) (err error) {
	s := m.find(name)
	if s == nil || s.auth == nil {
		return fmt.Errorf("MCP server %q does not use OAuth", name)
	}
	var cfg oauthConfig
	if len(s.entry.Config.OAuth) > 0 {
		if err := json.Unmarshal(s.entry.Config.OAuth, &cfg); err != nil {
			return fmt.Errorf("oauth: %w", err)
		}
	}
	// ponytail: no Client ID Metadata Document or metadata URL override; they
	// need a hosted tau client document and a discovery override in go-sdk.
	if cfg.ClientRegistration == "cimd" || cfg.AuthServerMetadataURL != "" {
		return errors.New(`oauth.clientRegistration "cimd" and oauth.authServerMetadataUrl aren't supported`)
	}
	secret := cfg.ClientSecret
	if secret != "" {
		if secret, err = ai.ResolveConfigValue(ctx, secret, nil); err != nil {
			return fmt.Errorf("MCP server %q oauth.clientSecret: %w", name, err)
		}
	}

	ctx, cancel := context.WithTimeoutCause(ctx, signInTimeout, errors.New("sign-in timed out"))
	defer cancel()
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	defer func() {
		if err != nil && errors.Is(context.Cause(ctx), context.Canceled) {
			err = ErrSignInCancelled
		}
	}()

	stored, err := m.credentials.Load(ctx, name, s.entry.Config.URL)
	if err != nil {
		return err
	}
	listener, redirect, path, err := listenForCallback(ctx, cfg, stored)
	if err != nil {
		return err
	}
	var expected atomic.Value
	expected.Store("")
	callbacks := make(chan authorizationResponse, 1)
	srv := &http.Server{ReadHeaderTimeout: oauthRequestTimeout, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		result, err := responseFromQuery(r.URL.Query(), expected.Load().(string))
		if err != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, oauth.OAuthErrorHTML("Signing in to the MCP server did not complete.", err.Error())) // the browser may have gone
			if !strings.Contains(err.Error(), "different sign-in") {
				select {
				case callbacks <- authorizationResponse{err: err}:
				default:
				}
			}
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, oauth.OAuthSuccessHTML("Signed in to the MCP server. You may now close this page.")) // the browser may have gone
		select {
		case callbacks <- authorizationResponse{result: result}:
		default:
		}
	})}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(listener) }()
	defer func() {
		if closeErr := srv.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		if serveErr := <-served; !errors.Is(serveErr, http.ErrServerClosed) {
			err = errors.Join(err, serveErr)
		}
	}()

	fetch := func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		authURL, err := url.Parse(args.URL)
		if err != nil {
			return nil, fmt.Errorf("authorization URL: %w", err)
		}
		state := authURL.Query().Get("state")
		expected.Store(state)
		if prompt.ShowURL != nil {
			prompt.ShowURL(args.URL)
		}
		promptCtx, stopPrompt := context.WithCancel(ctx)
		defer stopPrompt()
		pasted := make(chan authorizationResponse, 1)
		if prompt.RedirectURL != nil {
			go func() {
				input, err := prompt.RedirectURL(promptCtx)
				if err == nil {
					var result *auth.AuthorizationResult
					result, err = responseFromRedirectURL(input, state, redirect)
					pasted <- authorizationResponse{result: result, err: err}
					return
				}
				pasted <- authorizationResponse{err: err}
			}()
		}
		var r authorizationResponse
		select {
		case r = <-callbacks:
		case r = <-pasted:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
		return r.result, r.err
	}

	stepUp := challengeParam(s.auth.lastChallenge(), "error") == "insufficient_scope"
	hcfg := &auth.AuthorizationCodeHandlerConfig{
		RedirectURL:              redirect,
		AuthorizationCodeFetcher: fetch,
		RequestRefreshToken:      true,
		Client:                   &http.Client{Timeout: oauthRequestTimeout},
		ScopeFilter: func(discovered []string) []string {
			scopes := strings.Fields(cfg.Scope)
			if stepUp && stored != nil {
				scopes = append(scopes, stored.Scopes...)
			}
			scopes = append(scopes, discovered...)
			slices.Sort(scopes)
			return slices.Compact(scopes)
		},
		NewTokenSource: func(_ context.Context, oc *oauth2.Config, token *oauth2.Token) (oauth2.TokenSource, error) {
			cred := &Credential{
				ClientID: oc.ClientID, ClientSecret: oc.ClientSecret, AuthStyle: oc.Endpoint.AuthStyle,
				TokenURL: oc.Endpoint.TokenURL, RedirectURL: redirect, Scopes: oc.Scopes, Token: token,
			}
			return oauth2.StaticTokenSource(token), m.credentials.Save(context.WithoutCancel(ctx), name, s.entry.Config.URL, cred)
		},
	}
	switch {
	case cfg.ClientID != "":
		hcfg.PreregisteredClient = &oauthex.ClientCredentials{ClientID: cfg.ClientID}
		if secret != "" {
			hcfg.PreregisteredClient.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: secret}
		}
	// A registered client can't use another redirect URI, so it's reused
	// only for the same one.
	case stored != nil && stored.ClientID != "" && stored.RedirectURL == redirect:
		hcfg.PreregisteredClient = &oauthex.ClientCredentials{ClientID: stored.ClientID}
		if stored.ClientSecret != "" {
			hcfg.PreregisteredClient.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: stored.ClientSecret}
		}
	default:
		hcfg.DynamicClientRegistrationConfig = &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			RedirectURIs:            []string{redirect},
			ClientName:              cmp.Or(cfg.ClientName, "tau"),
			GrantTypes:              []string{"authorization_code", "refresh_token"},
			ResponseTypes:           []string{"code"},
			TokenEndpointAuthMethod: "none",
		}}
	}
	handler, err := auth.NewAuthorizationCodeHandler(hcfg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.entry.Config.URL, nil)
	if err != nil {
		return err
	}
	// The flow starts from the server's last challenge, which carries its
	// resource metadata URL and the scope it asked for.
	resp := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{"Www-Authenticate": s.auth.lastChallenge()},
		Body:       io.NopCloser(strings.NewReader("")),
	}
	if err := handler.Authorize(ctx, req, resp); err != nil {
		if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
			return cause
		}
		return err
	}
	return nil
}

// listenForCallback listens on the configured callback address, or on the
// port of the stored client's redirect URI so that client stays valid,
// falling back to a free port. It returns the redirect URI and its path.
func listenForCallback(ctx context.Context, cfg oauthConfig, stored *Credential) (net.Listener, string, string, error) {
	u, err := url.Parse(cmp.Or(cfg.CallbackURL, "http://127.0.0.1/callback"))
	if err != nil {
		return nil, "", "", fmt.Errorf("oauth.callbackUrl: %w", err)
	}
	host := u.Hostname()
	listenHost := host
	if host == "localhost" {
		listenHost = "127.0.0.1"
	}
	port := u.Port()
	if port == "" && cfg.CallbackPort > 0 {
		port = strconv.Itoa(cfg.CallbackPort)
	}
	fixed := port != ""
	if !fixed && stored != nil {
		if r, err := url.Parse(stored.RedirectURL); err == nil {
			port = r.Port()
		}
	}
	lc := net.ListenConfig{}
	listener, err := lc.Listen(ctx, "tcp", net.JoinHostPort(listenHost, cmp.Or(port, "0")))
	if err != nil && !fixed && port != "" {
		listener, err = lc.Listen(ctx, "tcp", net.JoinHostPort(listenHost, "0"))
	}
	if err != nil {
		return nil, "", "", fmt.Errorf("listen for the OAuth callback: %w", err)
	}
	// A configured URI with a port is sent exactly as written, since servers
	// compare it as a string.
	if cfg.CallbackURL != "" && u.Port() != "" {
		return listener, cfg.CallbackURL, u.Path, nil
	}
	u.Host = net.JoinHostPort(host, strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))
	return listener, u.String(), u.Path, nil
}

// responseFromQuery reads an authorization response.
func responseFromQuery(q url.Values, state string) (*auth.AuthorizationResult, error) {
	if state == "" || q.Get("state") != state {
		return nil, errors.New("the redirect URL belongs to a different sign-in")
	}
	if e := q.Get("error"); e != "" {
		return nil, errors.New(cmp.Or(q.Get("error_description"), e))
	}
	code := q.Get("code")
	if code == "" {
		return nil, errors.New("the redirect URL does not contain an authorization code")
	}
	return &auth.AuthorizationResult{Code: code, State: state, Iss: q.Get("iss")}, nil
}

// responseFromRedirectURL reads a redirect URL pasted from the browser's
// address bar. It must match the sign-in's redirect URI.
func responseFromRedirectURL(input, state, redirect string) (*auth.AuthorizationResult, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, ErrSignInCancelled
	}
	u, err := url.Parse(input)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("expected the full redirect URL from the browser address bar")
	}
	want, err := url.Parse(redirect)
	if err != nil {
		return nil, fmt.Errorf("redirect URI: %w", err)
	}
	if u.Scheme != want.Scheme || u.Host != want.Host || u.Path != want.Path {
		return nil, errors.New("the redirect URL does not match this sign-in's redirect URI")
	}
	q := u.Query()
	if e := q.Get("error"); e != "" {
		return nil, errors.New(cmp.Or(q.Get("error_description"), e))
	}
	return responseFromQuery(q, state)
}
