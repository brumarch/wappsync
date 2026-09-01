package wa

import (
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow/proto/waE2E"
)

// Content é o resultado de achatar um waE2E.Message em texto.
type Content struct {
	Kind       string // text, image, video, audio, document, sticker, location, contact, poll, reaction, event, revoke, other
	Body       string
	QuotedID   string
	QuotedText string
	Revision   int  // > 0 quando é uma edição
	Deleted    bool // revogação ("apagada para todos")
	TargetID   string
}

// Describe extrai texto legível de uma mensagem do WhatsApp.
//
// Mídia nunca é baixada: imagens, áudios e documentos viram marcadores com a
// legenda/nome do arquivo, que é o que interessa para um resumo.
func Describe(msg *waE2E.Message) Content {
	return describe(msg, 0)
}

func describe(msg *waE2E.Message, depth int) Content {
	var c Content
	if msg == nil || depth > 4 {
		return c
	}

	// Invólucros: desembrulha e reprocessa. A lista mora em unwrapOnce porque
	// attachmentOf precisa descascar exatamente a mesma coisa.
	if inner := unwrapOnce(msg); inner != nil {
		return describe(inner, depth+1)
	}

	// Edições e revogações vêm dentro de ProtocolMessage.
	if pm := msg.GetProtocolMessage(); pm != nil {
		switch pm.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			c.Kind = "revoke"
			c.Deleted = true
			c.Body = "[mensagem apagada]"
			c.TargetID = pm.GetKey().GetID()
			return c
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			inner := describe(pm.GetEditedMessage(), depth+1)
			inner.Revision = 1
			inner.TargetID = pm.GetKey().GetID()
			if inner.Body != "" {
				inner.Body += " [editada]"
			}
			return inner
		}
	}
	if em := msg.GetEditedMessage(); em.GetMessage() != nil {
		inner := describe(em.GetMessage(), depth+1)
		inner.Revision = 1
		return inner
	}

	switch {
	case msg.GetConversation() != "":
		c.Kind, c.Body = "text", msg.GetConversation()

	case msg.GetExtendedTextMessage() != nil:
		et := msg.GetExtendedTextMessage()
		c.Kind, c.Body = "text", et.GetText()
		if t := et.GetTitle(); t != "" && !strings.Contains(c.Body, t) {
			c.Body = strings.TrimSpace(c.Body + "\n[link: " + t + "]")
		}
		c.quoteFrom(et.GetContextInfo(), depth)

	case msg.GetImageMessage() != nil:
		c.Kind = "image"
		c.Body = media("imagem", msg.GetImageMessage().GetCaption())
		c.quoteFrom(msg.GetImageMessage().GetContextInfo(), depth)

	case msg.GetVideoMessage() != nil:
		v := msg.GetVideoMessage()
		label := "vídeo"
		if v.GetGifPlayback() {
			label = "gif"
		}
		c.Kind = "video"
		c.Body = media(fmt.Sprintf("%s %ds", label, v.GetSeconds()), v.GetCaption())
		c.quoteFrom(v.GetContextInfo(), depth)

	case msg.GetPtvMessage() != nil:
		c.Kind = "video"
		c.Body = media(fmt.Sprintf("vídeo-mensagem %ds", msg.GetPtvMessage().GetSeconds()), "")

	case msg.GetAudioMessage() != nil:
		a := msg.GetAudioMessage()
		label := "áudio"
		if a.GetPTT() {
			label = "áudio (voz)"
		}
		c.Kind = "audio"
		c.Body = media(fmt.Sprintf("%s %ds", label, a.GetSeconds()), "")
		c.quoteFrom(a.GetContextInfo(), depth)

	case msg.GetDocumentMessage() != nil:
		d := msg.GetDocumentMessage()
		name := d.GetFileName()
		if name == "" {
			name = d.GetTitle()
		}
		c.Kind = "document"
		c.Body = media("documento: "+name, d.GetCaption())
		c.quoteFrom(d.GetContextInfo(), depth)

	case msg.GetStickerMessage() != nil:
		c.Kind, c.Body = "sticker", "[figurinha]"
		c.quoteFrom(msg.GetStickerMessage().GetContextInfo(), depth)

	case msg.GetLocationMessage() != nil:
		l := msg.GetLocationMessage()
		parts := []string{fmt.Sprintf("%.5f,%.5f", l.GetDegreesLatitude(), l.GetDegreesLongitude())}
		if l.GetName() != "" {
			parts = append(parts, l.GetName())
		}
		if l.GetAddress() != "" {
			parts = append(parts, l.GetAddress())
		}
		c.Kind, c.Body = "location", "[localização: "+strings.Join(parts, " — ")+"]"

	case msg.GetLiveLocationMessage() != nil:
		c.Kind, c.Body = "location", "[localização em tempo real]"

	case msg.GetContactMessage() != nil:
		c.Kind = "contact"
		c.Body = "[contato: " + msg.GetContactMessage().GetDisplayName() + "]"

	case msg.GetContactsArrayMessage() != nil:
		c.Kind = "contact"
		c.Body = fmt.Sprintf("[%d contatos]", len(msg.GetContactsArrayMessage().GetContacts()))

	case msg.GetReactionMessage() != nil:
		r := msg.GetReactionMessage()
		c.Kind = "reaction"
		c.Body = "[reagiu " + r.GetText() + "]"
		c.TargetID = r.GetKey().GetID()

	case pollOf(msg) != nil:
		p := pollOf(msg)
		opts := make([]string, 0, len(p.GetOptions()))
		for _, o := range p.GetOptions() {
			opts = append(opts, o.GetOptionName())
		}
		c.Kind = "poll"
		c.Body = "[enquete: " + p.GetName() + " — " + strings.Join(opts, " | ") + "]"

	case msg.GetPollUpdateMessage() != nil:
		c.Kind, c.Body = "poll", "[voto em enquete]"

	case msg.GetEventMessage() != nil:
		e := msg.GetEventMessage()
		c.Kind = "event"
		c.Body = strings.TrimSpace("[evento: " + e.GetName() + " " + e.GetDescription() + "]")

	case msg.GetGroupInviteMessage() != nil:
		g := msg.GetGroupInviteMessage()
		c.Kind = "invite"
		c.Body = media("convite para o grupo "+g.GetGroupName(), g.GetCaption())

	case msg.GetAlbumMessage() != nil:
		c.Kind = "album"
		c.Body = fmt.Sprintf("[álbum com %d itens]", msg.GetAlbumMessage().GetExpectedImageCount())

	case msg.GetCall() != nil:
		c.Kind, c.Body = "call", "[chamada]"

	case msg.GetSenderKeyDistributionMessage() != nil, msg.GetMessageContextInfo() != nil:
		// Ruído de protocolo, sem conteúdo para o usuário.
		c.Kind = "skip"

	default:
		c.Kind = "other"
	}

	return c
}

func pollOf(msg *waE2E.Message) *waE2E.PollCreationMessage {
	for _, p := range []*waE2E.PollCreationMessage{
		msg.GetPollCreationMessage(),
		msg.GetPollCreationMessageV2(),
		msg.GetPollCreationMessageV3(),
	} {
		if p != nil {
			return p
		}
	}
	return nil
}

func (c *Content) quoteFrom(ci *waE2E.ContextInfo, depth int) {
	if ci == nil || ci.GetQuotedMessage() == nil {
		return
	}
	c.QuotedID = ci.GetStanzaID()
	q := describe(ci.GetQuotedMessage(), depth+1)
	c.QuotedText = truncate(q.Body, 200)
}

func media(label, caption string) string {
	if caption == "" {
		return "[" + label + "]"
	}
	return "[" + label + "] " + caption
}

func truncate(s string, n int) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
