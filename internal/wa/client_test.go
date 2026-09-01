package wa

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/bmar13/wapp-summarizer/internal/config"
	msgstore "github.com/bmar13/wapp-summarizer/internal/store"
)

// toStoreMessage e collectHistory não tocam em c.wa nem em c.db: dá para
// exercitá-las com um Client montado à mão e protobufs construídos aqui, sem
// conta, sem rede e sem banco.

func testClient(tweak func(*config.Config)) *Client {
	cfg := &config.Config{
		WindowDays:    3,
		RetentionDays: 45,
		HostID:        "maquina-teste",
		Export:        config.Export{MaxMessageChars: 4000},
	}
	if tweak != nil {
		tweak(cfg)
	}
	return &Client{cfg: cfg, names: map[string]string{}, log: waLog.Noop}
}

func user(n string) types.JID  { return types.NewJID(n, types.DefaultUserServer) }
func group(n string) types.JID { return types.NewJID(n, types.GroupServer) }

// liveMsg monta o evento equivalente ao de uma mensagem recebida ao vivo.
func liveMsg(chat, sender types.JID, id string, ts time.Time, body *waE2E.Message) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:    chat,
				Sender:  sender,
				IsGroup: chat.Server == types.GroupServer,
			},
			ID:        id,
			PushName:  "Fulano",
			Timestamp: ts,
		},
		Message: body,
	}
}

func TestToStoreMessageText(t *testing.T) {
	c := testClient(nil)
	now := time.Now()
	evt := liveMsg(user("5511999990000"), user("5511999990000"), "ABC123", now, textMsg("chego às 19h"))

	m, ok := c.toStoreMessage(evt, "live")
	if !ok {
		t.Fatal("mensagem de texto recente foi descartada")
	}
	if m.ID != "ABC123" {
		t.Errorf("ID = %q", m.ID)
	}
	if m.ChatJID != "5511999990000@s.whatsapp.net" {
		t.Errorf("ChatJID = %q", m.ChatJID)
	}
	if m.SenderName != "Fulano" {
		t.Errorf("SenderName = %q, queria o PushName", m.SenderName)
	}
	if m.Kind != "text" || m.Body != "chego às 19h" {
		t.Errorf("conteúdo = %q/%q", m.Kind, m.Body)
	}
	if m.Source != "live" {
		t.Errorf("Source = %q", m.Source)
	}
	if m.IsGroup {
		t.Error("conversa individual marcada como grupo")
	}
}

// O PushName aprendido numa mensagem tem que valer para as próximas do mesmo
// remetente, mesmo quando elas chegarem sem PushName.
func TestToStoreMessageLearnsPushName(t *testing.T) {
	c := testClient(nil)
	sender := user("5511999990000")
	now := time.Now()

	if _, ok := c.toStoreMessage(liveMsg(group("123-456"), sender, "A", now, textMsg("oi")), "live"); !ok {
		t.Fatal("primeira mensagem descartada")
	}
	if got := c.getName(sender.String()); got != "Fulano" {
		t.Fatalf("nome não foi memorizado: %q", got)
	}

	mudo := liveMsg(group("123-456"), sender, "B", now, textMsg("de novo"))
	mudo.Info.PushName = ""
	m, ok := c.toStoreMessage(mudo, "live")
	if !ok {
		t.Fatal("segunda mensagem descartada")
	}
	if m.SenderName != "Fulano" {
		t.Errorf("SenderName = %q, queria o nome memorizado", m.SenderName)
	}
}

// Nome vindo da agenda/grupo é melhor que o PushName, que o próprio remetente escolhe.
func TestToStoreMessageKnownNameBeatsPushName(t *testing.T) {
	c := testClient(nil)
	sender := user("5511999990000")
	c.setName(sender.String(), "Maria (trabalho)")

	m, ok := c.toStoreMessage(liveMsg(group("123-456"), sender, "A", time.Now(), textMsg("oi")), "live")
	if !ok {
		t.Fatal("mensagem descartada")
	}
	if m.SenderName != "Maria (trabalho)" {
		t.Errorf("SenderName = %q", m.SenderName)
	}
}

