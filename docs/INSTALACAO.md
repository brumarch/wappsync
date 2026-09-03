# Instalação do wappsync

Passo a passo para colocar o `wappsync` para rodar em **Windows**, **macOS**,
**Linux** e **WSL**: o que instalar, como compilar, como configurar e como
deixar rodando sozinho.

O que o programa faz, por que é somente-leitura e o que sai da sua máquina
está no [README](../README.md). Este guia é só o "como".

Validação: os passos de Windows e de Linux/WSL foram executados pelo autor
(Windows 11 + WSL Ubuntu 26.04). Os de macOS foram conferidos contra a
documentação do Homebrew e do whisper.cpp, não executados — se algo divergir,
abra uma issue.

---

## 1. O que você precisa

| | Obrigatório? | Para quê |
|---|---|---|
| **Go 1.26+** | sim | compilar. Não precisa de compilador C: o SQLite é puro Go |
| **git** | sim | clonar o repositório |
| Uma pasta sincronizada (Google Drive, OneDrive, Dropbox…) **ou** `rclone` | uma das duas | levar os arquivos para a nuvem. A pasta sincronizada é o caminho simples: o app do provedor já roda na máquina |
| `ffmpeg` + `whisper-cli` + um modelo `ggml-*.bin` | só para transcrever áudio | nota de voz é Opus; o whisper.cpp só lê WAV. Sem transcrição, nada disso é necessário |

Regra prática: **comece sem transcrição**. Go, git e a pasta sincronizada
bastam para ver texto chegando na nuvem em dez minutos. Áudio vem depois.

O binário se chama `wappsync` (`wappsync.exe` no Windows). Ele lê um
`config.toml` no diretório atual, ou em `~/.wappsync/config.toml`, ou no que
você passar com `-config`.

---

## 2. Windows

Abra o PowerShell (não precisa ser administrador).

### Dependências

```powershell
winget install --id GoLang.Go -e
winget install --id Git.Git -e
```

Feche e reabra o terminal para o `PATH` valer. Confira:

```powershell
go version     # go1.26 ou mais novo
git --version
```

Opcionais, só se for transcrever áudio:

```powershell
winget install --id Gyan.FFmpeg -e
```

Para o Whisper não há pacote: baixe o pré-compilado. Em
<https://github.com/ggml-org/whisper.cpp/releases> pegue `whisper-bin-x64.zip`
(CPU comum; há variantes `cublas` para GPU NVIDIA) e extraia em `C:\whisper`.
Dentro há uma pasta `Release` com `whisper-cli.exe` e várias DLLs — **elas
precisam ficar juntas**, não copie só o `.exe`. Depois o modelo:

```powershell
curl.exe -L -o C:\whisper\ggml-small.bin https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-small.bin
```

São ~465 MB. `small` é o equilíbrio para português; `base` é mais rápido e
pior, `medium` é melhor e lento.

Como `C:\whisper\Release` não está no `PATH`, aponte o caminho completo no
`config.toml` mais adiante (`binary = 'C:/whisper/Release/whisper-cli.exe'`).

Só se for usar o backend `rclone` em vez de uma pasta sincronizada:

```powershell
winget install --id Rclone.Rclone -e
```

### Compilar

```powershell
git clone https://github.com/brumarch/wappsync
cd wappsync
go build -o wappsync.exe .\cmd\wappsync
```

