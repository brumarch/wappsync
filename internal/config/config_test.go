package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// write grava um config.toml temporário e devolve o caminho.
func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// toml devolve um caminho no formato de string literal do TOML (aspas simples),
// que não interpreta a barra invertida do Windows.
func tomlPath(p string) string { return "'" + filepath.ToSlash(p) + "'" }

func minimal(t *testing.T, extra string) *Config {
	t.Helper()
	dir := t.TempDir()
	// extra vem ANTES da primeira tabela: no TOML, chaves de topo escritas
	// depois de um [header] pertenceriam àquela tabela, não à raiz.
	body := "host_id = \"maquina-teste\"\n" +
		extra +
		"[paths]\ndata_dir = " + tomlPath(filepath.Join(dir, "data")) + "\n" +
		"[remote]\nbackend = \"folder\"\n" +
		"[remote.folder]\npath = " + tomlPath(filepath.Join(dir, "drive")) + "\n"
	cfg, err := Load(write(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg := minimal(t, "")

	if cfg.WindowDays != 3 {
		t.Errorf("window_days = %d, queria 3", cfg.WindowDays)
	}
	if cfg.RetentionDays != 45 {
		t.Errorf("retention_days = %d, queria 45", cfg.RetentionDays)
	}
	if cfg.Export.IntervalMinutes != 10 {
		t.Errorf("interval_minutes = %d, queria 10", cfg.Export.IntervalMinutes)
	}
	if !cfg.Export.IncludeFromMe {
		t.Error("include_from_me deveria vir ligado")
	}
	if !cfg.Merge.Enabled || !cfg.Merge.GuardMonotonic {
		t.Error("as travas de merge deveriam vir ligadas por padrão")
	}
	if cfg.Window() != 72*time.Hour {
		t.Errorf("Window() = %v", cfg.Window())
	}
	if cfg.Interval() != 10*time.Minute {
		t.Errorf("Interval() = %v", cfg.Interval())
	}
}

func TestLoadRejectsInvalidWindow(t *testing.T) {
	if _, err := Load(write(t, "window_days = 0\n")); err == nil {
		t.Error("window_days = 0 deveria falhar")
	}
}

// Guardar menos localmente do que se publica seria perder dado silenciosamente.
func TestRetentionIsRaisedToWindow(t *testing.T) {
	cfg := minimal(t, "window_days = 30\nretention_days = 5\n")
	if cfg.RetentionDays != 30 {
		t.Errorf("retention_days = %d, deveria ter subido para 30", cfg.RetentionDays)
	}
}

// A proteção mais importante deste pacote: a sessão do WhatsApp e o SQLite
// nunca podem ficar dentro da pasta que o Drive sincroniza.
func TestRejectsDataDirInsideSyncedFolder(t *testing.T) {
	dir := t.TempDir()
	drive := filepath.Join(dir, "drive")

	body := "host_id = \"m\"\n" +
		"[paths]\ndata_dir = " + tomlPath(filepath.Join(drive, "wapp-data")) + "\n" +
		"[remote]\nbackend = \"folder\"\n" +
		"[remote.folder]\npath = " + tomlPath(drive) + "\n"

	_, err := Load(write(t, body))
	if err == nil {
		t.Fatal("data_dir dentro da pasta sincronizada deveria ser recusado")
	}
	if !strings.Contains(err.Error(), "sincronizada") {
		t.Errorf("mensagem de erro pouco clara: %v", err)
	}
}

func TestDataDirEqualToSyncedFolderIsRejected(t *testing.T) {
	dir := t.TempDir()
	drive := filepath.Join(dir, "drive")
	body := "host_id = \"m\"\n" +
		"[paths]\ndata_dir = " + tomlPath(drive) + "\n" +
		"[remote]\nbackend = \"folder\"\n" +
		"[remote.folder]\npath = " + tomlPath(drive) + "\n"

	if _, err := Load(write(t, body)); err == nil {
		t.Error("data_dir igual à pasta sincronizada deveria ser recusado")
	}
}

func TestBackendValidation(t *testing.T) {
	cases := map[string]struct {
		body      string
		wantError string
	}{
		"folder sem path": {
			body:      "host_id = \"m\"\n[remote]\nbackend = \"folder\"\n",
			wantError: "remote.folder",
		},
		"rclone sem remote": {
			body:      "host_id = \"m\"\n[remote]\nbackend = \"rclone\"\n",
			wantError: "remote.rclone",
		},
		"backend inexistente": {
			body:      "host_id = \"m\"\n[remote]\nbackend = \"dropbox\"\n",
			wantError: "desconhecido",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, tc.body))
			if err == nil {
				t.Fatal("deveria falhar")
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Errorf("erro %q não menciona %q", err, tc.wantError)
			}
		})
	}
}

