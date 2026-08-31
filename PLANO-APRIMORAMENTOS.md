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

Cobertura após esta rodada: `config` 88,8% · `merge` 86,2% · `export` 84,9% ·
`store` 68,1% · `wa` 29,4% · `cmd` 13,5%.

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

### B2 — Corpus de fixtures reais anonimizadas · impacto alto · esforço médio

**Problema.** `internal/wa` está em 29,4%. `toStoreMessage` e `ingestHistory`
não têm teste porque exigem `*events.Message` e `*whatsmeow.Client` reais.

**Proposta.** Capturar uma vez, de uma sessão real, os protobufs de mensagens e
de history sync; anonimizar (JIDs, nomes, corpos) e versionar em `testdata/`.
Desbloqueia testar o caminho de ingestão com o que o WhatsApp realmente manda,
que é diferente do que se imagina ao montar fixtures à mão.

**Cuidados.** Exige decisão de privacidade do usuário: o corpus sai de conversas
reais de terceiros. Anonimização precisa ser verificada antes de commitar.
Alternativa mais barata: refatorar `ingestHistory` para receber a função de
parse por parâmetro, tornando-a testável sem cliente.

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

## Descartados

| Ideia | Por quê |
|---|---|
| Piso numérico de cobertura | Incentiva teste de fachada; alvos nomeados funcionam melhor |
| Mock do socket do whatsmeow | Custo altíssimo e confiança falsa — ver "O que não é testável" no CLAUDE.md |
| Fuzzing | Superfície de parsing pequena demais para o retorno |