func TestToStoreMessageFromMe(t *testing.T) {
	c := testClient(nil)
	evt := liveMsg(user("5511999990000"), user("5511888880000"), "A", time.Now(), textMsg("já vou"))
	evt.Info.IsFromMe = true

	m, ok := c.toStoreMessage(evt, "live")
	if !ok {
		t.Fatal("mensagem própria descartada")
	}
	if m.SenderName != "eu" {
		t.Errorf("SenderName = %q, queria eu", m.SenderName)
	}
	if !m.IsFromMe {
		t.Error("IsFromMe perdido")
	}
}

// Fora da janela de retenção nada entra: o banco local não pode crescer sem limite.
func TestToStoreMessageDropsOutsideRetention(t *testing.T) {
	c := testClient(func(cfg *config.Config) { cfg.RetentionDays = 2 })
	velha := time.Now().Add(-3 * 24 * time.Hour)

	if _, ok := c.toStoreMessage(liveMsg(user("551199"), user("551199"), "A", velha, textMsg("antiga")), "live"); ok {
		t.Error("mensagem além da retenção foi aceita")
	}
}

func TestToStoreMessageAppliesChatFilter(t *testing.T) {
	agora := time.Now()
	casos := []struct {
		nome  string
		chat  types.JID
		tweak func(*config.Config)
		quer  bool
	}{
		{"conversa comum", user("551199"), nil, true},
		{"grupo", group("123-456"), nil, true},
		{"bot", types.NewJID("meta-ai", types.BotServer), nil, false},
		{"newsletter desligada", types.NewJID("777", types.NewsletterServer), nil, false},
		{"newsletter ligada", types.NewJID("777", types.NewsletterServer),
			func(cfg *config.Config) { cfg.Export.IncludeNewsletters = true }, true},
		{"status desligado", types.StatusBroadcastJID, nil, false},
		{"status ligado", types.StatusBroadcastJID,
			func(cfg *config.Config) { cfg.Export.IncludeStatusBroadcast = true }, true},
		{"lista de transmissão", types.NewJID("998877", types.BroadcastServer), nil, true},
	}

	for _, tc := range casos {
		t.Run(tc.nome, func(t *testing.T) {
			c := testClient(tc.tweak)
			_, ok := c.toStoreMessage(liveMsg(tc.chat, user("551199"), "A", agora, textMsg("oi")), "live")
			if ok != tc.quer {
				t.Errorf("aceito = %v, queria %v", ok, tc.quer)
			}
		})
	}
}

// Ruído de protocolo e mensagens sem corpo não viram linha no resumo.
func TestToStoreMessageDropsEmptyContent(t *testing.T) {
	c := testClient(nil)
	agora := time.Now()

	casos := map[string]*waE2E.Message{
		"skip de protocolo": {MessageContextInfo: &waE2E.MessageContextInfo{}},
		"sem conteúdo":      {},
	}
	for nome, body := range casos {
		t.Run(nome, func(t *testing.T) {
			if _, ok := c.toStoreMessage(liveMsg(user("551199"), user("551199"), "A", agora, body), "live"); ok {
				t.Error("mensagem sem conteúdo foi aceita")
			}
		})
	}
}

// Uma revogação chega com ID próprio, mas precisa ser gravada sob o ID da
// mensagem alvo — senão o UPSERT por prio não sobrepõe a versão original.
func TestToStoreMessageRevokeUsesTargetID(t *testing.T) {
	c := testClient(nil)
	evt := liveMsg(user("551199"), user("551199"), "ID-DA-REVOGACAO", time.Now(), &waE2E.Message{
		ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_REVOKE.Enum(),
			Key:  &waCommon.MessageKey{ID: proto.String("ID-DO-ALVO")},
		},
	})

	m, ok := c.toStoreMessage(evt, "live")
	if !ok {
		t.Fatal("revogação descartada")
	}
	if m.ID != "ID-DO-ALVO" {
		t.Errorf("ID = %q, queria o da mensagem apagada", m.ID)
	}
	if !m.Deleted {
		t.Error("Deleted não marcado")
	}
}

