# Decisões de arquitetura

Este documento registra as decisões técnicas do serviço, o motivo de cada uma,
as interpretações adotadas onde o enunciado admitia mais de uma leitura e o
trabalho que não foi concluído.

## Sumário

- [Organização e dependências](#organização-e-dependências)
- [Dinheiro](#dinheiro)
- [Transações SQL e delimitação](#transações-sql-e-delimitação)
- [Idempotência](#idempotência)
- [Locks e concorrência](#locks-e-concorrência)
- [Reversões](#reversões)
- [Referências pendentes](#referências-pendentes)
- [Inbox e outbox](#inbox-e-outbox)
- [Autenticação](#autenticação)
- [Autorização](#autorização)
- [Uso do Fx](#uso-do-fx)
- [Shutdown](#shutdown)
- [Contrato HTTP](#contrato-http)
- [Observabilidade](#observabilidade)
- [Interpretações adotadas](#interpretações-adotadas)
- [Limitações e trabalho não concluído](#limitações-e-trabalho-não-concluído)

## Organização e dependências

O projeto é dividido em camadas com uma única regra: as dependências apontam
para dentro. O domínio não conhece PostgreSQL, SQS nem Fx. Os casos de uso
declaram interfaces descrevendo o que precisam, e a infraestrutura as
implementa.

```
infra (http, postgres, messaging, worker) + di + config
                    |
                    v
              usecase (orquestração e portas)
                    |
                    v
        domain + events + pkg (regras e contratos)
```

A consequência prática é que a suíte unitária roda em segundos sem banco e sem
fila, porque o miolo do sistema não tem nenhuma amarra em tecnologia. As portas
são declaradas no consumidor, e não junto da implementação, que é o que mantém
a seta de dependência apontando de `infra` para `usecase`.

Os pacotes ficam sob `cmd/internal`. É uma organização menos comum em Go, onde
`cmd/` costuma conter apenas pontos de entrada, mas o enunciado deixa a
organização a critério do candidato e a fronteira `internal` cumpre o papel de
impedir importação externa.

## Dinheiro

**Decisão.** Todo valor monetário é um `int64` de centavos com escala fixa de
duas casas e um código ISO 4217, encapsulado no objeto de valor `Money`. Nunca
é ponto flutuante, em nenhuma camada.

**Motivo.** `0.1 + 0.2` não é `0.3` em ponto flutuante. Em um sistema
financeiro isso é dinheiro desaparecendo, e o enunciado trata cálculo monetário
em ponto flutuante como critério eliminatório.

**Consequências.**

- Toda operação que pode sair da faixa representável devolve
  `ErrArithmeticOverflow` em vez de dar a volta silenciosamente.
- Na entrada externa, apenas a forma canônica é aceita, por exemplo `25.00`.
  Sem sinal, sem zeros à esquerda, sem notação científica, exatamente duas
  casas decimais. Isso garante que o mesmo valor sempre produza o mesmo hash de
  idempotência.
- No banco, `BIGINT` em unidades menores. `NUMERIC` também seria exato, mas
  `BIGINT` mapeia para o tipo do domínio sem conversão.
- No contrato HTTP e nos eventos, uma string decimal com a moeda, nunca um
  número JSON, de modo que nenhum cliente perca precisão ao fazer o parse.

**Biblioteca de acesso ao banco.** `pgx` v5 com SQL explícito, sem ORM. As
transações, os locks e as constraints precisam permanecer visíveis e
verificáveis, o que um ORM tende a esconder. O mapeamento de `Money` é direto:
`amount_cents BIGINT` mais `currency CHAR(3)`, lido de volta por
`domain.NewMoney`.

## Transações SQL e delimitação

**Decisão.** Existe um único lugar no serviço que abre e confirma transações: o
tipo `UnitOfWork`, em `infra/postgres`. Ele inicia a transação, constrói todos
os repositórios sobre ela e os entrega ao caso de uso como um conjunto.

```go
type UnitOfWork interface {
    Within(ctx context.Context, fn func(ctx context.Context, repos Repositories) error) error
}
```

**Motivo.** A garantia de que saldo, ledger, estado da operação, inbox e outbox
são confirmados atomicamente deixa de depender da disciplina de quem escreve o
código e passa a ser uma propriedade da estrutura. Não existe API para obter um
repositório avulso: o único caminho até um deles é através do `Within`.

**Nível de isolamento.** `READ COMMITTED` para o fluxo de escrita. É suficiente
porque a carteira é tomada com um lock de linha, que serializa os escritores da
mesma carteira sem bloquear operações sobre outras. A exceção é a
reconciliação, que usa `REPEATABLE READ` em modo somente leitura: comparar um
saldo lido em um instante com lançamentos lidos em outro reportaria uma
diferença que nunca existiu.

**Rollback.** O `defer` do rollback é incondicional. Depois de um commit
bem-sucedido ele retorna `pgx.ErrTxClosed`, que é ignorado. A vantagem é que
qualquer saída da função, inclusive um panic ou um contexto cancelado, passa
pelo rollback, de modo que nenhum caminho deixa uma transação aberta.

## Idempotência

**Decisão.** Uma operação financeira é identificada por
`(providerId, externalTransactionId)`, e não pela chave de idempotência. A
chave é registrada como recebida e comparada, mas nunca substitui a identidade
da operação.

**Motivo.** O enunciado exige que a mesma operação não possa ser reaplicada com
outra chave. Se a identidade fosse a chave, um provedor reenviando a mesma
aposta com uma chave nova moveria dinheiro duas vezes.

**Hash de conteúdo.** Um SHA-256 sobre uma forma canônica em JSON dos campos de
negócio, identificado pelo algoritmo `sha256-canonical-json-v1`:

- As chaves são serializadas em ordem alfabética em todos os níveis.
- Espaços insignificantes são descartados.
- Números preservam o literal que o cliente enviou.
- Os campos nulos são removidos.
- Os metadados de transporte são removidos apenas no nível mais externo:
  `idempotencyKey`, `messageId`, `messageGroupId`, `messageDeduplicationId`,
  `sequenceNumber`, `receiptHandle`, `type`, `occurredAt`, `sentAt`,
  `timestamp`, `requestId`, `correlationId` e `traceId`.

**Equivalência entre transportes.** É o ponto central da decisão. Por HTTP o
hash cobre os bytes crus do corpo da requisição. Por SQS ele cobre os bytes
crus do objeto `data` da mensagem, e não a mensagem inteira. Como o corpo HTTP
carrega exatamente os mesmos campos de negócio que o `data`, e como a forma
canônica descarta os metadados de transporte, a mesma operação enviada pelos
dois caminhos produz o mesmo hash. Isso é verificado por teste unitário e
também por teste de integração contra o banco real.

**Comportamento.**

- Chave e conteúdo equivalentes: devolve o resultado persistido com
  `idempotentReplay: true`.
- Chave reutilizada com conteúdo diferente: conflito, HTTP 409. Responder com o
  resultado guardado esconderia o erro do cliente.
- Operação já concluída: o replay responde com o saldo observado no
  processamento original, e não com o saldo atual, que pode já ter se movido.

## Locks e concorrência

**Decisão.** Combinação de lock pessimista e controle otimista.

1. `SELECT ... FOR UPDATE` na carteira, dentro da transação de negócio.
2. `UPDATE wallets SET ... WHERE id = $4 AND version = $5`, condicionado à
   versão lida.
3. Retry limitado a três tentativas da transação inteira quando o passo 2 não
   afeta nenhuma linha.

**Motivo.** O lock de linha é o que faz duas apostas sobre a mesma carteira
decidirem uma depois da outra, em vez de ambas lerem o mesmo saldo. É lock de
linha e não de tabela, portanto carteiras diferentes continuam totalmente
paralelas.

A checagem de versão é a segunda linha de defesa. Se o lock falhar por qualquer
motivo, um lost update vira um erro visível, `ErrConcurrentUpdate`, em vez de
corrupção silenciosa. Um trigger no banco reforça a mesma regra: uma mudança de
saldo que não incremente a versão em exatamente um é recusada.

**Retry da transação inteira.** Repetir apenas a escrita seria incorreto. A
decisão de aceitar ou rejeitar a aposta foi tomada sobre um saldo que não
existe mais, então ela precisa ser tomada de novo sobre o saldo que existe.
Cada tentativa é uma transação nova, e a idempotência da operação é o que
impede que um replay aplique algo duas vezes.

**Limite de tentativas.** Três. Um retry só compensa enquanto a contenção é
momentânea; depois disso, falhar e deixar a mensagem voltar mais tarde
distribui a carga em vez de alimentá-la. Esgotadas as tentativas, o HTTP
responde 503 e o consumidor SQS deixa a mensagem voltar a ficar visível.

## Reversões

**Regras.** `REFUND` devolve integralmente uma `BET` processada. `ROLLBACK`
desfaz integralmente uma `BET`, `WIN` ou `REFUND` processada, com movimento
contrário ao original. Reversões parciais estão fora do escopo, portanto os
valores precisam ser iguais.

**Direção do ROLLBACK.** É a única operação cuja direção não é propriedade dela
mesma: é o oposto do que a transação referenciada fez, e só é conhecida depois
que a referência é resolvida. Um `ROLLBACK` de uma `BET` credita, de um `WIN`
debita.

**Concordância.** A operação e sua referência precisam concordar em provedor,
jogador, carteira, moeda, rodada e valor. Qualquer divergência resulta em
`REFERENCE_MISMATCH`.

**Uma reversão por referência.** Um índice único parcial sobre
`reference_transaction_id`, restrito a `state = 'PROCESSED'` e
`kind IN ('REFUND','ROLLBACK')`, impede que a mesma aposta receba um `REFUND` e
também um `ROLLBACK`. Como o índice não inclui o tipo, a combinação dos dois
sobre a mesma aposta é impossível, que é o que impede a devolução dupla do
mesmo débito. A aplicação verifica a mesma condição antes, para transformar a
corrida em uma resposta de negócio, `ALREADY_REVERSED`, em vez de um erro.

**Saldo insuficiente em reversão.** Recebe um código distinto,
`REVERSAL_INSUFFICIENT_FUNDS`, e não `INSUFFICIENT_FUNDS`. São situações
auditadas separadamente: uma é um jogador sem saldo, a outra é uma
inconsistência entre o provedor e a carteira.

## Referências pendentes

**Decisão.** Uma reversão cuja referência ainda não chegou é persistida em
`PENDING_REFERENCE` e retomada por um worker, com backoff exponencial e dois
limites simultâneos: número máximo de tentativas, padrão dez, e um prazo total
contado a partir da criação, padrão trinta minutos.

**Motivo dos dois limites.** Eles protegem contra falhas diferentes. O contador
de tentativas limita uma referência que nunca chega. O prazo limita um worker
que reiniciou tantas vezes que o contador nunca cresceu. Qualquer um dos dois
encerra a espera.

**Ao esgotar.** A operação é finalizada como `REJECTED` com o código
`REFERENCE_NOT_FOUND`, e o evento `WagerTransactionRejected` é produzido.

**Referência existente mas não concluída.** Se a referência existe e ainda está
pendente, a reversão continua esperando, porque rejeitá-la agora recusaria uma
operação que está prestes a se tornar válida. Se a referência terminou sem
sucesso, isto é, chegou a um estado terminal que não é `PROCESSED`, a reversão
é rejeitada com `REFERENCE_NOT_PROCESSED`, já que não há nada a desfazer.

**Retomada não é um caminho separado.** A linha armazenada contém todos os
campos da requisição original, então o worker reconstrói o comando a partir
dela e o submete ao mesmo fluxo que uma entrega nova percorreria. Resolução,
movimentação e eventos passam exatamente pelas mesmas regras.

**Conclusão da mensagem de entrada.** Conforme o enunciado permite, uma
mensagem que resultou em `PENDING_REFERENCE` é concluída e removida da fila
assim que a pendência está persistida. A continuidade passa a ser
responsabilidade do worker de referências, e é justamente por isso que ele é
indispensável: sem ele, nada retomaria aquelas operações.

**Controle de retry no schema.** A migration `000006` acrescenta
`reference_attempts`, `reference_next_attempt_at`, `reference_locked_by` e
`reference_locked_until` em `wager_transactions`. O desenho espelha o claim da
outbox de propósito: `FOR UPDATE SKIP LOCKED` permite que várias instâncias
peguem trabalho disjunto sem coordenação, e o lease faz o trabalho abandonado
por uma instância que morreu voltar a ficar disponível sozinho.

## Inbox e outbox

**O problema.** Não existe transação que abranja o PostgreSQL e o SQS. Publicar
antes do commit pode anunciar algo que sofreu rollback. Commitar e morrer antes
de publicar perde o evento. O enunciado trata publicação anterior ao commit
como critério eliminatório.

**Outbox.** O evento é gravado como uma linha, na mesma transação do saldo, do
ledger e do estado da operação. A publicação deixa de fazer parte da operação
financeira e passa a ser uma entrega assíncrona feita por um worker separado.

- O `event_id` é a chave primária e é preservado em toda republicação, que é
  como o consumidor reconhece uma repetição.
- O payload é um snapshot imutável, garantido por trigger. Um publisher só
  registra progresso, nunca reescreve o que o evento diz.
- O claim usa `FOR UPDATE SKIP LOCKED` em uma única instrução, de modo que
  vários publishers pegam conjuntos disjuntos sem bloquear uns aos outros.
- O claim é um lease com prazo. Um publisher que morre segurando eventos não
  trava nada: o prazo vence e outra instância assume.
- Todos os instantes comparados vêm de `now()` no banco, nunca do relógio de
  uma instância, de modo que uma máquina com relógio adiantado não consegue
  reivindicar trabalho antes da hora.
- Um evento nunca é descartado. Uma entrega que continua falhando continua
  voltando, com espera cada vez maior, porque perder um evento cujo registro
  foi confirmado é exatamente o que a outbox existe para evitar.

**Inbox.** A entrega do SQS é ao menos uma vez, então a mesma mensagem pode
chegar duas vezes por razões que nada têm a ver com o remetente. A identidade
da mensagem é registrada na mesma transação das alterações de domínio.

- A chave primária é `(consumerName, messageId)`, e o `messageId` é o do
  envelope da mensagem, conforme o enunciado exige.
- O hash do conteúdo é verificado em reentregas: o mesmo identificador com
  conteúdo diferente é conflito, não duplicata, e tratá-lo como repetição
  descartaria uma operação real em silêncio.
- Uma mensagem aceita e não concluída significa que o processo morreu antes do
  commit. Nada do que ele fez é visível, então o tratamento recomeça e a
  idempotência de domínio decide o que ainda falta fazer.

**Ordem no SQS FIFO.** O `MessageGroupId` dos eventos de saída é o identificador
do agregado, o que ordena os eventos de uma mesma carteira e mantém carteiras
distintas em paralelo, exatamente a mesma forma do lock de linha. O
`MessageDeduplicationId` é o `eventId`. A janela de cinco minutos do broker é
conveniência, não garantia: a garantia real continua sendo o `eventId` e a
inbox, porque cinco minutos é curto demais para uma falha longa.

**Contrato de roteamento e consumo.** Os eventos de saída são publicados em
`wallet-events.fifo`. Cada mensagem carrega os atributos `eventType` e
`correlationId`, de modo que um consumidor consegue filtrar ou rotear sem fazer
o parse do corpo. O corpo é o envelope completo, com `eventId`, `eventType`,
`version`, `aggregateType`, `aggregateId`, `correlationId`, `causationId`
opcional, `occurredAt` em RFC 3339 UTC e `data` tipado. Consumidores devem
deduplicar por `eventId` e, quando precisarem da ordem total de uma carteira,
usar o campo `walletVersion` de `WalletBalanceChanged`, que ordena os eventos
independentemente de agrupamento ou reentrega.

## Autenticação

**Decisão.** Keycloak como provedor de identidade, fluxo `client_credentials`,
validação local de JWT.

**Motivo da escolha.** É recomendado pelo enunciado, roda em contêiner, importa
um realm a partir de um arquivo versionado e implementa OIDC de forma completa,
o que dispensa qualquer emissão própria de token.

**Como a validação funciona.** O serviço lê os metadados do realm uma única vez
na inicialização e obtém as chaves públicas. A verificação de cada requisição é
local e não faz chamada de rede, o que é o que torna viável verificar todas as
requisições. Três checagens, cada uma fechando um buraco diferente:

- A assinatura prova que o token foi emitido pelo provedor e não forjado.
- A expiração prova que ele ainda vale, de modo que um token vazado para de
  funcionar.
- A audiência prova que o token foi emitido para este serviço, o que impede que
  um token emitido para outro serviço do mesmo realm seja reaproveitado aqui.

**Falha na inicialização.** Um provedor de identidade inalcançável impede o
processo de subir. Um serviço que não consegue verificar tokens não deveria
começar a aceitar requisições que teria de recusar de qualquer forma.

**Emissor dentro e fora do Docker.** O Keycloak é configurado com
`KC_HOSTNAME_URL=http://localhost:8080`, de modo que todo token declara o mesmo
emissor, seja ele solicitado a partir do host ou de outro contêiner. Como o
contêiner da API alcança o realm pelo nome `keycloak`, existe uma configuração
separada, `OIDC_DISCOVERY_URL`, que define apenas de onde as chaves são lidas.
O emissor de cada token continua sendo verificado contra `OIDC_ISSUER`.

**Modo de desenvolvimento.** Com `OIDC_ENABLED=false` o serviço confia nos
cabeçalhos `X-Provider-Id` e `X-Service-Role`. É um auxílio de desenvolvimento,
registra um aviso a cada inicialização e não deve ser usado em ambiente
alcançável por rede não confiável.

## Autorização

**Modelo.** Duas identidades possíveis, ambas derivadas exclusivamente do token
verificado.

| Identidade | Origem no token | Permissões |
| --- | --- | --- |
| Serviço interno | Role de realm `wallet-internal` | Abrir carteiras, ler qualquer carteira, paginar extratos, reconciliar |
| Provedor | Claim `azp` | Enviar operações em seu próprio nome e ler apenas as próprias transações |

**Motivo de usar uma role para o serviço interno.** Marcar o serviço interno por
uma role de realm, e não pelo nome do client, significa que acrescentar um
segundo client interno é uma questão de conceder a role, sem alterar código.

**Isolamento entre provedores.** É verificado na entrada e na saída. Um provedor
não consegue enviar uma operação sob outro nome, o que devolve 403, e não
consegue ler uma transação que não é dele. Nesse segundo caso a resposta é 404,
e não 403, deliberadamente: um provedor não deve conseguir descobrir que uma
transação existe testando identificadores.

**Operações de carteira.** Restritas ao serviço interno. Um provedor movimenta
dinheiro pelos endpoints de wagering e nunca toca em uma carteira diretamente,
o que impede que descubra o saldo de um jogador com quem não tem relação.

**Acesso à mensageria.** Controlado por credenciais e políticas do broker. O
consumidor mantém todas as validações de domínio independentemente de quem
tenha conseguido colocar uma mensagem na fila.

## Uso do Fx

**Decisão.** Toda a composição usa Uber Fx, organizada em seis módulos:
`observability`, `persistence`, `messaging`, `usecase`, `worker` e `http`.

**Motivo.** Sem um contêiner de injeção, o `main` seria uma sequência longa de
construções em ordem obrigatória, com o desligamento ordenado escrito à mão. O
Fx resolve o grafo por tipo, constrói sob demanda e desfaz na ordem inversa.

**Adaptação de interface.** Funções como
`func(uow *postgres.UnitOfWork) usecase.UnitOfWork { return uow }` são o ponto
exato em que o sistema declara qual implementação satisfaz cada porta. Trocar
de implementação é trocar essa linha.

**Validação do grafo.** Um teste chama `fx.ValidateApp`, que resolve o grafo sem
executar nenhum construtor. Um provider faltando é detectado em milissegundos,
sem precisar de banco nem de fila.

**Ordem de desligamento.** O Fx desfaz na ordem inversa da construção. Como o
pool de conexões é construído cedo e os workers depois, no desligamento os
workers param primeiro e o pool fecha por último, nunca o contrário.

## Shutdown

**Decisão.** `SIGINT` e `SIGTERM` disparam o desligamento ordenado, com prazo
configurável, padrão de trinta segundos.

**Servidor HTTP.** Para de aceitar conexões novas e aguarda as requisições em
andamento até o prazo.

**Consumidor SQS.** Para imediatamente de buscar mensagens. As mensagens que
estavam sendo tratadas continuam em um contexto próprio, desacoplado do sinal
de parada, porque interromper uma transação financeira pela metade é pior do
que demorar mais para encerrar. As mensagens já recebidas mas ainda não
iniciadas têm a visibilidade devolvida imediatamente, em vez de esperar o
visibility timeout.

**Workers.** Cada um recebe um contexto que é cancelado na parada e expõe um
canal que fecha quando o laço realmente retornou. Esperar por esse canal é a
diferença entre um desligamento que terminou o trabalho e um que apenas pediu
para terminar.

**Quando o prazo estoura.** O processo desiste de esperar e o trabalho em
andamento é recuperado por outra instância: uma mensagem não confirmada volta a
ficar visível e um evento reivindicado tem o lease vencido. Nenhum dos dois
casos perde trabalho.

## Contrato HTTP

**Roteador.** `net/http` puro, com os padrões de método introduzidos no Go 1.22.
Nenhuma dependência de roteador foi necessária.

**Cabeçalho de idempotência.** `Idempotency-Key` é obrigatório em
`POST /wagering/transactions` e é armazenado exatamente como recebido. O
servidor nunca calcula uma chave no lugar da que o cliente apresentou, de modo
que a resposta sempre se refere à chave efetivamente enviada.

**Hash sobre os bytes crus.** O corpo é lido uma vez e submetido ao hash antes
de qualquer parse, porque o hash precisa cobrir exatamente o que o cliente
enviou, e não uma reserialização do que o servidor entendeu.

**Paginação do extrato.** Cursor opaco, codificado em base64 sobre a sequência
de armazenamento `wallet_ledger.seq`. O cliente nunca conhece a chave de
ordenação, o que deixa a API livre para alterá-la. A sequência de uma linha
nunca muda depois de escrita, então um cursor obtido agora continua apontando
para o mesmo ponto depois que novos lançamentos são acrescentados.

**Reconciliação.** Somente leitura. Reporta a divergência na resposta, no log e
em uma métrica, e nunca corrige o saldo: uma correção automática destruiria a
evidência do que a causou.

**Mapeamento de erros.** Decidido em um único lugar, pela pergunta "o que o
cliente deve fazer em seguida". Um 4xx significa que a requisição precisa mudar
antes de valer a pena reenviar. Um 409 significa que a operação existe, mas não
como esta requisição a descreve. Um 422 significa que a requisição foi
entendida e recusada por regra de negócio, com um código estável. Um 202
significa que a resposta ainda não está pronta. Um 503 significa reenviar a
mesma coisa mais tarde. Uma rejeição nunca é 500: é uma decisão que o serviço
tomou e confirmou.

**Falhas internas.** Um 5xx nunca ecoa a mensagem original, que pode carregar
uma query, o nome de uma constraint ou uma string de conexão. O erro completo
vai para o log com o `correlationId`.

## Observabilidade

**Logs.** JSON em `stdout`, com os identificadores disponíveis em cada linha.
Credenciais e valores monetários completos nunca são registrados.

**Métricas.** Formato Prometheus em `/metrics`, em um registry próprio e não no
global, de modo que as métricas expostas sejam exatamente as que o serviço
declara. A mais importante é `wager_outbox_lag_seconds`: ela permanece próxima
de zero enquanto a publicação acompanha e cresce assim que ela para, que é a
falha invisível de fora, já que a API continua respondendo.

**Health checks.** Liveness não consulta nenhuma dependência, porque reiniciar o
processo não resolve um banco momentaneamente indisponível, e reiniciar todas as
instâncias ao mesmo tempo transformaria uma oscilação do banco em indisponibilidade
total. Readiness verifica PostgreSQL e SQS e responde 503, o que retira a
instância do balanceamento sem reiniciá-la.

## Interpretações adotadas

**`WIN` com referência.** O enunciado diz que um `WIN` pode informar uma aposta
da mesma rodada como referência, sem caracterizá-lo como reversão. A
interpretação adotada é que a referência de um `WIN` é informativa: ela é
persistida em `reference_external_transaction_id`, mas não é resolvida para uma
transação interna e não bloqueia o processamento. Um `WIN` nunca fica em
`PENDING_REFERENCE` por causa dela.

**Saldo em replay de rejeição.** Uma transação rejeitada não armazena saldo,
porque nenhuma movimentação ocorreu. O replay de uma rejeição devolve o
`failureCode` sem o campo `balance`, em vez de devolver `0.00`, que seria lido
como carteira vazia.

**Carteira de outro jogador.** Uma operação sobre uma carteira que existe mas
pertence a outro jogador é rejeitada com `WALLET_NOT_FOUND`. Informar que a
carteira existe permitiria a um provedor sondar quais carteiras existem para
jogadores com quem ele não tem relação.

**Correlação na retomada de referência pendente.** Quando o worker retoma uma
reversão, ele inicia uma correlação nova, porque a operação que originalmente
trouxe aquela reversão terminou há muito tempo. Os eventos produzidos
permanecem ligados à operação pelo `aggregateId`, que é o identificador da
transação.

**Ledger de partida simples.** O enunciado trata partidas dobradas como
diferencial opcional. O ledger implementado é de partida simples, com direção,
valor e os saldos antes e depois, validados aritmeticamente por constraint.

## Limitações e trabalho não concluído

**Cenários de falha não cobertos por teste automatizado.** Os seguintes
cenários foram verificados manualmente e estão documentados em `TESTING.md`,
mas não possuem teste automatizado: interrupção do consumidor entre o commit e
a remoção da mensagem, reinicialização da aplicação com pendências em aberto e
execução com três instâncias independentes. Os dois primeiros exigem matar o
processo em um ponto específico, o que é feito por script e não por `go test`.

**Sem testes de carga.** O enunciado os trata como diferencial opcional e eles
não foram realizados. Não há, portanto, números de throughput, p50, p95 ou p99.

**Sem tracing distribuído.** OpenTelemetry é tratado como diferencial opcional e
não foi integrado. O `correlationId` cumpre parcialmente o papel, permitindo
reconstruir um fluxo a partir dos logs.

**Sem coletor de métricas no ambiente local.** As métricas são expostas em
formato Prometheus, mas o Docker Compose não inclui um Prometheus nem um
Grafana. A verificação é feita lendo o endpoint diretamente.

**Relação entre provedor e carteira não é modelada.** O serviço verifica que a
carteira pertence ao jogador informado, mas não existe um vínculo explícito
entre provedor e carteira. Qualquer provedor autenticado pode operar sobre
qualquer carteira desde que informe o jogador correto. Modelar esse vínculo
exigiria uma tabela de relacionamento que o enunciado não descreve.

**Sem reprocessamento automático da dead letter queue.** Mensagens que chegam à
DLQ ficam lá para inspeção. Não há ferramenta de redirecionamento de volta para
a fila principal depois de corrigido o problema.
