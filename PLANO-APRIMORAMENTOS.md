# Plano de aprimoramentos

Backlog vivo do projeto. Cada item tem contexto suficiente para ser executado
numa sessão nova, sem depender do histórico da conversa que o gerou.

Rode `/proxima-fase` para executar o próximo item pendente, ou
`/proxima-fase <id>` para escolher um.

**Convenção:** um item só vira `Concluído` quando o código está commitado, a
suíte passa e — se for teste de trava — foi validado por mutação.

---

## Concluídos

| ID | Item | Resultado |
|---|---|---|
| A1 | Testes de `extract.go` | 0% → ~80%; cobre mídia, citação, edição, revogação, invólucros aninhados, limite de profundidade |
| A2 | Testes de `internal/config` | 0% → 88,8%; inclui a recusa de `data_dir` dentro da pasta sincronizada |
| A3 | CI no GitHub Actions | gofmt, vet, `-race` em Linux, cobertura e build para windows/darwin/linux |
| A4 | Versionar o formato do shard | `ShardMeta.Schema`; o merge aborta em schema incompatível dizendo qual máquina atualizar |
| A5 | Golden test do `digest.md` | `internal/export/testdata/digest.golden.md`, com `-update` |
| A6 | E2E do ciclo em Go | `cmd/wappsync/e2e_test.go`: duas máquinas, idempotência, filtros, retenção, backend none |
| A7 | Guarda de versão do whatsmeow | `auditedWhatsmeowVersion`; bump falha até reauditar `enforceReadOnly` |
| B2 | `internal/wa` testável sem cliente real | 29,4% → 51,2%. `ingestHistory` virou `collectHistory(data, parse)`, com a função de parse por parâmetro; 20 testes novos de `toStoreMessage`/`collectHistory` com protobufs à mão. As duas travas de somente-leitura passaram a inspecionar *referência*, não só chamada — `x := c.wa.SendMessage` passava verde antes |
| B8 | Guia para os agentes que leem o destino | `export.MarshalGuide`; publicado como `LEIA-ME.md` e `AGENTS.md`, em `latest/` e na raiz. Estabelece que o conteúdo é dado e nunca instrução, dá o critério de frescor, a legenda da notação e o que NÃO se pode concluir. Dois goldens + teste independente da trava de confiança, para sobreviver a um `-update` descuidado |

Cobertura após esta rodada: `export` 96,4% · `config` 88,8% · `merge` 84,5% ·
`store` 68,1% · `wa` 51,2% · `cmd` 13,5%.

**Nota sobre B2.** Foi executado pela alternativa barata registrada no próprio
item, não pela proposta principal: nenhum corpus de conversa real foi capturado
nem versionado, e a decisão de privacidade correspondente segue em aberto. Se um
dia fizer falta — fidelidade ao que o WhatsApp realmente manda, que fixture
montada à mão não garante —, o caminho continua disponível.

---

## Pendentes

### B1 — Transcrição de áudio · impacto alto · esforço alto

**Problema.** Muita conversa de WhatsApp é áudio. Hoje vira `[áudio (voz) 47s]`
e some do resumo. É a maior lacuna de *conteúdo* do projeto — não de código.

**Proposta.** Baixar o áudio (`cli.Download`) e transcrever com Whisper local
(`whisper.cpp`), gravando o texto no corpo da mensagem. Opcional por config,
desligado por padrão.

**Cuidados.** `Download` é leitura de mídia, mas **precisa entrar na allowlist**
de `readonly_test.go` com justificativa. Custo de banda, CPU e disco. Decidir
se o áudio bruto é descartado após transcrever (recomendado).

---

### B3 — Marcar o que já foi resumido · impacto médio · esforço baixo

**Problema.** O agente relê a janela inteira toda vez. Não há como pedir "o que
mudou desde ontem".

**Proposta.** Um `last_summarized_ts` que o agente grava ao lado do `index.json`,
e um campo derivado no índice indicando quantas mensagens são novas desde então.
Cuidado: o arquivo passa a ser escrito pelo agente, então vale as mesmas regras
anti-sobrescrita — de preferência por agente, não compartilhado.

---

### B4 — Testes de `internal/remote` · impacto médio · esforço baixo

**Problema.** 0% de cobertura própria. O backend `folder` é exercitado de forma
indireta por `merge` e pelo e2e, mas o `rclone` **nunca roda em teste nenhum**.