func TestToStoreMessageEditUsesTargetID(t *testing.T) {
	c := testClient(nil)
	evt := liveMsg(user("551199"), user("551199"), "ID-DA-EDICAO", time.Now(), &waE2E.Message{
		ProtocolMessage: &waE2E.ProtocolMessage{
			Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
			Key:           &waCommon.MessageKey{ID: proto.String("ID-DO-ALVO")},
			EditedMessage: textMsg("agora é 20h"),
		},
	})

	m, ok := c.toStoreMessage(evt, "live")
	if !ok {
		t.Fatal("edição descartada")
	}
	if m.ID != "ID-DO-ALVO" {
		t.Errorf("ID = %q, queria o da mensagem original", m.ID)
	}
	if m.Revision != 1 {
		t.Errorf("Revision = %d, queria 1", m.Revision)
	}
	if !strings.Contains(m.Body, "agora é 20h") {
		t.Errorf("Body = %q", m.Body)
	}
}

func TestToStoreMessageTruncates(t *testing.T) {
	c := testClient(func(cfg *config.Config) { cfg.Export.MaxMessageChars = 10 })
	evt := liveMsg(user("551199"), user("551199"), "A", time.Now(), textMsg(strings.Repeat("á", 50)))

	m, ok := c.toStoreMessage(evt, "live")
	if !ok {
		t.Fatal("mensagem longa descartada")
	}
	if r := []rune(m.Body); len(r) != 11 || r[10] != '…' {
		t.Errorf("Body tem %d runas e termina em %q; queria 10 + reticências", len(r), string(r[len(r)-1]))
	}
}

// --- history sync ---

// fakeParseWeb faz, para este caminho, o mesmo que ParseWebMessage: monta o
// *events.Message a partir do protobuf. Substituí-lo é o que torna
// collectHistory testável sem um cliente conectado.
func fakeParseWeb(chatJID types.JID, webMsg *waWeb.WebMessageInfo) (*events.Message, error) {
	key := webMsg.GetKey()
	if key.GetID() == "" {
		return nil, fmt.Errorf("mensagem do histórico sem ID")
	}
	sender := chatJID
	if p := key.GetParticipant(); p != "" {
		j, err := types.ParseJID(p)
		if err != nil {
			return nil, err
		}
		sender = j
	}
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:     chatJID,
				Sender:   sender,
				IsFromMe: key.GetFromMe(),
				IsGroup:  chatJID.Server == types.GroupServer,
			},
			ID:        key.GetID(),
			PushName:  webMsg.GetPushName(),
			Timestamp: time.Unix(int64(webMsg.GetMessageTimestamp()), 0),
		},
		Message: webMsg.GetMessage(),
	}, nil
}

func histMsg(id, participant string, ts time.Time, body *waE2E.Message) *waHistorySync.HistorySyncMsg {
	if participant == "" {
		participant = "5511777770000@s.whatsapp.net"
	}
	key := &waCommon.MessageKey{
		ID:          proto.String(id),
		Participant: proto.String(participant),
	}
	return &waHistorySync.HistorySyncMsg{
		Message: &waWeb.WebMessageInfo{
			Key:              key,
			Message:          body,
			MessageTimestamp: proto.Uint64(uint64(ts.Unix())),
			PushName:         proto.String("Beltrano"),
		},
	}
}

func byID(ms []msgstore.Message) map[string]msgstore.Message {
	out := make(map[string]msgstore.Message, len(ms))
	for _, m := range ms {
		out[m.ID] = m
	}
	return out
}

