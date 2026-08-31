package export

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bmar13/wapp-summarizer/internal/config"
	"github.com/bmar13/wapp-summarizer/internal/store"
)

func baseCfg() *config.Config {
	return &config.Config{
		WindowDays: 3,
		HostID:     "maquina-a",
		Export: config.Export{
			Formats:           []string{"jsonl", "markdown"},
			IncludeFromMe:     true,
			MediaPlaceholders: true,
			MaxMessageChars:   4000,
		},
	}
}

func msgs(now time.Time) []store.Message {
	return []store.Message{
		{
			ID: "M1", ChatJID: "fam@g.us", SenderJID: "joao@s.whatsapp.net",
			SenderName: "João", IsGroup: true, Timestamp: now.Add(-2 * time.Hour),
			Kind: "text", Body: "chego 19h", Source: "live",
		},
		{
			ID: "M2", ChatJID: "fam@g.us", SenderJID: "eu@s.whatsapp.net",
			SenderName: "eu", IsFromMe: true, IsGroup: true, Timestamp: now.Add(-time.Hour),
			Kind: "text", Body: "beleza", Source: "live",
		},
		{
			ID: "M3", ChatJID: "trab@g.us", SenderJID: "ana@s.whatsapp.net",
			SenderName: "Ana", IsGroup: true, Timestamp: now.Add(-30 * time.Minute),
			Kind: "text", Body: "meu celular é (11) 98765-4321", Source: "live",
		},
	}
}

func chats() map[string]store.Chat {
	return map[string]store.Chat{
		"fam@g.us":  {JID: "fam@g.us", Name: "Família", IsGroup: true},
		"trab@g.us": {JID: "trab@g.us", Name: "Trabalho", IsGroup: true},
	}
}

func TestBuildAppliesWindow(t *testing.T) {
	now := time.Now()
	cfg := baseCfg()
	all := append(msgs(now), store.Message{
		ID: "VELHA", ChatJID: "fam@g.us", Timestamp: now.Add(-96 * time.Hour),
		Kind: "text", Body: "antiga", Source: "live",
	})

	recs := Build(cfg, all, chats(), now.Add(-72*time.Hour))
	for _, r := range recs {
		if r.ID == "VELHA" {
			t.Fatal("mensagem fora da janela foi exportada")
		}
	}
	if len(recs) != 3 {
		t.Errorf("exportou %d, queria 3", len(recs))
	}
}

func TestBuildExcludeFilter(t *testing.T) {
	now := time.Now()
	cfg := baseCfg()
	cfg.Filter.Exclude = []string{"Trabalho"}

	recs := Build(cfg, msgs(now), chats(), now.Add(-72*time.Hour))
	for _, r := range recs {
		if r.Chat == "trab@g.us" {
			t.Fatal("conversa excluída vazou para o export")
		}
	}
	if len(recs) != 2 {
		t.Errorf("exportou %d, queria 2", len(recs))
	}
}

func TestBuildIncludeOnlyIsAllowlist(t *testing.T) {
	now := time.Now()
	cfg := baseCfg()
	cfg.Filter.IncludeOnly = []string{"Família"}

	recs := Build(cfg, msgs(now), chats(), now.Add(-72*time.Hour))
	if len(recs) != 2 {
		t.Fatalf("exportou %d, queria 2", len(recs))
	}
	for _, r := range recs {
		if r.Chat != "fam@g.us" {
			t.Errorf("conversa fora da allowlist exportada: %s", r.Chat)
		}
	}
}

func TestBuildSkipsOwnMessagesWhenDisabled(t *testing.T) {
	now := time.Now()
	cfg := baseCfg()
	cfg.Export.IncludeFromMe = false

	recs := Build(cfg, msgs(now), chats(), now.Add(-72*time.Hour))
	for _, r := range recs {
		if r.FromMe {
			t.Fatal("mensagem própria exportada com include_from_me = false")
		}
	}
}

func TestBuildRedacts(t *testing.T) {
	now := time.Now()
	cfg := baseCfg()
	cfg.RedactRegexp = []*regexp.Regexp{regexp.MustCompile(`\(\d{2}\) \d{4,5}-\d{4}`)}

	recs := Build(cfg, msgs(now), chats(), now.Add(-72*time.Hour))
	for _, r := range recs {
		if strings.Contains(r.Text, "98765-4321") {
			t.Fatalf("número não foi redigido: %q", r.Text)
		}
	}
}

