package wa

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"

	"github.com/bmar13/wapp-summarizer/internal/config"
	"github.com/bmar13/wapp-summarizer/internal/export"
	msgstore "github.com/bmar13/wapp-summarizer/internal/store"
	"github.com/bmar13/wapp-summarizer/internal/transcribe"
)

func audioMsg(mime string, seconds uint32, length uint64, ptt bool) *waE2E.Message {
	return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
		Mimetype:   proto.String(mime),
		Seconds:    proto.Uint32(seconds),
		FileLength: proto.Uint64(length),
		PTT:        proto.Bool(ptt),
		DirectPath: proto.String("/v/t62/aud"),
		MediaKey:   []byte("chave"),
	}}
}

// fakeTranscriber devolve sempre o mesmo texto (ou erro), sem ffmpeg nem
// whisper. É o que permite testar a costura inteira: corpo, arquivo e banco.
type fakeTranscriber struct {
	texto  string
	err    error
	vistos []string
}

func (f *fakeTranscriber) Transcribe(_ context.Context, path string) (string, error) {
	f.vistos = append(f.vistos, path)
	return f.texto, f.err
}

func TestAttachmentOfAudio(t *testing.T) {
	cases := []struct {
		name    string
		msg     *waE2E.Message
		wantOK  bool
		wantExt string
	}{
		{"nota de voz ogg/opus", audioMsg("audio/ogg; codecs=opus", 47, 5000, true), true, ".ogg"},
		{"áudio mp3 anexado", audioMsg("audio/mpeg", 120, 900000, false), true, ".mp3"},
		{"m4a", audioMsg("audio/mp4", 30, 4000, false), true, ".m4a"},
		{"formato que o ffmpeg não reconheceria", audioMsg("audio/x-esotérico", 10, 100, true), false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			att, ok := attachmentOf(tc.msg)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, queria %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if att.Kind != config.KindAudio {
				t.Errorf("Kind = %q, queria %q", att.Kind, config.KindAudio)
			}
			if att.Ext != tc.wantExt {
				t.Errorf("Ext = %q, queria %q", att.Ext, tc.wantExt)
			}
			if att.Seconds == 0 {
				t.Error("Seconds veio zerado: o limite de duração não teria como valer")
			}
		})
	}
}

// Trava. Áudio nunca pode virar arquivo publicado — o que sai da máquina é a
// transcrição. Se algum mimetype de áudio entrasse em mediaExts, o caminho de
// publicação passaria a subir `.ogg` para a nuvem sem ninguém pedir.
func TestAudioIsNeverPublishedAsFile(t *testing.T) {
	for mime, ext := range audioExts {
		if got := extForMime(mime); got != "" {
			t.Errorf("mimetype de áudio %q é publicável como %q; ele só deve existir em audioExts", mime, got)
		}
		if !strings.HasPrefix(ext, ".") {
			t.Errorf("extensão %q de %q não começa com ponto", ext, mime)
		}
	}
}

// Trava. Sem transcritor, áudio não é baixado: baixar para depois apagar sem
// produzir nada seria gastar banda e escrever no disco o dado mais sensível
// que este programa toca, à toa.
func TestAudioIsNotDownloadedWithoutTranscriber(t *testing.T) {
	c := testClient(func(cfg *config.Config) {
		cfg.Media = config.Media{
			Enabled:   true,
			MaxFileMB: 20,
			Chats:     []config.MediaChat{{Match: "Família", Kinds: []string{config.KindAudio}}},
		}
		cfg.Transcribe = config.Transcribe{MaxSeconds: 600}
	})
	c.setName("fam@g.us", "Família")

	m := msgstore.Message{ID: "M1", ChatJID: "fam@g.us", Timestamp: time.Now()}
	if _, ok := c.mediaJobFor(m, audioMsg("audio/ogg", 30, 5000, true), "Família"); ok {
		t.Error("áudio foi enfileirado sem transcritor configurado")
	}

	// Com transcritor, o mesmo áudio passa — o que prova que a recusa acima
	// veio da ausência dele, e não de outro filtro.
	c.transcriber = &fakeTranscriber{texto: "oi"}
	if _, ok := c.mediaJobFor(m, audioMsg("audio/ogg", 30, 5000, true), "Família"); !ok {
		t.Error("áudio autorizado não foi enfileirado")
	}
}

func TestAudioRespectsMaxSeconds(t *testing.T) {
	c := testClient(func(cfg *config.Config) {
		cfg.Media = config.Media{
			Enabled:   true,
			MaxFileMB: 20,
			Chats:     []config.MediaChat{{Match: "Família", Kinds: []string{config.KindAudio}}},
		}
		cfg.Transcribe = config.Transcribe{Enabled: true, MaxSeconds: 60}
	})
	c.setName("fam@g.us", "Família")
	c.transcriber = &fakeTranscriber{texto: "oi"}

	m := msgstore.Message{ID: "M1", ChatJID: "fam@g.us", Timestamp: time.Now()}

	if _, ok := c.mediaJobFor(m, audioMsg("audio/ogg", 61, 5000, true), "Família"); ok {
		t.Error("áudio acima do limite de duração foi enfileirado")
	}
	if _, ok := c.mediaJobFor(m, audioMsg("audio/ogg", 60, 5000, true), "Família"); !ok {
		t.Error("áudio exatamente no limite foi recusado")
	}
}

