// Package export transforma as mensagens do banco local nos artefatos que os
// agentes leem: JSONL canônico, digest em Markdown e um índice de frescor.
package export

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bmar13/wapp-summarizer/internal/config"
	"github.com/bmar13/wapp-summarizer/internal/store"
)

// SchemaVersion muda quando o formato do Record muda de forma incompatível.
const SchemaVersion = "wapp-summarizer/1"

// Record é uma mensagem no formato publicado. Uma linha de JSONL.
type Record struct {
	ID         string    `json:"id"`
	Chat       string    `json:"chat"`
	ChatName   string    `json:"chat_name,omitempty"`
	IsGroup    bool      `json:"group"`
	FromMe     bool      `json:"from_me"`
	Sender     string    `json:"sender,omitempty"`
	SenderName string    `json:"sender_name,omitempty"`
	Timestamp  time.Time `json:"ts"`
	Kind       string    `json:"kind"`
	Text       string    `json:"text"`
	ReplyToID  string    `json:"reply_to,omitempty"`
	ReplyText  string    `json:"reply_text,omitempty"`
	Edited     bool      `json:"edited,omitempty"`
	Deleted    bool      `json:"deleted,omitempty"`

	// Prio é a precedência desta versão do registro. Usada no merge entre
	// máquinas para decidir quem vence; não é conteúdo.
	Prio int `json:"_prio"`
}

// Key identifica unicamente uma mensagem entre todas as máquinas.
func (r Record) Key() string { return r.Chat + "\x00" + r.ID }

type ChatSummary struct {
	JID     string    `json:"jid"`
	Name    string    `json:"name,omitempty"`
	IsGroup bool      `json:"group"`
	Count   int       `json:"count"`
	LastTS  time.Time `json:"last_ts"`
}

// ShardMeta descreve a contribuição de uma máquina.
type ShardMeta struct {
	Host        string    `json:"host"`
	DeviceJID   string    `json:"device_jid,omitempty"`
	GeneratedAt time.Time `json:"generated_at"`
	Messages    int       `json:"messages"`
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
}

// Index é o manifesto que o agente lê primeiro para saber se o dado está fresco.
type Index struct {
	Schema      string    `json:"schema"`
	GeneratedAt time.Time `json:"generated_at"`
	GeneratedBy string    `json:"generated_by"`
	WindowDays  int       `json:"window_days"`
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	Messages    int       `json:"messages"`
	// LastMessageTS é o timestamp da mensagem mais recente publicada. É a
	// referência de monotonicidade do merge: um índice novo nunca pode
	// regredir este campo.
	LastMessageTS time.Time     `json:"last_message_ts"`
	Chats         []ChatSummary `json:"chats"`
	Shards        []ShardMeta   `json:"shards,omitempty"`
}

// Build converte mensagens cruas em registros publicáveis, aplicando janela,
// filtros de chat e regras de privacidade.
func Build(cfg *config.Config, msgs []store.Message, chats map[string]store.Chat, from time.Time) []Record {
	out := make([]Record, 0, len(msgs))
	for _, m := range msgs {
		if m.Timestamp.Before(from) {
			continue
		}
		if m.IsFromMe && !cfg.Export.IncludeFromMe {
			continue
		}
		chatName := chats[m.ChatJID].Name
		if !cfg.ChatAllowed(m.ChatJID, chatName) {
			continue
		}
		if !cfg.Export.MediaPlaceholders && isMediaOnly(m) {
			continue
		}

		out = append(out, Record{
			ID:         m.ID,
			Chat:       m.ChatJID,
			ChatName:   chatName,
			IsGroup:    m.IsGroup,
			FromMe:     m.IsFromMe,
			Sender:     m.SenderJID,
			SenderName: m.SenderName,
			Timestamp:  m.Timestamp.UTC(),
			Kind:       m.Kind,
			Text:       cfg.Redact(m.Body),
			ReplyToID:  m.QuotedID,
			ReplyText:  cfg.Redact(m.QuotedText),
			Edited:     m.Revision > 0,
			Deleted:    m.Deleted,
			Prio:       m.Rank(),
		})
	}
	Sort(out)
	return out
}

// Sort ordena cronologicamente, com desempate estável por chat+id.
func Sort(recs []Record) {
	sort.SliceStable(recs, func(i, j int) bool {
		if !recs[i].Timestamp.Equal(recs[j].Timestamp) {
			return recs[i].Timestamp.Before(recs[j].Timestamp)
		}
		return recs[i].Key() < recs[j].Key()
	})
}

func isMediaOnly(m store.Message) bool {
	switch m.Kind {
	case "image", "video", "audio", "document", "sticker", "album":
		return strings.HasPrefix(m.Body, "[") && strings.HasSuffix(strings.TrimSpace(m.Body), "]")
	}
	return false
}

