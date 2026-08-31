# wapp-summarizer

Ponte entre o seu WhatsApp e uma pasta privada em nuvem, para que agentes de IA
(Hermes, OpenClaw, Claude, …) leiam e resumam suas conversas **sem receber acesso
à sua conta**.

O agente lê arquivos. Nunca a sessão. Essa é a barreira física.

```
 celular ──┐
           │  (dispositivo vinculado, IP residencial)
    ┌──────▼───────┐        ┌──────────────┐        ┌───────────────┐
    │ wappsync     │ SQLite │ shard próprio│ merge  │  latest/      │
    │ (seu PC)     ├───────►│ shards/<host>├───────►│  digest.md    │◄── agente
    └──────────────┘  local │   .jsonl     │        │  messages.jsonl│
                            └──────────────┘        │  index.json   │
    ┌──────────────┐                ▲               └───────────────┘
    │ wappsync     ├────────────────┘
    │ (seu Mac)    │
    └──────────────┘
```

---

## Por que este desenho

**Cada máquina é um aparelho vinculado próprio.** Você pareia o Windows, o Mac e
o WSL separadamente (o WhatsApp permite ~4 aparelhos). Cada um usa o IP
residencial da sua casa — nada sai de datacenter, que é o padrão que a Meta
costuma sinalizar. Se um computador estiver desligado, os outros continuam
capturando.

> **Nunca copie `session.db` entre máquinas.** Duas conexões com a mesma sessão
> derrubam uma à outra em loop, e é justamente o tipo de comportamento anômalo
> que chama atenção. Pareie cada máquina.

**Cada máquina só escreve o próprio arquivo.** É isso que resolve o problema de
sobrescrita: `shards/bruno-win.jsonl` e `shards/bruno-mac.jsonl` nunca colidem,
então não existe "lost update" no Drive. A visão que o agente lê (`latest/`) é
sempre reconstruída lendo **todos** os shards, o que torna a operação idempotente
e comutativa — dois merges simultâneos convergem para o mesmo resultado.

