# Guia de testes e validação

Este documento descreve como preparar o ambiente, executar cada camada de
testes e validar manualmente as garantias do serviço. Todos os comandos foram
executados contra o ambiente do Docker Compose.

## Sumário

- [Preparação do ambiente](#preparação-do-ambiente)
- [Testes unitários](#testes-unitários)
- [Testes de integração](#testes-de-integração)
- [Validação manual da API](#validação-manual-da-api)
- [Autenticação e isolamento](#autenticação-e-isolamento)
- [Idempotência](#idempotência)
- [Entrada por SQS](#entrada-por-sqs)
- [Outbox e publicação](#outbox-e-publicação)
- [Referências pendentes](#referências-pendentes)
- [Concorrência com múltiplas instâncias](#concorrência-com-múltiplas-instâncias)
- [Simulação de falhas](#simulação-de-falhas)
- [Reconciliação](#reconciliação)
- [Observabilidade](#observabilidade)
- [Checklist de validação](#checklist-de-validação)

## Preparação do ambiente

```bash
docker compose up -d --build
```

Aguarde até que todos os serviços estejam saudáveis:

```bash
docker compose ps
curl http://localhost:8081/health/ready
```

A resposta esperada é
`{"status":"ok","checks":{"postgres":"ok","sqs":"ok"}}`.

Para começar de um estado limpo, descartando dados de execuções anteriores:

```bash
docker compose down -v
docker compose up -d --build
```

### Função auxiliar para tokens

Os exemplos abaixo usam esta função. Defina-a uma vez na sessão do shell:

```bash
tok() {
  curl -s -X POST http://localhost:8080/realms/wager/protocol/openid-connect/token \
    -d 'grant_type=client_credentials' \
    -d "client_id=$1" -d "client_secret=$1-secret" \
    | grep -oE '"access_token":"[^"]+' | cut -d'"' -f4
}

INTERNAL=$(tok wallet-internal)
PROVIDER_A=$(tok provider-a)
PROVIDER_B=$(tok provider-b)
```

Os tokens expiram em cinco minutos. Basta chamar a função novamente.

## Testes unitários

Não dependem de nenhuma infraestrutura e rodam em segundos.

```bash
go vet ./...
go test ./...
go test -race ./...
```

Cobrem parsing e operações de `Money`, escala, limites numéricos, entradas
inválidas, incompatibilidade de moedas, invariantes da carteira, transições de
estado, as regras dos cinco tipos externos, a política de valor zero de cada
tipo, a abertura interna com seus metadados e eventos, o conflito de payload
para a mesma chave, o contrato dos eventos, o mapeamento de erros HTTP, as
regras de isolamento entre provedores, o backoff e o orçamento de tentativas
dos workers, além da resolução do grafo de dependências do Fx.

Sobre `-race`: o detector de corridas do Go exige cgo. No Windows é necessário
um compilador C instalado, por exemplo MinGW-w64 ou TDM-GCC. Sem ele o comando
falha com `-race requires cgo`.

## Testes de integração

Exigem PostgreSQL e LocalStack de pé. São protegidos por build tag, de modo que
`go test ./...` nunca os executa por engano.

```bash
docker compose up -d postgres localstack migrate
go test -tags=integration -count=1 -v ./cmd/internal/test/integration/...
```

Se o ambiente não estiver disponível, os testes são marcados como `SKIP` com
uma mensagem explicando o que falta, em vez de falharem.

Se todos forem pulados com `password authentication failed for user
"wager_user"`, outro PostgreSQL está ocupando a porta 5432, normalmente o
container de outro projeto. O banco deste projeto então nem chega a subir. Para
identificar e liberar a porta:

```bash
docker ps --format "{{.Names}}: {{.Ports}}" | grep 5432
docker stop <nome-do-container>
docker compose up -d
```

Para rodar com o detector de corridas, que é onde os testes de concorrência
mais importam, acrescente `-race`. Isso exige um compilador C, conforme a seção
anterior:

```bash
go test -race -tags=integration -count=1 ./cmd/internal/test/integration/...
```

Eles nunca truncam nada, porque o ledger recusa `DELETE` e `TRUNCATE` por
design. Cada teste trabalha com identificadores próprios e verifica apenas as
linhas que criou, o que permite executá-los repetidamente contra um banco que
já contém dados.

O que cada um verifica:

| Teste | Garantia |
| --- | --- |
| `TestDuasApostasDisputandoOMesmoSaldo` | Cenário obrigatório: 100.00 recebe duas apostas de 80.00, uma passa, uma é rejeitada, saldo final 20.00, um único débito no ledger |
| `TestMesmaApostaCinquentaVezesEmParalelo` | A mesma aposta enviada 50 vezes em paralelo produz um único débito |
| `TestCarteirasDiferentesProcessamEmParalelo` | O lock é de linha, não de tabela: carteiras distintas não se bloqueiam |
| `TestDoisPublishersPegamEventosDisjuntos` | `FOR UPDATE SKIP LOCKED` faz publishers concorrentes pegarem conjuntos disjuntos |
| `TestLeaseVencidoLiberaTrabalhoAbandonado` | Eventos presos por um publisher que morreu voltam a ficar disponíveis |
| `TestLedgerRecusaUpdateEDelete` | O ledger é append-only no banco |
| `TestBancoRecusaSaldoNegativo` | Última linha de defesa contra saldo negativo |
| `TestSaldoNaoMudaSemMoverAVersao` | Um lost update vira erro visível |
| `TestOutboxRecusaReescritaDoPayload` | O snapshot do evento é imutável |
| `TestUmaUnicaAberturaPorCarteira` | O crédito inicial não pode ser duplicado |
| `TestUmaCarteiraPorJogadorEMoeda` | Um jogador tem uma carteira por moeda |
| `TestTransacaoTerminalNaoMudaMais` | Uma transação terminal não aceita transição |
| `TestLedgerRecusaAritmeticaQueNaoFecha` | `saldoDepois = saldoAntes ± valor` |
| `TestReentregaDaMesmaMensagemMoveDinheiroUmaVez` | Deduplicação pela inbox |
| `TestMesmoMessageIdComConteudoDiferenteEConflito` | Mesmo identificador com outro conteúdo é conflito |
| `TestMesmaOperacaoPorHTTPEPorSQSEUmMovimentoSo` | Equivalência entre os dois transportes |
| `TestEventoPublicadoChegaLegivelNaFila` | O evento sobrevive à viagem até o SQS e volta legível |
| `TestReconciliacaoFechaDepoisDeVariasOperacoes` | Saldo armazenado bate com créditos menos débitos |

Execução esperada:

```
--- PASS: TestDuasApostasDisputandoOMesmoSaldo (0.07s)
    concurrency_test.go:157: de 50 envios: 1 aplicado, 49 replays idempotentes, 0 conflitos transitorios
--- PASS: TestMesmaApostaCinquentaVezesEmParalelo (0.18s)
...
ok      github.com/FrancoBorba/distributed-wallet-api/cmd/internal/test/integration
```

## Validação manual da API

A forma mais confortável é pelo Swagger UI, em
<http://localhost:8081/swagger/index.html>. Use o botão **Authorize** e informe
`Bearer <token>`.

Pela linha de comando, o roteiro completo:

```bash
PLAYER=$(uuidgen)   # no Windows: powershell -c "[guid]::NewGuid().ToString()"

# 1. Abrir a carteira com 1000.00
WALLET=$(curl -s -X POST http://localhost:8081/wallets \
  -H "Authorization: Bearer $INTERNAL" -H 'Content-Type: application/json' \
  -d "{\"playerId\":\"$PLAYER\",\"initialBalance\":{\"amount\":\"1000.00\",\"currency\":\"BRL\"}}")
echo "$WALLET"

WID=$(echo "$WALLET" | grep -oE '"id":"[^"]+' | cut -d'"' -f4)

# 2. Segunda abertura para o mesmo jogador e moeda: conflito
curl -s -o /dev/null -w "%{http_code}\n" -X POST http://localhost:8081/wallets \
  -H "Authorization: Bearer $INTERNAL" -H 'Content-Type: application/json' \
  -d "{\"playerId\":\"$PLAYER\",\"initialBalance\":{\"amount\":\"500.00\",\"currency\":\"BRL\"}}"
# esperado: 409

# 3. Aposta de 25.00
BET="{\"providerId\":\"provider-a\",\"externalTransactionId\":\"t-1\",\"playerId\":\"$PLAYER\",\"walletId\":\"$WID\",\"roundId\":\"r-1\",\"gameId\":\"g\",\"kind\":\"BET\",\"money\":{\"amount\":\"25.00\",\"currency\":\"BRL\"}}"

curl -s -X POST http://localhost:8081/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER_A" -H 'Idempotency-Key: provider-a:t-1' \
  -H 'Content-Type: application/json' -d "$BET"
# esperado: status PROCESSED, balance 975.00, idempotentReplay false

# 4. Aposta sem saldo
curl -s -w "\n%{http_code}\n" -X POST http://localhost:8081/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER_A" -H 'Idempotency-Key: provider-a:t-2' \
  -H 'Content-Type: application/json' \
  -d "$(echo "$BET" | sed 's/25.00/5000.00/; s/t-1/t-2/')"
# esperado: 422, failureCode INSUFFICIENT_FUNDS

# 5. Extrato com cursor opaco
curl -s "http://localhost:8081/wallets/$WID/ledger?limit=1" \
  -H "Authorization: Bearer $INTERNAL"
# esperado: uma entrada e um nextCursor que não revela a ordenação
```

Resultados observados nesta validação:

| Passo | Esperado | Observado |
| --- | --- | --- |
| Abertura com 1000.00 | 201, versão 1 | Confere |
| Abertura duplicada | 409 | Confere |
| Aposta de 25.00 | 200, saldo 975.00 | Confere |
| Aposta de 5000.00 | 422, `INSUFFICIENT_FUNDS` | Confere |
| Extrato com `limit=1` | Uma entrada e cursor opaco | Confere |

## Autenticação e isolamento

```bash
# Sem token
curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8081/wallets/$WID
# esperado: 401

# Cabeçalho antigo, sem token: não vale mais nada
curl -s -o /dev/null -w "%{http_code}\n" -H 'X-Service-Role: internal' \
  http://localhost:8081/wallets/$WID
# esperado: 401

# Provedor tentando abrir uma carteira
curl -s -o /dev/null -w "%{http_code}\n" -X POST http://localhost:8081/wallets \
  -H "Authorization: Bearer $PROVIDER_A" -H 'Content-Type: application/json' \
  -d "{\"playerId\":\"$PLAYER\",\"initialBalance\":{\"amount\":\"10.00\",\"currency\":\"BRL\"}}"
# esperado: 403

# provider-b enviando uma operação em nome de provider-a
curl -s -o /dev/null -w "%{http_code}\n" -X POST http://localhost:8081/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER_B" -H 'Idempotency-Key: provider-a:t-9' \
  -H 'Content-Type: application/json' -d "$(echo "$BET" | sed 's/t-1/t-9/')"
# esperado: 403

# provider-b lendo uma transação de provider-a
TX=$(curl -s "http://localhost:8081/providers/provider-a/wagering/transactions/t-1" \
  -H "Authorization: Bearer $PROVIDER_A" | grep -oE '"transactionId":"[^"]+' | cut -d'"' -f4)

curl -s -o /dev/null -w "%{http_code}\n" \
  "http://localhost:8081/wagering/transactions/$TX" -H "Authorization: Bearer $PROVIDER_B"
# esperado: 404, e não 403, para não permitir sondar identificadores
```

Todos os cinco casos foram observados conforme o esperado.

Para inspecionar as claims de um token:

```bash
echo "$PROVIDER_A" | cut -d. -f2 | base64 -d 2>/dev/null | head -c 400
```

O token de `provider-a` carrega `iss` igual a
`http://localhost:8080/realms/wager`, `aud` contendo `wallet-api` e `azp` igual
a `provider-a`. O de `wallet-internal` carrega a role de realm
`wallet-internal`.

## Idempotência

```bash
# Reenvio da mesma aposta com a mesma chave e o mesmo corpo
curl -s -X POST http://localhost:8081/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER_A" -H 'Idempotency-Key: provider-a:t-1' \
  -H 'Content-Type: application/json' -d "$BET"
# esperado: idempotentReplay true, mesmo transactionId, saldo 975.00

# Mesma chave com corpo diferente
curl -s -w "\n%{http_code}\n" -X POST http://localhost:8081/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER_A" -H 'Idempotency-Key: provider-a:t-1' \
  -H 'Content-Type: application/json' -d "$(echo "$BET" | sed 's/25.00/99.00/')"
# esperado: 409, IDEMPOTENCY_CONFLICT
```

Repare que o replay devolve o saldo observado no processamento original, e não o
saldo atual. Para comprovar, envie outras operações entre o envio e o reenvio: o
`balance` da resposta do replay continua sendo o daquele instante.

## Entrada por SQS

```bash
docker compose exec localstack awslocal sqs send-message \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
  --message-group-id "$WID" \
  --message-deduplication-id "msg-001" \
  --message-body "{\"messageId\":\"msg-001\",\"type\":\"WagerTransactionRequested\",\"occurredAt\":\"2026-09-21T12:00:00.000Z\",\"data\":{\"providerId\":\"provider-a\",\"externalTransactionId\":\"sqs-win-1\",\"idempotencyKey\":\"provider-a:sqs-win-1\",\"playerId\":\"$PLAYER\",\"walletId\":\"$WID\",\"roundId\":\"r-1\",\"gameId\":\"g\",\"kind\":\"WIN\",\"money\":{\"amount\":\"50.00\",\"currency\":\"BRL\"}}}"

sleep 5
curl -s "http://localhost:8081/wallets/$WID" -H "Authorization: Bearer $INTERNAL"
# esperado: saldo acrescido de 50.00
```

### Reentrega da mesma mensagem

Para contornar a janela de deduplicação do próprio SQS e exercitar a
deduplicação da aplicação, reenvie o mesmo corpo com um
`message-deduplication-id` diferente:

```bash
docker compose exec localstack awslocal sqs send-message \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
  --message-group-id "$WID" \
  --message-deduplication-id "forcando-reentrega-$RANDOM" \
  --message-body '<o mesmo corpo acima, com o mesmo messageId msg-001>'

sleep 5
docker compose exec postgres psql -U wager_user -d wager_db \
  -c "SELECT message_id, attempts, completed_at IS NOT NULL AS concluida FROM inbox;"
```

O esperado é `attempts` igual a 2 para `msg-001`, com o saldo inalterado: a
mensagem foi recebida duas vezes e o dinheiro se moveu uma vez.

### Equivalência entre transportes

Envie por HTTP a mesma operação que já entrou pela fila, usando o mesmo
`externalTransactionId`:

```bash
curl -s -X POST http://localhost:8081/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER_A" -H 'Idempotency-Key: provider-a:sqs-win-1' \
  -H 'Content-Type: application/json' \
  -d "{\"providerId\":\"provider-a\",\"externalTransactionId\":\"sqs-win-1\",\"playerId\":\"$PLAYER\",\"walletId\":\"$WID\",\"roundId\":\"r-1\",\"gameId\":\"g\",\"kind\":\"WIN\",\"money\":{\"amount\":\"50.00\",\"currency\":\"BRL\"}}"
# esperado: idempotentReplay true
```

Esta é a comprovação prática de que o hash canônico produz o mesmo valor pelos
dois caminhos. Observado conforme o esperado.

## Outbox e publicação

```bash
docker compose exec postgres psql -U wager_user -d wager_db -c \
  "SELECT event_type, aggregate_type, attempts, published_at IS NOT NULL AS publicado
     FROM outbox ORDER BY created_at DESC LIMIT 10;"
```

O esperado é `publicado` verdadeiro e `attempts` igual a 1 para todos, enquanto
o SQS estiver disponível. Uma abertura com saldo positivo grava dois eventos,
`WagerTransactionProcessed` e `WalletBalanceChanged`. Uma aposta rejeitada grava
apenas `WagerTransactionRejected`, sem `WalletBalanceChanged`, porque nenhum
saldo se moveu.

Para ler os eventos que chegaram à fila de saída:

```bash
docker compose exec localstack awslocal sqs receive-message \
  --queue-url http://localhost:4566/000000000000/wallet-events.fifo \
  --max-number-of-messages 10
```

### Interrupção entre o commit e a publicação

```bash
# Interrompe o SQS, deixando o banco de pé
docker compose stop localstack

# Envie uma operação: ela é confirmada no banco e o evento fica pendente
curl -s -X POST http://localhost:8081/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER_A" -H 'Idempotency-Key: provider-a:t-offline' \
  -H 'Content-Type: application/json' -d "$(echo "$BET" | sed 's/t-1/t-offline/')"
# esperado: 200. A operação financeira não depende do broker

docker compose exec postgres psql -U wager_user -d wager_db -c \
  "SELECT COUNT(*) AS pendentes FROM outbox WHERE published_at IS NULL;"
# esperado: maior que zero

curl -s http://localhost:8081/metrics | grep wager_outbox_lag_seconds
# esperado: crescendo

# Restabeleça o broker
docker compose start localstack
sleep 20

docker compose exec postgres psql -U wager_user -d wager_db -c \
  "SELECT COUNT(*) AS pendentes FROM outbox WHERE published_at IS NULL;"
# esperado: zero. Nenhum evento foi perdido
```

## Referências pendentes

### Reversão que chega antes da referência

```bash
# O REFUND chega primeiro, apontando para uma aposta que ainda não existe
curl -s -w "\n%{http_code}\n" -X POST http://localhost:8081/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER_A" -H 'Idempotency-Key: provider-a:refund-cedo' \
  -H 'Content-Type: application/json' \
  -d "{\"providerId\":\"provider-a\",\"externalTransactionId\":\"refund-cedo\",\"playerId\":\"$PLAYER\",\"walletId\":\"$WID\",\"roundId\":\"r-9\",\"gameId\":\"g\",\"kind\":\"REFUND\",\"money\":{\"amount\":\"40.00\",\"currency\":\"BRL\"},\"referenceExternalTransactionId\":\"bet-atrasada\"}"
# esperado: 202, status PENDING_REFERENCE

# Agora a aposta atrasada chega
curl -s -o /dev/null -w "%{http_code}\n" -X POST http://localhost:8081/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER_A" -H 'Idempotency-Key: provider-a:bet-atrasada' \
  -H 'Content-Type: application/json' \
  -d "{\"providerId\":\"provider-a\",\"externalTransactionId\":\"bet-atrasada\",\"playerId\":\"$PLAYER\",\"walletId\":\"$WID\",\"roundId\":\"r-9\",\"gameId\":\"g\",\"kind\":\"BET\",\"money\":{\"amount\":\"40.00\",\"currency\":\"BRL\"}}"
# esperado: 200

# O worker retoma a reversão sozinho
sleep 8
curl -s "http://localhost:8081/providers/provider-a/wagering/transactions/refund-cedo" \
  -H "Authorization: Bearer $PROVIDER_A"
# esperado: status PROCESSED
```

Observado nesta validação: o saldo antes da sequência e depois dela é idêntico,
porque a aposta debitou 40.00 e a reversão creditou os mesmos 40.00.

### Expiração por esgotamento do orçamento

Reinicie a API com limites curtos para observar a expiração em segundos:

```bash
docker compose stop api
REFERENCE_MAX_ATTEMPTS=3 REFERENCE_TTL=2m REFERENCE_POLL_INTERVAL=2s \
  REFERENCE_RETRY_BASE_DELAY=1s go run ./cmd/server
```

Em outro terminal:

```bash
curl -s -X POST http://localhost:8081/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER_A" -H 'Idempotency-Key: provider-a:refund-orfao' \
  -H 'Content-Type: application/json' \
  -d "{\"providerId\":\"provider-a\",\"externalTransactionId\":\"refund-orfao\",\"playerId\":\"$PLAYER\",\"walletId\":\"$WID\",\"roundId\":\"r-8\",\"gameId\":\"g\",\"kind\":\"REFUND\",\"money\":{\"amount\":\"25.00\",\"currency\":\"BRL\"},\"referenceExternalTransactionId\":\"nunca-vai-chegar\"}"
# esperado: 202, PENDING_REFERENCE

sleep 20
curl -s "http://localhost:8081/providers/provider-a/wagering/transactions/refund-orfao" \
  -H "Authorization: Bearer $PROVIDER_A"
# esperado: status REJECTED, failureCode REFERENCE_NOT_FOUND
```

Observado nesta validação: a transação foi expirada na quarta tentativa, com o
worker registrando `pending reference expired` e produzindo o evento
`WagerTransactionRejected`.

## Concorrência com múltiplas instâncias

```bash
docker compose up -d --scale api=3
docker compose ps api
```

As três instâncias recebem portas do intervalo publicado, uma cada. Cada uma é
um processo independente, com pool de conexões e memória próprios.

```bash
for p in 8081 8082 8083; do
  echo "porta $p: $(curl -s http://localhost:$p/health/ready)"
done
```

### Duas apostas de 80.00 sobre 100.00, em instâncias diferentes

```bash
PLAYER=$(uuidgen)
WID=$(curl -s -X POST http://localhost:8081/wallets \
  -H "Authorization: Bearer $INTERNAL" -H 'Content-Type: application/json' \
  -d "{\"playerId\":\"$PLAYER\",\"initialBalance\":{\"amount\":\"100.00\",\"currency\":\"BRL\"}}" \
  | grep -oE '"id":"[^"]+' | cut -d'"' -f4)

for i in 1 2; do
  PORT=$((8080+i))
  (curl -s -o "/tmp/r$i.json" -w "%{http_code}\n" \
     -X POST http://localhost:$PORT/wagering/transactions \
     -H "Authorization: Bearer $PROVIDER_A" \
     -H "Idempotency-Key: provider-a:multi-bet-$i" \
     -H 'Content-Type: application/json' \
     -d "{\"providerId\":\"provider-a\",\"externalTransactionId\":\"multi-bet-$i\",\"playerId\":\"$PLAYER\",\"walletId\":\"$WID\",\"roundId\":\"r1\",\"gameId\":\"g\",\"kind\":\"BET\",\"money\":{\"amount\":\"80.00\",\"currency\":\"BRL\"}}") &
done
wait

cat /tmp/r1.json /tmp/r2.json
curl -s "http://localhost:8081/wallets/$WID" -H "Authorization: Bearer $INTERNAL"
```

Resultado observado:

```
instancia 1 -> HTTP 200: "status":"PROCESSED"
instancia 2 -> HTTP 422: "status":"REJECTED" "failureCode":"INSUFFICIENT_FUNDS"
saldo final: 20.00
reconciliacao: "consistent":true
```

Uma aposta processada, uma rejeição por saldo insuficiente, saldo final de
20.00 e um único débito no ledger, com as duas decisões tomadas por processos
diferentes.

### Dois publishers disputando a mesma outbox

Com três instâncias no ar, cada uma roda seu próprio publicador da outbox
contra a mesma tabela. A verificação de que eles pegam conjuntos disjuntos é
automatizada em `TestDoisPublishersPegamEventosDisjuntos`. Para observar na
prática, gere carga e confira que nenhum evento foi publicado duas vezes:

```bash
docker compose exec postgres psql -U wager_user -d wager_db -c \
  "SELECT locked_by, COUNT(*) FROM outbox GROUP BY locked_by;"
```

## Simulação de falhas

### Interrupção do consumidor entre o commit e a remoção da mensagem

Este cenário depende de matar o processo em um ponto específico, então é feito
com o serviço rodando em primeiro plano.

```bash
# 1. Pare o consumidor da fila mantendo o restante de pé
docker compose stop api

# 2. Enfileire uma operação
docker compose exec localstack awslocal sqs send-message \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
  --message-group-id "$WID" --message-deduplication-id "falha-1" \
  --message-body '<mensagem de operação>'

# 3. Suba o serviço e mate-o assim que o log indicar a operação tratada
docker compose start api
docker compose logs -f api   # aguarde a linha "wager operation handled"
docker compose kill api      # sem SIGTERM: interrompe antes da remoção

# 4. Suba novamente e observe a reentrega
docker compose start api
sleep 10
docker compose exec postgres psql -U wager_user -d wager_db -c \
  "SELECT message_id, attempts FROM inbox ORDER BY received_at DESC LIMIT 1;"
```

O esperado é `attempts` maior que 1, indicando reentrega, com o saldo da
carteira inalterado em relação ao primeiro processamento.

### Indisponibilidade temporária do PostgreSQL

```bash
docker compose stop postgres

curl -s http://localhost:8081/health/ready
# esperado: 503 e postgres marcado como unavailable

curl -s -o /dev/null -w "%{http_code}\n" -X POST http://localhost:8081/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER_A" -H 'Idempotency-Key: provider-a:sem-banco' \
  -H 'Content-Type: application/json' -d "$BET"
# esperado: 503, TEMPORARILY_UNAVAILABLE

docker compose start postgres
sleep 10
curl -s http://localhost:8081/health/ready
# esperado: volta a ok, sem reiniciar o processo
```

O processo não é reiniciado durante a indisponibilidade, porque o liveness não
consulta dependências. Esse é o comportamento desejado: reiniciar todas as
instâncias por causa de uma oscilação do banco transformaria um problema
momentâneo em indisponibilidade total.

### Encerramento gracioso

```bash
docker compose stop api      # envia SIGTERM e aguarda
docker compose logs api | tail -20
```

O esperado é a sequência de parada registrada no log: o servidor HTTP para de
aceitar conexões, cada worker informa `worker stopped cleanly` e o pool de
conexões é fechado por último.

### Dead letter queue

Envie uma mensagem malformada e observe o percurso até a DLQ:

```bash
docker compose exec localstack awslocal sqs send-message \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
  --message-group-id "grupo-dlq" --message-deduplication-id "dlq-$RANDOM" \
  --message-body '{"messageId":"quebrada-1","type":"WagerTransactionRequested","occurredAt":"2026-09-21T12:00:00Z","data":{"providerId":""}}'

# Cinco recebimentos depois, a política de redrive move a mensagem
sleep 120
docker compose exec localstack awslocal sqs receive-message \
  --queue-url http://localhost:4566/000000000000/wager-transactions-dlq.fifo

curl -s http://localhost:8081/metrics | grep wager_dead_lettered_total
```

## Reconciliação

```bash
curl -s -X POST "http://localhost:8081/wallets/$WID/reconciliation" \
  -H "Authorization: Bearer $INTERNAL"
```

```json
{
  "walletId": "...",
  "storedBalance": { "amount": "975.00", "currency": "BRL" },
  "calculatedBalance": { "amount": "975.00", "currency": "BRL" },
  "difference": { "amount": "0.00", "currency": "BRL" },
  "consistent": true,
  "checkedEntries": 2
}
```

Para conferir todas as carteiras diretamente no banco, ao final de uma bateria
de testes:

```bash
docker compose exec postgres psql -U wager_user -d wager_db -c "
SELECT w.id,
       w.balance_cents AS armazenado,
       COALESCE(SUM(CASE WHEN l.direction = 'CREDIT' THEN l.amount_cents
                         ELSE -l.amount_cents END), 0) AS calculado
  FROM wallets w
  LEFT JOIN wallet_ledger l ON l.wallet_id = w.id
 GROUP BY w.id, w.balance_cents
HAVING w.balance_cents <> COALESCE(SUM(CASE WHEN l.direction = 'CREDIT' THEN l.amount_cents
                                            ELSE -l.amount_cents END), 0);"
```

O resultado esperado é nenhuma linha: toda carteira bate com seu ledger.

## Observabilidade

```bash
# Logs estruturados
docker compose logs -f api

# Seguir uma operação pelo correlationId
docker compose logs api | grep '<correlationId>'

# Métricas
curl -s http://localhost:8081/metrics | grep '^wager_'
```

Métricas relevantes durante uma bateria de testes:

```
wager_operations_total{kind="BET",state="PROCESSED",transport="http"}
wager_operations_total{kind="BET",state="REJECTED",transport="http"}
wager_duplicates_total{reason="business-identity",transport="http"}
wager_duplicates_total{reason="inbox",transport="sqs"}
wager_conflicts_total{kind="idempotency"}
wager_outbox_lag_seconds
wager_outbox_pending_events
wager_reconciliation_divergences_total
```

`wager_reconciliation_divergences_total` deve permanecer em zero. Qualquer valor
acima disso indica uma carteira cujo saldo o ledger não explica, e é o alarme
mais grave que o serviço produz.

## Checklist de validação

| Garantia | Como verificar | Situação |
| --- | --- | --- |
| Precisão monetária, sem ponto flutuante | `go test ./cmd/internal/domain/...` | Verificado |
| Ledger auditável e imutável | `TestLedgerRecusaUpdateEDelete` | Verificado |
| Saldo nunca negativo | `TestBancoRecusaSaldoNegativo` | Verificado |
| Sem movimentação duplicada | `TestMesmaApostaCinquentaVezesEmParalelo` | Verificado |
| Idempotência persistente | Seção [Idempotência](#idempotência) | Verificado |
| Conflito de payload detectado | 409 `IDEMPOTENCY_CONFLICT` | Verificado |
| Replay devolve o saldo original | Seção [Idempotência](#idempotência) | Verificado |
| Equivalência HTTP e SQS | `TestMesmaOperacaoPorHTTPEPorSQSEUmMovimentoSo` | Verificado |
| Duas apostas de 80.00 sobre 100.00 | `TestDuasApostasDisputandoOMesmoSaldo` e teste entre instâncias | Verificado |
| Carteiras distintas em paralelo | `TestCarteirasDiferentesProcessamEmParalelo` | Verificado |
| Três instâncias independentes | `docker compose up -d --scale api=3` | Verificado |
| Publicação nunca antes do commit | Estrutura do `UnitOfWork` e `TestOutboxRecusaReescritaDoPayload` | Verificado |
| Recuperação entre commit e publicação | Seção [Outbox](#outbox-e-publicação) | Verificado |
| Publishers concorrentes disjuntos | `TestDoisPublishersPegamEventosDisjuntos` | Verificado |
| Trabalho abandonado recuperado | `TestLeaseVencidoLiberaTrabalhoAbandonado` | Verificado |
| Deduplicação pela inbox | `TestReentregaDaMesmaMensagemMoveDinheiroUmaVez` | Verificado |
| Reversão fora de ordem resolvida | Seção [Referências pendentes](#referências-pendentes) | Verificado |
| Expiração com `REFERENCE_NOT_FOUND` | Seção [Referências pendentes](#referências-pendentes) | Verificado |
| Uma reversão por referência | `ALREADY_REVERSED` e índice único parcial | Verificado |
| Autenticação efetiva | Seção [Autenticação](#autenticação-e-isolamento) | Verificado |
| Isolamento entre provedores | 403 no envio, 404 na leitura | Verificado |
| Operações de carteira restritas | 403 para provedor | Verificado |
| Encerramento seguro | `docker compose stop api` e log de parada | Verificado |
| Reconciliação consistente | Seção [Reconciliação](#reconciliação) | Verificado |
| Ausência de corridas de dados | `go test -race` unitário e de integração | Verificado |
| Interrupção entre commit e remoção | Roteiro manual nesta página | Roteiro documentado, sem automação |
| Testes de carga | Diferencial opcional | Não realizado |