func TestCollectHistory(t *testing.T) {
	c := testClient(nil)
	t0 := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	t1 := time.Now().Add(-1 * time.Hour).Truncate(time.Second)

	data := &waHistorySync.HistorySync{
		SyncType: waHistorySync.HistorySync_RECENT.Enum(),
		Conversations: []*waHistorySync.Conversation{{
			ID:   proto.String("123-456@g.us"),
			Name: proto.String("Família"),
			Messages: []*waHistorySync.HistorySyncMsg{
				histMsg("M1", "5511999990000@s.whatsapp.net", t0, textMsg("primeira")),
				histMsg("M2", "5511888880000@s.whatsapp.net", t1, textMsg("segunda")),
			},
		}},
	}

	batch, chats, _ := c.collectHistory(data, fakeParseWeb)

	if len(batch) != 2 {
		t.Fatalf("batch tem %d mensagens, queria 2", len(batch))
	}
	got := byID(batch)
	if got["M1"].Body != "primeira" || got["M2"].Body != "segunda" {
		t.Errorf("corpos errados: %+v", got)
	}
	if !got["M1"].IsGroup {
		t.Error("mensagem de grupo não marcada como grupo")
	}
	if got["M1"].Source != "history" {
		t.Errorf("Source = %q, queria history", got["M1"].Source)
	}
	if got["M1"].SenderJID != "5511999990000@s.whatsapp.net" {
		t.Errorf("SenderJID = %q", got["M1"].SenderJID)
	}

	ch, ok := chats["123-456@g.us"]
	if !ok {
		t.Fatalf("chat não coletado: %+v", chats)
	}
	if ch.Name != "Família" {
		t.Errorf("Name = %q", ch.Name)
	}
	if !ch.IsGroup {
		t.Error("IsGroup falso para @g.us")
	}
	// LastTS é o maior timestamp do lote, não o da última mensagem da lista.
	if !ch.LastTS.Equal(t1) {
		t.Errorf("LastTS = %v, queria %v", ch.LastTS, t1)
	}
}

// LastTS tem que ser o máximo, mesmo quando o histórico vem fora de ordem.
func TestCollectHistoryLastTSIsMaxNotLast(t *testing.T) {
	c := testClient(nil)
	novo := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	velho := time.Now().Add(-5 * time.Hour).Truncate(time.Second)

	data := &waHistorySync.HistorySync{
		Conversations: []*waHistorySync.Conversation{{
			ID: proto.String("123-456@g.us"),
			Messages: []*waHistorySync.HistorySyncMsg{
				histMsg("M1", "", novo, textMsg("recente")),
				histMsg("M2", "", velho, textMsg("antiga")),
			},
		}},
	}

	_, chats, _ := c.collectHistory(data, fakeParseWeb)
	if got := chats["123-456@g.us"].LastTS; !got.Equal(novo) {
		t.Errorf("LastTS = %v, queria %v", got, novo)
	}
}

func TestCollectHistorySkipsUnparseableConversationID(t *testing.T) {
	c := testClient(nil)
	data := &waHistorySync.HistorySync{
		Conversations: []*waHistorySync.Conversation{
			// Pontos demais no usuário: é o que types.ParseJID de fato recusa.
			{ID: proto.String("55.11.99@s.whatsapp.net"), Messages: []*waHistorySync.HistorySyncMsg{
				histMsg("M1", "", time.Now(), textMsg("some")),
			}},
			{ID: proto.String("123-456@g.us"), Messages: []*waHistorySync.HistorySyncMsg{
				histMsg("M2", "", time.Now(), textMsg("fica")),
			}},
		},
	}

	batch, chats, _ := c.collectHistory(data, fakeParseWeb)
	if len(batch) != 1 || batch[0].ID != "M2" {
		t.Errorf("batch = %+v, queria só M2", batch)
	}
	if len(chats) != 1 {
		t.Errorf("chats = %+v, queria só o de JID válido", chats)
	}
}

// Uma mensagem que o parser recusa não pode derrubar o resto do lote.
func TestCollectHistorySkipsParseFailure(t *testing.T) {
	c := testClient(nil)
	data := &waHistorySync.HistorySync{
		Conversations: []*waHistorySync.Conversation{{
			ID: proto.String("123-456@g.us"),
			Messages: []*waHistorySync.HistorySyncMsg{
				histMsg("", "", time.Now(), textMsg("sem id, o parser recusa")),
				histMsg("M2", "", time.Now(), textMsg("boa")),
			},
		}},
	}

	batch, _, _ := c.collectHistory(data, fakeParseWeb)
	if len(batch) != 1 || batch[0].ID != "M2" {
		t.Errorf("batch = %+v, queria só M2", batch)
	}
}

