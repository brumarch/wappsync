package wa

import (
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// Describe é uma função pura: dá para exercitar o formato real do WhatsApp
// montando os protobufs à mão, sem rede e sem conta.

func textMsg(s string) *waE2E.Message {
	return &waE2E.Message{Conversation: proto.String(s)}
}

func TestDescribeText(t *testing.T) {
	got := Describe(textMsg("chego às 19h"))
	if got.Kind != "text" {
		t.Errorf("Kind = %q, queria text", got.Kind)
	}
	if got.Body != "chego às 19h" {
		t.Errorf("Body = %q", got.Body)
	}
	if got.Deleted || got.Revision != 0 {
		t.Errorf("mensagem simples marcada como editada/apagada: %+v", got)
	}
}

func TestDescribeExtendedText(t *testing.T) {
	got := Describe(&waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:  proto.String("olha isso https://exemplo.com"),
			Title: proto.String("Título da página"),
		},
	})
	if got.Kind != "text" {
		t.Errorf("Kind = %q", got.Kind)
	}
	if !strings.Contains(got.Body, "olha isso") {
		t.Errorf("perdeu o texto: %q", got.Body)
	}
	if !strings.Contains(got.Body, "Título da página") {
		t.Errorf("perdeu o título do link: %q", got.Body)
	}
}

// O título não deve ser duplicado quando já aparece no corpo.
func TestDescribeExtendedTextSkipsRedundantTitle(t *testing.T) {
	got := Describe(&waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:  proto.String("veja Título da página aqui"),
			Title: proto.String("Título da página"),
		},
	})
	if strings.Count(got.Body, "Título da página") != 1 {
		t.Errorf("título duplicado: %q", got.Body)
	}
}

// Mídia nunca é baixada: vira marcador, preservando a legenda quando existe.
func TestDescribeMedia(t *testing.T) {
	cases := []struct {
		name     string
		msg      *waE2E.Message
		wantKind string
		wantSub  []string
	}{
		{
			name:     "imagem com legenda",
			msg:      &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("no aniversário")}},
			wantKind: "image",
			wantSub:  []string{"[imagem]", "no aniversário"},
		},
		{
			name:     "imagem sem legenda",
			msg:      &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}},
			wantKind: "image",
			wantSub:  []string{"[imagem]"},
		},
		{
			name:     "audio de voz",
			msg:      &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true), Seconds: proto.Uint32(34)}},
			wantKind: "audio",
			wantSub:  []string{"áudio (voz)", "34s"},
		},
		{
			name:     "audio comum",
			msg:      &waE2E.Message{AudioMessage: &waE2E.AudioMessage{Seconds: proto.Uint32(12)}},
			wantKind: "audio",
			wantSub:  []string{"[áudio 12s]"},
		},
		{
			name:     "video",
			msg:      &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Seconds: proto.Uint32(8), Caption: proto.String("olha o gol")}},
			wantKind: "video",
			wantSub:  []string{"vídeo 8s", "olha o gol"},
		},
		{
			name:     "gif",
			msg:      &waE2E.Message{VideoMessage: &waE2E.VideoMessage{GifPlayback: proto.Bool(true), Seconds: proto.Uint32(3)}},
			wantKind: "video",
			wantSub:  []string{"gif"},
		},
		{
			name:     "documento",
			msg:      &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("contrato.pdf")}},
			wantKind: "document",
			wantSub:  []string{"documento: contrato.pdf"},
		},
		{
			name:     "figurinha",
			msg:      &waE2E.Message{StickerMessage: &waE2E.StickerMessage{}},
			wantKind: "sticker",
			wantSub:  []string{"[figurinha]"},
		},
		{
			name: "localizacao",
			msg: &waE2E.Message{LocationMessage: &waE2E.LocationMessage{
				DegreesLatitude:  proto.Float64(-23.5505),
				DegreesLongitude: proto.Float64(-46.6333),
				Name:             proto.String("Av. Paulista"),
			}},
			wantKind: "location",
			wantSub:  []string{"-23.55050", "Av. Paulista"},
		},
		{
			name:     "contato",
			msg:      &waE2E.Message{ContactMessage: &waE2E.ContactMessage{DisplayName: proto.String("Ana Souza")}},
			wantKind: "contact",
			wantSub:  []string{"[contato: Ana Souza]"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Describe(tc.msg)
			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %q, queria %q", got.Kind, tc.wantKind)
			}
			for _, sub := range tc.wantSub {
				if !strings.Contains(got.Body, sub) {
					t.Errorf("Body = %q, faltou %q", got.Body, sub)
				}
			}
		})
	}
}

