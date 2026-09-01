# wapp-summarizer

Captura o WhatsApp via [whatsmeow](https://github.com/tulir/whatsmeow) numa máquina
pessoal e publica uma janela recente (padrão 3 dias) numa pasta privada em nuvem,
para que agentes de IA leiam e resumam **sem receber acesso à conta**. A barreira é
física: o agente lê arquivos, nunca a sessão.

Backlog em [PLANO-APRIMORAMENTOS.md](PLANO-APRIMORAMENTOS.md). O comando
`/proxima-fase` executa itens dele.

## Os dois invariantes

Tudo neste projeto existe para sustentar duas propriedades. Se uma mudança
ameaçar qualquer uma delas, pare e traga a decisão para o usuário.

### 1. Somente-leitura: nunca envia, responde, reage ou marca como lido

Verificado por `internal/wa/readonly_test.go`:

- `TestClientCallsAreAllowlisted` — `c.wa` é o único `*whatsmeow.Client` do
  módulo; todo **acesso** a ele precisa estar numa allowlist explícita.
- `TestNoWriteAPIAnywhere` — ~40 APIs de escrita banidas em qualquer arquivo.

Ambas inspecionam *referência*, não chamada: `x := c.wa.SendMessage` seguido de
`x(...)` escapa de uma verificação restrita a `CallExpr`. Não afrouxe isso para
`CallExpr` — `collectHistory` recebe `c.wa.ParseWebMessage` como method value,
então o padrão está em uso e o buraco seria real.
- `enforceReadOnly` fecha o único caminho de envio não solicitado: um *retry
  receipt* pedindo reenvio.

`c.wa.Download` é a única entrada da allowlist que vai *buscar* conteúdo, e é
gatilhada por config: sem `[media].enabled` e sem um `[[media.chat]]` casando
com a conversa, nunca é chamada. Continua leitura — não notifica, não marca como
lido, não emite recibo.

**Se `TestClientCallsAreAllowlisted` falhar, a pergunta certa não é "como faço
passar".** É: por que apareceu uma chamada nova ao cliente do WhatsApp? Adicionar
à allowlist é uma decisão consciente de ampliar o que o programa faz na conta —
nunca um reflexo para calar o teste.

### 2. Nunca sobrescrever com dado pior

Várias máquinas escrevem na mesma pasta de nuvem, sem coordenação, sobre uma
sincronização eventualmente consistente. Quatro mecanismos, em camadas:

1. Cada máquina só escreve arquivos com o próprio nome — `shards/<host_id>.jsonl`,
   `alertas/<host_id>.md` e `media/<host_id>/`. Sem arquivo mutável
   compartilhado, não existe *lost update*. Um `latest/ALERTA.md` comum seria a
   exceção que reabre a classe inteira; `TestAlertFileIsPerHost` e
   `TestMediaPathIsPerHost` travam isso.
2. Precedência por `prio`, não por ordem de chegada: o UPSERT tem
   `WHERE excluded.prio > messages.prio`.
3. O shard próprio se funde com a versão já publicada antes de subir.
4. `latest/` só é publicado se passar por: lease, schema compatível, todo shard
   conhecido legível, e monotonicidade do último timestamp.

**Mudar `store.Message.Rank()` exige bumpar `export.SchemaVersion`.** Prioridades
de fórmulas diferentes continuam sendo números comparáveis — nada quebra, o merge
só passa a escolher a versão errada da mensagem, em silêncio. Quem cobra isso é
`TestRankFormulaIsPinnedToSchemaVersion`, que fixa a tabela de prioridades e a
versão de schema no mesmo lugar.

`remote.Backend.Delete` é a **única operação destrutiva** do programa. Só é
chamada na poda de `media/<host_id>/`, só alcança arquivo da própria máquina, e
recusa caminho com `..` em duas camadas (`mediaNameFor` e `safeRelPath`) porque
o caminho vem de um JSONL que mora na pasta compartilhada.

## Comandos

```bash
go build -o wappsync.exe ./cmd/wappsync    # binário
go test ./... -count=1                     # suíte
gofmt -l . && go vet ./...                 # antes de qualquer commit
go test ./internal/export/ -run TestDigestGolden -update   # regravar o golden
```

## Armadilhas deste repo e desta máquina

- **Nunca edite arquivo com acento via PowerShell `Get-Content`/`Set-Content`.**
  A ida e volta corrompe UTF-8 (`Família` vira `FamÃ­lia`). Use as ferramentas
  Edit/Write, ou Python com encoding explícito. Já custou um arquivo inteiro.
- **`-race` não roda aqui**: exige cgo, e não há gcc. É o motivo principal da CI
  existir. `handleEvent` roda nas goroutines do whatsmeow em paralelo com o ciclo
  de export, então corrida é risco concreto.
- **Go 1.26+ obrigatório** (exigência do whatsmeow). Driver SQLite puro Go
  (`modernc.org/sqlite`): nenhum compilador C deve virar dependência.
- **O linker deixa `.exe~`**; já coberto pelo `.gitignore`, mas confira
  `git status` antes de commitar — 27 MB já escaparam uma vez.
- **`.gitignore` ignora `*.jsonl`**, com exceção de `**/testdata/**`. Fixture nova
  fora de `testdata/` some sem aviso.
- **Fim de linha.** O `.gitattributes` tem `* text=auto`, que no Windows converte
  para CRLF no checkout. Isso já quebrou duas coisas, e as duas estão corrigidas
  na raiz — mas fixture nova **fora** de `testdata/` precisa da mesma proteção:
  - `*.go text eol=lf` — sem isso o gofmt (que normaliza para LF) passa a listar
    o repositório inteiro depois de qualquer checkout, e `gofmt -l .` vira ruído.
  - `**/testdata/** -text` — goldens são comparados byte a byte; com CRLF eles
    quebram localmente e continuam **verdes na CI** (Linux), que é o pior modo
    de falha possível.
- **Não há ffmpeg nem whisper nesta máquina, e a CI também não instala.** Código
  que roda processo externo (`internal/transcribe`, e o `rclone` em
  `internal/remote`) é testado com binário falso no PATH: um script que registra
  os argumentos recebidos e imprime uma saída fixa. Isso cobre o que é nosso — a
  linha de comando montada e o parse da saída — sem fingir que cobre o resto.
- **`exec.CommandContext` sozinho não faz timeout funcionar.** Ele mata o filho,
  mas um neto herda o pipe de stdout e o `Wait` fica preso até ELE terminar.
  Sempre defina `cmd.WaitDelay`. Sem isso, um whisper travado segura a fila para
  sempre — e o teste do timeout demora 30s em vez de 200ms.
- **Nada de banco, sessão ou `config.toml` real versionado.**

## O que não é testável

A conexão real com o WhatsApp (login, recepção de eventos, history sync) exige
uma conta e não tem teste. Não tente cobrir isso com mocks elaborados do socket:
o custo é alto e a confiança que dá é falsa.

O caminho honesto é empurrar lógica para funções puras e testá-las com protobufs
montados à mão — como em `extract_test.go`. Ao mexer em `internal/wa`, prefira
extrair a lógica nova para uma função pura a enterrá-la no handler de eventos.

## Convenções

- Comentários, mensagens de erro, commits e documentação em **pt-BR**.
- Comentário explica **por quê**, não o quê.
- **Teste de guarda só conta depois de validado por mutação**: quebre
  deliberadamente o que ele protege e confirme que ele falha. Um teste de trava
  que passa nas duas situações é pior que nenhum, porque dá falsa segurança.
- Trabalhe em branch; `main` é a base dos PRs.
