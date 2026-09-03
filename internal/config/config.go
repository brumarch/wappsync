// Package config carrega e valida o arquivo TOML de configuração.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Paths struct {
	DataDir string `toml:"data_dir"`
}

type Export struct {
	IntervalMinutes        int      `toml:"interval_minutes"`
	Formats                []string `toml:"formats"`
	IncludeFromMe          bool     `toml:"include_from_me"`
	IncludeStatusBroadcast bool     `toml:"include_status_broadcast"`
	IncludeNewsletters     bool     `toml:"include_newsletters"`
	MediaPlaceholders      bool     `toml:"media_placeholders"`
	MaxMessageChars        int      `toml:"max_message_chars"`
}

type Filter struct {
	IncludeOnly []string `toml:"include_only"`
	Exclude     []string `toml:"exclude"`
}

type Privacy struct {
	RedactPhoneNumbers bool     `toml:"redact_phone_numbers"`
	RedactPatterns     []string `toml:"redact_patterns"`
}

// MediaChat é a política de download de UM conjunto de chats.
//
// Match usa a mesma semântica de [filter]: JID exato ou pedaço do nome. Não
// inventamos um segundo jeito de nomear chat justamente porque quem edita este
// arquivo já aprendeu aquele.
type MediaChat struct {
	Match string   `toml:"match"`
	Kinds []string `toml:"kinds"`
}

// Media é a política de download de anexos.
//
// O default é não baixar nada: Enabled desligado, Kinds vazio e nenhum chat
// listado. A ausência de configuração significa "não baixa", nunca "baixa
// tudo" — é o que mantém o custo de um erro de digitação em zero mensagem
// vazada. Baixar de todos os chats é uma decisão que o usuário escreve por
// extenso em Kinds.
//
// A forma espelha [filter]: um padrão amplo (Kinds vale para todo chat
// exportado), acréscimos por chat (Chats) e exceções que prevalecem sobre
// ambos (Exclude). Quem já aprendeu include_only/exclude não precisa aprender
// uma segunda regra de precedência.
type Media struct {
	Enabled   bool        `toml:"enabled"`
	MaxFileMB int         `toml:"max_file_mb"`
	Kinds     []string    `toml:"kinds"`
	Chats     []MediaChat `toml:"chat"`
	Exclude   []MediaChat `toml:"exclude"`
}

// Transcribe configura a transcrição local de áudio.
//
// Duas dependências externas que o resto do projeto não tem: um binário de
// Whisper e o ffmpeg. Ficam como caminho no config, e não embutidas, porque
// nenhuma das duas é Go — e cravá-las traria compilador C de volta, que é
// exatamente o que o driver SQLite puro Go existe para evitar.
type Transcribe struct {
	Enabled bool   `toml:"enabled"`
	Binary  string `toml:"binary"`
	Model   string `toml:"model"`
	FFmpeg  string `toml:"ffmpeg"`
	// Language é o código ISO do idioma, ou "auto". Cravar "pt" num áudio em
	// inglês produz transcrição errada com cara de certa, que é pior que
	// nenhuma.
	Language       string `toml:"language"`
	Threads        int    `toml:"threads"`
	TimeoutMinutes int    `toml:"timeout_minutes"`
	// MaxSeconds descarta áudio longo demais antes de gastar CPU. O tempo de
	// transcrição cresce com a duração, e um ciclo de export não pode ficar
	// atrás de um áudio de uma hora.
	MaxSeconds int `toml:"max_seconds"`
}

type FolderRemote struct {
	Path string `toml:"path"`
}

type RcloneRemote struct {
	Binary string `toml:"binary"`
	Remote string `toml:"remote"`
}

type Remote struct {
	Backend string       `toml:"backend"`
	Prefix  string       `toml:"prefix"`
	Folder  FolderRemote `toml:"folder"`
	Rclone  RcloneRemote `toml:"rclone"`
}

