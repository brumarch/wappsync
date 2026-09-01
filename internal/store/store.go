// Package store guarda as mensagens capturadas num SQLite local.
//
// Regra central: a escrita nunca degrada um registro. Cada mensagem tem um
// "prio"; um UPSERT só sobrescreve se o novo prio for maior. Assim um history
// sync antigo (corpo vazio, sem nome do remetente) jamais apaga o que já foi
// capturado ao vivo, e uma edição/revogação sempre vence.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // driver puro Go: sem cgo, sem compilador C
)

type Message struct {
	ID         string
	ChatJID    string
	SenderJID  string
	SenderName string
	IsFromMe   bool
	IsGroup    bool
	Timestamp  time.Time
	Kind       string
	Body       string
	QuotedID   string
	QuotedText string
	Revision   int
	Deleted    bool
	Source     string // "live" | "history"

	// Media é o caminho relativo do anexo publicado, ex.
	// "media/bruno-win/ab12....jpg". Guarda o caminho COMPLETO, com o host,
	// e não só o nome do arquivo, porque o registro viaja para outras
	// máquinas no merge e precisa continuar apontando para o lugar certo.
	Media string
}

// Rank define a precedência de uma versão da mensagem. Maior vence.
//
// ATENÇÃO: este número viaja entre máquinas como Record.Prio e é comparado
// com o de shards produzidos por outros binários. Mudar a fórmula exige
// bumpar export.SchemaVersion — senão máquinas com fórmulas diferentes
// continuam comparando números sem erro nenhum, e o merge passa a escolher
// a versão errada da mensagem em silêncio.
func (m Message) Rank() int {
	r := m.Revision * 1000
	if m.Deleted {
		r += 500
	}
	// O anexo baixado é conteúdo que o marcador não tem, então pesa acima de
	// "tem corpo": entre duas versões da mesma mensagem, a que traz o arquivo
	// ganha. Sem este bit as duas empatam, e como o UPSERT é `>` estrito, o
	// download nunca entraria no banco — nem venceria o merge, onde o
	// desempate cairia no nome do remetente, que é arbitrário.
	if m.Media != "" {
		r += 200
	}
	if m.Body != "" {
		r += 100
	}
	if m.Source == "live" {
		r += 10
	}
	if m.SenderName != "" {
		r++
	}
	return r
}

type Chat struct {
	JID     string
	Name    string
	IsGroup bool
	LastTS  time.Time
}

type Stats struct {
	Messages   int
	Chats      int
	OldestTS   time.Time
	NewestTS   time.Time
	InWindow   int
	WindowFrom time.Time
}

