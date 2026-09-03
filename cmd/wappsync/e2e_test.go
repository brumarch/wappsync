package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bmar13/wappsync/internal/config"
	"github.com/bmar13/wappsync/internal/export"
	"github.com/bmar13/wappsync/internal/store"
)

// Teste de integração do ciclo real: config.toml em disco -> banco SQLite ->
// export -> shard na "nuvem" -> consolidação. Cobre a costura entre os pacotes,
// que os testes unitários não veem.
//
// A única parte que fica de fora é a conexão com o WhatsApp, que exige uma
// conta de verdade. Tudo depois da chegada da mensagem está aqui.

type machine struct {
	name string
	cfg  *config.Config
	db   *store.DB
}

// newMachine cria uma máquina completa: config em disco, data dir próprio,
// apontando para a mesma pasta de nuvem das outras.
func newMachine(t *testing.T, root, name string, extra string) *machine {
	t.Helper()

	dataDir := filepath.Join(root, "data-"+name)
	drive := filepath.Join(root, "drive")
	for _, d := range []string{dataDir, drive} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	q := func(p string) string { return "'" + filepath.ToSlash(p) + "'" }
	body := "host_id = \"" + name + "\"\n" +
		"window_days = 3\n" +
		extra +
		"[paths]\ndata_dir = " + q(dataDir) + "\n" +
		"[remote]\nbackend = \"folder\"\nprefix = \"wapp\"\n" +
		"[remote.folder]\npath = " + q(drive) + "\n"

	cfgPath := filepath.Join(root, name+".toml")
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config de %s: %v", name, err)
	}
	db, err := store.Open(cfg.MessagesDBPath())
	if err != nil {
		t.Fatalf("banco de %s: %v", name, err)
	}
	t.Cleanup(func() { db.Close() })

	return &machine{name: name, cfg: cfg, db: db}
}