**Três travas impedem regressão** (detalhadas em [Anti-sobrescrita](#anti-sobrescrita)).

---

## Garantia de somente-leitura

Este programa **não consegue enviar nem responder mensagens**. Não é uma promessa
no README — é verificada por teste a cada `go test`.

Não envia mensagem, não responde, não reage, não edita, não apaga, não cria nem
altera grupos, não marca como lido e não mostra "digitando" ou "online".

### Como isso é garantido

**Allowlist de chamadas.** `c.wa` é o único `*whatsmeow.Client` do módulo.
`TestClientCallsAreAllowlisted` percorre a AST de todo o código-fonte e falha se
qualquer chamada nesse cliente não estiver numa lista explícita — hoje dez
entradas, todas leitura, conexão ou pareamento. Nada alcança a conta sem passar
por ali.

**Denylist de API.** `TestNoWriteAPIAnywhere` falha se `SendMessage`, `MarkRead`,
`SendChatPresence`, `BuildReaction`, `LeaveGroup` e outras ~40 operações de
escrita aparecerem em qualquer lugar do módulo, sob qualquer nome de variável.
Cobre o caso de alguém criar um segundo cliente.

Por serem baseados em AST e não em texto, os dois ignoram nomes que apareçam em
comentários ou strings — inclusive a própria denylist.

**Recusa de reenvio induzido.** Existe um caminho em que o whatsmeow enviaria
uma mensagem sem que a peçamos: um *retry receipt* de outro aparelho pedindo o
reenvio de algo. Na prática ele já morre sozinho (nunca enviamos nada, então não
há mensagem em cache), mas `enforceReadOnly` recusa explicitamente — `PreRetryCallback`
sempre retorna `false` e `GetMessageForRetry` sempre retorna `nil`. Depender de
um default do whatsmeow seria frágil.

Os testes foram validados por mutação: injetando `SendMessage`, `MarkRead` e
`SendChatPresence` no código, ambos falham e apontam arquivo e linha.

### O que ele *de fato* emite

Honestidade sobre o tráfego de saída — nada disso é mensagem, mas existe:

| Emitido | O que é | Visível para terceiros? |
|---|---|---|
| ACK de stanza | confirmação de protocolo de que o pacote chegou | não |
| Recibo de entrega | tipo `inactive`, porque nunca chamamos `SendPresence` | ✓✓ cinza, como qualquer aparelho vinculado |
| Retry receipt | pede ao remetente que reenvie o que não decriptou | não |
| ACK de history sync | confirma o recebimento do bloco de histórico | não |

Em particular: **não envia recibo de leitura** (o ✓✓ azul não aparece por causa
deste programa) e **nunca fica "online"**, porque `SendPresence` não é chamado em
lugar nenhum — nem por nós, nem internamente pelo whatsmeow.

As duas únicas operações que alteram a conta são `login` (vincula este aparelho)
e `logout` (desvincula), ambas invocadas explicitamente por você na linha de
comando. `logout` só remove acesso.

---

## Instalação

Requer **Go 1.26+** ([go.dev/dl](https://go.dev/dl/)). Não precisa de compilador C:
o driver SQLite é puro Go.

```bash
git clone https://github.com/bmar13/wapp-summarizer && cd wapp-summarizer && go build ./cmd/wappsync
```

Repita em cada máquina, ou compile cruzado a partir de uma só:

```bash
GOOS=windows GOARCH=amd64 go build -o wappsync.exe ./cmd/wappsync
```

```bash
GOOS=darwin GOARCH=arm64 go build -o wappsync-mac ./cmd/wappsync
```

---

## Uso

### 1. Configurar

```bash
./wappsync init
```

Isso cria um `config.toml` comentado. Edite pelo menos:

```toml
window_days = 3            # a janela que os agentes vão ler
host_id = "bruno-win"      # nome curto e estável desta máquina

[remote.folder]
path = 'G:/Meu Drive/wapp' # pasta que o app do Drive já sincroniza
```

### 2. Parear esta máquina

```bash
./wappsync login
```

Mostra um QR no terminal: **WhatsApp > Aparelhos conectados > Conectar aparelho**.

Se o terminal não renderizar o QR (SSH, por exemplo), pareie por número:

```bash
./wappsync login -phone +5511999999999
```

Para puxar ~1 ano de histórico em vez das últimas semanas:

```bash
./wappsync login -full-history
```

### 3. Deixar rodando

```bash
./wappsync run
```

Captura mensagens continuamente e publica a cada `interval_minutes`.
`Ctrl+C` faz um último ciclo antes de sair.

### 4. Conferir

```bash
./wappsync status
```

---

## O que os agentes leem

Aponte o agente para `<pasta>/wapp-summarizer/latest/`:

| Arquivo | Para quê |
|---|---|
| `index.json` | **Ler primeiro.** Janela coberta, contagens, lista de conversas, quais máquinas contribuíram e quando. É como o agente sabe se o dado está fresco. |
| `digest.md` | Histórico legível, agrupado por conversa e por dia. É o arquivo para resumos e briefings. |
| `messages.jsonl` | Uma mensagem por linha, canônico. Para filtrar, contar e processar. |
| `LEIA-ME.md` | Explica os arquivos acima, gerado junto. Um agente que abrir a pasta sem contexto se vira sozinho. |

Uma linha de `messages.jsonl`:

```json
{"id":"3EB0C4","chat":"120363@g.us","chat_name":"Família","group":true,"from_me":false,"sender":"5511999999999@s.whatsapp.net","sender_name":"João","ts":"2026-08-31T12:12:03Z","kind":"text","text":"chego 19h","_prio":111}
```

Mídia **nunca é baixada**. Uma foto vira `[imagem] legenda`, um áudio vira
`[áudio (voz) 34s]`. É o que interessa para um resumo, sem gigabytes na nuvem.

---

## Anti-sobrescrita

O problema: N máquinas escrevendo na mesma pasta, sem coordenação, sobre uma
sincronização eventualmente consistente. Quatro mecanismos, em camadas:

**1. Sem arquivo mutável compartilhado.** Cada máquina escreve só
`shards/<host_id>.jsonl`. Duas máquinas jamais escrevem no mesmo arquivo. Isso
elimina a classe inteira de "sobrescrevi com dado antigo" na origem.

**2. Precedência por `prio`, nunca por ordem de chegada.** Cada mensagem carrega
um `_prio` derivado de: é uma edição? foi apagada? tem corpo? veio ao vivo ou de
history sync? tem nome do remetente? No banco local o `UPSERT` tem
`WHERE excluded.prio > messages.prio` — um history sync antigo, com corpo vazio,
literalmente não consegue sobrescrever o que já foi capturado ao vivo. O mesmo
critério vale no merge entre máquinas, e por ser determinístico todas as máquinas
chegam ao mesmo resultado.

**3. Auto-recuperação do shard.** Antes de publicar, a máquina lê o próprio shard
já na nuvem e funde com o local. Se você apagar o banco local ou reparear, o que
estava publicado não se perde.

**4. Travas antes de publicar `latest/`:**

- *Lease cooperativo* — se outra máquina começou a consolidar há menos de
  `lease_minutes`, esta pula o ciclo. Não é um lock distribuído: é economia de
  trabalho. A corretude vem da idempotência, não daqui.
- *Completude* — todo shard que participou do consolidado anterior precisa estar
  legível agora. Se o Drive ainda não sincronizou o shard do Mac, a consolidação
  **aborta e mantém o `latest/` anterior**, em vez de publicar uma visão parcial.
- *Monotonicidade* — se a mensagem mais recente do novo consolidado for **anterior**
  à do consolidado publicado, aborta. Uma contagem menor é aceita (mensagens saem
  da janela de 3 dias naturalmente); um retrocesso no tempo, não.

Toda vez que uma trava dispara, o motivo aparece no log e o `latest/` anterior
permanece intacto.

---

## Configuração

Tudo em `config.toml`, sem recompilar. Os campos que você provavelmente vai mexer:

| Campo | Padrão | O quê |
|---|---|---|
| `window_days` | `3` | Janela publicada. É o número principal. |
| `retention_days` | `45` | Quanto guardar localmente. Manter alto permite aumentar `window_days` depois sem ter perdido nada. |
| `export.interval_minutes` | `10` | Frequência de publicação. |
| `filter.exclude` | `[]` | Conversas que nunca saem da máquina (nome ou JID). |
| `filter.include_only` | `[]` | Se preenchido, vira allowlist: só estas conversas são exportadas. |
| `privacy.redact_phone_numbers` | `false` | Troca telefones no corpo por `[TEL]`. |
| `privacy.redact_patterns` | `[]` | Regexes extras (CPF, cartão, token) → `[REDACTED]`. |
| `export.include_status_broadcast` | `false` | Status/stories. Costuma ser ruído. |

Para descobrir nomes exatos de grupos e preencher os filtros:

```bash
./wappsync groups
```

### Backends de nuvem

**`folder`** (padrão, recomendado): escreve numa pasta local que o cliente do
Google Drive, OneDrive ou Dropbox já sincroniza. Zero credenciais no script — a
autenticação é a do app oficial que você já tem instalado. A escrita é atômica
(arquivo temporário + rename), então o cliente de sync nunca sobe um arquivo pela
metade.

**`rclone`**: chama o binário `rclone` para falar direto com a API do Drive, S3,
B2, R2 etc. Use quando não houver cliente de desktop — servidor, WSL headless, ou
quando você quiser um bucket S3 de verdade.

```toml
[remote]
backend = "rclone"

[remote.rclone]
remote = "gdrive:wapp"        # ou "s3-privado:meu-bucket/wapp"
```

---

## Rodar como serviço

**Windows** (Agendador de Tarefas, ao logon):

```powershell
schtasks /create /tn wappsync /tr "C:\caminho\wappsync.exe run -config C:\caminho\config.toml" /sc onlogon /rl limited
```

**macOS** (LaunchAgent em `~/Library/LaunchAgents/com.wappsync.plist`, com
`RunAtLoad` e `KeepAlive`), ou simplesmente:

```bash
nohup ./wappsync run > ~/wappsync.log 2>&1 &
```

**Linux/WSL** (systemd user unit):

```bash
systemctl --user enable --now wappsync
```

---

## Rodar em Docker

> **Estado:** a imagem ainda não existe. Esta seção são as decisões de projeto,
> escritas antes da implementação — ver **B7** em
> [PLANO-APRIMORAMENTOS.md](PLANO-APRIMORAMENTOS.md). O `compose` abaixo é um
> ponto de partida, não algo já validado.

O projeto containeriza bem: Go puro com `CGO_ENABLED=0` (o driver SQLite é
`modernc.org/sqlite`), o que dá um binário estático. A CI já compila
`linux/amd64` exatamente nessas condições.

Um servidor de casa é, na verdade, o **melhor** host: fica sempre ligado, que é
justamente o ponto fraco de um notebook.

### O IP continua sendo o seu

O Docker faz NAT pela rede do host, então no servidor de casa a saída é o mesmo
IP residencial dos seus outros computadores. A premissa do projeto continua de
pé. Duas coisas a quebrariam:

- **Rodar num VPS** (Hetzner, DO, AWS): IP de datacenter, exatamente o que este
  desenho evita. Container não muda isso.
- **Vários containers no mesmo host**: são três aparelhos vinculados saindo da
  mesma origem. Não dá redundância nenhuma — uma falha do host derruba os três —
  e provavelmente chama mais atenção, não menos. Continua valendo **um
  pareamento por máquina física**.

### Seis armadilhas

**1. O backend padrão não serve.** `folder` pressupõe o app do Google Drive
sincronizando uma pasta local, que não existe dentro do container. Ou você monta
a pasta sincronizada do host, ou usa `backend = "rclone"` — que é o encaixe
natural aqui, com o `rclone.conf` montado como secret.

**2. Defina `host_id` explicitamente.** Quando vazio, ele cai no hostname — e
hostname de container é o ID, que muda a cada recriação. O resultado seria um
shard novo na nuvem a cada `docker compose up --force-recreate`, acumulando
arquivos órfãos. É a pegadinha mais fácil de não perceber.

**3. Fuso horário.** O digest é renderizado em `time.Local`, e container sem
tzdata é UTC — seu briefing mostraria 09:12 para uma mensagem das 06:12.
Precisa de `TZ=America/Sao_Paulo` **e** tzdata presente; numa imagem `scratch`
não há, então use uma base que tenha, ou embuta com `import _ "time/tzdata"`.
Os testes não pegam isso: o golden usa UTC de propósito.

**4. Volume persistente é obrigatório.** `session.db` é credencial. Sem volume,
todo restart perde o pareamento e queima mais um dos ~4 slots de aparelho
vinculado do WhatsApp. `messages.db` também: sem ele some a auto-recuperação do
shard descrita em [Anti-sobrescrita](#anti-sobrescrita).

**5. SQLite não pode ficar em share de rede.** O banco usa WAL, e WAL sobre
NFS/SMB corrompe. Em servidor caseiro é comum montar um NAS — aqui não dá. Disco
local ou volume nomeado. É a mesma lógica da validação que já existe no config
(`data_dir` não pode estar dentro da pasta sincronizada), estendida.

**6. `stop_grace_period`.** O ciclo final no shutdown tem timeout de 60s e o
Docker mata em 10s por padrão. Não corrompe nada — a escrita é atômica por
temp + rename — mas você perde o último ciclo.

### Esqueleto de compose

```yaml
services:
  wappsync:
    build: .
    container_name: wappsync
    restart: unless-stopped
    stop_grace_period: 90s
    environment:
      TZ: America/Sao_Paulo
    volumes:
      - wappsync-data:/data                                   # disco local, nunca NAS
      - ./config.toml:/etc/wappsync/config.toml:ro
      - ./rclone.conf:/root/.config/rclone/rclone.conf:ro
    command: ["run", "-config", "/etc/wappsync/config.toml"]

volumes:
  wappsync-data:
```

Com `host_id = "servidor-casa"`, `[paths].data_dir = "/data"` e
`[remote].backend = "rclone"` no `config.toml`.

O `login` é interativo uma única vez, porque precisa exibir o QR:

```bash
docker compose run --rm -it wappsync login -config /etc/wappsync/config.toml
```

Se o QR não renderizar bem, pareie por número com `-phone +5511999999999`.
Depois disso o `run` é daemon puro.

> `restart: unless-stopped` **não** cobre sessão derrubada: hoje o `run` não sai
> com código diferente de zero quando o WhatsApp desvincula o aparelho, então o
> container fica de pé sem capturar nada. É o item **B5** do plano.

### Talvez você não precise de Docker

Se o servidor for Linux, um serviço systemd é mais simples e evita de saída as
armadilhas 2, 3 e 4 — o binário é estático de qualquer forma. O Docker ganha se
você quiser empacotar o rclone junto, ou distribuir para vários dispositivos
heterogêneos.

Para ARM (Raspberry Pi e afins) falta adicionar `linux/arm64` à matriz da CI;
hoje ela cobre só `linux/amd64`.

---

## Riscos e limites

**Termos de uso.** O WhatsApp não oferece API de cliente para uso pessoal e os
Termos proíbem clientes não oficiais. O whatsmeow implementa o protocolo do
WhatsApp Web e o dispositivo aparece na lista de aparelhos conectados como
qualquer outro. Ainda assim, o risco de banimento existe e é seu. Rodar de IP
residencial, com poucos aparelhos e **sem enviar mensagens** (este projeto só lê)
é o perfil de menor risco — mas não é garantia.

**Privacidade de terceiros.** As mensagens incluem pessoas que não consentiram
com este processamento. A pasta deve ser privada, sem compartilhamento por link,
e o conteúdo não deve ser redistribuído.

**A pasta em nuvem é legível pelo provedor.** Google e Amazon conseguem ler o que
está lá. Se isso importa, veja "Sugestões" abaixo — mas note que criptografar
também impede o agente de ler, a menos que ele tenha a chave.

**Não é backup.** Mensagens fora da janela são removidas a cada ciclo.

---

## Sugestões e próximos passos

Coisas que fazem sentido, em ordem aproximada de retorno:

1. **Duas máquinas, não uma.** É o principal ganho de robustez: cobre desligamento,
   queda de internet e reboot, e a fusão por `prio` cuida da duplicidade.

2. **Comece com `filter.include_only`.** Em vez de exportar tudo e depois excluir,
   comece com os 5–10 grupos que realmente importam. Menos dado na nuvem, resumos
   melhores, e você amplia depois.

3. **Marcar o que já foi resumido.** Hoje o agente relê a janela inteira toda vez.
   Um `last_summarized_ts` gravado pelo agente ao lado do `index.json` permitiria
   resumos incrementais ("o que mudou desde ontem") em vez de sempre 3 dias.

4. **Transcrição de áudio.** Muita conversa de WhatsApp é áudio, e hoje vira
   `[áudio (voz) 34s]` — invisível para o resumo. Baixar o áudio e passar por
   Whisper local (`whisper.cpp`) resolveria, ao custo de banda e CPU. É a maior
   lacuna de conteúdo do projeto hoje.

5. **Criptografia em repouso.** `age` ou `gpg` no arquivo publicado protege contra
   o provedor de nuvem. Só vale a pena se o agente puder descriptografar — o que
   move o problema para "onde guardo a chave". Faz mais sentido se o agente roda
   na sua máquina.

6. **Modo somente-local.** `backend = "none"` já grava em `<data_dir>/out/`. Se
   os seus agentes rodam na mesma máquina, você não precisa de nuvem nenhuma — e
   a superfície de exposição cai a zero.

7. **Alerta de sessão caída.** Se o WhatsApp derrubar o pareamento, o `run`
   registra no log e para de capturar silenciosamente. Um webhook ou notificação
   quando `events.LoggedOut` chega evitaria descobrir dias depois.

---

## Estrutura

```
cmd/wappsync/       CLI e loop principal
internal/config/    config.toml (+ exemplo embutido)
internal/store/     SQLite local, UPSERT por prio
internal/wa/        whatsmeow: pareamento, eventos, history sync, extração de texto
internal/export/    Record, JSONL, digest Markdown, index
internal/remote/    backends folder e rclone
internal/merge/     fusão entre máquinas e as travas anti-regressão
```

Baseado em [tulir/whatsmeow](https://github.com/tulir/whatsmeow).
