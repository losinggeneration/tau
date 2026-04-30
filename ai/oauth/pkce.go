package oauth

import "golang.org/x/oauth2"

// PKCEParams contains the generated code verifier and challenge.
type PKCEParams struct {
	Verifier  string
	Challenge string
}

// GeneratePKCEParams generates a PKCE code verifier and S256 challenge.
func GeneratePKCEParams() (*PKCEParams, error) {
	verifier := oauth2.GenerateVerifier()
	return &PKCEParams{
		Verifier:  verifier,
		Challenge: oauth2.S256ChallengeFromVerifier(verifier),
	}, nil
}

// GenerateCodeVerifier generates just the PKCE code verifier.
// This is useful when you need the verifier separately for storing.
func GenerateCodeVerifier() (string, error) {
	return oauth2.GenerateVerifier(), nil
}

// GenerateCodeChallenge computes the PKCE code challenge from a verifier using S256 method.
func GenerateCodeChallenge(verifier string) string {
	return oauth2.S256ChallengeFromVerifier(verifier)
}
