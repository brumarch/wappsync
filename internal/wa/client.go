// Package wa liga o whatsmeow ao banco local de mensagens.
//
// Cada máquina roda a sua PRÓPRIA sessão (um dispositivo vinculado próprio).
// Nunca copie session.db entre máquinas: as duas seriam desconectadas.
package wa

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/bmar13/wapp-summarizer/internal/config"
	msgstore "github.com/bmar13/wapp-summarizer/internal/store"
	"github.com/bmar13/wapp-summarizer/internal/transcribe"
)

type Client struct {
	cfg       *config.Config
	db        *msgstore.DB
	container *sqlstore.Container
	wa        *whatsmeow.Client
	log       waLog.Logger

	mu    sync.RWMutex
	names map[string]string // jid -> melhor nome conhecido

	// lost recebe o primeiro motivo pelo qual a captura parou. Bufferizado em
	// 1: quem sinaliza são as goroutines do whatsmeow, que não podem bloquear.
	lost chan SessionLoss

	// Fila de anexos. nil quando [media].enabled está desligado — e é o nil
	// que faz enqueueMedia virar no-op, sem espalhar o if pelo código.
	mediaQ    chan mediaJob
	mediaDone chan struct{}

	// Transcrição tem fila própria: o Whisper leva minutos e o download leva
	// segundos. Ver startTranscribeWorker.
	transcriber transcriber
	transQ      chan transcribeJob
	transDone   chan struct{}
}

// New abre a sessão local e monta o cliente do WhatsApp (sem conectar ainda).
func New(ctx context.Context, cfg *config.Config, db *msgstore.DB, verbose bool) (*Client, error) {
	if err := os.MkdirAll(cfg.Paths.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("criando %s: %w", cfg.Paths.DataDir, err)
	}

	level := "WARN"
	if verbose {
		level = "INFO"
	}
	dbLog := waLog.Stdout("db", level, true)
	clientLog := waLog.Stdout("wa", level, true)

	// Identifica-se como um navegador desktop comum. Ajuda a passar
	// despercebido e deixa o dispositivo reconhecível na lista do celular.
	store.DeviceProps.Os = strPtr("wapp-summarizer (" + cfg.HostID + ")")

	dsn := "file:" + cfg.SessionDBPath() + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)"
	container, err := sqlstore.New(ctx, "sqlite", dsn, dbLog)
	if err != nil {
		return nil, fmt.Errorf("abrindo sessão em %s: %w", cfg.SessionDBPath(), err)
	}

	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		container.Close()
		return nil, fmt.Errorf("lendo dispositivo: %w", err)
	}

	tr, err := newTranscriber(transcribe.Options{
		Binary:   cfg.Transcribe.Binary,
		Model:    cfg.Transcribe.Model,
		FFmpeg:   cfg.Transcribe.FFmpeg,
		Language: cfg.Transcribe.Language,
		Threads:  cfg.Transcribe.Threads,
		Timeout:  time.Duration(cfg.Transcribe.TimeoutMinutes) * time.Minute,
	}, cfg.Transcribe.Enabled)
	if err != nil {
		container.Close()
		// Falhar aqui, e não no primeiro áudio: transcrição configurada e
		// quebrada tem que impedir o programa de subir, senão vira um aviso
		// de log por nota de voz que ninguém lê.
		return nil, fmt.Errorf("transcrição: %w", err)
	}

	c := &Client{
		cfg:         cfg,
		db:          db,
		container:   container,
		wa:          whatsmeow.NewClient(device, clientLog),
		log:         clientLog,
		names:       map[string]string{},
		lost:        make(chan SessionLoss, 1),
		transcriber: tr,
	}
	enforceReadOnly(c.wa)
	c.startMediaWorker()
	c.startTranscribeWorker()
	c.wa.AddEventHandler(c.handleEvent)
	return c, nil
}