type Merge struct {
	Enabled        bool `toml:"enabled"`
	GuardMonotonic bool `toml:"guard_monotonic"`
	LeaseMinutes   int  `toml:"lease_minutes"`
}

type Config struct {
	WindowDays    int    `toml:"window_days"`
	RetentionDays int    `toml:"retention_days"`
	HostID        string `toml:"host_id"`

	Paths   Paths   `toml:"paths"`
	Export  Export  `toml:"export"`
	Filter  Filter  `toml:"filter"`
	Privacy Privacy `toml:"privacy"`
	Media   Media   `toml:"media"`

	Transcribe Transcribe `toml:"transcribe"`
	Remote     Remote     `toml:"remote"`
	Merge      Merge      `toml:"merge"`

	// Derivados (não vêm do arquivo).
	SourcePath   string           `toml:"-"`
	RedactRegexp []*regexp.Regexp `toml:"-"`
}

// phoneRe cobre formatos comuns escritos dentro do corpo da mensagem.
var phoneRe = regexp.MustCompile(`(?:\+?\d{1,3}[\s.-]?)?(?:\(\d{2,3}\)[\s.-]?|\d{2,3}[\s.-])\d{4,5}[\s.-]?\d{4}`)

func defaults() Config {
	return Config{
		WindowDays:    3,
		RetentionDays: 45,
		Export: Export{
			IntervalMinutes:   10,
			Formats:           []string{"jsonl", "markdown"},
			IncludeFromMe:     true,
			MediaPlaceholders: true,
			MaxMessageChars:   4000,
		},
		Media: Media{
			MaxFileMB: 20,
		},
		Transcribe: Transcribe{
			Binary:         "whisper-cli",
			FFmpeg:         "ffmpeg",
			Language:       "auto",
			TimeoutMinutes: 10,
			MaxSeconds:     600,
		},
		Remote: Remote{
			Backend: "folder",
			Prefix:  "wapp-summarizer",
			Rclone:  RcloneRemote{Binary: "rclone"},
		},
		Merge: Merge{
			Enabled:        true,
			GuardMonotonic: true,
			LeaseMinutes:   15,
		},
	}
}

