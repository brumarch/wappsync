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
// O default é não baixar nada: Enabled desligado, e nenhum chat listado. A
// ausência de uma entrada significa "não baixa", nunca "baixa tudo" — é o que
// mantém o custo de um erro de digitação em zero mensagem vazada.
type Media struct {
	Enabled   bool        `toml:"enabled"`
	MaxFileMB int         `toml:"max_file_mb"`
	Chats     []MediaChat `toml:"chat"`
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
	Remote  Remote  `toml:"remote"`
	Merge   Merge   `toml:"merge"`

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
	for i, mc := range c.Media.Chats {
		if strings.TrimSpace(mc.Match) == "" {
			return fmt.Errorf("media.chat[%d]: match vazio", i)
		}
		if len(mc.Kinds) == 0 {
			return fmt.Errorf("media.chat[%d] (%q): kinds vazio; remova a entrada se não quer baixar nada", i, mc.Match)
		}
		// Um kind desconhecido é erro, não algo a ignorar. Este arquivo é
		// escrito em pt-BR o tempo todo, e kinds = ["imagem"] simplesmente
		// nunca casaria — o usuário concluiria que o download não funciona.
		for _, k := range mc.Kinds {
			if !MediaKindKnown(k) {
				return fmt.Errorf("media.chat[%d] (%q): kind desconhecido %q (use %s)",
					i, mc.Match, k, strings.Join(MediaKinds(), " ou "))
			}
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

// mediaKinds são os tipos de anexo que este binário sabe baixar, na ordem em
// que aparecem na mensagem de erro. Áudio e vídeo ficam de fora de propósito:
// aceitar o valor no config sem implementar o download faria o programa ignorar
// em silêncio uma linha que o usuário escreveu esperando efeito.
var mediaKinds = []string{"image", "document"}

// MediaKinds devolve os tipos de anexo suportados.
func MediaKinds() []string { return append([]string(nil), mediaKinds...) }

// MediaKindKnown informa se um valor de media.chat.kinds é suportado.
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
// São três condições, e todas as três precisam valer:
//
//  1. a trava mestra [media].enabled está ligada;
//  2. o chat sai daqui de qualquer forma (ChatAllowed) — baixar mídia de uma
//     conversa que nem é exportada seria trazer para o disco um dado que o
//     usuário mandou não publicar;
//  3. alguma entrada [[media.chat]] casa com o chat E lista este kind.
//
// Chat não listado significa "não baixa". A permissão é sempre aditiva: nunca
// existe uma entrada que TIRE permissão, então a ordem das entradas no arquivo
// não é carga semântica e duas regras que casam com o mesmo chat se somam.
func (c *Config) MediaAllowed(jid, name, kind string) bool {
	if !c.Media.Enabled || !c.ChatAllowed(jid, name) {
		return false
	}
	for _, mc := range c.Media.Chats {
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
