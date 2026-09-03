package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bmar13/wapp-summarizer/internal/config"
	"github.com/bmar13/wapp-summarizer/internal/store"
)

const (
	jidFamilia    = "120363000000000001@g.us"
	jidFinanceiro = "120363000000000002@g.us"
	jidAcao       = "5511999999999@s.whatsapp.net"
)

// chatsDeTeste tem acento de propósito (restrição 3 do B11): o nome vai para o
// arquivo como comentário e precisa chegar lá em UTF-8.
func chatsDeTeste() []setupChat {
	return []setupChat{
		{JID: jidFamilia, Name: "Família", IsGroup: true},
		{JID: jidFinanceiro, Name: "Financeiro", IsGroup: true},
		{JID: jidAcao, Name: "Ação Social"},
	}
}

// configAntigo é um config editado à mão, com comentário próprio, uma entrada
// [[media.chat]] e uma seção depois das que o setup governa — tudo o que o
// setup não pode atropelar ou precisa remover de propósito.
const configAntigo = `# meu comentário no topo
host_id = "maquina-teste"

[export]
interval_minutes = 7

[filter]
include_only = []
exclude = ["Velho"]

[media]
enabled = true
max_file_mb = 33

[[media.chat]]
match = "Velho"
kinds = ["image"]

[transcribe]
enabled = false
language = "pt"

[remote]
# comentário do remote
backend = "none"
`

func configDeTeste(t *testing.T) (string, *config.Config) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(configAntigo), 0o600); err != nil {
		t.Fatal(err)
	}
	cur, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, cur
}

func envDeTeste(modelo string) setupEnv {
	return setupEnv{
		fileExists: func(p string) bool { return p == modelo },
		haveBinary: func(string) bool { return true },
	}
}

// roteiro junta as respostas como se fossem digitadas, uma por linha.
func roteiro(respostas ...string) *strings.Reader {
	return strings.NewReader(strings.Join(respostas, "\n") + "\n")
}