// Uma citação precisa carregar o texto citado: é o que dá contexto ao resumo.
func TestDescribeQuotedMessage(t *testing.T) {
	got := Describe(&waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String("concordo"),
			ContextInfo: &waE2E.ContextInfo{
				StanzaID:      proto.String("ORIGINAL-1"),
				QuotedMessage: textMsg("vamos remarcar para sexta?"),
			},
		},
	})
	if got.QuotedID != "ORIGINAL-1" {
		t.Errorf("QuotedID = %q", got.QuotedID)
	}
	if got.QuotedText != "vamos remarcar para sexta?" {
		t.Errorf("QuotedText = %q", got.QuotedText)
	}
	if got.Body != "concordo" {
		t.Errorf("Body = %q", got.Body)
	}
}

func TestDescribeQuotedTextIsTruncated(t *testing.T) {
	long := strings.Repeat("palavra ", 100)
	got := Describe(&waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        proto.String("resposta"),
			ContextInfo: &waE2E.ContextInfo{QuotedMessage: textMsg(long)},
		},
	})
	if r := []rune(got.QuotedText); len(r) > 201 {
		t.Errorf("citação com %d runas, deveria ser cortada em ~200", len(r))
	}
	if !strings.HasSuffix(got.QuotedText, "…") {
		t.Errorf("corte sem reticências: %q", got.QuotedText)
	}
}

// Revogação: precisa virar Deleted, e apontar a mensagem alvo para que o
// UPSERT por prio substitua a versão original no banco.
func TestDescribeRevoke(t *testing.T) {
	got := Describe(&waE2E.Message{
		ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_REVOKE.Enum(),
			Key:  &waCommon.MessageKey{ID: proto.String("ALVO-1")},
		},
	})
	if !got.Deleted {
		t.Error("revogação não marcou Deleted")
	}
	if got.Kind != "revoke" {
		t.Errorf("Kind = %q", got.Kind)
	}
	if got.TargetID != "ALVO-1" {
		t.Errorf("TargetID = %q, queria ALVO-1", got.TargetID)
	}
}

// Edição: precisa trazer o texto NOVO, marcar Revision e apontar o alvo.
func TestDescribeEdit(t *testing.T) {
	got := Describe(&waE2E.Message{
		ProtocolMessage: &waE2E.ProtocolMessage{
			Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
			Key:           &waCommon.MessageKey{ID: proto.String("ALVO-2")},
			EditedMessage: textMsg("20h, não 19h"),
		},
	})
	if got.Revision != 1 {
		t.Errorf("Revision = %d, queria 1", got.Revision)
	}
	if !strings.Contains(got.Body, "20h, não 19h") {
		t.Errorf("perdeu o texto editado: %q", got.Body)
	}
	if !strings.Contains(got.Body, "[editada]") {
		t.Errorf("não sinalizou a edição: %q", got.Body)
	}
	if got.TargetID != "ALVO-2" {
		t.Errorf("TargetID = %q, queria ALVO-2", got.TargetID)
	}
}

// Invólucros aninhados: efêmera, visualização única, documento com legenda.
// É onde mora a maior parte das mensagens reais de grupo.
func TestDescribeUnwrapsWrappers(t *testing.T) {
	inner := textMsg("mensagem interna")

	cases := map[string]*waE2E.Message{
		"efemera":          {EphemeralMessage: &waE2E.FutureProofMessage{Message: inner}},
		"view once":        {ViewOnceMessage: &waE2E.FutureProofMessage{Message: inner}},
		"view once v2":     {ViewOnceMessageV2: &waE2E.FutureProofMessage{Message: inner}},
		"doc com legenda":  {DocumentWithCaptionMessage: &waE2E.FutureProofMessage{Message: inner}},
		"grupo mencionado": {GroupMentionedMessage: &waE2E.FutureProofMessage{Message: inner}},
		"device sent":      {DeviceSentMessage: &waE2E.DeviceSentMessage{Message: inner}},
	}

	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			got := Describe(msg)
			if got.Body != "mensagem interna" {
				t.Errorf("não desembrulhou: Kind=%q Body=%q", got.Kind, got.Body)
			}
		})
	}
}