// BuildIndex resume um conjunto de registros.
func BuildIndex(cfg *config.Config, recs []Record, from, to time.Time, by string) Index {
	byChat := map[string]*ChatSummary{}
	for _, r := range recs {
		cs, ok := byChat[r.Chat]
		if !ok {
			cs = &ChatSummary{JID: r.Chat, Name: r.ChatName, IsGroup: r.IsGroup}
			byChat[r.Chat] = cs
		}
		if cs.Name == "" {
			cs.Name = r.ChatName
		}
		cs.Count++
		if r.Timestamp.After(cs.LastTS) {
			cs.LastTS = r.Timestamp
		}
	}
	chats := make([]ChatSummary, 0, len(byChat))
	for _, cs := range byChat {
		chats = append(chats, *cs)
	}
	sort.Slice(chats, func(i, j int) bool { return chats[i].LastTS.After(chats[j].LastTS) })

	var lastTS time.Time
	for _, r := range recs {
		if r.Timestamp.After(lastTS) {
			lastTS = r.Timestamp
		}
	}

	return Index{
		Schema:        SchemaVersion,
		GeneratedAt:   to.UTC(),
		GeneratedBy:   by,
		WindowDays:    cfg.WindowDays,
		From:          from.UTC(),
		To:            to.UTC(),
		Messages:      len(recs),
		LastMessageTS: lastTS.UTC(),
		Chats:         chats,
	}
}

// MarshalJSONL serializa os registros, um JSON por linha.
func MarshalJSONL(recs []Record) ([]byte, error) {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	enc := json.NewEncoder(w)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			return nil, err
		}
	}
	if err := w.Flush(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// UnmarshalJSONL lê um arquivo JSONL, ignorando linhas corrompidas.
func UnmarshalJSONL(data []byte) ([]Record, int) {
	var (
		recs    []Record
		skipped int
	)
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil || r.ID == "" || r.Chat == "" {
			skipped++
			continue
		}
		recs = append(recs, r)
	}
	if sc.Err() != nil {
		skipped++
	}
	return recs, skipped
}

func MarshalIndex(idx Index) ([]byte, error) {
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// MarshalMarkdown gera o digest legível — é o arquivo que o agente vai ler para
// produzir resumos. Agrupado por chat, cronológico dentro de cada chat.
func MarshalMarkdown(idx Index, recs []Record, loc *time.Location) []byte {
	if loc == nil {
		loc = time.Local
	}
	var b strings.Builder

	fmt.Fprintf(&b, "# WhatsApp — últimos %d dia(s)\n\n", idx.WindowDays)
	// O fuso vai explícito: o agente precisa dele para interpretar os horários
	// das mensagens, que são renderizados sem offset.
	fmt.Fprintf(&b, "- Janela: **%s** → **%s** (horários em UTC%s)\n",
		idx.From.In(loc).Format("2006-01-02 15:04"),
		idx.To.In(loc).Format("2006-01-02 15:04"),
		idx.To.In(loc).Format("-07:00"))
	fmt.Fprintf(&b, "- Mensagens: **%d** em **%d** conversa(s)\n", idx.Messages, len(idx.Chats))
	fmt.Fprintf(&b, "- Gerado por: `%s` em %s\n", idx.GeneratedBy, idx.GeneratedAt.In(loc).Format(time.RFC3339))
	if len(idx.Shards) > 0 {
		b.WriteString("- Máquinas contribuindo: ")
		parts := make([]string, 0, len(idx.Shards))
		for _, s := range idx.Shards {
			parts = append(parts, fmt.Sprintf("`%s` (%d msgs, %s)",
				s.Host, s.Messages, s.GeneratedAt.In(loc).Format("02/01 15:04")))
		}
		b.WriteString(strings.Join(parts, ", ") + "\n")
	}
	b.WriteString("\n---\n\n## Índice\n\n")
	for _, c := range idx.Chats {
		kind := "DM"
		if c.IsGroup {
			kind = "grupo"
		}
		fmt.Fprintf(&b, "- %s (%s) — %d msgs, última %s\n",
			displayName(c.Name, c.JID), kind, c.Count, c.LastTS.In(loc).Format("02/01 15:04"))
	}
	b.WriteString("\n---\n")

	byChat := map[string][]Record{}
	for _, r := range recs {
		byChat[r.Chat] = append(byChat[r.Chat], r)
	}

	for _, c := range idx.Chats {
		rs := byChat[c.JID]
		if len(rs) == 0 {
			continue
		}
		kind := "DM"
		if c.IsGroup {
			kind = "grupo"
		}
		fmt.Fprintf(&b, "\n## %s (%s) — %d mensagens\n", displayName(c.Name, c.JID), kind, len(rs))
		fmt.Fprintf(&b, "<!-- jid: %s -->\n", c.JID)

		lastDay := ""
		for _, r := range rs {
			t := r.Timestamp.In(loc)
			if day := t.Format("2006-01-02"); day != lastDay {
				lastDay = day
				fmt.Fprintf(&b, "\n### %s\n\n", day)
			}
			sender := r.SenderName
			if sender == "" {
				sender = shortJID(r.Sender)
			}
			line := oneLine(r.Text)
			if r.ReplyText != "" {
				line = "↩︎ \"" + oneLine(r.ReplyText) + "\" · " + line
			}
			if r.Deleted {
				line = "~~" + line + "~~"
			}
			fmt.Fprintf(&b, "- `%s` **%s**: %s\n", t.Format("15:04"), sender, line)
		}
	}
	return []byte(b.String())
}

func displayName(name, jid string) string {
	if name != "" {
		return name
	}
	return shortJID(jid)
}

func shortJID(jid string) string {
	if i := strings.IndexByte(jid, '@'); i > 0 {
		return jid[:i]
	}
	if jid == "" {
		return "desconhecido"
	}
	return jid
}

// oneLine achata quebras de linha para o Markdown não virar sopa de bullets.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	return strings.TrimSpace(s)
}
