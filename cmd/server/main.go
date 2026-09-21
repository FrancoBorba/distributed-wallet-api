/*
@Author: Franco Ribeiro Borba
@Description: Entry point of the service. It does three things and nothing
else: it loads the configuration, so an impossible setting stops the process
here with a readable message instead of deep inside a dependency graph; it
hands that configuration to the Fx application, which builds and starts
everything; and it lets Fx own the lifetime, which means SIGINT and SIGTERM
trigger the ordered shutdown of the HTTP server, the workers and their
dependencies. The stop deadline is the configured one: components get that long
to conclude what they are holding, and anything still unfinished is recovered by
another instance, because an unconfirmed message becomes visible again and a
claimed event has its lease expire.
@Date : 20/09/2026
@Update: 20/09/2026
*/
package main

import (
	"fmt"
	"os"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/config"
	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/di"
	_ "github.com/FrancoBorba/distributed-wallet-api/docs"
	"go.uber.org/fx"
)

//	@title			Distributed Wallet API
//	@version		1.0
//	@description	Wallet service for game providers, with equivalent guarantees over HTTP and SQS.
//	@description
//	@description	**Money** always travels as a decimal string with its currency, never as a number.
//	@description
//	@description	**Idempotency**: an operation is identified by (providerId, externalTransactionId). Replaying it returns the stored result with `idempotentReplay: true` and the balance observed when it was originally processed. Reusing the key with a different body is a 409.
//	@description
//	@description	**Status codes**: 200 processed, 202 accepted and waiting for a reference, 400 invalid input, 401 no credential, 403 acting for another provider, 404 not found, 409 idempotency conflict, 422 refused by a business rule with a stable `failureCode`, 503 transient, send it again.
//	@description
//	@description	**Authentication**: every business endpoint requires an OAuth 2.0 bearer token issued by Keycloak through the client_credentials flow. The provider identity comes from the token, never from a header. Wallet operations are restricted to the internal service.

//	@securityDefinitions.apikey	BearerAuth
//	@in							header
//	@name						Authorization
//	@description				Token OAuth 2.0 obtido no Keycloak pelo fluxo client_credentials. Informe "Bearer " seguido do token.

//	@BasePath	/
//	@accept		json
//	@produce	json

//	@tag.name			wallets
//	@tag.description	Wallet operations, restricted to the internal service

//	@tag.name			wagering
//	@tag.description	Provider operations over a wallet

//	@tag.name			health
//	@tag.description	Public liveness and readiness probes

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration: %v\n", err)
		os.Exit(1)
	}

	fx.New(
		fx.Supply(cfg),
		di.Module,
		fx.StopTimeout(cfg.ShutdownTimeout),
	).Run()
}