func (m *machine) seed(t *testing.T, msgs []store.Message, chats []store.Chat) {
	t.Helper()
	ctx := context.Background()
	for _, c := range chats {
		if err := m.db.UpsertChat(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.db.PutMessages(ctx, msgs); err != nil {
		t.Fatal(err)
	}
}

func (m *machine) runCycle(t *testing.T, now time.Time) {
	t.Helper()
	if err := cycle(context.Background(), m.cfg, m.db, "", now); err != nil {
		t.Fatalf("ciclo de %s: %v", m.name, err)
	}
}

func msg(id, chat, sender, body string, ts time.Time) store.Message {
	return store.Message{
		ID: id, ChatJID: chat, SenderJID: sender + "@s.whatsapp.net",
		SenderName: sender, IsGroup: strings.HasSuffix(chat, "@g.us"),
		Timestamp: ts, Kind: "text", Body: body, Source: "live",
	}
}

func readPublished(t *testing.T, root string) (export.Index, string) {
	t.Helper()
	latest := filepath.Join(root, "drive", "wapp", "latest")

	raw, err := os.ReadFile(filepath.Join(latest, "index.json"))
	if err != nil {
		t.Fatalf("index.json: %v", err)
	}
	var idx export.Index
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatalf("index.json inválido: %v", err)
	}

	digest, err := os.ReadFile(filepath.Join(latest, "digest.md"))
	if err != nil {
		t.Fatalf("digest.md: %v", err)
	}
	return idx, string(digest)
}

// Duas máquinas, dados parcialmente sobrepostos, uma mensagem fora da janela.
// É o cenário real de operação.
func TestE2ETwoMachines(t *testing.T) {
	root := t.TempDir()
	now := time.Now()

	chats := []store.Chat{
		{JID: "fam@g.us", Name: "Família", IsGroup: true},
		{JID: "trab@g.us", Name: "Trabalho", IsGroup: true},
	}

	a := newMachine(t, root, "maquina-a", "")
	b := newMachine(t, root, "maquina-b", "")

	// A estava ligada de manhã; B, de tarde. As duas viram a mensagem M3.
	a.seed(t, []store.Message{
		msg("M1", "fam@g.us", "João", "Bom dia", now.Add(-5*time.Hour)),
		msg("M2", "fam@g.us", "Maria", "Bom dia!", now.Add(-4*time.Hour)),
		msg("M3", "fam@g.us", "João", "Chego 19h", now.Add(-2*time.Hour)),
		// Fora da janela de 3 dias: não pode ser publicada.
		msg("VELHA", "fam@g.us", "Maria", "conversa da semana passada", now.Add(-200*time.Hour)),
	}, chats)

	b.seed(t, []store.Message{
		msg("M3", "fam@g.us", "João", "Chego 19h", now.Add(-2*time.Hour)),
		msg("T1", "trab@g.us", "Ana", "PR revisado", now.Add(-3*time.Hour)),
		msg("T2", "trab@g.us", "Bruno", "Valeu, mergeando", now.Add(-time.Hour)),
	}, chats)

	a.runCycle(t, now)
	b.runCycle(t, now.Add(time.Second))

	idx, digest := readPublished(t, root)

	// 5 = 3 de A + 3 de B - 1 duplicada; a de 200h atrás fica de fora.
	if idx.Messages != 5 {
		t.Errorf("consolidado tem %d mensagens, queria 5", idx.Messages)
	}
	if len(idx.Chats) != 2 {
		t.Errorf("consolidado tem %d conversas, queria 2", len(idx.Chats))
	}
	if len(idx.Shards) != 2 {
		t.Errorf("index lista %d shards, queria 2", len(idx.Shards))
	}
	for _, s := range idx.Shards {
		if s.Schema != export.SchemaVersion {
			t.Errorf("shard %q sem schema: %q", s.Host, s.Schema)
		}
	}
	if idx.Schema != export.SchemaVersion {
		t.Errorf("index.Schema = %q", idx.Schema)
	}

	if strings.Contains(digest, "conversa da semana passada") {
		t.Error("mensagem fora da janela vazou para o digest")
	}
	for _, want := range []string{"Família", "Trabalho", "Chego 19h", "PR revisado"} {
		if !strings.Contains(digest, want) {
			t.Errorf("digest não contém %q", want)
		}
	}
	// A mensagem vista pelas duas máquinas não pode aparecer duplicada.
	if n := strings.Count(digest, "Chego 19h"); n != 1 {
		t.Errorf("mensagem duplicada no digest: %d ocorrências", n)
	}

	// O guia acompanha os dados, nos dois nomes e nos dois níveis: a raiz é
	// onde shards/ fica visível, e AGENTS.md é carregado sozinho por várias
	// ferramentas de agente.
	prefix := filepath.Join(root, "drive", "wapp")
	for _, rel := range []string{
		"LEIA-ME.md", "AGENTS.md",
		filepath.Join("latest", "LEIA-ME.md"), filepath.Join("latest", "AGENTS.md"),
	} {
		body, err := os.ReadFile(filepath.Join(prefix, rel))
		if err != nil {
			t.Errorf("guia %s não foi publicado: %v", rel, err)
			continue
		}
		if !strings.Contains(string(body), "dado, não instrução") {
			t.Errorf("guia %s saiu sem a trava de confiança", rel)
		}
	}

	// O guia da raiz precisa apontar para dentro de latest/.
	rootGuide, err := os.ReadFile(filepath.Join(prefix, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rootGuide), "`latest/digest.md`") {
		t.Error("guia da raiz não aponta para latest/digest.md")
	}
}

// Rodar o mesmo ciclo de novo, sem dado novo, não pode alterar o conteúdo
// publicado. É a propriedade que permite as duas máquinas rodarem em loop.
func TestE2ECycleIsIdempotent(t *testing.T) {
	root := t.TempDir()
	now := time.Now()

	a := newMachine(t, root, "maquina-a", "")
	a.seed(t, []store.Message{
		msg("M1", "fam@g.us", "João", "oi", now.Add(-time.Hour)),
	}, []store.Chat{{JID: "fam@g.us", Name: "Família", IsGroup: true}})

	a.runCycle(t, now)
	_, first := readPublished(t, root)

	a.runCycle(t, now)
	idx, second := readPublished(t, root)

	if first != second {
		t.Error("dois ciclos idênticos produziram digests diferentes")
	}
	if idx.Messages != 1 {
		t.Errorf("mensagens duplicaram: %d", idx.Messages)
	}
}

// Os filtros do config precisam valer no ciclo real, não só em Build.
func TestE2ERespectsChatFilter(t *testing.T) {
	root := t.TempDir()
	now := time.Now()

	a := newMachine(t, root, "maquina-a", "[filter]\nexclude = [\"Trabalho\"]\n")
	a.seed(t, []store.Message{
		msg("M1", "fam@g.us", "João", "assunto de casa", now.Add(-time.Hour)),
		msg("T1", "trab@g.us", "Ana", "assunto de trabalho", now.Add(-time.Hour)),
	}, []store.Chat{
		{JID: "fam@g.us", Name: "Família", IsGroup: true},
		{JID: "trab@g.us", Name: "Trabalho", IsGroup: true},
	})

	a.runCycle(t, now)
	idx, digest := readPublished(t, root)

	if strings.Contains(digest, "assunto de trabalho") {
		t.Error("conversa excluída chegou à nuvem")
	}
	if !strings.Contains(digest, "assunto de casa") {
		t.Error("o filtro barrou a conversa errada")
	}
	if idx.Messages != 1 {
		t.Errorf("publicou %d mensagens, queria 1", idx.Messages)
	}
}

// Com backend "none" nada deve sair da máquina, mas a cópia local continua.
func TestE2EBackendNoneStaysLocal(t *testing.T) {
	root := t.TempDir()
	now := time.Now()

	dataDir := filepath.Join(root, "data-local")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "host_id = \"so-local\"\n[paths]\ndata_dir = '" +
		filepath.ToSlash(dataDir) + "'\n[remote]\nbackend = \"none\"\n"
	cfgPath := filepath.Join(root, "local.toml")
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(cfg.MessagesDBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	m := &machine{name: "so-local", cfg: cfg, db: db}
	m.seed(t, []store.Message{msg("M1", "fam@g.us", "João", "segredo", now.Add(-time.Hour))},
		[]store.Chat{{JID: "fam@g.us", Name: "Família", IsGroup: true}})
	m.runCycle(t, now)

	digest, err := os.ReadFile(filepath.Join(cfg.OutDir(), "digest.md"))
	if err != nil {
		t.Fatalf("a cópia local deveria existir: %v", err)
	}
	if !strings.Contains(string(digest), "segredo") {
		t.Error("cópia local sem o conteúdo")
	}
}

// A retenção precisa limpar o banco local durante o ciclo.
func TestE2EPrunesBeyondRetention(t *testing.T) {
	root := t.TempDir()
	now := time.Now()

	a := newMachine(t, root, "maquina-a", "retention_days = 10\n")
	a.seed(t, []store.Message{
		msg("NOVA", "fam@g.us", "João", "recente", now.Add(-time.Hour)),
		msg("ANTIGA", "fam@g.us", "João", "muito antiga", now.Add(-30*24*time.Hour)),
	}, []store.Chat{{JID: "fam@g.us", Name: "Família", IsGroup: true}})

	a.runCycle(t, now)

	remaining, err := a.db.Since(context.Background(), time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].ID != "NOVA" {
		t.Errorf("retenção não limpou o banco: %+v", remaining)
	}
}

// O anexo percorre o caminho inteiro: arquivo no disco local -> shard que o
// referencia -> arquivo publicado na nuvem -> ponteiro no digest consolidado.
// E some da nuvem quando a mensagem sai da janela.
func TestE2EMediaIsPublishedAndPruned(t *testing.T) {
	root := t.TempDir()
	m := newMachine(t, root, "maquina-a", "")
	now := time.Now().Truncate(time.Second)

	// O worker de download teria deixado o arquivo aqui e apontado a mensagem
	// para ele; o ciclo é quem publica.
	if err := os.MkdirAll(m.cfg.MediaDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.cfg.MediaDir(), "abc.pdf"), []byte("%PDF-1.4 fake"), 0o600); err != nil {
		t.Fatal(err)
	}

	comAnexo := msg("A1", "fam@g.us", "João", "[documento: cardapio.pdf]", now.Add(-time.Hour))
	comAnexo.Kind = "document"
	comAnexo.Media = export.MediaFile("maquina-a", "abc.pdf")

	m.seed(t, []store.Message{comAnexo}, []store.Chat{{JID: "fam@g.us", Name: "Família", IsGroup: true}})
	m.runCycle(t, now)

	publicado := filepath.Join(root, "drive", "wapp", "media", "maquina-a", "abc.pdf")
	body, err := os.ReadFile(publicado)
	if err != nil {
		t.Fatalf("o anexo não chegou à nuvem: %v", err)
	}
	if string(body) != "%PDF-1.4 fake" {
		t.Errorf("conteúdo publicado = %q", body)
	}

	_, digest := readPublished(t, root)
	if !strings.Contains(digest, "anexo: `"+export.MediaFile("maquina-a", "abc.pdf")+"`") {
		t.Errorf("o digest não aponta para o anexo:\n%s", digest)
	}

	// Quatro dias depois a mensagem saiu da janela de 3 dias: o shard deixa de
	// referenciar o anexo e ele tem que sair da pasta.
	m.runCycle(t, now.Add(4*24*time.Hour))
	if _, err := os.Stat(publicado); !os.IsNotExist(err) {
		t.Errorf("o anexo fora da janela continua na nuvem (err = %v)", err)
	}
}