// Load lê o TOML em path, aplica defaults e valida.
func Load(path string) (*Config, error) {
	cfg := defaults()
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("lendo %s: %w", path, err)
	}
	cfg.SourcePath = path
	if err := cfg.finalize(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) finalize() error {
	if c.WindowDays < 1 {
		return fmt.Errorf("window_days deve ser >= 1 (got %d)", c.WindowDays)
	}
	if c.RetentionDays < c.WindowDays {
		c.RetentionDays = c.WindowDays
	}
	if c.Export.IntervalMinutes < 1 {
		c.Export.IntervalMinutes = 10
	}
	if c.Merge.LeaseMinutes < 1 {
		c.Merge.LeaseMinutes = 15
	}

	if c.HostID == "" {
		h, err := os.Hostname()
		if err != nil || h == "" {
			h = "unknown-host"
		}
		c.HostID = h
	}
	c.HostID = slug(c.HostID)
	if c.HostID == "" {
		return fmt.Errorf("host_id inválido após normalização")
	}

	if c.Paths.DataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("não consegui determinar o diretório home: %w", err)
		}
		c.Paths.DataDir = filepath.Join(home, ".wapp-summarizer")
	}
	c.Paths.DataDir = filepath.Clean(os.ExpandEnv(c.Paths.DataDir))

	switch c.Remote.Backend {
	case "folder":
		if c.Remote.Folder.Path == "" {
			return fmt.Errorf(`remote.backend = "folder" exige [remote.folder].path`)
		}
		c.Remote.Folder.Path = filepath.Clean(os.ExpandEnv(c.Remote.Folder.Path))
		if inside(c.Paths.DataDir, c.Remote.Folder.Path) {
			return fmt.Errorf("paths.data_dir (%s) está dentro da pasta sincronizada (%s); "+
				"a sessão do WhatsApp e o SQLite não podem ser sincronizados", c.Paths.DataDir, c.Remote.Folder.Path)
		}
	case "rclone":
		if c.Remote.Rclone.Remote == "" {
			return fmt.Errorf(`remote.backend = "rclone" exige [remote.rclone].remote`)
		}
		if c.Remote.Rclone.Binary == "" {
			c.Remote.Rclone.Binary = "rclone"
		}
	case "none", "":
		c.Remote.Backend = "none"
	default:
		return fmt.Errorf("remote.backend desconhecido: %q (use folder, rclone ou none)", c.Remote.Backend)
	}

	if c.Privacy.RedactPhoneNumbers {
		c.RedactRegexp = append(c.RedactRegexp, phoneRe)
	}
	for _, p := range c.Privacy.RedactPatterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return fmt.Errorf("privacy.redact_patterns: regex inválida %q: %w", p, err)
		}
		c.RedactRegexp = append(c.RedactRegexp, re)
	}

	for _, f := range c.Export.Formats {
		switch f {
		case "jsonl", "markdown":
		default:
			return fmt.Errorf("export.formats: formato desconhecido %q (use jsonl ou markdown)", f)
		}
	}

	if c.Media.MaxFileMB < 1 {
		c.Media.MaxFileMB = 20
	}
	if c.Transcribe.TimeoutMinutes < 1 {
		c.Transcribe.TimeoutMinutes = 10
	}
	if c.Transcribe.MaxSeconds < 1 {
		c.Transcribe.MaxSeconds = 600
	}
	if c.Transcribe.Language == "" {
		c.Transcribe.Language = "auto"
	}
	if c.Transcribe.Binary == "" {
		c.Transcribe.Binary = "whisper-cli"
	}
	if c.Transcribe.FFmpeg == "" {
		c.Transcribe.FFmpeg = "ffmpeg"
	}
	if c.Transcribe.Enabled {
		if c.Transcribe.Model == "" {
			return fmt.Errorf(`transcribe.enabled exige transcribe.model (caminho do .bin do Whisper)`)
		}
		c.Transcribe.Model = filepath.Clean(os.ExpandEnv(c.Transcribe.Model))
		// Conferir aqui e não na primeira transcrição: o erro aparece ao subir
		// o programa, e não horas depois num aviso de log que ninguém lê.
		if _, err := os.Stat(c.Transcribe.Model); err != nil {
			return fmt.Errorf("transcribe.model não está acessível em %s: %w", c.Transcribe.Model, err)
		}
	}
	if err := c.validateMediaKinds("media.kinds", c.Media.Kinds, true); err != nil {
		return err
	}
	for i, mc := range c.Media.Chats {
		if strings.TrimSpace(mc.Match) == "" {
			return fmt.Errorf("media.chat[%d]: match vazio", i)
		}
		if len(mc.Kinds) == 0 {
			return fmt.Errorf("media.chat[%d] (%q): kinds vazio; remova a entrada se não quer baixar nada", i, mc.Match)
		}
		where := fmt.Sprintf("media.chat[%d] (%q)", i, mc.Match)
		if err := c.validateMediaKinds(where, mc.Kinds, true); err != nil {
			return err
		}
	}
	for i, mc := range c.Media.Exclude {
		if strings.TrimSpace(mc.Match) == "" {
			return fmt.Errorf("media.exclude[%d]: match vazio", i)
		}
		if len(mc.Kinds) == 0 {
			return fmt.Errorf("media.exclude[%d] (%q): kinds vazio; diga quais tipos ficam de fora, ou remova a entrada", i, mc.Match)
		}
		// Excluir áudio com a transcrição desligada é inofensivo — a exceção
		// só deixa de ter efeito. Exigir o mesmo pareamento aqui obrigaria a
		// editar a lista de exceções toda vez que a transcrição fosse
		// desligada por um tempo.
		where := fmt.Sprintf("media.exclude[%d] (%q)", i, mc.Match)
		if err := c.validateMediaKinds(where, mc.Kinds, false); err != nil {
			return err
		}
	}
	return nil
}

