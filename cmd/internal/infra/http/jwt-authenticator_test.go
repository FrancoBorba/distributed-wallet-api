/*
@Author: Franco Ribeiro Borba
@Description: Unit tests for the token handling that needs no identity provider
*/
package http

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/config"
)

// Um token ausente ou malformado precisa ser recusado antes de qualquer
// verificação criptográfica, senão o serviço gasta trabalho com lixo.
func TestJWTAuthenticator_CredencialAusenteOuMalformada(t *testing.T) {
	authenticator := &JWTAuthenticator{cfg: config.OIDC{ProviderClaim: "azp"}}

	tests := []struct {
		name   string
		header string
	}{
		{"sem header", ""},
		{"só espaços", "   "},
		{"esquema errado", "Basic dXNlcjpwYXNz"},
		{"Bearer sem token", "Bearer "},
		{"Bearer com espaços", "Bearer     "},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/wagering/transactions/x", nil)
			if tc.header != "" {
				request.Header.Set("Authorization", tc.header)
			}

			_, err := authenticator.Authenticate(request)
			if !errors.Is(err, ErrMissingToken) {
				t.Errorf("erro = %v, quero %v", err, ErrMissingToken)
			}
		})
	}
}

func TestHasRole(t *testing.T) {
	roles := []string{"offline_access", "wallet-internal", "uma_authorization"}

	if !hasRole(roles, "wallet-internal") {
		t.Error("a role concedida deveria ser reconhecida")
	}
	if hasRole(roles, "wallet-admin") {
		t.Error("uma role não concedida não pode ser reconhecida")
	}

	// Uma role vazia na configuração não pode transformar todo mundo em
	// serviço interno por acidente.
	if hasRole(roles, "") {
		t.Error("uma role vazia nunca concede nada")
	}
}