func TestJSONLRoundTrip(t *testing.T) {
	now := time.Now()
	cfg := baseCfg()
	recs := Build(cfg, msgs(now), chats(), now.Add(-72*time.Hour))

	data, err := MarshalJSONL(recs)
	if err != nil {
		t.Fatal(err)
	}
	back, skipped := UnmarshalJSONL(data)
	if skipped != 0 {
		t.Fatalf("%d linhas descartadas", skipped)
	}
	if len(back) != len(recs) {
		t.Fatalf("round-trip: %d != %d", len(back), len(recs))
	}
	for i := range recs {
		if back[i].ID != recs[i].ID || back[i].Text != recs[i].Text || !back[i].Timestamp.Equal(recs[i].Timestamp) {
			t.Errorf("registro %d divergiu: %+v vs %+v", i, back[i], recs[i])
		}
	}
}

func TestUnmarshalJSONLTolerateCorruptLines(t *testing.T) {
	data := []byte("{\"id\":\"A\",\"chat\":\"g@g.us\",\"ts\":\"2026-08-31T10:00:00Z\"}\nlixo aqui\n\n{\"id\":\"B\",\"chat\":\"g@g.us\",\"ts\":\"2026-08-31T11:00:00Z\"}\n")
	recs, skipped := UnmarshalJSONL(data)
	if len(recs) != 2 {
		t.Errorf("leu %d registros, queria 2", len(recs))
	}
	if skipped != 1 {
		t.Errorf("contou %d linhas ruins, queria 1", skipped)
	}
}

func TestMarkdownStructure(t *testing.T) {
	now := time.Now()
	cfg := baseCfg()
	from := now.Add(-72 * time.Hour)
	recs := Build(cfg, msgs(now), chats(), from)
	idx := BuildIndex(cfg, recs, from, now, cfg.HostID)

	md := string(MarshalMarkdown(idx, recs, time.UTC))

	for _, want := range []string{
		"# WhatsApp — últimos 3 dia(s)",
		"## Índice",
		"<!-- jid: fam@g.us -->",
		"Família",
		"Trabalho",
		"**João**",
		"chego 19h",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("digest.md não contém %q", want)
		}
	}
}

// Quebras de linha viram um separador visível para não estourar a lista do Markdown.
func TestMarkdownFlattensNewlines(t *testing.T) {
	now := time.Now()
	cfg := baseCfg()
	from := now.Add(-72 * time.Hour)
	recs := Build(cfg, []store.Message{{
		ID: "M1", ChatJID: "fam@g.us", SenderName: "João", IsGroup: true,
		Timestamp: now.Add(-time.Hour), Kind: "text",
		Body: "linha um\nlinha dois", Source: "live",
	}}, chats(), from)
	idx := BuildIndex(cfg, recs, from, now, cfg.HostID)

	md := string(MarshalMarkdown(idx, recs, time.UTC))
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "- `") && strings.Contains(line, "linha um") {
			if !strings.Contains(line, "linha dois") {
				t.Error("a mensagem foi quebrada em várias linhas do Markdown")
			}
			return
		}
	}
	t.Error("linha da mensagem não encontrada no digest")
}

func TestBuildIndexTracksLastMessage(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	cfg := baseCfg()
	from := now.Add(-72 * time.Hour)
	recs := Build(cfg, msgs(now), chats(), from)
	idx := BuildIndex(cfg, recs, from, now, cfg.HostID)

	if idx.Messages != 3 {
		t.Errorf("index conta %d, queria 3", idx.Messages)
	}
	if len(idx.Chats) != 2 {
		t.Errorf("index tem %d conversas, queria 2", len(idx.Chats))
	}
	want := now.Add(-30 * time.Minute).UTC()
	if !idx.LastMessageTS.Equal(want) {
		t.Errorf("LastMessageTS = %v, queria %v", idx.LastMessageTS, want)
	}
	// Conversas ordenadas por atividade mais recente.
	if idx.Chats[0].JID != "trab@g.us" {
		t.Errorf("primeira conversa é %s, queria trab@g.us", idx.Chats[0].JID)
	}
}