// backend "none" é válido e vale como modo somente-local.
func TestBackendNoneIsValid(t *testing.T) {
	cfg, err := Load(write(t, "host_id = \"m\"\n[remote]\nbackend = \"none\"\n"))
	if err != nil {
		t.Fatalf("backend none deveria ser aceito: %v", err)
	}
	if cfg.Remote.Backend != "none" {
		t.Errorf("backend = %q", cfg.Remote.Backend)
	}
	// Backend ausente também vira "none".
	cfg, err = Load(write(t, "host_id = \"m\"\n[remote]\nbackend = \"\"\n"))
	if err != nil || cfg.Remote.Backend != "none" {
		t.Errorf("backend vazio deveria virar none: %v / %q", err, cfg.Remote.Backend)
	}
}

func TestHostIDIsSlugified(t *testing.T) {
	cfg := minimal(t, "")
	if cfg.HostID != "maquina-teste" {
		t.Errorf("HostID = %q", cfg.HostID)
	}

	dir := t.TempDir()
	body := "host_id = \"Bruno's PC (Casa)\"\n" +
		"[paths]\ndata_dir = " + tomlPath(filepath.Join(dir, "d")) + "\n" +
		"[remote]\nbackend = \"none\"\n"
	got, err := Load(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	// O host_id vira nome de arquivo na nuvem: sem espaços, acentos ou símbolos.
	if strings.ContainsAny(got.HostID, " '()") {
		t.Errorf("HostID = %q ainda tem caracteres inválidos para nome de arquivo", got.HostID)
	}
	if got.HostID == "" {
		t.Error("HostID ficou vazio após normalização")
	}
}

func TestHostIDFallsBackToHostname(t *testing.T) {
	dir := t.TempDir()
	body := "[paths]\ndata_dir = " + tomlPath(filepath.Join(dir, "d")) + "\n" +
		"[remote]\nbackend = \"none\"\n"
	cfg, err := Load(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HostID == "" {
		t.Error("host_id vazio deveria cair no hostname")
	}
}

func TestInvalidRedactPatternIsRejected(t *testing.T) {
	_, err := Load(write(t, "host_id = \"m\"\n[remote]\nbackend = \"none\"\n"+
		"[privacy]\nredact_patterns = ['[a-']\n"))
	if err == nil {
		t.Fatal("regex inválida deveria falhar no carregamento, não em produção")
	}
	if !strings.Contains(err.Error(), "redact_patterns") {
		t.Errorf("erro não aponta o campo: %v", err)
	}
}

func TestInvalidFormatIsRejected(t *testing.T) {
	_, err := Load(write(t, "host_id = \"m\"\n[remote]\nbackend = \"none\"\n"+
		"[export]\nformats = [\"pdf\"]\n"))
	if err == nil || !strings.Contains(err.Error(), "formats") {
		t.Errorf("formato inválido deveria ser recusado, veio: %v", err)
	}
}

func TestRedactPhoneNumbers(t *testing.T) {
	cfg := minimal(t, "[privacy]\nredact_phone_numbers = true\n")

	got := cfg.Redact("me liga no (11) 98765-4321 hoje")
	if strings.Contains(got, "98765") {
		t.Errorf("telefone não redigido: %q", got)
	}
	if !strings.Contains(got, "[TEL]") {
		t.Errorf("faltou o marcador: %q", got)
	}
	if !strings.Contains(got, "me liga no") || !strings.Contains(got, "hoje") {
		t.Errorf("redação comeu texto ao redor: %q", got)
	}
}

func TestRedactCustomPattern(t *testing.T) {
	cfg := minimal(t, "[privacy]\nredact_patterns = ['\\d{3}\\.\\d{3}\\.\\d{3}-\\d{2}']\n")

	got := cfg.Redact("meu CPF é 123.456.789-00 ok")
	if strings.Contains(got, "123.456") {
		t.Errorf("CPF não redigido: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("faltou o marcador: %q", got)
	}
}

func TestRedactIsNoopWithoutRules(t *testing.T) {
	cfg := minimal(t, "")
	in := "telefone (11) 98765-4321 e CPF 123.456.789-00"
	if got := cfg.Redact(in); got != in {
		t.Errorf("redigiu sem regra configurada: %q", got)
	}
}

func TestChatAllowed(t *testing.T) {
	const famJID = "120363000000000000@g.us"

	cases := []struct {
		name        string
		includeOnly []string
		exclude     []string
		jid, chat   string
		want        bool
	}{
		{name: "sem filtro passa tudo", jid: famJID, chat: "Família", want: true},
		{name: "exclui por nome", exclude: []string{"Trabalho"}, jid: "x@g.us", chat: "Trabalho", want: false},
		{name: "exclui por nome parcial", exclude: []string{"traba"}, jid: "x@g.us", chat: "Grupo Trabalho SP", want: false},
		{name: "exclui por JID exato", exclude: []string{famJID}, jid: famJID, chat: "Família", want: false},
		{name: "include_only vira allowlist", includeOnly: []string{"Família"}, jid: famJID, chat: "Família", want: true},
		{name: "fora da allowlist é barrado", includeOnly: []string{"Família"}, jid: "x@g.us", chat: "Trabalho", want: false},
		{
			name:        "exclude tem prioridade sobre include_only",
			includeOnly: []string{"Família"}, exclude: []string{"Família"},
			jid: famJID, chat: "Família", want: false,
		},
		{name: "casamento de nome ignora maiúsculas", exclude: []string{"FAMÍLIA"}, jid: famJID, chat: "família", want: false},
		{name: "chat sem nome não casa por texto", exclude: []string{"Trabalho"}, jid: "x@g.us", chat: "", want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Filter: Filter{IncludeOnly: tc.includeOnly, Exclude: tc.exclude}}
			if got := cfg.ChatAllowed(tc.jid, tc.chat); got != tc.want {
				t.Errorf("ChatAllowed(%q, %q) = %v, queria %v", tc.jid, tc.chat, got, tc.want)
			}
		})
	}
}

// Uma entrada vazia no filtro não pode casar com tudo e esvaziar o export.
func TestEmptyFilterEntryMatchesNothing(t *testing.T) {
	cfg := &Config{Filter: Filter{Exclude: []string{"", "   "}}}
	if !cfg.ChatAllowed("x@g.us", "Família") {
		t.Error("entrada vazia no exclude barrou uma conversa")
	}
}

func TestHasFormat(t *testing.T) {
	cfg := minimal(t, "[export]\nformats = [\"jsonl\"]\n")
	if !cfg.HasFormat("jsonl") {
		t.Error("HasFormat(jsonl) = false")
	}
	if cfg.HasFormat("markdown") {
		t.Error("HasFormat(markdown) = true, mas não está configurado")
	}
}

func TestDerivedPaths(t *testing.T) {
	cfg := minimal(t, "")
	for name, p := range map[string]string{
		"SessionDBPath":  cfg.SessionDBPath(),
		"MessagesDBPath": cfg.MessagesDBPath(),
		"OutDir":         cfg.OutDir(),
	} {
		if !strings.HasPrefix(p, cfg.Paths.DataDir) {
			t.Errorf("%s = %q, deveria estar dentro de data_dir %q", name, p, cfg.Paths.DataDir)
		}
	}
	if cfg.SessionDBPath() == cfg.MessagesDBPath() {
		t.Error("sessão e mensagens não podem compartilhar o mesmo arquivo")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nao-existe.toml")); err == nil {
		t.Error("arquivo inexistente deveria falhar")
	}
}

// O exemplo embutido precisa continuar sendo um config válido: é o que
// `wappsync init` entrega, e um usuário novo bate nele primeiro.
func TestEmbeddedExampleIsValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := WriteExample(path); err != nil {
		t.Fatal(err)
	}

	// Como vem, falha por falta do path do Drive — e essa mensagem é a
	// primeira instrução que o usuário recebe.
	if _, err := Load(path); err == nil {
		t.Error("o exemplo deveria exigir que o usuário preencha o destino")
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	filled := strings.Replace(string(body), `path = ""`,
		"path = "+tomlPath(filepath.Join(dir, "drive")), 1)
	filled = strings.Replace(filled, `data_dir = ""`,
		"data_dir = "+tomlPath(filepath.Join(dir, "data")), 1)

	filledPath := filepath.Join(dir, "preenchido.toml")
	if err := os.WriteFile(filledPath, []byte(filled), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filledPath); err != nil {
		t.Errorf("exemplo preenchido não carrega: %v", err)
	}
}

func TestWriteExampleDoesNotOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := WriteExample(path); err != nil {
		t.Fatal(err)
	}
	if err := WriteExample(path); err == nil {
		t.Error("WriteExample sobrescreveu um config existente")
	}
}
