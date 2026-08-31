package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTemp(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func get(t *testing.T, db *DB, chat, id string) Message {
	t.Helper()
	msgs, err := db.Since(context.Background(), time.Unix(0, 0))
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	for _, m := range msgs {
		if m.ChatJID == chat && m.ID == id {
			return m
		}
	}
	t.Fatalf("mensagem %s/%s não encontrada", chat, id)
	return Message{}
}

// Um history sync antigo não pode apagar o que já foi capturado ao vivo.
// É a garantia central pedida: nunca sobrescrever com dado pior.
func TestUpsertNeverDegrades(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	ts := time.Now().Truncate(time.Second)

	live := Message{
		ID: "A1", ChatJID: "g@g.us", SenderJID: "u@s.whatsapp.net",
		SenderName: "João", Timestamp: ts, Kind: "text",
		Body: "chego às 19h", Source: "live",
	}
	if _, err := db.PutMessage(ctx, live); err != nil {
		t.Fatalf("put live: %v", err)
	}

	// Mesma mensagem vinda de history sync, sem corpo e sem nome do remetente.
	stale := live
	stale.Body = ""
	stale.SenderName = ""
	stale.Source = "history"
	written, err := db.PutMessage(ctx, stale)
	if err != nil {
		t.Fatalf("put stale: %v", err)
	}
	if written {
		t.Error("a versão degradada sobrescreveu a versão ao vivo")
	}
	if got := get(t, db, "g@g.us", "A1"); got.Body != "chego às 19h" || got.SenderName != "João" {
		t.Errorf("registro degradado: body=%q sender=%q", got.Body, got.SenderName)
	}
}

// Uma edição tem prioridade maior e deve vencer, mesmo chegando de history sync.
func TestEditWins(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	ts := time.Now().Truncate(time.Second)

	base := Message{ID: "A1", ChatJID: "g@g.us", Timestamp: ts, Kind: "text", Body: "19h", Source: "live"}
	if _, err := db.PutMessage(ctx, base); err != nil {
		t.Fatal(err)
	}

	edit := base
	edit.Body = "20h [editada]"
	edit.Revision = 1
	edit.Source = "history"
	written, err := db.PutMessage(ctx, edit)
	if err != nil {
		t.Fatal(err)
	}
	if !written {
		t.Fatal("a edição não foi aplicada")
	}
	if got := get(t, db, "g@g.us", "A1"); got.Body != "20h [editada]" || got.Revision != 1 {
		t.Errorf("edição não venceu: %+v", got)
	}
}

// Uma revogação vence uma mensagem normal, mas não uma edição posterior.
func TestDeleteWinsOverPlain(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	ts := time.Now().Truncate(time.Second)

	base := Message{ID: "A1", ChatJID: "g@g.us", Timestamp: ts, Kind: "text", Body: "ops", Source: "live"}
	if _, err := db.PutMessage(ctx, base); err != nil {
		t.Fatal(err)
	}
	del := base
	del.Deleted = true
	del.Body = "[mensagem apagada]"
	del.Kind = "revoke"
	if _, err := db.PutMessage(ctx, del); err != nil {
		t.Fatal(err)
	}
	if got := get(t, db, "g@g.us", "A1"); !got.Deleted {
		t.Errorf("revogação não venceu: %+v", got)
	}
}

// Reaplicar o mesmo lote não deve mudar nada: a ingestão é idempotente.
func TestUpsertIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	ts := time.Now().Truncate(time.Second)

	batch := []Message{
		{ID: "A1", ChatJID: "g@g.us", Timestamp: ts, Body: "um", Kind: "text", Source: "live"},
		{ID: "A2", ChatJID: "g@g.us", Timestamp: ts.Add(time.Minute), Body: "dois", Kind: "text", Source: "live"},
	}
	first, err := db.PutMessages(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	if first != 2 {
		t.Fatalf("primeira gravação escreveu %d, queria 2", first)
	}
	second, err := db.PutMessages(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	if second != 0 {
		t.Errorf("regravar o mesmo lote alterou %d linhas, queria 0", second)
	}
}

// A retenção remove o que passou do prazo e preserva o resto.
func TestPrune(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	now := time.Now()

	if _, err := db.PutMessages(ctx, []Message{
		{ID: "velha", ChatJID: "g@g.us", Timestamp: now.Add(-60 * 24 * time.Hour), Body: "x", Source: "live"},
		{ID: "nova", ChatJID: "g@g.us", Timestamp: now, Body: "y", Source: "live"},
	}); err != nil {
		t.Fatal(err)
	}
	n, err := db.Prune(ctx, now.Add(-45*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("prune removeu %d, queria 1", n)
	}
	msgs, err := db.Since(ctx, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].ID != "nova" {
		t.Errorf("sobrou %+v", msgs)
	}
}

// Um nome vazio nunca pode apagar um nome de conversa já conhecido.
func TestChatNameNotErasedByEmpty(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)

	if err := db.UpsertChat(ctx, Chat{JID: "g@g.us", Name: "Família", IsGroup: true}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertChat(ctx, Chat{JID: "g@g.us", Name: "", IsGroup: true}); err != nil {
		t.Fatal(err)
	}
	chats, err := db.Chats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if chats["g@g.us"].Name != "Família" {
		t.Errorf("nome apagado: %q", chats["g@g.us"].Name)
	}
}