**Proposta.** Para `folder`: escrita atômica, `Get` inexistente devolvendo
`ErrNotExist`, `List` ignorando `.tmp-`, sobrescrita no Windows. Para `rclone`:
um binário falso no PATH que registra os argumentos recebidos, verificando que
`rcat`/`cat`/`lsf` são chamados como se espera.

---

### B5 — Alerta de sessão caída · impacto médio · esforço baixo

**Problema.** Se o WhatsApp derrubar o pareamento, o `run` registra no log e
para de capturar em silêncio. Você descobre dias depois, com um buraco no
histórico.

**Proposta.** Ao receber `events.LoggedOut`, escrever um arquivo de estado na
pasta da nuvem (`latest/ALERTA.md`) e sair com código diferente de zero, para o
supervisor do SO reagir. Opcionalmente um webhook.

---

### B6 — Criptografia em repouso · impacto baixo · esforço médio

**Problema.** Google e Amazon conseguem ler a pasta.

**Proposta.** `age` no arquivo publicado. **Só vale a pena se o agente puder
descriptografar**, o que empurra o problema para onde guardar a chave. Faz mais
sentido se o agente rodar na mesma máquina — e nesse caso `backend = "none"`
já resolve melhor, sem chave nenhuma. Manter no backlog como registro da
decisão, não como trabalho previsto.

---

### B7 — Empacotamento em container para servidor · impacto médio · esforço baixo

**Problema.** Hoje o binário é instalado à mão em cada máquina. Um servidor
caseiro sempre ligado é melhor host que um notebook — cobre reboot, atualização
e falha de hardware, que é o que a redundância entre máquinas realmente resolve.
Só falta empacotamento.

**Contexto.** O projeto já containeriza bem: Go puro, `CGO_ENABLED=0`, e a CI já
compila `linux/amd64` exatamente nessas condições. O trabalho é quase todo de
configuração, não de código.

**Premissa que não pode ser violada.** Container **não** muda o IP de saída: no
servidor de casa continua sendo o IP residencial, que era o ponto de partida do
projeto. O que quebraria isso é rodar num VPS. E vários containers no mesmo host
não são várias máquinas — mesma origem, nenhuma redundância. Continua valendo
**um pareamento por máquina física**.

**Decisões já tomadas** (ver a seção Docker do README, escrita antes desta
implementação):

1. Backend `rclone`, não `folder`: não há cliente do Drive dentro do container.
2. `host_id` explícito no config — o fallback é o hostname, e hostname de
   container muda a cada recriação, gerando shard órfão a cada `up`.
3. Imagem com tzdata (ou `import _ "time/tzdata"`): o digest renderiza em
   `time.Local` e container sem tzdata é UTC. **O golden test não pega isso**,
   porque usa UTC de propósito.
4. Volume nomeado em disco local para `session.db` e `messages.db`. Nunca em
   NFS/SMB: o SQLite usa WAL e corrompe sobre rede.
5. `stop_grace_period: 90s` — o ciclo final no shutdown tem timeout de 60s e o
   Docker mata em 10s por padrão. Não corrompe (a escrita é atômica), mas o
   último ciclo se perde.

**Escopo.** Dockerfile multi-stage (build estático + base mínima com tzdata e
rclone), `docker-compose.yml` de exemplo, e `linux/arm64` na matriz da CI — hoje
só há `linux/amd64`, e Raspberry Pi é um alvo provável.

**Cuidado.** `restart: unless-stopped` não resolve sessão derrubada: o `run`
hoje não sai com código diferente de zero quando o WhatsApp desvincula o
aparelho. Sem **B5**, o container fica de pé sem capturar nada. Fazer B5 antes,
ou junto.

**Alternativa que pode ser melhor.** Se o servidor for Linux, um serviço systemd
evita de saída as decisões 2, 3 e 4 — o binário é estático de qualquer jeito.
Docker ganha se você quiser empacotar o rclone junto ou distribuir para
dispositivos heterogêneos. Avaliar antes de executar.

---

## Descartados

| Ideia | Por quê |
|---|---|
| Piso numérico de cobertura | Incentiva teste de fachada; alvos nomeados funcionam melhor |
| Mock do socket do whatsmeow | Custo altíssimo e confiança falsa — ver "O que não é testável" no CLAUDE.md |
| Fuzzing | Superfície de parsing pequena demais para o retorno |