A primeira compilação baixa as dependências (~1 minuto). Confira com
`.\wappsync.exe help`. Copie o `wappsync.exe` para onde quiser deixá-lo —
por exemplo `C:\wappsync\` — e siga para [Primeiros passos](#7-primeiros-passos).

### Rodar como serviço

Agendador de Tarefas, ao logon, com o `-config` em caminho absoluto (a tarefa
não herda o diretório atual) e a saída num log. Criar uma tarefa "ao logon"
exige **PowerShell como administrador**; o `--%` faz o PowerShell passar o
resto da linha ao `schtasks` sem interpretar o `>>`:

```powershell
schtasks --% /create /tn wappsync /sc onlogon /rl limited /tr "cmd /c \"C:\wappsync\wappsync.exe run -config C:\wappsync\config.toml >> C:\wappsync\wappsync.log 2>&1\""
```

Para começar agora sem relogar: `schtasks /run /tn wappsync`. Para parar:
`schtasks /end /tn wappsync`. Para remover: `schtasks /delete /tn wappsync /f`.

Assim a tarefa mostra uma janela de console enquanto roda. Para escondê-la,
abra `taskschd.msc` → propriedades da tarefa → "Executar estando o usuário
conectado ou não" (pede a sua senha, para a tarefa rodar sem sessão).

O Agendador não reinicia o processo sozinho se ele cair; se quiser isso, abra a
tarefa em `taskschd.msc` → Configurações → "Se a tarefa falhar, reiniciar a
cada…". Não marque "reiniciar" indiscriminadamente: quando o WhatsApp
desvincula a máquina o `run` sai com código 3 de propósito, e reiniciar não
resolve — só `wappsync login` resolve.

---

## 3. macOS

Requer o [Homebrew](https://brew.sh).

### Dependências

```bash
brew install go git
```

Opcionais, só para transcrever áudio:

```bash
brew install ffmpeg whisper-cpp
```

O pacote `whisper-cpp` instala o `whisper-cli` no `PATH`. O modelo:

```bash
mkdir -p ~/whisper
curl -L -o ~/whisper/ggml-small.bin https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-small.bin
```

Só se for usar o backend `rclone`: `brew install rclone`.

### Compilar

```bash
git clone https://github.com/brumarch/wappsync
cd wappsync
go build -o wappsync ./cmd/wappsync
mkdir -p ~/.local/bin && cp wappsync ~/.local/bin/
```

Garanta que `~/.local/bin` está no `PATH` (`echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zshrc`).
Como o binário foi compilado aqui, o Gatekeeper não reclama. Siga para
[Primeiros passos](#7-primeiros-passos).

### Rodar como serviço

LaunchAgent em `~/Library/LaunchAgents/com.wappsync.plist` (troque `SEU_USUARIO`):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.wappsync</string>
  <key>ProgramArguments</key>
  <array>
    <string>/Users/SEU_USUARIO/.local/bin/wappsync</string>
    <string>run</string>
    <string>-config</string>
    <string>/Users/SEU_USUARIO/.wappsync/config.toml</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key>
  <dict><key>SuccessfulExit</key><false/></dict>
  <key>ThrottleInterval</key><integer>300</integer>
  <key>StandardOutPath</key><string>/Users/SEU_USUARIO/.wappsync/wappsync.log</string>
  <key>StandardErrorPath</key><string>/Users/SEU_USUARIO/.wappsync/wappsync.log</string>
</dict>
</plist>
```

```bash
launchctl load ~/Library/LaunchAgents/com.wappsync.plist     # liga
launchctl unload ~/Library/LaunchAgents/com.wappsync.plist   # desliga
```

O launchd não tem como excluir o código de saída 3 do reinício (ao contrário
do systemd). Por isso o `ThrottleInterval` de 5 minutos: se o WhatsApp
desvincular a máquina, o processo vai subir, avisar de novo e sair, a cada 5
minutos, até você rodar `wappsync login`. O alerta em `alertas/<host_id>.md`
na nuvem continua sendo o sinal para olhar.

---

## 4. Linux

Instruções para Debian/Ubuntu e Fedora; outras distribuições, adapte o
gerenciador de pacotes.

### Dependências

**Ubuntu 26.04+** tem Go 1.26 no repositório:

```bash
sudo apt install golang-go git
```

