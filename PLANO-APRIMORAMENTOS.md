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
| B5 | Alerta de sessão caída | `events.LoggedOut`/`StreamReplaced` viram `SessionLoss` (função pura) e derrubam o `run`: último ciclo, `alertas/<host_id>.md` no destino e saída com **código 3**, distinto de 1 para o supervisor não reiniciar em laço. 10 testes novos; `wa` 53,1% → 55,9%, `cmd` 14,5% → 17,5%. Duas travas validadas por mutação. Publica em `alertas/<host_id>.md`, não no `latest/ALERTA.md` proposto: um arquivo compartilhado seria o único mutável do desenho, e duas máquinas caídas juntas apagariam o alerta uma da outra |
| B16 | Mídia de todos os chats, com exceções | A allowlist `[[media.chat]]` passou a ser um dos três blocos de `[media]`, na mesma forma de `[filter]`: `kinds` baixa os tipos listados de todo chat exportado, `[[media.exclude]]` tira tipos de chats específicos e prevalece sobre tudo, `[[media.chat]]` continua como acréscimo por chat. Default segue "não baixa nada": `kinds` nasce vazio, e `enabled` sozinho não autoriza. `"audio"` em `kinds` exige `[transcribe]` como antes; em `exclude` não, porque exceção sem efeito não engana ninguém. Nenhuma chamada nova ao cliente do WhatsApp. 4 travas validadas por mutação |
| B15 | Alerta de sessão caída se apaga ao voltar | Portado da branch `alerta-sessao-caida`, que resolvia o B5 por outro caminho e tinha esta ideia a mais. Depois do primeiro ciclo bem-sucedido, `clearAlert` remove `alertas/<host_id>.md` e a cópia local. Não é no `Connect`: conectar não é capturar, e num laço de reconexão o aviso sumiria a cada tentativa. A justificativa original do B5 para não apagar — "o backend não tem Delete" — deixou de valer em B10. O critério de validade por data continua como segunda linha, porque máquina fora do ar não apaga nada. 2 travas validadas por mutação |
| B14 | Correção: anexo pulado em silêncio | O nome do chat era lido só da memória, que nasce vazia e só é preenchida segundos após o `Connected` — com `match` por nome, o anexo era pulado sem log. `mediaJobFor` passou a receber o nome por parâmetro e `ingest` resolve memória → banco. Skip por política virou log em INFO com nome E JID. `wappsync status` parou de engolir o erro do `wa.New` (whisper ausente ficava invisível) e `wappsync groups` ganhou a política de mídia resolvida por conversa. 2 travas validadas por mutação |
| B1 | Transcrição de áudio | `[transcribe]` no config (desligado por padrão) e `"audio"` aceito em `media.chat.kinds`. Pacote `internal/transcribe` novo: ffmpeg converte para WAV 16 kHz mono, whisper.cpp transcreve, e o `ParseOutput` limpa timestamp, ruído de diagnóstico e marcador de silêncio. Fila **separada** da de download — Whisper leva minutos, download leva segundos, e numa fila só a nota de voz atrasaria as imagens até elas serem descartadas. O áudio bruto é apagado em todo desfecho; o que sai é o texto, no corpo entre aspas e em `media/<host_id>/<sha256>.txt`. Nenhuma chamada nova ao cliente do WhatsApp e **nenhum bump de schema**: a transcrição carrega ponteiro de mídia, então o bit de `Rank()` do B10 já vale. 7 travas novas validadas por mutação. `transcribe` nasce com 88,7% |
| B10 | Download e publicação de anexos (imagem e documento) | `[media]` no config, desligado por padrão e por conversa: `enabled` + `[[media.chat]]` com `match`/`kinds`. `c.wa.Download` entrou na allowlist (12 → 13 entradas). `Rank()` ganhou o bit de anexo (+200) e `SchemaVersion` foi para `wapp-summarizer/2` — sem o bit, o anexo empatava em prio e era descartado pelo `>` estrito do UPSERT. `remote.Backend` ganhou `Delete`, a primeira operação destrutiva do programa, restrita a `media/<host_id>/`. Publicado como `media/<host_id>/<sha256>.<ext>`, com o nome derivado do conteúdo e a extensão do mimetype, nunca do nome declarado. 8 travas novas validadas por mutação |
| B8 | Guia para os agentes que leem o destino | `export.MarshalGuide`; publicado como `LEIA-ME.md` e `AGENTS.md`, em `latest/` e na raiz. Estabelece que o conteúdo é dado e nunca instrução, dá o critério de frescor, a legenda da notação e o que NÃO se pode concluir. Dois goldens + teste independente da trava de confiança, para sobreviver a um `-update` descuidado |