func TestTranscribedBody(t *testing.T) {
	cases := []struct{ name, marker, transcript, want string }{
		{
			"marcador é mantido junto da fala",
			"[áudio (voz) 47s]", "chego às 19h",
			`[áudio (voz) 47s] "chego às 19h"`,
		},
		{
			// Sem transcrição o marcador continua sozinho: aspas vazias
			// sugeririam que alguém falou nada.
			"sem transcrição fica só o marcador",
			"[áudio (voz) 47s]", "   ",
			"[áudio (voz) 47s]",
		},
		{"sem marcador", "", "chego às 19h", `"chego às 19h"`},
		{"espaços em volta são aparados", " [áudio 4s] ", "  oi  ", `[áudio 4s] "oi"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := transcribedBody(tc.marker, tc.transcript); got != tc.want {
				t.Errorf("transcribedBody(%q, %q) = %q, queria %q", tc.marker, tc.transcript, got, tc.want)
			}
		})
	}
}

// A duração precisa sobreviver à transcrição: um resumo que só lê o texto não
// distingue "ele escreveu" de "ele mandou quatro minutos de áudio".
func TestTranscribedBodyKeepsDuration(t *testing.T) {
	marker := Describe(audioMsg("audio/ogg", 47, 5000, true)).Body
	corpo := transcribedBody(marker, "chego às 19h")

	if !strings.Contains(corpo, "47s") {
		t.Errorf("a duração se perdeu: %q", corpo)
	}
	if !strings.Contains(corpo, "chego às 19h") {
		t.Errorf("a fala se perdeu: %q", corpo)
	}
}

// clienteComBanco monta um Client com banco real e transcritor falso.
func clienteComBanco(t *testing.T, fake *fakeTranscriber) (*Client, *msgstore.DB, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := msgstore.Open(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	c := testClient(func(cfg *config.Config) {
		cfg.Paths.DataDir = dir
		cfg.HostID = "maquina-a"
		cfg.Export.MaxMessageChars = 4000
		cfg.Media = config.Media{Enabled: true, MaxFileMB: 20,
			Chats: []config.MediaChat{{Match: "fam@g.us", Kinds: []string{config.KindAudio}}}}
		cfg.Transcribe = config.Transcribe{Enabled: true, MaxSeconds: 600}
	})
	c.db = db
	c.transcriber = fake
	return c, db, dir
}

func TestRunTranscriptionWritesTextAndPointer(t *testing.T) {
	ctx := context.Background()
	fake := &fakeTranscriber{texto: "chego às 19h, avisa a Maria"}
	c, db, dir := clienteComBanco(t, fake)

	original := msgstore.Message{
		ChatJID: "fam@g.us", ID: "M1", SenderName: "João", Timestamp: time.Now(),
		Kind: "audio", Body: "[áudio (voz) 47s]", Source: "live",
	}
	if _, err := db.PutMessage(ctx, original); err != nil {
		t.Fatal(err)
	}

	audio := filepath.Join(dir, "nota.ogg")
	if err := os.WriteFile(audio, []byte("opus"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.runTranscription(transcribeJob{chatJID: "fam@g.us", msgID: "M1", path: audio})

	m, ok, err := db.Message(ctx, "fam@g.us", "M1")
	if err != nil || !ok {
		t.Fatalf("mensagem sumiu: %v", err)
	}
	if !strings.Contains(m.Body, "47s") || !strings.Contains(m.Body, "avisa a Maria") {
		t.Errorf("corpo = %q", m.Body)
	}
	if m.Media == "" {
		t.Fatal("a transcrição não recebeu ponteiro; sem ele o prio não sobe e a escrita seria recusada")
	}
	if !strings.HasSuffix(m.Media, ".txt") {
		t.Errorf("o ponteiro não aponta para um texto: %q", m.Media)
	}
	if !strings.HasPrefix(m.Media, export.MediaDir+"/maquina-a/") {
		t.Errorf("o ponteiro não está na pasta desta máquina: %q", m.Media)
	}
	if m.Rank() <= original.Rank() {
		t.Errorf("prio não subiu (%d -> %d): o merge poderia escolher a versão sem transcrição",
			original.Rank(), m.Rank())
	}

	// O texto completo tem que estar no arquivo, e o áudio tem que ter sumido.
	conteudo, err := os.ReadFile(filepath.Join(c.cfg.MediaDir(), filepath.Base(m.Media)))
	if err != nil {
		t.Fatalf("arquivo da transcrição: %v", err)
	}
	if strings.TrimSpace(string(conteudo)) != fake.texto {
		t.Errorf("arquivo = %q, queria %q", conteudo, fake.texto)
	}
	if _, err := os.Stat(audio); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("o áudio bruto continua no disco (err = %v)", err)
	}
}

// Trava. O áudio some em QUALQUER desfecho — inclusive quando o whisper falha.
// Guardá-lo seria manter no disco, por acidente, o dado mais sensível que este
// programa toca.
func TestRawAudioIsAlwaysRemoved(t *testing.T) {
	ctx := context.Background()

	cases := map[string]*fakeTranscriber{
		"whisper falhou":    {err: errors.New("modelo corrompido")},
		"áudio sem fala":    {texto: ""},
		"transcrição feita": {texto: "chego às 19h"},
	}

	for nome, fake := range cases {
		t.Run(nome, func(t *testing.T) {
			c, db, dir := clienteComBanco(t, fake)
			if _, err := db.PutMessage(ctx, msgstore.Message{
				ChatJID: "fam@g.us", ID: "M1", Timestamp: time.Now(),
				Kind: "audio", Body: "[áudio (voz) 47s]", Source: "live",
			}); err != nil {
				t.Fatal(err)
			}

			audio := filepath.Join(dir, "nota.ogg")
			if err := os.WriteFile(audio, []byte("opus"), 0o600); err != nil {
				t.Fatal(err)
			}
			c.runTranscription(transcribeJob{chatJID: "fam@g.us", msgID: "M1", path: audio})

			if _, err := os.Stat(audio); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("o áudio bruto sobreviveu a %q (err = %v)", nome, err)
			}
		})
	}
}

// Áudio sem fala mantém o marcador intacto: nada de aspas vazias no digest.
func TestSilentAudioKeepsMarker(t *testing.T) {
	ctx := context.Background()
	c, db, dir := clienteComBanco(t, &fakeTranscriber{texto: ""})

	if _, err := db.PutMessage(ctx, msgstore.Message{
		ChatJID: "fam@g.us", ID: "M1", Timestamp: time.Now(),
		Kind: "audio", Body: "[áudio (voz) 47s]", Source: "live",
	}); err != nil {
		t.Fatal(err)
	}
	audio := filepath.Join(dir, "nota.ogg")
	if err := os.WriteFile(audio, []byte("opus"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.runTranscription(transcribeJob{chatJID: "fam@g.us", msgID: "M1", path: audio})

	m, _, err := db.Message(ctx, "fam@g.us", "M1")
	if err != nil {
		t.Fatal(err)
	}
	if m.Body != "[áudio (voz) 47s]" {
		t.Errorf("corpo = %q, queria o marcador intacto", m.Body)
	}
	if m.Media != "" {
		t.Errorf("silêncio virou arquivo publicado: %q", m.Media)
	}
}

func TestStageAudioQueuesAndDropsWithoutLeaking(t *testing.T) {
	c, _, _ := clienteComBanco(t, &fakeTranscriber{texto: "oi"})
	job := mediaJob{chatJID: "fam@g.us", msgID: "M1",
		att: attachment{Kind: config.KindAudio, Ext: ".ogg"}}

	c.transQ = make(chan transcribeJob, 1)
	c.stageAudio(job, []byte("opus"))

	select {
	case enfileirado := <-c.transQ:
		if _, err := os.Stat(enfileirado.path); err != nil {
			t.Errorf("o áudio enfileirado não está no disco: %v", err)
		}
		if !strings.HasSuffix(enfileirado.path, ".ogg") {
			t.Errorf("temporário sem a extensão do áudio: %q", enfileirado.path)
		}
		// Quem consome é que apaga; aqui o consumidor é o teste. Sem isto, o
		// arquivo legítimo deste caso apareceria como vazamento do próximo.
		if err := os.Remove(enfileirado.path); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("nada foi enfileirado")
	}

	// Fila cheia: o áudio é descartado, e o temporário não pode ficar para trás.
	c.transQ = make(chan transcribeJob, 1)
	c.transQ <- transcribeJob{}
	c.stageAudio(job, []byte("opus"))

	sobras, err := os.ReadDir(c.cfg.AudioDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range sobras {
		if strings.HasSuffix(e.Name(), ".ogg") {
			t.Errorf("temporário vazou quando a fila estava cheia: %s", e.Name())
		}
	}
}

// Com a transcrição desligada o campo tem que ser nil de verdade. Um ponteiro
// tipado nulo dentro de uma interface é != nil, e todo `if c.transcriber == nil`
// passaria a mentir — ligando o worker sem ter o que chamar.
func TestNewTranscriberReturnsUntypedNilWhenDisabled(t *testing.T) {
	tr, err := newTranscriber(transcribe.Options{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if tr != nil {
		t.Errorf("transcritor desligado não é nil: %#v", tr)
	}
}