Em Ubuntu mais antigo e no Debian estável o `golang-go` é velho demais. Use o
tarball oficial (troque a versão pela mais recente em <https://go.dev/dl/>):

```bash
curl -fsSL https://go.dev/dl/go1.27.1.linux-amd64.tar.gz | sudo tar -C /usr/local -xz
echo 'export PATH="/usr/local/go/bin:$HOME/.local/bin:$PATH"' >> ~/.bashrc
source ~/.bashrc
```

**Fedora**: `sudo dnf install golang git`.

Opcionais, só para transcrever áudio. O ffmpeg:

```bash
sudo apt install ffmpeg          # Debian/Ubuntu
sudo dnf install ffmpeg-free     # Fedora (o pacote "ffmpeg" completo exige RPM Fusion)
```

O Whisper tem pré-compilado para Ubuntu x86-64 e arm64 (testado no 26.04):

```bash
sudo mkdir -p /opt/whisper && cd /opt/whisper
sudo curl -L -o whisper.tar.gz https://github.com/ggml-org/whisper.cpp/releases/latest/download/whisper-bin-ubuntu-x64.tar.gz
sudo tar xzf whisper.tar.gz --strip-components=1 && sudo rm whisper.tar.gz
sudo curl -L -o ggml-small.bin https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-small.bin
/opt/whisper/whisper-cli --help | head -2     # tem que imprimir "usage:"
```

O `whisper-cli` carrega as `.so` da própria pasta, então não o mova sozinho —
aponte o caminho completo no config (`binary = '/opt/whisper/whisper-cli'`).
Se o pré-compilado não rodar na sua distribuição (glibc antiga, outra
arquitetura), compile — não validado pelo autor, segue a documentação do
projeto:

```bash
sudo apt install cmake build-essential
git clone https://github.com/ggml-org/whisper.cpp && cd whisper.cpp
cmake -B build -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF
cmake --build build -j --config Release
sudo install build/bin/whisper-cli /usr/local/bin/
```

Só se for usar o backend `rclone`: `sudo -v && curl https://rclone.org/install.sh | sudo bash`.

### Compilar

```bash
git clone https://github.com/brumarch/wappsync
cd wappsync
go build -o wappsync ./cmd/wappsync
mkdir -p ~/.local/bin && install wappsync ~/.local/bin/
```

Confira com `wappsync help` e siga para [Primeiros passos](#7-primeiros-passos).

### Rodar como serviço

Unit de usuário em `~/.config/systemd/user/wappsync.service`:

```ini
[Unit]
Description=wappsync - captura do WhatsApp para a pasta em nuvem
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%h/.local/bin/wappsync run -config %h/.wappsync/config.toml
Restart=always
RestartSec=30
# Código 3 = o WhatsApp desvinculou esta máquina. Reiniciar não resolve;
# só `wappsync login` resolve. Sem esta linha o serviço entraria em laço.
RestartPreventExitStatus=3

[Install]
WantedBy=default.target
```

```bash
systemctl --user daemon-reload
systemctl --user enable --now wappsync
systemctl --user status wappsync
journalctl --user -u wappsync -f          # log
sudo loginctl enable-linger $USER         # continua rodando sem você logado
```

---

## 5. WSL

Antes de tudo: **numa máquina Windows, o caminho natural é o binário Windows**
([seção 2](#2-windows)). Use o WSL se você prefere as ferramentas Linux — e
aceite três particularidades:

1. **A nuvem sincroniza no Windows, não no WSL.** O app do Drive/OneDrive roda
   do lado de fora. Aponte o backend `folder` para a pasta montada:
   `path = '/mnt/c/Users/SEU_USUARIO/Google Drive/wapp'`. Com OneDrive, marque
   essa pasta como "Sempre manter neste dispositivo" — com "Arquivos sob
   demanda", o que o WSL escreve pode não subir e o que ele lê pode não estar
   lá. A alternativa que ignora tudo isso é o backend `rclone`.
2. **Serviço exige systemd ligado.** Confira `/etc/wsl.conf`:
   ```ini
   [boot]
   systemd=true
   ```
   Se precisou adicionar, rode `wsl --shutdown` no PowerShell e reabra.
   `systemctl is-system-running` tem que responder `running`.
3. **O WSL desliga quando você fecha o último terminal**, levando o serviço
   junto. Para mantê-lo vivo sem terminal aberto, uma tarefa do Agendador do
   Windows ao logon que segura a distribuição:
   ```powershell
   schtasks /create /tn wsl-keepalive /sc onlogon /rl limited /tr "wsl.exe -d Ubuntu --exec sleep infinity"
   ```
   (PowerShell como administrador, pelo mesmo motivo da seção 2; a janela
   pode ser escondida do mesmo jeito.) E, dentro do WSL, `sudo loginctl enable-linger $USER` para o serviço de
   usuário subir sem login. Quando o Windows dorme, o WSL dorme junto — é
   esperado; o `run` retoma ao acordar.

Feito isso, siga a [seção 4 (Linux)](#4-linux) inteira: dependências,
compilação e a unit do systemd são idênticas. O pré-compilado do Whisper
foi testado exatamente neste cenário (Ubuntu 26.04 no WSL 2).

---

## 6. Compilar — referência

O que os passos acima já fizeram, e as variações:

```bash
go build -o wappsync ./cmd/wappsync            # binário para a máquina atual
go test ./... -count=1                         # suíte inteira (~15 s)
```

**Sem compilador C, de propósito.** O driver SQLite é puro Go. A CI compila
com `CGO_ENABLED=0` para garantir que continue assim; se um dia `go build`
pedir `gcc`, algo entrou errado.

**Compilação cruzada** a partir de qualquer sistema — útil para preparar as
outras máquinas de uma só:

```bash
GOOS=windows GOARCH=amd64 go build -o wappsync.exe   ./cmd/wappsync
GOOS=darwin  GOARCH=arm64 go build -o wappsync-mac   ./cmd/wappsync   # Apple Silicon
GOOS=darwin  GOARCH=amd64 go build -o wappsync-intel ./cmd/wappsync
GOOS=linux   GOARCH=amd64 go build -o wappsync-linux ./cmd/wappsync
```

No PowerShell as variáveis se definem antes: `$env:GOOS="linux"; go build ...`.

**Instalar direto pelo Go**, sem clonar (deixa o binário em `$(go env GOPATH)/bin`):

```bash
go install github.com/brumarch/wappsync/cmd/wappsync@latest
```

**Go mais antigo que 1.26**: desde o Go 1.21, o `go build` lê a linha `go
1.26.0` do `go.mod` e baixa a toolchain certa sozinho. Só falha se o Go for
anterior a 1.21, ou se `GOTOOLCHAIN=local` estiver definido.

**`-race` precisa de gcc.** O detector de corrida do Go usa cgo, então
`go test -race` não roda onde não há compilador C (Windows sem MSYS2, por
exemplo). A CI roda em Linux e cobre isso.

---

## 7. Primeiros passos

Igual em todo sistema. Nos exemplos, `wappsync` é o binário que você compilou
(`.\wappsync.exe` no PowerShell).

**1. Criar o config.** Numa pasta que vai ficar (por exemplo `~/.wappsync` ou
`C:\wappsync`):

```bash
wappsync init
```

Edite o `config.toml` criado. Três campos:

```toml
host_id = "bruno-win"            # nome curto e estável desta máquina; aparece nos arquivos na nuvem

[remote.folder]
path = 'G:/Meu Drive/wapp'       # uma pasta que o app do Drive/OneDrive/Dropbox já sincroniza
                                 # (macOS: '/Users/bruno/Google Drive/wapp'; WSL: '/mnt/c/Users/bruno/Google Drive/wapp')

[transcribe]                     # só se instalou o Whisper
binary = 'C:/whisper/Release/whisper-cli.exe'   # ou '/opt/whisper/whisper-cli'; no macOS com brew, deixe "whisper-cli"
model  = 'C:/whisper/ggml-small.bin'
```

Não ligue `[transcribe].enabled` nem mexa em `[filter]`/`[media]` à mão — o
`setup` faz isso no passo 3. Tudo o mais tem default razoável e está comentado
no arquivo.

**2. Parear.** Mostra um QR no terminal; no celular: WhatsApp → Aparelhos
conectados → Conectar aparelho.

```bash
wappsync login
```

Se o terminal não renderizar o QR (SSH, por exemplo): `wappsync login -phone +5511999999999`
mostra um código para digitar no celular. `-full-history` pede ~1 ano de
histórico em vez das últimas semanas.

**3. Escolher o que sai.** Um questionário: exporta todas as conversas?
transcreve áudio? baixa imagem, documento, áudio de todas? — e, a cada "sim",
os números das conversas que ficam de fora. Só apertar Enter produz um config
que exporta texto e não baixa anexo nenhum.

```bash
wappsync setup
```

Ele mostra o que vai gravar, pede confirmação e deixa uma cópia do arquivo
anterior em `config.toml.bak`.

**4. Rodar.**

```bash
wappsync run
```

Captura continuamente e publica a cada `interval_minutes` (10 por padrão).
`Ctrl+C` faz um último ciclo antes de sair. Quando estiver satisfeito, troque
pelo serviço da sua seção.

**5. Conferir.**

```bash
wappsync status      # sessão, banco local, o que está publicado
wappsync groups      # cada grupo com a política de anexo JÁ RESOLVIDA
```

Na nuvem, `<pasta>/wappsync/latest/` é o que os agentes leem — o `LEIA-ME.md`
lá dentro explica o formato para eles.

Se usar `-config` com caminho absoluto, os comandos funcionam de qualquer
diretório: `wappsync status -config /home/bruno/.wappsync/config.toml`.

---

## 8. Problemas comuns

**`go: go.mod requires go >= 1.26.0`** — o Go é anterior a 1.21 (que não sabe
baixar toolchain) ou `GOTOOLCHAIN=local` está definido. Instale pelo tarball
oficial ou pelo winget/brew, que estão sempre atualizados.

**`transcrição: binário do whisper não encontrado no PATH`** ou o mesmo para
`ffmpeg` — `[transcribe].enabled = true` e o binário não está no `PATH` nem
em `binary`/`ffmpeg` com caminho completo. O programa **não sobe** de
propósito: falhar no início é melhor que uma nota de voz por vez em silêncio.
Corrija o caminho ou desligue a transcrição.

**`transcribe.model não está acessível`** — o `.bin` não está onde o config
diz. Baixe de novo; um download interrompido deixa um arquivo menor que 465 MB.

**O QR não aparece ou vem quebrado** — o terminal não renderiza blocos
Unicode. Use `wappsync login -phone +55...` e digite o código no celular.

**`run` saiu com código 3 e apareceu `alertas/<host_id>.md` na nuvem** — o
WhatsApp desvinculou esta máquina (isso acontece sozinho depois de ~14 dias
sem o celular ficar online, ou se você removeu o aparelho). Rode
`wappsync login` de novo. Não adianta reiniciar o serviço.

**Configurei anexo e nada é baixado** — a decisão de baixar acontece quando a
mensagem **chega**; o que já estava no banco não ganha anexo retroativamente.
Espere mídia nova, e confira em `wappsync groups` a política resolvida da
conversa. Rodar `setup` ou `groups` **com o `run` ativo** também derruba o
`run` (o WhatsApp só aceita uma conexão por aparelho): pare o serviço antes.

**No OneDrive os arquivos não aparecem na outra máquina** — "Arquivos sob
demanda" está ligado na pasta. Marque `wapp/` como "Sempre manter neste
dispositivo" nas duas pontas.

**Windows: acentos errados nas perguntas do `setup`** — é o console antigo
(conhost com página de código 850). O arquivo gravado sai certo de qualquer
jeito; para ver certo, use o Windows Terminal.
