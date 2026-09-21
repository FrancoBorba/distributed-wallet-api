# Distributed Wallet API

Serviço em Go para processamento de operações financeiras de provedores de
jogos em ambiente distribuído. Oferece uma API HTTP e um consumidor SQS que
movimentam carteiras de jogadores com garantias equivalentes, preservando
precisão monetária, integridade do ledger e idempotência persistente mesmo sob
concorrência, reentrega de mensagens e falhas entre etapas do processamento.

## Sumário

- [Pré-requisitos](#pré-requisitos)
- [Execução rápida](#execução-rápida)
- [Portas e endereços](#portas-e-endereços)
- [Autenticação](#autenticação)
- [Exemplos de chamadas](#exemplos-de-chamadas)
- [Migrations](#migrations)
- [Filas](#filas)
- [Execução fora do Docker](#execução-fora-do-docker)
- [Variáveis de ambiente](#variáveis-de-ambiente)
- [Testes](#testes)
- [Observabilidade](#observabilidade)
- [Estrutura do projeto](#estrutura-do-projeto)

## Pré-requisitos

| Requisito | Versão | Observação |
| --- | --- | --- |
| Docker e Docker Compose | Engine 24 ou superior | Suficiente para rodar tudo |
| Go | 1.25.6 | Necessário apenas para rodar os testes ou a aplicação fora do Docker |
| gcc (MinGW no Windows) | qualquer | Necessário apenas para `go test -race`, que exige cgo |

Nenhuma configuração manual é necessária em um checkout limpo. Os valores
padrão de todas as variáveis de ambiente correspondem ao ambiente do Docker
Compose, e o arquivo `.env.example` documenta cada uma delas.

## Execução rápida

```bash
docker compose up --build
```

O comando sobe seis serviços e os coordena por dependência de saúde:

1. `postgres` e `keycloak_db`, os bancos.
2. `migrate`, que aplica as migrations pendentes e encerra.
3. `keycloak`, que importa o realm `wager` na primeira inicialização.
4. `localstack`, que provisiona as quatro filas com a política de redrive.
5. `api`, que só inicia depois que todos os anteriores estão prontos.

A primeira execução leva alguns minutos por causa do download das imagens e da
inicialização do Keycloak. Quando tudo estiver de pé:

```bash
curl http://localhost:8081/health/ready
# {"status":"ok","checks":{"postgres":"ok","sqs":"ok"}}
```

A documentação interativa fica em <http://localhost:8081/swagger/index.html>.

Para encerrar preservando os dados:

```bash
docker compose down
```

Para encerrar descartando os volumes e começar do zero:

```bash
docker compose down -v
```

## Portas e endereços

| Serviço | Endereço | Observação |
| --- | --- | --- |
| API | <http://localhost:8081> | Porta 8081 porque o Keycloak ocupa a 8080 |
| Swagger UI | <http://localhost:8081/swagger/index.html> | A raiz `/` redireciona para cá |
| Métricas | <http://localhost:8081/metrics> | Formato Prometheus, público |
| Health | `/health/live` e `/health/ready` | Públicos |
| Keycloak | <http://localhost:8080> | Console em `/admin`, usuário `admin`, senha `admin` |
| PostgreSQL | `localhost:5432` | Base `wager_db`, usuário `wager_user`, senha `wager_password` |
| LocalStack | <http://localhost:4566> | SQS |

## Autenticação

Todos os endpoints de negócio exigem um token OAuth 2.0 emitido pelo Keycloak,
obtido pelo fluxo `client_credentials`. O realm `wager` é importado
automaticamente com três clients:

| Client | Segredo | Identidade resultante | Pode |
| --- | --- | --- | --- |
| `wallet-internal` | `wallet-internal-secret` | Serviço interno | Abrir carteiras, ler qualquer uma, reconciliar |
| `provider-a` | `provider-a-secret` | Provedor `provider-a` | Operar e ler somente as próprias transações |
| `provider-b` | `provider-b-secret` | Provedor `provider-b` | Operar e ler somente as próprias transações |

Os segredos acima são valores locais de desenvolvimento e não devem ser usados
em nenhum outro ambiente.

Obtenção de um token:

```bash
TOKEN=$(curl -s -X POST \
  http://localhost:8080/realms/wager/protocol/openid-connect/token \
  -d 'grant_type=client_credentials' \
  -d 'client_id=provider-a' \
  -d 'client_secret=provider-a-secret' | jq -r .access_token)
```

O token é enviado no cabeçalho `Authorization: Bearer $TOKEN`. A API valida a
assinatura localmente, usando as chaves públicas do realm, e verifica emissor,
expiração e audiência. A identidade do provedor vem da claim `azp` do token, e
nunca de um cabeçalho enviado pelo cliente.

No Swagger UI, use o botão **Authorize** e informe `Bearer <token>`.

### Modo de desenvolvimento sem identidade

Definir `OIDC_ENABLED=false` faz o serviço aceitar os cabeçalhos
`X-Provider-Id` e `X-Service-Role: internal` sem nenhuma verificação. Isso
existe apenas para desenvolvimento local sem o Keycloak e registra um aviso no
log a cada inicialização. Não deve ser usado em qualquer ambiente alcançável
por uma rede não confiável.

## Exemplos de chamadas

Os exemplos assumem `INTERNAL` e `PROVIDER_A` como tokens obtidos conforme a
seção anterior.

### Abrir uma carteira

```bash
curl -X POST http://localhost:8081/wallets \
  -H "Authorization: Bearer $INTERNAL" \
  -H 'Content-Type: application/json' \
  -d '{
        "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
        "initialBalance": { "amount": "1000.00", "currency": "BRL" }
      }'
```

```json
{
  "id": "0192f291-27dd-7d3f-8071-5f8685deef37",
  "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
  "balance": { "amount": "1000.00", "currency": "BRL" },
  "version": 1
}
```

### Enviar uma operação

```bash
curl -X POST http://localhost:8081/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER_A" \
  -H 'Idempotency-Key: provider-a:transaction-123' \
  -H 'Content-Type: application/json' \
  -d '{
        "providerId": "provider-a",
        "externalTransactionId": "transaction-123",
        "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
        "walletId": "0192f291-27dd-7d3f-8071-5f8685deef37",
        "roundId": "round-987",
        "gameId": "fortune-chimp",
        "kind": "BET",
        "money": { "amount": "25.00", "currency": "BRL" }
      }'
```

```json
{
  "transactionId": "0192f298-345e-7e38-af88-e43f851a819d",
  "status": "PROCESSED",
  "balance": { "amount": "975.00", "currency": "BRL" },
  "idempotentReplay": false
}
```

Para reversões, acrescente `referenceExternalTransactionId` ao corpo e use
`kind` igual a `REFUND` ou `ROLLBACK`.

### Enviar uma operação pela fila

```bash
docker compose exec localstack awslocal sqs send-message \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
  --message-group-id 0192f291-27dd-7d3f-8071-5f8685deef37 \
  --message-deduplication-id msg-123 \
  --message-body '{
    "messageId": "msg-123",
    "type": "WagerTransactionRequested",
    "occurredAt": "2026-09-21T12:00:00.000Z",
    "data": {
      "providerId": "provider-a",
      "externalTransactionId": "transaction-456",
      "idempotencyKey": "provider-a:transaction-456",
      "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
      "walletId": "0192f291-27dd-7d3f-8071-5f8685deef37",
      "roundId": "round-987",
      "gameId": "fortune-chimp",
      "kind": "WIN",
      "money": { "amount": "50.00", "currency": "BRL" }
    }
  }'
```

### Demais endpoints

| Método e rota | Descrição |
| --- | --- |
| `GET /wallets/{walletId}` | Lê uma carteira. Restrito ao serviço interno |
| `GET /wallets/{walletId}/ledger?cursor=&limit=` | Pagina o extrato com cursor opaco |
| `POST /wallets/{walletId}/reconciliation` | Reconstrói o saldo pelo ledger e compara |
| `GET /wagering/transactions/{transactionId}` | Lê uma transação pelo identificador interno |
| `GET /providers/{providerId}/wagering/transactions/{externalTransactionId}` | Lê pelo identificador do provedor |

### Códigos de resposta

| Situação | HTTP | Código no corpo |
| --- | --- | --- |
| Operação concluída | 200 | — |
| Aceita, aguardando a referência | 202 | — |
| Entrada inválida | 400 | `INVALID_REQUEST` |
| Cabeçalho `Idempotency-Key` ausente | 400 | `MISSING_IDEMPOTENCY_KEY` |
| Sem credencial válida | 401 | `UNAUTHENTICATED` |
| Agindo por outro provedor | 403 | `FORBIDDEN` |
| Carteira ou transação inexistente | 404 | `NOT_FOUND` |
| Chave reutilizada com outro conteúdo | 409 | `IDEMPOTENCY_CONFLICT` |
| Carteira já existente para jogador e moeda | 409 | `WALLET_ALREADY_EXISTS` |
| Rejeição de negócio | 422 | o `failureCode` da operação |
| Indisponibilidade transitória | 503 | `TEMPORARILY_UNAVAILABLE` |

Rejeições de negócio devolvem o corpo de `WagerResponse` com `status` igual a
`REJECTED` e um `failureCode` estável. Os códigos possíveis são
`INSUFFICIENT_FUNDS`, `REVERSAL_INSUFFICIENT_FUNDS`, `REFERENCE_NOT_FOUND`,
`REFERENCE_NOT_PROCESSED`, `REFERENCE_MISMATCH`, `ALREADY_REVERSED`,
`WALLET_NOT_FOUND`, `CURRENCY_MISMATCH` e `INTERNAL_ERROR`.

## Migrations

No Docker Compose as migrations são aplicadas automaticamente pelo serviço
`migrate`, que roda uma vez e encerra. A aplicação não executa migrations na
inicialização, de modo que várias instâncias subindo ao mesmo tempo não
disputam alterações de schema.

Para aplicar ou reverter manualmente, com o CLI do `golang-migrate`:

```bash
export DATABASE_URL='postgres://wager_user:wager_password@localhost:5432/wager_db?sslmode=disable'

migrate -path ./migrations -database "$DATABASE_URL" up        # aplica tudo
migrate -path ./migrations -database "$DATABASE_URL" down 1    # reverte a última
migrate -path ./migrations -database "$DATABASE_URL" down -all # reverte tudo
migrate -path ./migrations -database "$DATABASE_URL" version   # versão atual
```

Sem instalar nada, pela imagem oficial:

```bash
docker run --rm -v "$PWD/migrations:/migrations" --network host \
  migrate/migrate:v4.17.1 -path=/migrations \
  -database 'postgres://wager_user:wager_password@localhost:5432/wager_db?sslmode=disable' up
```

Se uma migration falhar pela metade, o `migrate` marca a versão como suja e se
recusa a continuar. Corrija o SQL, execute
`migrate ... force <versão anterior>` e rode `up` novamente. O detalhe de cada
migration está em [migrations/README.md](migrations/README.md).

## Filas

O script [scripts/localstack/init-queues.sh](scripts/localstack/init-queues.sh)
roda assim que o LocalStack fica pronto e cria quatro filas FIFO:

| Fila | Papel |
| --- | --- |
| `wager-transactions.fifo` | Operações recebidas dos provedores |
| `wager-transactions-dlq.fifo` | Dead letter da fila acima |
| `wallet-events.fifo` | Eventos de integração publicados pelo serviço |
| `wallet-events-dlq.fifo` | Dead letter da fila acima |

A política de redrive move uma mensagem para a dead letter depois de cinco
recebimentos sem remoção. O visibility timeout das filas de trabalho é de
sessenta segundos, maior que o tempo máximo de tratamento de uma mensagem.

Para inspecionar:

```bash
docker compose exec localstack awslocal sqs list-queues

docker compose exec localstack awslocal sqs get-queue-attributes \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
  --attribute-names All
```

## Execução fora do Docker

Útil durante o desenvolvimento, com as dependências no Compose e a aplicação
no host:

```bash
docker compose up -d postgres keycloak_db keycloak localstack migrate
go run ./cmd/server
```

Os valores padrão já apontam para `localhost`, de modo que nenhuma variável
precisa ser definida. Se preferir partir de um arquivo, copie o exemplo:

```bash
cp .env.example .env
```

O arquivo `.env` não é lido automaticamente pela aplicação; exporte as
variáveis ou use uma ferramenta como `direnv`.

## Variáveis de ambiente

Todas as variáveis, seus padrões e o efeito de cada uma estão documentados em
[.env.example](.env.example). As que mais importam:

| Variável | Padrão | Efeito |
| --- | --- | --- |
| `DATABASE_URL` | `postgres://wager_user:...@localhost:5432/wager_db` | Conexão com o PostgreSQL |
| `HTTP_ADDRESS` | `:8081` | Endereço de escuta da API |
| `OIDC_ENABLED` | `true` | Desligar aceita cabeçalhos sem verificação |
| `OIDC_ISSUER` | `http://localhost:8080/realms/wager` | Emissor exigido em todo token |
| `OIDC_AUDIENCE` | `wallet-api` | Audiência exigida em todo token |
| `SQS_INBOUND_QUEUE_URL` | fila local | Fila de operações de entrada |
| `SQS_EVENTS_QUEUE_URL` | fila local | Destino dos eventos de saída |
| `CONSUMER_CONCURRENCY` | `4` | Mensagens tratadas em paralelo |
| `CONSUMER_VISIBILITY_TIMEOUT` | `60s` | Precisa ser maior que `CONSUMER_HANDLER_TIMEOUT` |
| `REFERENCE_MAX_ATTEMPTS` | `10` | Tentativas de resolver uma referência pendente |
| `REFERENCE_TTL` | `30m` | Prazo total de espera por uma referência |
| `SHUTDOWN_TIMEOUT` | `30s` | Prazo para os workers concluírem o trabalho em andamento |

A configuração é validada na inicialização. Combinações impossíveis, como um
visibility timeout menor que o tempo de tratamento, impedem o processo de subir
e produzem uma mensagem explicando o motivo.

## Testes

```bash
go vet ./...              # análise estática
go test ./...             # testes unitários
go test -race ./...       # testes unitários com o detector de corridas
```

Os testes unitários não dependem de nenhuma infraestrutura e rodam em segundos.

Os testes de integração exigem o ambiente de pé e são protegidos por build tag,
de modo que `go test ./...` nunca os executa por engano:

```bash
docker compose up -d postgres localstack migrate
go test -tags=integration -count=1 ./cmd/internal/test/integration/...
```

O guia completo de validação, incluindo os cenários de concorrência, múltiplas
instâncias e simulação de falhas, está em [TESTING.md](TESTING.md).

Observação sobre `-race`: o detector de corridas do Go exige cgo. No Windows é
necessário um compilador C instalado, por exemplo o MinGW-w64 ou o TDM-GCC.
Sem ele o comando falha com `-race requires cgo`.

## Observabilidade

**Logs.** Formato JSON, emitidos em `stdout`. Cada linha carrega os
identificadores disponíveis para rastrear a operação: `correlationId`,
`messageId`, `transactionId`, `walletId` e `providerId`. Credenciais e valores
monetários completos nunca são registrados.

```bash
docker compose logs -f api
```

**Métricas.** Formato Prometheus em `/metrics`.

| Métrica | Tipo | Significado |
| --- | --- | --- |
| `wager_operations_total` | contador | Operações por transporte, tipo e estado final |
| `wager_duplicates_total` | contador | Operações reconhecidas como repetição |
| `wager_conflicts_total` | contador | Conflitos de idempotência e de concorrência |
| `wager_retries_total` | contador | Tentativas reagendadas, por componente |
| `wager_dead_lettered_total` | contador | Mensagens deixadas para a dead letter |
| `wager_processing_duration_seconds` | histograma | Latência de processamento por transporte |
| `wager_outbox_lag_seconds` | medidor | Idade do evento não publicado mais antigo |
| `wager_outbox_pending_events` | medidor | Eventos confirmados e ainda não publicados |
| `wager_reconciliation_divergences_total` | contador | Reconciliações em que o saldo não bateu com o ledger |

A métrica mais importante é `wager_outbox_lag_seconds`. Ela permanece próxima
de zero enquanto a publicação acompanha o ritmo e cresce assim que ela para,
que é exatamente a falha invisível de fora, já que a API continua respondendo
normalmente.

**Health checks.** `GET /health/live` informa apenas que o processo está de pé
e não consulta nenhuma dependência, porque reiniciar o processo não resolve um
banco momentaneamente indisponível. `GET /health/ready` verifica o PostgreSQL e
o SQS e responde 503 quando algum deles não responde, o que retira a instância
do balanceamento sem reiniciá-la.

## Estrutura do projeto

```
cmd/
  server/                 Ponto de entrada
  internal/
    domain/               Regras de negócio: dinheiro, carteira, transação, ledger
    events/               Contratos de mensagem: envelope de saída e mensagem de entrada
    usecase/              Orquestração e as portas que a infraestrutura implementa
    config/               Configuração lida do ambiente e validada
    di/                   Composição com Uber Fx e ciclo de vida
    pkg/idempotency/      Hash canônico de JSON
    infra/
      http/               Rotas, handlers, middleware e autenticação
      postgres/           Todo o SQL do serviço
      messaging/          Consumidor e publicador SQS
      worker/             Publicador da outbox e resolvedor de referências
      metrics/            Coletores Prometheus
    test/integration/     Testes contra PostgreSQL e SQS reais
migrations/               Schema versionado
scripts/
  keycloak/               Realm importado automaticamente
  localstack/             Provisionamento das filas
docs/                     Especificação Swagger gerada
```

As dependências apontam sempre para dentro. O domínio não conhece PostgreSQL,
SQS nem Fx; os casos de uso declaram interfaces e a infraestrutura as
implementa. As decisões de arquitetura e suas justificativas estão em
[ARCHITECTURE.md](ARCHITECTURE.md).

### Regeneração da especificação Swagger

Os arquivos em `docs/` são gerados e não devem ser editados à mão. Depois de
alterar as anotações dos handlers:

```bash
go install github.com/swaggo/swag/cmd/swag@latest
swag init -g cmd/server/main.go -o docs --parseInternal --parseDepth 3
```