// validateMediaKinds recusa kind desconhecido e, quando produces é verdadeiro,
// "audio" sem transcrição.
//
// Um kind desconhecido é erro, não algo a ignorar. Este arquivo é escrito em
// pt-BR o tempo todo, e kinds = ["imagem"] simplesmente nunca casaria — o
// usuário concluiria que o download não funciona.
//
// Áudio sem transcrição não produz nada: o arquivo bruto não é publicado (um
// .ogg na nuvem é peso sem leitor), então a linha ficaria escrita no config
// sem qualquer efeito. Isso só vale para linhas que MANDAM baixar; uma exceção
// sem efeito não engana ninguém.
func (c *Config) validateMediaKinds(where string, kinds []string, produces bool) error {
	for _, k := range kinds {
		if !MediaKindKnown(k) {
			return fmt.Errorf("%s: kind desconhecido %q (use %s)",
				where, k, strings.Join(MediaKinds(), " ou "))
		}
		if produces && k == KindAudio && !c.Transcribe.Enabled {
			return fmt.Errorf(`%s pede kind "audio", mas [transcribe].enabled está desligado; `+
				"sem transcrição o áudio não vira nada — ligue a transcrição ou remova o kind", where)
		}
	}
	return nil
}

// Window é a janela temporal exportada.
func (c *Config) Window() time.Duration {
	return time.Duration(c.WindowDays) * 24 * time.Hour
}

// Retention é a janela guardada no banco local.
func (c *Config) Retention() time.Duration {
	return time.Duration(c.RetentionDays) * 24 * time.Hour
}

func (c *Config) Interval() time.Duration {
	return time.Duration(c.Export.IntervalMinutes) * time.Minute
}

func (c *Config) HasFormat(name string) bool {
	for _, f := range c.Export.Formats {
		if f == name {
			return true
		}
	}
	return false
}

// SessionDBPath é o SQLite de credenciais do whatsmeow (uma sessão por máquina).
func (c *Config) SessionDBPath() string {
	return filepath.Join(c.Paths.DataDir, "session.db")
}

// MessagesDBPath é o SQLite com o histórico capturado.
func (c *Config) MessagesDBPath() string {
	return filepath.Join(c.Paths.DataDir, "messages.db")
}

// MediaDir guarda os anexos baixados, antes e depois de publicados.
func (c *Config) MediaDir() string {
	return filepath.Join(c.Paths.DataDir, "media")
}

// AudioDir guarda o áudio baixado enquanto ele espera a transcrição. Nada aqui
// é publicado, e o arquivo é apagado assim que o texto sai.
func (c *Config) AudioDir() string {
	return filepath.Join(c.Paths.DataDir, "audio")
}

// OutDir guarda as cópias locais do que foi publicado.
func (c *Config) OutDir() string {
	return filepath.Join(c.Paths.DataDir, "out")
}

// Redact aplica as regras de privacidade ao corpo de uma mensagem.
func (c *Config) Redact(s string) string {
	for _, re := range c.RedactRegexp {
		if re == phoneRe {
			s = re.ReplaceAllString(s, "[TEL]")
			continue
		}
		s = re.ReplaceAllString(s, "[REDACTED]")
	}
	return s
}

// ChatAllowed decide se um chat entra na exportação.
// exclude tem prioridade; se include_only não estiver vazio, funciona como allowlist.
func (c *Config) ChatAllowed(jid, name string) bool {
	for _, pat := range c.Filter.Exclude {
		if matches(pat, jid, name) {
			return false
		}
	}
	if len(c.Filter.IncludeOnly) == 0 {
		return true
	}
	for _, pat := range c.Filter.IncludeOnly {
		if matches(pat, jid, name) {
			return true
		}
	}
	return false
}

