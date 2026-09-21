/*
@Author: Franco Ribeiro Borba
@Description: Unit tests for the dependency graph
*/
package di

import (
	"testing"

	"github.com/FrancoBorba/distributed-wallet-api/cmd/internal/config"
	"go.uber.org/fx"
)

// O grafo do Fx só falha quando a aplicação sobe, o que em produção significa
// descobrir um provider faltando no deploy. ValidateApp resolve o grafo sem
// executar nenhum construtor, então este teste pega a fiação quebrada sem
// precisar de PostgreSQL nem de SQS.
func TestModule_GrafoResolve(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load erro inesperado: %v", err)
	}

	if err := fx.ValidateApp(fx.Supply(cfg), Module); err != nil {
		t.Fatalf("grafo de dependências inválido: %v", err)
	}
}

func TestConfig_RecusaTimeoutsIncompativeis(t *testing.T) {
	// O visibility timeout precisa ser maior que o tempo máximo do handler,
	// senão a mensagem volta a ficar visível enquanto ainda está sendo tratada
	// e um segundo consumidor pega uma operação que já está em andamento.
	t.Setenv("CONSUMER_VISIBILITY_TIMEOUT", "10s")
	t.Setenv("CONSUMER_HANDLER_TIMEOUT", "30s")

	if _, err := config.Load(); err == nil {
		t.Fatal("config.Load deveria recusar um visibility timeout menor que o do handler")
	}
}

func TestConfig_RecusaLongPollingAcimaDoLimite(t *testing.T) {
	t.Setenv("CONSUMER_WAIT_TIME", "45s")

	if _, err := config.Load(); err == nil {
		t.Fatal("config.Load deveria recusar um wait time acima dos 20s do SQS")
	}
}