// enforceReadOnly fixa a postura somente-leitura do cliente.
//
// Nenhum caminho deste programa chama SendMessage, MarkRead, SendPresence ou
// qualquer mutação de grupo — isso é verificado por TestClientIsReadOnly. O que
// esta função cobre é a única forma de o whatsmeow enviar uma mensagem sem que
// a peçamos: um retry receipt de outro aparelho pedindo o reenvio de algo.
//
// Na prática esse caminho já morre sozinho (nunca enviamos nada, logo não há
// mensagem em cache para reenviar), mas depender de um default é frágil.
// Aqui a recusa é explícita e sobrevive a mudanças de default no whatsmeow.
func enforceReadOnly(cli *whatsmeow.Client) {
	// Não guardar mensagens enviadas para atender pedidos de reenvio.
	cli.UseRetryMessageStore = false

	// Nunca localizar uma mensagem para reenviar.
	cli.GetMessageForRetry = func(requester, to types.JID, id types.MessageID) *waE2E.Message {
		return nil
	}

	// Recusar todo pedido de reenvio antes que ele seja processado.
	cli.PreRetryCallback = func(_ *events.Receipt, _ types.MessageID, _ int, _ *waE2E.Message) bool {
		return false
	}

	// Não pedir ao celular o reenvio de mensagens que falharam ao decriptar.
	cli.AutomaticMessageRerequestFromPhone = false
}

func (c *Client) Close() {
	// Antes do Disconnect: o worker pode estar baixando, e ele escreve no
	// banco que o chamador fecha logo em seguida.
	c.stopMediaWorker()
	// Depois da fila de download: ela ainda pode estar enfileirando áudio.
	c.stopTranscribeWorker()
	if c.wa != nil {
		c.wa.Disconnect()
	}
	if c.container != nil {
		c.container.Close()
	}
}

func (c *Client) IsPaired() bool { return c.wa.Store.ID != nil }

func (c *Client) JID() string {
	if c.wa.Store.ID == nil {
		return ""
	}
	return c.wa.Store.ID.String()
}

// Login pareia esta máquina como um novo dispositivo vinculado.
// Se phone estiver vazio usa QR code; caso contrário usa código de 8 dígitos
// (útil quando o terminal não renderiza QR bem, ex. via SSH).
func (c *Client) Login(ctx context.Context, phone string) error {
	if c.IsPaired() {
		return fmt.Errorf("esta máquina já está pareada como %s (use `wappsync logout` para trocar)", c.JID())
	}

	qrChan, err := c.wa.GetQRChannel(ctx)
	if err != nil {
		return fmt.Errorf("abrindo canal de QR: %w", err)
	}
	if err := c.wa.Connect(); err != nil {
		return fmt.Errorf("conectando: %w", err)
	}

	if phone != "" {
		code, err := c.wa.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, "Chrome (Windows)")
		if err != nil {
			return fmt.Errorf("gerando código de pareamento: %w", err)
		}
		fmt.Printf("\nNo celular: WhatsApp > Aparelhos conectados > Conectar com número de telefone\n")
		fmt.Printf("Código: %s\n\n", code)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case evt, ok := <-qrChan:
			if !ok {
				if c.IsPaired() {
					fmt.Printf("Pareado como %s\n", c.JID())
					return nil
				}
				return fmt.Errorf("canal de pareamento fechou sem sucesso")
			}
			switch evt.Event {
			case "code":
				if phone == "" {
					fmt.Println("\nEscaneie no celular: WhatsApp > Aparelhos conectados > Conectar aparelho")
					qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
					fmt.Printf("(o código expira em %s)\n", evt.Timeout.Round(time.Second))
				}
			case "success":
				fmt.Printf("\nPareado como %s\n", c.JID())
				return nil
			case "timeout":
				return fmt.Errorf("tempo esgotado sem parear")
			default:
				return fmt.Errorf("pareamento falhou: %s", evt.Event)
			}
		}
	}
}