// mediaKinds são os tipos de anexo que este binário sabe tratar, na ordem em
// que aparecem na mensagem de erro. Vídeo fica de fora de propósito: aceitar o
// valor no config sem implementar o tratamento faria o programa ignorar em
// silêncio uma linha que o usuário escreveu esperando efeito.
//
// "audio" não vira arquivo publicado: o que sai é a transcrição. Por isso ele
// exige [transcribe].enabled — ver a validação em finalize.
var mediaKinds = []string{"image", "document", "audio"}

// KindAudio é o kind cujo produto é texto, não arquivo.
const KindAudio = "audio"

// MediaKinds devolve os tipos de anexo suportados.
func MediaKinds() []string { return append([]string(nil), mediaKinds...) }

// MediaKindKnown informa se um valor de kinds (em [media], [[media.chat]] ou
// [[media.exclude]]) é suportado.
func MediaKindKnown(kind string) bool {
	for _, k := range mediaKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// MediaAllowed decide se o anexo de uma mensagem pode ser baixado.
//
// São quatro condições, e todas precisam valer:
//
//  1. a trava mestra [media].enabled está ligada;
//  2. o chat sai daqui de qualquer forma (ChatAllowed) — baixar mídia de uma
//     conversa que nem é exportada seria trazer para o disco um dado que o
//     usuário mandou não publicar;
//  3. nenhuma entrada [[media.exclude]] casa com o chat E lista este kind;
//  4. o kind está em [media].kinds (vale para todo chat exportado) OU alguma
//     entrada [[media.chat]] casa com o chat E lista este kind.
//
// Nada configurado significa "não baixa". A exceção prevalece sobre o resto,
// como o exclude de [filter] prevalece sobre include_only: assim a ordem das
// entradas no arquivo não é carga semântica — mover um bloco de lugar nunca
// muda o que sai da máquina — e duas regras que casam com o mesmo chat se
// somam, sem uma anular a outra.
func (c *Config) MediaAllowed(jid, name, kind string) bool {
	if !c.Media.Enabled || !c.ChatAllowed(jid, name) {
		return false
	}
	if listsKind(c.Media.Exclude, jid, name, kind) {
		return false
	}
	for _, k := range c.Media.Kinds {
		if k == kind {
			return true
		}
	}
	return listsKind(c.Media.Chats, jid, name, kind)
}

// listsKind informa se alguma entrada casa com o chat e lista o kind.
func listsKind(entries []MediaChat, jid, name, kind string) bool {
	for _, mc := range entries {
		if !matches(mc.Match, jid, name) {
			continue
		}
		for _, k := range mc.Kinds {
			if k == kind {
				return true
			}
		}
	}
	return false
}

// MediaKindsFor lista os tipos de anexo que seriam baixados neste chat.
//
// Existe para o `wappsync groups` conseguir mostrar a política resolvida, com o
// nome real do chat. Ler o TOML e tentar prever o que ele faz é justamente o
// passo em que se erra — e o erro é silencioso, porque um chat sem política
// simplesmente não baixa nada.
func (c *Config) MediaKindsFor(jid, name string) []string {
	var out []string
	for _, k := range mediaKinds {
		if c.MediaAllowed(jid, name, k) {
			out = append(out, k)
		}
	}
	return out
}

func matches(pattern, jid, name string) bool {
	p := strings.ToLower(strings.TrimSpace(pattern))
	if p == "" {
		return false
	}
	if strings.EqualFold(jid, pattern) {
		return true
	}
	return name != "" && strings.Contains(strings.ToLower(name), p)
}

func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '.':
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// inside reporta se child está dentro de parent.
func inside(child, parent string) bool {
	cAbs, err1 := filepath.Abs(child)
	pAbs, err2 := filepath.Abs(parent)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(pAbs, cAbs)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