// O histórico passa pelos mesmos filtros do caminho ao vivo.
func TestCollectHistoryAppliesChatFilter(t *testing.T) {
	c := testClient(nil)
	data := &waHistorySync.HistorySync{
		Conversations: []*waHistorySync.Conversation{
			{ID: proto.String("777@newsletter"), Messages: []*waHistorySync.HistorySyncMsg{
				histMsg("N1", "", time.Now(), textMsg("canal"))}},
			{ID: proto.String("123-456@g.us"), Messages: []*waHistorySync.HistorySyncMsg{
				histMsg("G1", "", time.Now(), textMsg("grupo"))}},
		},
	}

	batch, chats, _ := c.collectHistory(data, fakeParseWeb)
	if len(batch) != 1 || batch[0].ID != "G1" {
		t.Errorf("batch = %+v, queria só o grupo", batch)
	}
	if _, achou := chats["777@newsletter"]; achou {
		t.Error("newsletter filtrada ainda virou chat")
	}
}

// Fora da retenção o histórico é descartado igual ao vivo: parear não pode
// encher o banco com um ano de conversa.
func TestCollectHistoryRespectsRetention(t *testing.T) {
	c := testClient(func(cfg *config.Config) { cfg.RetentionDays = 2 })
	data := &waHistorySync.HistorySync{
		Conversations: []*waHistorySync.Conversation{{
			ID: proto.String("123-456@g.us"),
			Messages: []*waHistorySync.HistorySyncMsg{
				histMsg("VELHA", "", time.Now().Add(-10*24*time.Hour), textMsg("ano passado")),
				histMsg("NOVA", "", time.Now(), textMsg("hoje")),
			},
		}},
	}

	batch, _, _ := c.collectHistory(data, fakeParseWeb)
	if len(batch) != 1 || batch[0].ID != "NOVA" {
		t.Errorf("batch = %+v, queria só NOVA", batch)
	}
}

func TestCollectHistoryFallsBackToDisplayName(t *testing.T) {
	c := testClient(nil)
	data := &waHistorySync.HistorySync{
		Conversations: []*waHistorySync.Conversation{{
			ID:          proto.String("5511999990000@s.whatsapp.net"),
			DisplayName: proto.String("Zé da Padaria"),
			Messages: []*waHistorySync.HistorySyncMsg{
				histMsg("M1", "", time.Now(), textMsg("pão saiu")),
			},
		}},
	}

	_, chats, _ := c.collectHistory(data, fakeParseWeb)
	if got := chats["5511999990000@s.whatsapp.net"].Name; got != "Zé da Padaria" {
		t.Errorf("Name = %q, queria o DisplayName", got)
	}
}

// Os pushnames do history sync alimentam o cache de nomes para os lotes seguintes.
func TestCollectHistoryRecordsPushnames(t *testing.T) {
	c := testClient(nil)
	data := &waHistorySync.HistorySync{
		Pushnames: []*waHistorySync.Pushname{
			{ID: proto.String("5511999990000@s.whatsapp.net"), Pushname: proto.String("Ana")},
			{ID: proto.String("5511888880000@s.whatsapp.net"), Pushname: proto.String("")},
		},
	}

	c.collectHistory(data, fakeParseWeb)
	if got := c.getName("5511999990000@s.whatsapp.net"); got != "Ana" {
		t.Errorf("nome = %q, queria Ana", got)
	}
	if got := c.getName("5511888880000@s.whatsapp.net"); got != "" {
		t.Errorf("pushname vazio virou nome %q", got)
	}
}

func TestBestContactName(t *testing.T) {
	casos := []struct {
		nome string
		info types.ContactInfo
		quer string
	}{
		{"nome completo ganha", types.ContactInfo{FullName: "Ana Silva", FirstName: "Ana", PushName: "aninha"}, "Ana Silva"},
		{"cai para o primeiro nome", types.ContactInfo{FirstName: "Ana", PushName: "aninha"}, "Ana"},
		{"cai para o comercial", types.ContactInfo{BusinessName: "Padaria do Zé", PushName: "zé"}, "Padaria do Zé"},
		{"último recurso é o pushname", types.ContactInfo{PushName: "aninha"}, "aninha"},
		{"sem nome nenhum", types.ContactInfo{}, ""},
	}
	for _, tc := range casos {
		t.Run(tc.nome, func(t *testing.T) {
			if got := bestContactName(tc.info); got != tc.quer {
				t.Errorf("bestContactName = %q, queria %q", got, tc.quer)
			}
		})
	}
}