func TestSetupParseSelection(t *testing.T) {
	cases := []struct {
		in   string
		want []int
		err  bool
	}{
		{"", nil, false},
		{"1 3 5-7", []int{0, 2, 4, 5, 6}, false},
		{"2,4", []int{1, 3}, false},
		{"3 3 1", []int{0, 2}, false},
		{"0", nil, true},
		{"9", nil, true},
		{"3-1", nil, true},
		{"x", nil, true},
	}
	for _, tc := range cases {
		got, err := parseSelection(tc.in, 8)
		if (err != nil) != tc.err {
			t.Errorf("parseSelection(%q): err = %v", tc.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseSelection(%q) = %v, queria %v", tc.in, got, tc.want)
		}
	}
}

func TestSetupWritesPolicyEndToEnd(t *testing.T) {
	path, cur := configDeTeste(t)
	modelo := filepath.Join(t.TempDir(), "ggml-small.bin")
	if err := os.WriteFile(modelo, []byte("modelo"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Exporta tudo menos o Financeiro (2); transcreve; imagens não; documentos
	// sim, menos da Família (1 na lista de exportadas); áudio sim, menos da
	// Ação Social (2 na lista de exportadas); grava.
	in := roteiro("s", "2", "s", modelo, "n", "s", "1", "s", "2", "s")
	var out strings.Builder
	if err := setupFlow(in, &out, chatsDeTeste(), cur, envDeTeste(modelo)); err != nil {
		t.Fatalf("setupFlow: %v\n%s", err, out.String())
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("o config gravado não carrega: %v", err)
	}
	if got := cfg.Filter.Exclude; !reflect.DeepEqual(got, []string{jidFinanceiro}) {
		t.Errorf("filter.exclude = %v", got)
	}
	if len(cfg.Filter.IncludeOnly) != 0 {
		t.Errorf("filter.include_only = %v, queria vazio", cfg.Filter.IncludeOnly)
	}
	if !cfg.Media.Enabled || !reflect.DeepEqual(cfg.Media.Kinds, []string{"document", "audio"}) {
		t.Errorf("media: enabled=%v kinds=%v", cfg.Media.Enabled, cfg.Media.Kinds)
	}
	if cfg.Media.MaxFileMB != 33 {
		t.Errorf("max_file_mb voltou ao default: %d", cfg.Media.MaxFileMB)
	}
	if len(cfg.Media.Chats) != 0 {
		t.Errorf("a [[media.chat]] antiga deveria ter sido removida: %+v", cfg.Media.Chats)
	}
	wantExc := []config.MediaChat{
		{Match: jidFamilia, Kinds: []string{"document"}},
		{Match: jidAcao, Kinds: []string{"audio"}},
	}
	if !reflect.DeepEqual(cfg.Media.Exclude, wantExc) {
		t.Errorf("media.exclude = %+v, queria %+v", cfg.Media.Exclude, wantExc)
	}
	if !cfg.Transcribe.Enabled || cfg.Transcribe.Model != filepath.Clean(modelo) || cfg.Transcribe.Language != "pt" {
		t.Errorf("transcribe = %+v", cfg.Transcribe)
	}

	// A política resolvida é o que importa, não o texto.
	for _, tc := range []struct {
		jid, name, kind string
		want            bool
	}{
		{jidFamilia, "Família", "document", false},
		{jidFamilia, "Família", "audio", true},
		{jidAcao, "Ação Social", "document", true},
		{jidAcao, "Ação Social", "audio", false},
		{jidFinanceiro, "Financeiro", "document", false},
		{jidFamilia, "Família", "image", false},
	} {
		if got := cfg.MediaAllowed(tc.jid, tc.name, tc.kind); got != tc.want {
			t.Errorf("MediaAllowed(%s, %s) = %v, queria %v", tc.name, tc.kind, got, tc.want)
		}
	}

	raw, _ := os.ReadFile(path)
	texto := string(raw)
	for _, frag := range []string{"# meu comentário no topo", `host_id = "maquina-teste"`, "interval_minutes = 7", "# comentário do remote", `backend = "none"`} {
		if !strings.Contains(texto, frag) {
			t.Errorf("o setup apagou algo que não governa: %q sumiu", frag)
		}
	}
	if !strings.Contains(texto, "# Ação Social") || !strings.Contains(texto, "# Família") {
		t.Error("nome com acento não chegou ao arquivo em UTF-8")
	}
	if strings.Contains(texto, "Velho") {
		t.Error("a política antiga continua no arquivo")
	}
	bak, err := os.ReadFile(path + ".bak")
	if err != nil || string(bak) != configAntigo {
		t.Errorf("backup ausente ou diferente do original: %v", err)
	}
	if _, err := os.Stat(path + ".setup-tmp"); err == nil {
		t.Error("temporário ficou para trás")
	}
}

func TestSetupIncludeOnly(t *testing.T) {
	path, cur := configDeTeste(t)
	// Não exporta tudo; escolhe 1 e 3; sem transcrição; sem imagem; sem
	// documento; grava.
	in := roteiro("n", "1 3", "n", "n", "n", "s")
	var out strings.Builder
	if err := setupFlow(in, &out, chatsDeTeste(), cur, envDeTeste("")); err != nil {
		t.Fatalf("setupFlow: %v\n%s", err, out.String())
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Filter.IncludeOnly; !reflect.DeepEqual(got, []string{jidFamilia, jidAcao}) {
		t.Errorf("include_only = %v", got)
	}
	if cfg.Media.Enabled || len(cfg.Media.Kinds) != 0 || cfg.Transcribe.Enabled {
		t.Errorf("nada de anexo deveria estar ligado: %+v %+v", cfg.Media, cfg.Transcribe)
	}
	if !cfg.ChatAllowed(jidFamilia, "Família") || cfg.ChatAllowed(jidFinanceiro, "Financeiro") {
		t.Error("include_only gravado não filtra como esperado")
	}
}

// Trava (restrição 4 do B11). Só apertar Enter tem que produzir um config que
// exporta texto e NÃO baixa nada: nem anexo, nem transcrição. Um assistente
// cujo padrão liga anexo é pior que nenhum.
func TestSetupDefaultsDownloadNothing(t *testing.T) {
	path, cur := configDeTeste(t)
	// Enter em tudo, e "s" só na confirmação final.
	in := roteiro("", "", "", "", "", "s")
	var out strings.Builder
	if err := setupFlow(in, &out, chatsDeTeste(), cur, envDeTeste("")); err != nil {
		t.Fatalf("setupFlow: %v\n%s", err, out.String())
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Filter.IncludeOnly) != 0 || len(cfg.Filter.Exclude) != 0 {
		t.Errorf("o padrão é exportar tudo: %+v", cfg.Filter)
	}
	if cfg.Media.Enabled || len(cfg.Media.Kinds) != 0 || len(cfg.Media.Exclude) != 0 || cfg.Transcribe.Enabled {
		t.Errorf("o padrão ligou anexo: media=%+v transcribe.enabled=%v", cfg.Media, cfg.Transcribe.Enabled)
	}
	for _, kind := range config.MediaKinds() {
		if cfg.MediaAllowed(jidFamilia, "Família", kind) {
			t.Errorf("padrão baixaria %s", kind)
		}
	}
}

// Trava. A confirmação final tem padrão NÃO, e recusar deixa o arquivo
// byte a byte como estava — sem backup, sem temporário.
func TestSetupDeclinedLeavesFileUntouched(t *testing.T) {
	path, cur := configDeTeste(t)
	in := roteiro("", "", "", "", "", "")
	var out strings.Builder
	err := setupFlow(in, &out, chatsDeTeste(), cur, envDeTeste(""))
	if !errors.Is(err, errSetupCancelled) {
		t.Fatalf("err = %v, queria errSetupCancelled", err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != configAntigo {
		t.Error("o arquivo mudou sem confirmação")
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Error("backup criado sem gravação")
	}
}

func TestSetupSpliceKeepsEverythingElse(t *testing.T) {
	original := "a = 1\r\n\r\n[filter]\r\nexclude = [\"x\"]\r\n\r\n[media]\r\nenabled = true\r\n\r\n[[media.chat]]\r\nmatch = \"x\"\r\nkinds = [\"image\"]\r\n\r\n[remote]\r\n# fica\r\nbackend = \"none\"\r\n\r\n[transcribe]\r\nenabled = true\r\n\r\n[merge]\r\nenabled = false\r\n"
	got := spliceConfig(original, "[filter]\nexclude = []\n\n[media]\nenabled = false\n\n[transcribe]\nenabled = false\n")
	want := "a = 1\r\n\r\n[filter]\r\nexclude = []\r\n\r\n[media]\r\nenabled = false\r\n\r\n[transcribe]\r\nenabled = false\r\n\r\n[remote]\r\n# fica\r\nbackend = \"none\"\r\n\r\n[merge]\r\nenabled = false\r\n"
	if got != want {
		t.Errorf("splice:\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
}

func TestSetupSpliceAppendsWhenSectionsAreMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	got := spliceConfig("host_id = \"m\"\n[remote]\nbackend = \"none\"\n", "[filter]\nexclude = []\n\n[media]\nenabled = false\n\n[transcribe]\nenabled = false\n")
	if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err != nil {
		t.Errorf("resultado não carrega: %v\n%s", err, got)
	}
	if !strings.HasPrefix(got, "host_id = \"m\"\n[remote]\nbackend = \"none\"\n\n[filter]\n") {
		t.Errorf("bloco deveria ir para o fim:\n%s", got)
	}
}

// Trava. Um config que o próprio wappsync recusaria nunca substitui o que
// estava funcionando — e não deixa temporário nem backup para trás.
func TestSetupRefusesToWriteInvalidConfig(t *testing.T) {
	path, _ := configDeTeste(t)
	err := writeConfigValidated(path, "window_days = 0\n")
	if err == nil {
		t.Fatal("config inválido foi gravado")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != configAntigo {
		t.Error("o original foi substituído por um config inválido")
	}
	for _, sufixo := range []string{".setup-tmp", ".bak"} {
		if _, err := os.Stat(path + sufixo); err == nil {
			t.Errorf("%s ficou para trás", sufixo)
		}
	}
}

func TestSetupMergeChatsSkipsContactsWithoutMessages(t *testing.T) {
	groups := []setupChat{{JID: jidFamilia, Name: "Família", IsGroup: true}}
	known := map[string]store.Chat{
		jidFamilia:                {JID: jidFamilia, Name: "Família", IsGroup: true, LastTS: time.Now()},
		"111@s.whatsapp.net":      {JID: "111@s.whatsapp.net", Name: "Zeca", LastTS: time.Now()},
		"222@s.whatsapp.net":      {JID: "222@s.whatsapp.net", Name: "Agenda sem mensagem"},
		"333@s.whatsapp.net":      {JID: "333@s.whatsapp.net", Name: "Ana", LastTS: time.Now()},
		"120363000000000009@g.us": {JID: "120363000000000009@g.us", Name: "Grupo que saí", IsGroup: true, LastTS: time.Now()},
		"444@s.whatsapp.net":      {JID: "444@s.whatsapp.net", LastTS: time.Now()},
	}
	got := mergeSetupChats(groups, known)
	var labels []string
	for _, c := range got {
		labels = append(labels, c.label())
	}
	want := []string{"Família", "444@s.whatsapp.net", "Ana", "Zeca"}
	if !reflect.DeepEqual(labels, want) {
		t.Errorf("lista = %v, queria %v", labels, want)
	}
}