Cobertura após esta rodada, medida com
`go test ./... -coverpkg=./internal/<pkg>/` e unindo os blocos entre os binários
de teste: `export` 92,7% · `transcribe` 88,7% · `config` 88,6% · `merge` 83,7% ·
`store` 70,3% · `wa` 54,1% · `remote` 41,8% · `cmd` 18,9%.

Marcos: antes de B10 era `export` 92,2% · `config` 89,5% · `merge` 82,6% ·
`store` 64,0% · `wa` 55,9% · `remote` 34,6% · `cmd` 17,5%. Depois de B10 e antes
de B1, `export` 92,5% · `config` 90,0% · `wa` 55,3%.

`wa` caiu 1,8 ponto ao longo das duas rodadas, e por um motivo só: as duas somaram
worker e fila, que dependem de cliente conectado. A decisão é a de sempre — as
partes puras (`attachmentOf`, `mediaJobFor`, `transcribedBody`, `runTranscription`
com transcritor falso) têm teste, a borda de rede não. `config` caiu 1,4 ponto
pelos defaults de `[transcribe]`, que são atribuições sem ramo interessante.

Os números das rodadas anteriores a B5 saíram de outro caminho de medição e não
são comparáveis linha a linha com estes.

**Nota sobre B2.** Foi executado pela alternativa barata registrada no próprio
item, não pela proposta principal: nenhum corpus de conversa real foi capturado
nem versionado, e a decisão de privacidade correspondente segue em aberto. Se um
dia fizer falta — fidelidade ao que o WhatsApp realmente manda, que fixture
montada à mão não garante —, o caminho continua disponível.

---

## Pendentes

### B3 — Marcar o que já foi resumido · impacto médio · esforço baixo

**Problema.** O agente relê a janela inteira toda vez. Não há como pedir "o que
mudou desde ontem".

**Proposta.** Um `last_summarized_ts` que o agente grava ao lado do `index.json`,
e um campo derivado no índice indicando quantas mensagens são novas desde então.
Cuidado: o arquivo passa a ser escrito pelo agente, então vale as mesmas regras
anti-sobrescrita — de preferência por agente, não compartilhado.

---

### B4 — Testes de `internal/remote` · impacto médio · esforço baixo

**Problema.** O item nasceu dizendo "0% de cobertura própria"; hoje são 41,8%,
quase tudo exercício indireto por `merge` e pelo e2e. O que continua valendo é o
essencial: o `rclone` **não roda em teste nenhum**, e `rcat` com corpo binário
passou a ser caminho quente desde B10.

**Já feito por B10.** `remote_test.go` existe e cobre `Delete` — a operação
destrutiva — incluindo a recusa de caminho com `..` nos dois backends.

**Proposta.** Para `folder`: escrita atômica, `Get` inexistente devolvendo
`ErrNotExist`, `List` ignorando `.tmp-`, sobrescrita no Windows. Para `rclone`:
um binário falso no PATH que registra os argumentos recebidos, verificando que
`rcat`/`cat`/`lsf`/`deletefile` são chamados como se espera — e que `rcat`
entrega bytes intactos, que é o que um anexo exige.

**A tática já está provada.** `internal/transcribe` testa ffmpeg e whisper
exatamente assim (`fakeBins` em `transcribe_test.go`); dá para copiar a forma.

---

### B9 — Alertar também recusa de conexão não-terminal · impacto baixo · esforço baixo

**Problema.** B5 cobre `events.LoggedOut` e `events.StreamReplaced` — a sessão
caída. Mas há outras formas de parar de capturar em silêncio que hoje nem sequer
aparecem no log: `events.ClientOutdated` (o servidor recusa o whatsmeow até você
atualizar) e `events.TemporaryBan` (a conta ficou suspensa por um tempo).