type DB struct{ sql *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS messages (
	chat_jid    TEXT    NOT NULL,
	id          TEXT    NOT NULL,
	sender_jid  TEXT    NOT NULL DEFAULT '',
	sender_name TEXT    NOT NULL DEFAULT '',
	is_from_me  INTEGER NOT NULL DEFAULT 0,
	is_group    INTEGER NOT NULL DEFAULT 0,
	ts          INTEGER NOT NULL,
	kind        TEXT    NOT NULL DEFAULT 'text',
	body        TEXT    NOT NULL DEFAULT '',
	quoted_id   TEXT    NOT NULL DEFAULT '',
	quoted_text TEXT    NOT NULL DEFAULT '',
	revision    INTEGER NOT NULL DEFAULT 0,
	deleted     INTEGER NOT NULL DEFAULT 0,
	source      TEXT    NOT NULL DEFAULT 'live',
	media       TEXT    NOT NULL DEFAULT '',
	prio        INTEGER NOT NULL DEFAULT 0,
	seen_at     INTEGER NOT NULL,
	PRIMARY KEY (chat_jid, id)
);
CREATE INDEX IF NOT EXISTS idx_messages_ts ON messages (ts);

CREATE TABLE IF NOT EXISTS chats (
	jid        TEXT PRIMARY KEY,
	name       TEXT    NOT NULL DEFAULT '',
	is_group   INTEGER NOT NULL DEFAULT 0,
	last_ts    INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS meta (
	k TEXT PRIMARY KEY,
	v TEXT NOT NULL
);
`

const upsertMessage = `
INSERT INTO messages
	(chat_jid, id, sender_jid, sender_name, is_from_me, is_group, ts, kind, body,
	 quoted_id, quoted_text, revision, deleted, source, media, prio, seen_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT (chat_jid, id) DO UPDATE SET
	sender_jid  = excluded.sender_jid,
	sender_name = excluded.sender_name,
	is_from_me  = excluded.is_from_me,
	is_group    = excluded.is_group,
	ts          = excluded.ts,
	kind        = excluded.kind,
	body        = excluded.body,
	quoted_id   = excluded.quoted_id,
	quoted_text = excluded.quoted_text,
	revision    = excluded.revision,
	deleted     = excluded.deleted,
	source      = excluded.source,
	media       = excluded.media,
	prio        = excluded.prio,
	seen_at     = excluded.seen_at
WHERE excluded.prio > messages.prio
`

// Open abre (ou cria) o banco de mensagens.
func Open(path string) (*DB, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("abrindo %s: %w", path, err)
	}
	// modernc.org/sqlite não é seguro para escrita concorrente sem serialização.
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(schema); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("criando schema: %w", err)
	}
	if err := migrate(sqlDB); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrando %s: %w", path, err)
	}
	return &DB{sql: sqlDB}, nil
}

// migrate adiciona colunas que versões anteriores não tinham.
//
// `CREATE TABLE IF NOT EXISTS` não mexe numa tabela que já existe, então um
// banco criado por um binário antigo continuaria sem as colunas novas e todo
// INSERT passaria a falhar depois da atualização. A checagem é por
// PRAGMA table_info e não por texto de erro: mensagem de driver muda.
func migrate(db *sql.DB) error {
	columns := map[string]string{
		"media": `ALTER TABLE messages ADD COLUMN media TEXT NOT NULL DEFAULT ''`,
	}

	rows, err := db.Query(`PRAGMA table_info(messages)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid         int
			name, ctype string
			notNull, pk int
			dflt        sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return err
		}
		delete(columns, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	for _, stmt := range columns {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) Close() error { return d.sql.Close() }

// PutMessages grava um lote inteiro em uma transação.
func (d *DB) PutMessages(ctx context.Context, msgs []Message) (int, error) {
	if len(msgs) == 0 {
		return 0, nil
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, upsertMessage)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	now := time.Now().Unix()
	written := 0
	for _, m := range msgs {
		if m.ID == "" || m.ChatJID == "" || m.Timestamp.IsZero() {
			continue
		}
		if m.Source == "" {
			m.Source = "live"
		}
		res, err := stmt.ExecContext(ctx,
			m.ChatJID, m.ID, m.SenderJID, m.SenderName, m.IsFromMe, m.IsGroup,
			m.Timestamp.Unix(), m.Kind, m.Body, m.QuotedID, m.QuotedText,
			m.Revision, m.Deleted, m.Source, m.Media, m.Rank(), now)
		if err != nil {
			return written, fmt.Errorf("gravando %s/%s: %w", m.ChatJID, m.ID, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			written++
		}
	}
	if err := tx.Commit(); err != nil {
		return written, err
	}
	return written, nil
}

func (d *DB) PutMessage(ctx context.Context, m Message) (bool, error) {
	n, err := d.PutMessages(ctx, []Message{m})
	return n > 0, err
}

// UpsertChat registra/atualiza o nome de um chat. Nome vazio nunca apaga um
// nome já conhecido.
func (d *DB) UpsertChat(ctx context.Context, c Chat) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO chats (jid, name, is_group, last_ts, updated_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT (jid) DO UPDATE SET
			name     = CASE WHEN excluded.name <> '' THEN excluded.name ELSE chats.name END,
			is_group = excluded.is_group,
			last_ts  = MAX(chats.last_ts, excluded.last_ts),
			updated_at = excluded.updated_at`,
		c.JID, c.Name, c.IsGroup, c.LastTS.Unix(), time.Now().Unix())
	return err
}

// Chats devolve o mapa jid -> chat conhecido.
func (d *DB) Chats(ctx context.Context) (map[string]Chat, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT jid, name, is_group, last_ts FROM chats`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]Chat{}
	for rows.Next() {
		var c Chat
		var ts int64
		if err := rows.Scan(&c.JID, &c.Name, &c.IsGroup, &ts); err != nil {
			return nil, err
		}
		c.LastTS = time.Unix(ts, 0)
		out[c.JID] = c
	}
	return out, rows.Err()
}

// messageColumns é a lista de colunas lida por Since e por message. Uma só,
// para que as duas não divirjam quando uma coluna nova aparecer.
const messageColumns = `chat_jid, id, sender_jid, sender_name, is_from_me, is_group, ts,
	kind, body, quoted_id, quoted_text, revision, deleted, source, media`

// scanMessage lê uma linha na ordem de messageColumns.
func scanMessage(sc interface{ Scan(...any) error }) (Message, error) {
	var m Message
	var ts int64
	err := sc.Scan(&m.ChatJID, &m.ID, &m.SenderJID, &m.SenderName,
		&m.IsFromMe, &m.IsGroup, &ts, &m.Kind, &m.Body,
		&m.QuotedID, &m.QuotedText, &m.Revision, &m.Deleted, &m.Source, &m.Media)
	m.Timestamp = time.Unix(ts, 0)
	return m, err
}

// Since devolve todas as mensagens a partir de cutoff, ordenadas cronologicamente.
func (d *DB) Since(ctx context.Context, cutoff time.Time) ([]Message, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE ts >= ? ORDER BY ts ASC, id ASC`,
		cutoff.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Message lê uma mensagem pela chave. ok = false quando ela não existe.
func (d *DB) Message(ctx context.Context, chatJID, id string) (Message, bool, error) {
	row := d.sql.QueryRowContext(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE chat_jid = ? AND id = ?`, chatJID, id)
	m, err := scanMessage(row)
	if err == sql.ErrNoRows {
		return Message{}, false, nil
	}
	return m, err == nil, err
}

// SetMedia associa a uma mensagem já gravada o anexo que acabou de ser baixado.
//
// Passa pelo MESMO upsert das outras escritas, de propósito. O anexo só entra
// se elevar o prio — que é exatamente o que o bit de mídia em Rank() garante.
// Um UPDATE direto contornaria a regra de precedência, e a regra é o
// invariante: é ela que impede uma versão pior de sobrescrever uma melhor.
func (d *DB) SetMedia(ctx context.Context, chatJID, id, media string) (bool, error) {
	m, ok, err := d.Message(ctx, chatJID, id)
	if err != nil || !ok {
		return false, err
	}
	if m.Media == media {
		return false, nil
	}
	m.Media = media
	return d.PutMessage(ctx, m)
}

// SetTranscription grava o corpo enriquecido pela transcrição e o arquivo do
// texto completo, de uma vez.
//
// Os dois juntos, e não em duas escritas: o corpo sozinho não muda o prio — o
// bit de Rank olha para Media — e a escrita seria recusada pelo UPSERT, que é
// `>` estrito. A transcrição só entra no banco porque vem acompanhada do
// ponteiro para o arquivo.
func (d *DB) SetTranscription(ctx context.Context, chatJID, id, body, media string) (bool, error) {
	m, ok, err := d.Message(ctx, chatJID, id)
	if err != nil || !ok {
		return false, err
	}
	if m.Media == media && m.Body == body {
		return false, nil
	}
	m.Body, m.Media = body, media
	return d.PutMessage(ctx, m)
}

// MediaPaths devolve os caminhos de anexo referenciados por mensagens dentro da
// janela. É o que decide o que precisa estar publicado — e, por complemento, o
// que já pode ser podado.
func (d *DB) MediaPaths(ctx context.Context, cutoff time.Time) (map[string]bool, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT DISTINCT media FROM messages WHERE ts >= ? AND media <> ''`, cutoff.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = true
	}
	return out, rows.Err()
}

// Prune apaga mensagens mais velhas que cutoff (retenção local).
func (d *DB) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := d.sql.ExecContext(ctx, `DELETE FROM messages WHERE ts < ?`, cutoff.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (d *DB) Stats(ctx context.Context, windowFrom time.Time) (Stats, error) {
	var s Stats
	s.WindowFrom = windowFrom
	var oldest, newest sql.NullInt64
	err := d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(ts), MAX(ts) FROM messages`).Scan(&s.Messages, &oldest, &newest)
	if err != nil {
		return s, err
	}
	if oldest.Valid {
		s.OldestTS = time.Unix(oldest.Int64, 0)
	}
	if newest.Valid {
		s.NewestTS = time.Unix(newest.Int64, 0)
	}
	if err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM chats`).Scan(&s.Chats); err != nil {
		return s, err
	}
	err = d.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE ts >= ?`, windowFrom.Unix()).Scan(&s.InWindow)
	return s, err
}

func (d *DB) SetMeta(ctx context.Context, k, v string) error {
	_, err := d.sql.ExecContext(ctx,
		`INSERT INTO meta (k, v) VALUES (?,?) ON CONFLICT (k) DO UPDATE SET v = excluded.v`, k, v)
	return err
}

func (d *DB) GetMeta(ctx context.Context, k string) (string, error) {
	var v string
	err := d.sql.QueryRowContext(ctx, `SELECT v FROM meta WHERE k = ?`, k).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}