// Aninhamento fundo não pode causar recursão infinita.
func TestDescribeStopsAtDepthLimit(t *testing.T) {
	msg := textMsg("fundo demais")
	for i := 0; i < 12; i++ {
		msg = &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: msg}}
	}
	got := Describe(msg) // não pode travar nem estourar a pilha
	if got.Body == "fundo demais" {
		t.Error("o limite de profundidade não foi aplicado")
	}
}

func TestDescribePoll(t *testing.T) {
	poll := &waE2E.PollCreationMessage{
		Name: proto.String("Onde almoçar?"),
		Options: []*waE2E.PollCreationMessage_Option{
			{OptionName: proto.String("Japonês")},
			{OptionName: proto.String("Padaria")},
		},
	}
	// As três variantes do campo têm que cair no mesmo tratamento.
	for name, msg := range map[string]*waE2E.Message{
		"v1": {PollCreationMessage: poll},
		"v2": {PollCreationMessageV2: poll},
		"v3": {PollCreationMessageV3: poll},
	} {
		t.Run(name, func(t *testing.T) {
			got := Describe(msg)
			if got.Kind != "poll" {
				t.Errorf("Kind = %q", got.Kind)
			}
			for _, sub := range []string{"Onde almoçar?", "Japonês", "Padaria"} {
				if !strings.Contains(got.Body, sub) {
					t.Errorf("Body = %q, faltou %q", got.Body, sub)
				}
			}
		})
	}
}

func TestDescribeReaction(t *testing.T) {
	got := Describe(&waE2E.Message{
		ReactionMessage: &waE2E.ReactionMessage{
			Text: proto.String("👍"),
			Key:  &waCommon.MessageKey{ID: proto.String("ALVO-3")},
		},
	})
	if got.Kind != "reaction" {
		t.Errorf("Kind = %q", got.Kind)
	}
	if !strings.Contains(got.Body, "👍") {
		t.Errorf("perdeu o emoji: %q", got.Body)
	}
	if got.TargetID != "ALVO-3" {
		t.Errorf("TargetID = %q", got.TargetID)
	}
}

// Ruído de protocolo tem que ser descartado, não virar linha no digest.
func TestDescribeSkipsProtocolNoise(t *testing.T) {
	for name, msg := range map[string]*waE2E.Message{
		"sender key": {SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{}},
		"ctx info":   {MessageContextInfo: &waE2E.MessageContextInfo{}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Describe(msg); got.Kind != "skip" {
				t.Errorf("Kind = %q, queria skip", got.Kind)
			}
		})
	}
}

func TestDescribeNilAndUnknown(t *testing.T) {
	if got := Describe(nil); got.Kind != "" || got.Body != "" {
		t.Errorf("nil devolveu %+v", got)
	}
	if got := Describe(&waE2E.Message{}); got.Kind != "other" {
		t.Errorf("mensagem vazia: Kind = %q, queria other", got.Kind)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abcdef", 0); got != "abcdef" {
		t.Errorf("limite 0 deveria não cortar, veio %q", got)
	}
	if got := truncate("abcdef", 10); got != "abcdef" {
		t.Errorf("texto menor que o limite foi alterado: %q", got)
	}
	if got := truncate("abcdef", 3); got != "abc…" {
		t.Errorf("truncate = %q, queria abc…", got)
	}
	// Corte por runas, não por bytes: não pode partir um caractere ao meio.
	if got := truncate("ãéíõü", 2); got != "ãé…" {
		t.Errorf("truncate multibyte = %q, queria ãé…", got)
	}
}
