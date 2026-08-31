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
