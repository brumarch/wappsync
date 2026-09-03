package export

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brumarch/wappsync/internal/config"
	"github.com/brumarch/wappsync/internal/store"
)

// -update regrava o arquivo golden. Use quando a MUDANÇA de formato for
// intencional, e leia o diff antes de commitar:
//
//	go test ./internal/export/ -run TestDigestGolden -update
var update = flag.Bool("update", false, "regrava os arquivos golden")

// O digest é o produto final: é ele que o agente lê para escrever o briefing.
// Uma regressão de formatação aqui não quebra teste nenhum e não gera erro —
// só piora silenciosamente o resumo. Daí o golden.
//
// O relógio é fixo e o fuso é UTC de propósito: sem isso o arquivo mudaria a
// cada execução e em cada máquina.
func TestDigestGolden(t *testing.T) {
	now := time.Date(2026, 3, 15, 18, 30, 0, 0, time.UTC)
	from := now.Add(-72 * time.Hour)

	cfg := &config.Config{
		WindowDays: 3,
		HostID:     "bruno-win",
		Export: config.Export{
			Formats:           []string{"jsonl", "markdown"},
			IncludeFromMe:     true,
			MediaPlaceholders: true,
			MaxMessageChars:   4000,
		},
	}

	at := func(dayOffset int, hour, min int) time.Time {
		return time.Date(2026, 3, 15+dayOffset, hour, min, 0, 0, time.UTC)
	}

	chats := map[string]store.Chat{
		"120363000000000001@g.us":      {JID: "120363000000000001@g.us", Name: "Família", IsGroup: true},
		"120363000000000002@g.us":      {JID: "120363000000000002@g.us", Name: "Squad Backend", IsGroup: true},
		"5511900000001@s.whatsapp.net": {JID: "5511900000001@s.whatsapp.net", Name: "Ana Souza"},
	}

	msgs := []store.Message{
		// Dois dias atrás, no grupo da família.
		{
			ID: "F1", ChatJID: "120363000000000001@g.us", SenderJID: "5511900000002@s.whatsapp.net",
			SenderName: "João", IsGroup: true, Timestamp: at(-2, 9, 12),
			Kind: "text", Body: "Bom dia! Alguém confirma o almoço de domingo?", Source: "live",
		},
		{
			ID: "F2", ChatJID: "120363000000000001@g.us", SenderJID: "5511900000003@s.whatsapp.net",
			SenderName: "Maria", IsGroup: true, Timestamp: at(-2, 9, 40),
			Kind: "text", Body: "Confirmo!", QuotedID: "F1",
			QuotedText: "Bom dia! Alguém confirma o almoço de domingo?", Source: "live",
		},
		{
			ID: "F3", ChatJID: "120363000000000001@g.us", SenderJID: "5511900000002@s.whatsapp.net",
			SenderName: "João", IsGroup: true, Timestamp: at(-1, 20, 5),
			Kind: "image", Body: "[imagem] o bolo ficou assim", Source: "live",
		},
		// Documento com anexo baixado: a linha tem que trazer o caminho, e é
		// esse ponteiro que faz o agente abrir o arquivo em vez de adivinhar.
		{
			ID: "F5", ChatJID: "120363000000000001@g.us", SenderJID: "5511900000003@s.whatsapp.net",
			SenderName: "Maria", IsGroup: true, Timestamp: at(-1, 20, 12),
			Kind: "document", Body: "[documento: cardapio.pdf]", Source: "live",
			Media: "media/bruno-win/9f2c4e1a.pdf",
		},
		// Mensagem apagada: tem que aparecer riscada.
		{
			ID: "F4", ChatJID: "120363000000000001@g.us", SenderJID: "5511900000003@s.whatsapp.net",
			SenderName: "Maria", IsGroup: true, Timestamp: at(-1, 20, 30),
			Kind: "revoke", Body: "[mensagem apagada]", Deleted: true, Source: "live",
		},
		// Grupo de trabalho, incluindo uma mensagem própria e uma editada.
		{
			ID: "T1", ChatJID: "120363000000000002@g.us", SenderJID: "5511900000004@s.whatsapp.net",
			SenderName: "Ana", IsGroup: true, Timestamp: at(0, 10, 0),
			Kind: "text", Body: "Subi o PR do parser, dá uma olhada", Source: "live",
		},
		{
			ID: "T2", ChatJID: "120363000000000002@g.us", SenderJID: "eu@s.whatsapp.net",
			SenderName: "eu", IsFromMe: true, IsGroup: true, Timestamp: at(0, 11, 15),
			Kind: "text", Body: "revisando agora [editada]", Revision: 1, Source: "live",
		},
		{
			ID: "T3", ChatJID: "120363000000000002@g.us", SenderJID: "5511900000004@s.whatsapp.net",
			SenderName: "Ana", IsGroup: true, Timestamp: at(0, 11, 20),
			Kind: "audio", Body: "[áudio (voz) 47s]", Source: "live",
		},
		// Áudio transcrito: o marcador de duração continua, a fala entra entre
		// aspas, e o texto completo fica no .txt apontado por Media.
		{
			ID: "T4", ChatJID: "120363000000000002@g.us", SenderJID: "5511900000004@s.whatsapp.net",
			SenderName: "Ana", IsGroup: true, Timestamp: at(0, 11, 22),
			Kind: "audio", Body: `[áudio (voz) 12s] "consigo revisar hoje à tarde, mas o deploy fica pra amanhã"`,
			Source: "live", Media: "media/bruno-win/4d81b7c3.txt",
		},
		// Conversa individual, com quebra de linha no corpo.
		{
			ID: "D1", ChatJID: "5511900000001@s.whatsapp.net", SenderJID: "5511900000001@s.whatsapp.net",
			SenderName: "Ana Souza", Timestamp: at(0, 14, 2),
			Kind: "text", Body: "lista do mercado:\n- café\n- pão", Source: "live",
		},
		// Sem nome de remetente: o digest deve cair no número.
		{
			ID: "D2", ChatJID: "5511900000001@s.whatsapp.net", SenderJID: "5511900000009@s.whatsapp.net",
			Timestamp: at(0, 14, 30), Kind: "text", Body: "quem é?", Source: "live",
		},
	}

	recs := Build(cfg, msgs, chats, from)
	idx := BuildIndex(cfg, recs, from, now, cfg.HostID)
	idx.Shards = []ShardMeta{
		{Host: "bruno-win", Schema: SchemaVersion, Messages: 11, GeneratedAt: now},
		{Host: "bruno-mac", Schema: SchemaVersion, Messages: 7, GeneratedAt: now.Add(-5 * time.Minute)},
	}

	got := MarshalMarkdown(idx, recs, time.UTC)
	goldenPath := filepath.Join("testdata", "digest.golden.md")

	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, got, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("golden regravado: %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("golden não encontrado (%v). Gere com:\n"+
			"  go test ./internal/export/ -run TestDigestGolden -update", err)
	}

	if string(got) != string(want) {
		t.Errorf("o digest mudou em relação ao golden.\n"+
			"Se a mudança for intencional, revise o diff e rode:\n"+
			"  go test ./internal/export/ -run TestDigestGolden -update\n\n"+
			"--- obtido ---\n%s\n--- esperado ---\n%s", got, want)
	}
}