// Connect abre a conexão de um dispositivo já pareado.
func (c *Client) Connect(ctx context.Context) error {
	if !c.IsPaired() {
		return fmt.Errorf("esta máquina não está pareada; rode `wappsync login` primeiro")
	}
	if err := c.wa.Connect(); err != nil {
		return fmt.Errorf("conectando: %w", err)
	}
	if !c.wa.WaitForConnection(30 * time.Second) {
		return fmt.Errorf("não conectou em 30s")
	}
	return nil
}

func (c *Client) Logout(ctx context.Context) error { return c.wa.Logout(ctx) }

// RefreshNames recarrega nomes de grupos e contatos para enriquecer o export.
func (c *Client) RefreshNames(ctx context.Context) error {
	var firstErr error

	if contacts, err := c.wa.Store.Contacts.GetAllContacts(ctx); err == nil {
		for jid, info := range contacts {
			if n := bestContactName(info); n != "" {
				c.setName(jid.ToNonAD().String(), n)
				_ = c.db.UpsertChat(ctx, msgstore.Chat{JID: jid.ToNonAD().String(), Name: n})
			}
		}
	} else {
		firstErr = fmt.Errorf("lendo contatos: %w", err)
	}

	groups, err := c.wa.GetJoinedGroups(ctx)
	if err != nil {
		if firstErr == nil {
			firstErr = fmt.Errorf("lendo grupos: %w", err)
		}
		return firstErr
	}
	for _, g := range groups {
		if g.Name == "" {
			continue
		}
		c.setName(g.JID.String(), g.Name)
		if err := c.db.UpsertChat(ctx, msgstore.Chat{JID: g.JID.String(), Name: g.Name, IsGroup: true}); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (c *Client) handleEvent(rawEvt any) {
	ctx := context.Background()

	// Perda de pareamento vem antes de tudo: a partir daqui nada mais é
	// capturado, e o `run` precisa saber disso para alertar e sair.
	if loss, ok := sessionLossFor(rawEvt); ok {
		c.log.Errorf("captura interrompida: %s — %s", loss.Reason, loss.Fix)
		c.signalLoss(loss)
		return
	}

	switch evt := rawEvt.(type) {
	case *events.Message:
		c.ingest(ctx, evt, "live")
	case *events.HistorySync:
		c.ingestHistory(ctx, evt)
	case *events.Connected:
		c.log.Infof("conectado")
		go func() {
			// Dá um tempo para o app state assentar antes de pedir nomes.
			time.Sleep(5 * time.Second)
			if err := c.RefreshNames(context.Background()); err != nil {
				c.log.Warnf("refresh de nomes: %v", err)
			}
		}()
	case *events.OfflineSyncCompleted:
		c.log.Infof("sincronização offline concluída (%d eventos)", evt.Count)
	case *events.GroupInfo:
		if evt.Name != nil && evt.Name.Name != "" {
			c.setName(evt.JID.String(), evt.Name.Name)
			_ = c.db.UpsertChat(ctx, msgstore.Chat{JID: evt.JID.String(), Name: evt.Name.Name, IsGroup: true})
		}
	}
}

// ingest converte um evento de mensagem e grava. Fora da janela de retenção é descartado.
func (c *Client) ingest(ctx context.Context, evt *events.Message, source string) {
	m, ok := c.toStoreMessage(evt, source)
	if !ok {
		return
	}
	if _, err := c.db.PutMessage(ctx, m); err != nil {
		c.log.Warnf("gravando mensagem %s: %v", m.ID, err)
		return
	}
	// Depois de gravar, nunca antes: o download termina num SetMedia, que não
	// acha linha nenhuma se a mensagem ainda não existe — e o arquivo baixado
	// viraria órfão na pasta.
	if job, ok := c.mediaJobFor(m, evt.Message, c.chatName(ctx, m.ChatJID)); ok {
		c.queueMediaJob(job)
	}
	_ = c.db.UpsertChat(ctx, msgstore.Chat{
		JID:     m.ChatJID,
		Name:    c.getName(m.ChatJID),
		IsGroup: m.IsGroup,
		LastTS:  m.Timestamp,
	})
}

func (c *Client) toStoreMessage(evt *events.Message, source string) (msgstore.Message, bool) {
	var zero msgstore.Message

	chat := evt.Info.Chat
	if !c.wantChat(chat) {
		return zero, false
	}
	cutoff := time.Now().Add(-c.cfg.Retention())
	if evt.Info.Timestamp.Before(cutoff) {
		return zero, false
	}

	content := Describe(evt.Message)
	if content.Kind == "skip" || (content.Body == "" && !content.Deleted) {
		return zero, false
	}

	sender := evt.Info.Sender.ToNonAD()
	senderName := evt.Info.PushName
	if n := c.getName(sender.String()); n != "" {
		senderName = n
	} else if senderName != "" {
		c.setName(sender.String(), senderName)
	}
	if evt.Info.IsFromMe {
		senderName = "eu"
	}

	// Uma edição/revogação se refere a outra mensagem: grava sob o ID do alvo
	// para que o UPSERT por prio sobreponha a versão original.
	id := evt.Info.ID
	if content.TargetID != "" && (content.Deleted || content.Revision > 0) {
		id = content.TargetID
	}

	return msgstore.Message{
		ID:         id,
		ChatJID:    chat.String(),
		SenderJID:  sender.String(),
		SenderName: senderName,
		IsFromMe:   evt.Info.IsFromMe,
		IsGroup:    evt.Info.IsGroup,
		Timestamp:  evt.Info.Timestamp,
		Kind:       content.Kind,
		Body:       truncate(content.Body, c.cfg.Export.MaxMessageChars),
		QuotedID:   content.QuotedID,
		QuotedText: content.QuotedText,
		Revision:   content.Revision,
		Deleted:    content.Deleted,
		Source:     source,
	}, true
}

// parseWebFunc converte o protobuf de uma mensagem do history sync no mesmo
// *events.Message que uma mensagem ao vivo produz.
//
// É parâmetro de collectHistory, e não uma chamada direta a c.wa, porque a
// única dependência do percurso do history sync no cliente conectado era essa.
// Com ela para fora, todo o resto vira função testável com protobufs montados à
// mão — ver "O que não é testável" no CLAUDE.md.
type parseWebFunc func(chatJID types.JID, webMsg *waWeb.WebMessageInfo) (*events.Message, error)

// ingestHistory processa o histórico que o WhatsApp empurra ao parear e nas
// sincronizações periódicas.
func (c *Client) ingestHistory(ctx context.Context, evt *events.HistorySync) {
	data := evt.Data
	if data == nil {
		return
	}
	batch, chats, pending := c.collectHistory(data, c.wa.ParseWebMessage)

	for _, ch := range chats {
		_ = c.db.UpsertChat(ctx, ch)
	}
	syncType := data.GetSyncType().String()
	written, err := c.db.PutMessages(ctx, batch)
	if err != nil {
		c.log.Warnf("history sync (%s): %v", syncType, err)
		return
	}
	// Mesma ordem do caminho ao vivo: as linhas primeiro, os anexos depois.
	for _, job := range pending {
		c.queueMediaJob(job)
	}
	if len(batch) > 0 {
		c.log.Infof("history sync %s: %d mensagens recebidas, %d novas/atualizadas", syncType, len(batch), written)
	}
}

// collectHistory percorre o history sync e devolve o que gravar, sem tocar no
// banco nem na rede. Os filtros são os mesmos do caminho ao vivo, porque quem
// decide o que entra continua sendo toStoreMessage.
func (c *Client) collectHistory(data *waHistorySync.HistorySync, parse parseWebFunc) ([]msgstore.Message, map[string]msgstore.Chat, []mediaJob) {
	var (
		batch   []msgstore.Message
		pending []mediaJob
	)
	chats := map[string]msgstore.Chat{}

	for _, conv := range data.GetConversations() {
		chatJID, err := types.ParseJID(conv.GetID())
		if err != nil {
			continue
		}
		if !c.wantChat(chatJID) {
			continue
		}
		name := conv.GetName()
		if name == "" {
			name = conv.GetDisplayName()
		}
		if name != "" {
			c.setName(chatJID.String(), name)
		}

		var lastTS time.Time
		for _, hm := range conv.GetMessages() {
			parsed, err := parse(chatJID, hm.GetMessage())
			if err != nil {
				continue
			}
			m, ok := c.toStoreMessage(parsed, "history")
			if !ok {
				continue
			}
			batch = append(batch, m)
			// O nome desta conversa foi gravado logo acima, antes do laço.
			if job, ok := c.mediaJobFor(m, parsed.Message, c.getName(chatJID.String())); ok {
				pending = append(pending, job)
			}
			if m.Timestamp.After(lastTS) {
				lastTS = m.Timestamp
			}
		}
		chats[chatJID.String()] = msgstore.Chat{
			JID:     chatJID.String(),
			Name:    c.getName(chatJID.String()),
			IsGroup: chatJID.Server == types.GroupServer,
			LastTS:  lastTS,
		}
	}

	// Nomes de exibição que só chegam no history sync. Depois do laço de
	// propósito: aqui já não afetam os nomes das mensagens deste lote, só os
	// dos lotes seguintes.
	for _, pn := range data.GetPushnames() {
		if pn.GetPushname() != "" && pn.GetID() != "" {
			c.setName(pn.GetID(), pn.GetPushname())
		}
	}
	return batch, chats, pending
}

// wantChat aplica os filtros estruturais (status, newsletters, bots).
func (c *Client) wantChat(jid types.JID) bool {
	switch jid.Server {
	case types.BroadcastServer:
		if jid.User == types.StatusBroadcastJID.User {
			return c.cfg.Export.IncludeStatusBroadcast
		}
		return true
	case types.NewsletterServer:
		return c.cfg.Export.IncludeNewsletters
	case types.BotServer:
		return false
	}
	return true
}

func (c *Client) setName(jid, name string) {
	if jid == "" || name == "" {
		return
	}
	c.mu.Lock()
	c.names[jid] = name
	c.mu.Unlock()
}

func (c *Client) getName(jid string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.names[jid]
}

// chatName resolve o melhor nome conhecido de um chat, caindo no banco quando a
// memória ainda não sabe.
//
// O mapa em memória nasce vazio a cada execução e só é preenchido pelo
// RefreshNames, que roda alguns segundos DEPOIS do Connected. Uma mensagem que
// chegue nessa janela seria avaliada com nome vazio — e uma política escrita
// como `match = "Família"` não casaria, sem que nada aparecesse no log. O banco
// sobrevive ao reinício, então é ele que cobre o intervalo.
func (c *Client) chatName(ctx context.Context, jid string) string {
	if n := c.getName(jid); n != "" {
		return n
	}
	if c.db == nil {
		return ""
	}
	if n := c.db.ChatName(ctx, jid); n != "" {
		c.setName(jid, n) // memoriza: a próxima mensagem do chat não consulta de novo
		return n
	}
	return ""
}

// Groups devolve os grupos em que a conta está, ordenados por nome.
func (c *Client) Groups(ctx context.Context) ([]*types.GroupInfo, error) {
	gs, err := c.wa.GetJoinedGroups(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(gs, func(i, j int) bool {
		return strings.ToLower(gs[i].Name) < strings.ToLower(gs[j].Name)
	})
	return gs, nil
}

func bestContactName(info types.ContactInfo) string {
	for _, n := range []string{info.FullName, info.FirstName, info.BusinessName, info.PushName} {
		if n != "" {
			return n
		}
	}
	return ""
}

func strPtr(s string) *string { return &s }

// SetFullSync pede ao WhatsApp o histórico completo (~1 ano) no momento do
// pareamento, em vez do padrão "recente". Só tem efeito antes do login.
func SetFullSync(full bool) {
	store.DeviceProps.RequireFullSync = &full
}
