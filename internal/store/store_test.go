package store

import (
	"context"
	"database/sql"
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

// ---------------------------------------------------------------- anexos ---

// SetMedia é o passo final do download, e passa pelo mesmo UPSERT das outras
// escritas: o anexo entra porque eleva o prio, não porque contorna a regra.
func TestSetMediaAttachesToStoredMessage(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	ts := time.Now().Truncate(time.Second)

	base := Message{
		ChatJID: "fam@g.us", ID: "M1", SenderName: "João", Timestamp: ts,
		Kind: "image", Body: "[imagem] na praia", Source: "live",
	}
	if _, err := db.PutMessage(ctx, base); err != nil {
		t.Fatal(err)
	}
	antes := get(t, db, "fam@g.us", "M1")

	ok, err := db.SetMedia(ctx, "fam@g.us", "M1", "media/maquina-a/abc.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("SetMedia não gravou: o download seria descartado pelo UPSERT")
	}

	depois := get(t, db, "fam@g.us", "M1")
	if depois.Media != "media/maquina-a/abc.jpg" {
		t.Errorf("Media = %q", depois.Media)
	}
	if depois.Body != antes.Body || depois.SenderName != antes.SenderName {
		t.Errorf("o anexo apagou conteúdo: %+v -> %+v", antes, depois)
	}
	if !(depois.Rank() > antes.Rank()) {
		t.Errorf("prio não subiu: %d -> %d", antes.Rank(), depois.Rank())
	}

	// Repetir não muda nada: o worker pode rodar duas vezes para o mesmo anexo.
	if again, err := db.SetMedia(ctx, "fam@g.us", "M1", "media/maquina-a/abc.jpg"); err != nil || again {
		t.Errorf("SetMedia repetido = %v / %v, queria false / nil", again, err)
	}
}

// Sem a linha no banco não há o que apontar — e o arquivo baixado viraria
// órfão na pasta. Por isso o download só é enfileirado depois da gravação.
func TestSetMediaOnUnknownMessageIsNoop(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)

	ok, err := db.SetMedia(ctx, "fam@g.us", "NAO-EXISTE", "media/maquina-a/abc.jpg")
	if err != nil {
		t.Fatalf("SetMedia: %v", err)
	}
	if ok {
		t.Error("SetMedia criou uma mensagem que não existia")
	}
}

func TestMediaPathsListsOnlyTheWindow(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	now := time.Now().Truncate(time.Second)

	msgs := []Message{
		{ChatJID: "g@g.us", ID: "A", Timestamp: now, Kind: "image", Body: "[imagem]",
			Source: "live", Media: "media/maquina-a/novo.jpg"},
		{ChatJID: "g@g.us", ID: "B", Timestamp: now.Add(-48 * time.Hour), Kind: "image",
			Body: "[imagem]", Source: "live", Media: "media/maquina-a/velho.jpg"},
		{ChatJID: "g@g.us", ID: "C", Timestamp: now, Kind: "text", Body: "sem anexo", Source: "live"},
	}
	if _, err := db.PutMessages(ctx, msgs); err != nil {
		t.Fatal(err)
	}

	paths, err := db.MediaPaths(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || !paths["media/maquina-a/novo.jpg"] {
		t.Errorf("MediaPaths = %v, queria só o anexo dentro da janela", paths)
	}
}

// Banco criado por um binário anterior não tem a coluna `media`, e
// CREATE TABLE IF NOT EXISTS não a acrescenta. Sem a migração, todo INSERT
// passaria a falhar logo depois da atualização.
func TestOpenMigratesDatabaseWithoutMediaColumn(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "antigo.db")

	antigo, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := antigo.Exec(`
		CREATE TABLE messages (
			chat_jid TEXT NOT NULL, id TEXT NOT NULL,
			sender_jid TEXT NOT NULL DEFAULT '', sender_name TEXT NOT NULL DEFAULT '',
			is_from_me INTEGER NOT NULL DEFAULT 0, is_group INTEGER NOT NULL DEFAULT 0,
			ts INTEGER NOT NULL, kind TEXT NOT NULL DEFAULT 'text',
			body TEXT NOT NULL DEFAULT '', quoted_id TEXT NOT NULL DEFAULT '',
			quoted_text TEXT NOT NULL DEFAULT '', revision INTEGER NOT NULL DEFAULT 0,
			deleted INTEGER NOT NULL DEFAULT 0, source TEXT NOT NULL DEFAULT 'live',
			prio INTEGER NOT NULL DEFAULT 0, seen_at INTEGER NOT NULL,
			PRIMARY KEY (chat_jid, id)
		)`); err != nil {
		t.Fatal(err)
	}
	if _, err := antigo.Exec(
		`INSERT INTO messages (chat_jid, id, ts, body, seen_at) VALUES ('g@g.us','VELHA',?, 'de antes', 0)`,
		time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := antigo.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open num banco sem a coluna media: %v", err)
	}
	defer db.Close()

	// A mensagem antiga continua legível, agora com Media vazio...
	velha := get(t, db, "g@g.us", "VELHA")
	if velha.Body != "de antes" || velha.Media != "" {
		t.Errorf("mensagem antiga veio errada: %+v", velha)
	}
	// ...e escrever com anexo passa a funcionar.
	if _, err := db.PutMessage(ctx, Message{
		ChatJID: "g@g.us", ID: "NOVA", Timestamp: time.Now(), Kind: "image",
		Body: "[imagem]", Source: "live", Media: "media/maquina-a/abc.jpg",
	}); err != nil {
		t.Fatalf("gravando com anexo depois da migração: %v", err)
	}
}