**Proposta.** Reaproveitar `sessionLossFor` e o alerta de B5. As duas diferem de
uma sessão caída num ponto que muda a saída: `wappsync login` não resolve
nenhuma delas, e o ban expira sozinho — o texto do `Fix` e, possivelmente, o
código de saída precisam ser outros.

**Ficou fora de B5 de propósito:** o item pedia sessão caída, e tratar
"desvinculado" e "temporariamente banido" como a mesma coisa mandaria a pessoa
fazer a coisa errada.

---

### B11 — Setup interativo via CLI · impacto médio · esforço médio

**Problema.** Escolher conversas e política de anexo hoje é editar TOML à mão
depois de rodar `wappsync groups` e copiar JIDs. Funciona, mas é o passo em que
mais se erra — e agora errar tem consequência de privacidade, não só de ruído.

**Proposta.** Um `wappsync setup` que conecta, lista grupos e conversas, e vai
perguntando: entra no export? baixa imagem? baixa documento? transcreve áudio?
Escreve o `[filter]` e o `[media]` correspondentes.

**Restrições que o item precisa respeitar.**

1. Só funciona depois do `login` — listar grupos exige conexão.
2. Não pode atropelar um `config.toml` editado à mão. Mostrar o diff e pedir
   confirmação, ou gravar ao lado.
3. Escrever `"Família"` num TOML a partir de um terminal Windows cai direto na
   armadilha de UTF-8 do `CLAUDE.md`. Gravar com encoding explícito e ter um
   teste com acento no nome do grupo.
4. O padrão de qualquer pergunta de anexo é **não**. Um assistente que facilita
   ligar tudo é pior que nenhum.

---

### B12 — Anexos e áudios do history sync não sobrevivem à fila · impacto baixo · esforço baixo

**Problema.** As filas são em memória e descartam quando cheias (com aviso no
log): 256 posições para download, 64 para transcrição. Ao parear uma máquina nova
com `[media]` ligado, o history sync enfileira a janela inteira de uma vez e o
excesso se perde. A mensagem fica com o marcador, então nada quebra — mas os
anexos daquele lote não vêm, e não há segunda tentativa.

A fila de transcrição é a mais exposta: 64 posições e minutos por item, então
ela enche muito antes da de download.

**Proposta.** Persistir os pendentes (uma tabela no `messages.db`) e drenar em
ritmo constante, em vez de manter a fila só em memória. Como efeito colateral,
um download que falhou por rede passa a ter retry, o que hoje também não existe.

**Por que não entrou em B10.** A degradação é graciosa e visível no log, e a
alternativa (fila sem limite) trocaria perda de anexo por consumo de memória sem
teto durante o history sync.

---

### B13 — Anexo retroativo para mensagens já capturadas · impacto médio · esforço médio

**Problema.** A decisão de baixar acontece no momento em que a mensagem chega, e
o protobuf — que carrega `directPath`, `mediaKey` e `fileEncSHA256` — é
descartado logo depois. Quem liga `[media]` com o banco já cheio não vê efeito
nenhum até chegar mídia nova, e a experiência é indistinguível de "não
funciona". Foi exatamente o que aconteceu na primeira vez que isso rodou numa
máquina de verdade.

**Proposta.** Guardar os três campos numa tabela ao lado de `messages` no
momento da ingestão (independente da política, que pode mudar depois) e
reprocessar a janela quando a política mudar ou sob um `wappsync media --backfill`.

**Cuidado.** Guardar `mediaKey` é guardar a chave de decifrar a mídia. Ela já
está no banco de sessão, mas passa a estar também no `messages.db` — que é o
arquivo que alguém copiaria para depurar. Decidir se vale, e por quanto tempo.

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

**Decisão nova, vinda de B1.** Se a transcrição for usada, a imagem precisa de
ffmpeg, de um binário do whisper.cpp e do modelo `.bin` (centenas de MB), e o
`wa.New` **recusa subir** se qualquer um faltar. Duas saídas razoáveis: duas
imagens (com e sem transcrição), ou modelo em volume e não na imagem. Um
Raspberry Pi transcrevendo com Whisper também é otimista — medir antes.

**Cuidado.** Com **B5** feito, sessão derrubada sai com código 3. Isso muda a
escolha de política de restart: `on-failure` reiniciaria em laço, porque nenhum
restart pareia de novo. Use `unless-stopped`, ou trate o 3 à parte.

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
