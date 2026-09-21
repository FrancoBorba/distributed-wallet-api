/*
@Author: Franco Ribeiro Borba
@Description: Authentication against an OAuth 2.0 / OIDC identity provider.
A caller presents a bearer token that the identity provider signed; this
verifies that signature with the provider's public keys, which are read once at
startup from the realm metadata and refreshed by the library as they rotate.
Verification is therefore local: no network call happens per request, which is
what makes checking every single request affordable. Three claims are checked
and each one closes a different hole. The signature proves the token was minted
by the provider and not forged. The expiry proves it is still valid, so a
leaked token stops working. The audience proves the token was minted for this
service, which is what stops a token issued for another service in the same
realm from being replayed here. What the token says the caller is then becomes
the Identity, and every authorization rule in the handlers works off that
without knowing where it came from.
@Date : 21/09/2026
@Update: -
*/
package http

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/config"
	"github.com/coreos/go-oidc/v3/oidc"
)

// bearerPrefix is the scheme of the Authorization header.
const bearerPrefix = "Bearer "

var (
	// ErrMissingToken is a request with no bearer token at all.
	ErrMissingToken = errors.New("an Authorization: Bearer token is required")
	// ErrInvalidToken is a token that failed verification: bad signature,
	// expired, issued by another realm or minted for another audience.
	ErrInvalidToken = errors.New("the token is not valid for this service")
	// ErrUnknownIdentity is a valid token that carries no identity this
	// service can act upon.
	ErrUnknownIdentity = errors.New("the token carries no provider identity")
)

// JWTAuthenticator verifies bearer tokens issued by the identity provider.
type JWTAuthenticator struct {
	verifier *oidc.IDTokenVerifier
	cfg      config.OIDC
}

// NewJWTAuthenticator reads the realm metadata and prepares verification. It
// runs at startup and fails the boot when the provider is unreachable, because
// a service that cannot verify tokens must not start accepting requests it
// would have to refuse anyway.
func NewJWTAuthenticator(ctx context.Context, cfg config.OIDC) (*JWTAuthenticator, error) {
	discoveryCtx, cancel := context.WithTimeout(ctx, cfg.DiscoveryTimeout)
	defer cancel()

	// When the metadata lives at a different URL than the issuer, the library
	// is told to expect the configured issuer instead of the one it would infer
	// from the discovery URL. The issuer of each token keeps being verified.
	discoveryURL := cfg.Issuer
	if cfg.DiscoveryURL != "" && cfg.DiscoveryURL != cfg.Issuer {
		discoveryURL = cfg.DiscoveryURL
		discoveryCtx = oidc.InsecureIssuerURLContext(discoveryCtx, cfg.Issuer)
	}

	provider, err := oidc.NewProvider(discoveryCtx, discoveryURL)
	if err != nil {
		return nil, fmt.Errorf("read the realm metadata of %s: %w", discoveryURL, err)
	}

	// ClientID is what the library compares against the aud claim, which is
	// the audience check this service depends on.
	verifier := provider.Verifier(&oidc.Config{ClientID: cfg.Audience})

	return &JWTAuthenticator{verifier: verifier, cfg: cfg}, nil
}

// tokenClaims is what the service reads out of a verified token. Everything
// else the provider puts in it is ignored on purpose: a claim this service does
// not use cannot influence a decision it takes.
type tokenClaims struct {
	RealmAccess struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

// Authenticate verifies the token and turns it into an identity.
func (a *JWTAuthenticator) Authenticate(r *http.Request) (Identity, error) {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if header == "" {
		return Identity{}, ErrMissingToken
	}
	if !strings.HasPrefix(header, bearerPrefix) {
		return Identity{}, ErrMissingToken
	}

	raw := strings.TrimSpace(strings.TrimPrefix(header, bearerPrefix))
	if raw == "" {
		return Identity{}, ErrMissingToken
	}

	// Verify checks the signature against the keys of the realm, the issuer,
	// the expiry and the audience. Anything that fails any of them is refused
	// with one message, since telling a caller which check failed only helps
	// somebody probing the service.
	token, err := a.verifier.Verify(r.Context(), raw)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	var claims tokenClaims
	if err := token.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	// The internal service is marked by a realm role rather than by its client
	// name, so adding a second internal client later is a matter of granting
	// the role and changes nothing here.
	if hasRole(claims.RealmAccess.Roles, a.cfg.InternalRole) {
		return Identity{Internal: true}, nil
	}

	providerID, err := a.providerOf(token)
	if err != nil {
		return Identity{}, err
	}

	return Identity{ProviderID: providerID}, nil
}

// providerOf reads the provider identity out of the configured claim. It is
// read from the verified token and never from the request, which is the whole
// point: a caller cannot choose which provider it is.
func (a *JWTAuthenticator) providerOf(token *oidc.IDToken) (string, error) {
	var claims map[string]any
	if err := token.Claims(&claims); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	value, found := claims[a.cfg.ProviderClaim]
	if !found {
		return "", fmt.Errorf("%w: claim %q is absent", ErrUnknownIdentity, a.cfg.ProviderClaim)
	}

	providerID, ok := value.(string)
	if !ok || strings.TrimSpace(providerID) == "" {
		return "", fmt.Errorf("%w: claim %q is empty", ErrUnknownIdentity, a.cfg.ProviderClaim)
	}

	return strings.TrimSpace(providerID), nil
}

// hasRole reports whether a role was granted.
func hasRole(roles []string, wanted string) bool {
	if wanted == "" {
		return false
	}

	for _, role := range roles {
		if role == wanted {
			return true
		}
	}

	return false
}
