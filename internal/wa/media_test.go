package wa

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"

	"github.com/bmar13/wappsync/internal/config"
	"github.com/bmar13/wappsync/internal/export"
	msgstore "github.com/bmar13/wappsync/internal/store"
)

// A decisão de baixar é função pura do config, do nome do chat e do protobuf.
// Dá para exercitar tudo o que importa sem rede, sem conta e sem disco.

func imageMsg(mime string, length uint64) *waE2E.Message {
	return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Mimetype:   proto.String(mime),
		Caption:    proto.String("na praia"),
		FileLength: proto.Uint64(length),
		DirectPath: proto.String("/v/t62/abc"),
		MediaKey:   []byte("chave"),
	}}
}

func docMsg(mime, fileName string, length uint64) *waE2E.Message {
	return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		Mimetype:   proto.String(mime),
		FileName:   proto.String(fileName),
		FileLength: proto.Uint64(length),
		DirectPath: proto.String("/v/t62/def"),
		MediaKey:   []byte("chave"),
	}}
}

func TestExtForMime(t *testing.T) {
	cases := []struct{ mime, want string }{
		{"image/jpeg", ".jpg"},
		{"IMAGE/JPEG", ".jpg"},
		{"image/jpeg; codecs=avc1", ".jpg"},
		{"  application/pdf  ", ".pdf"},
		{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", ".docx"},
		{"audio/ogg", ""},
		{"application/x-msdownload", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := extForMime(tc.mime); got != tc.want {
			t.Errorf("extForMime(%q) = %q, queria %q", tc.mime, got, tc.want)
		}
	}
}

func TestAttachmentOf(t *testing.T) {
	cases := []struct {
		name     string
		msg      *waE2E.Message
		wantOK   bool
		wantKind string
		wantExt  string
	}{
		{"imagem jpeg", imageMsg("image/jpeg", 1000), true, "image", ".jpg"},
		{"documento pdf", docMsg("application/pdf", "nota.pdf", 2000), true, "document", ".pdf"},
		{"texto não tem anexo", textMsg("oi"), false, "", ""},
		{"nil", nil, false, "", ""},
		{
			// Formato que o agente não abre não vale o espaço na pasta.
			"mimetype não suportado", imageMsg("image/heic", 1000), false, "", "",
		},
		{
			"documento com legenda é desembrulhado",
			&waE2E.Message{DocumentWithCaptionMessage: &waE2E.FutureProofMessage{
				Message: docMsg("application/pdf", "boleto.pdf", 500),
			}},
			true, "document", ".pdf",
		},
		{
			"efêmera é desembrulhada",
			&waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{
				Message: imageMsg("image/png", 500),
			}},
			true, "image", ".png",
		},
		{
			"enviada por outro aparelho é desembrulhada",
			&waE2E.Message{DeviceSentMessage: &waE2E.DeviceSentMessage{
				Message: imageMsg("image/webp", 500),
			}},
			true, "image", ".webp",
		},
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
			if att.Kind != tc.wantKind {
				t.Errorf("Kind = %q, queria %q", att.Kind, tc.wantKind)
			}
			if att.Ext != tc.wantExt {
				t.Errorf("Ext = %q, queria %q", att.Ext, tc.wantExt)
			}
			if att.Source == nil {
				t.Error("Source nil: não haveria o que passar para Download")
			}
		})
	}
}

// Trava. "Ver uma vez" é a única expectativa explícita que o remetente registra
// sobre a permanência do que mandou. O texto continua saindo — o arquivo não
// pode ir para uma pasta de nuvem que dura para sempre.
func TestViewOnceIsNeverDownloaded(t *testing.T) {
	wrappers := map[string]*waE2E.Message{
		"ViewOnceMessage": {ViewOnceMessage: &waE2E.FutureProofMessage{
			Message: imageMsg("image/jpeg", 500),
		}},
		"ViewOnceMessageV2": {ViewOnceMessageV2: &waE2E.FutureProofMessage{
			Message: imageMsg("image/jpeg", 500),
		}},
		"ViewOnceMessageV2Extension": {ViewOnceMessageV2Extension: &waE2E.FutureProofMessage{
			Message: docMsg("application/pdf", "x.pdf", 500),
		}},
	}

	for name, msg := range wrappers {
		t.Run(name, func(t *testing.T) {
			if _, ok := attachmentOf(msg); ok {
				t.Error("anexo de 'ver uma vez' foi considerado baixável")
			}
			// O conteúdo continua descrito: o resumo não perde a mensagem,
			// só o arquivo.
			if body := Describe(msg).Body; body == "" {
				t.Error("a mensagem sumiu do texto; era só o arquivo que não podia ir")
			}
		})
	}
}

// Trava. A extensão vem do mimetype, nunca do nome declarado na mensagem: o
// nome é texto de terceiro e vira caminho no disco e na pasta da nuvem.
func TestExtensionNeverComesFromFileName(t *testing.T) {
	hostis := []string{
		"../../.bashrc",
		"foto.exe",
		"C:\\Windows\\System32\\evil.dll",
		"nota.pdf.exe",
		"",
	}
	for _, name := range hostis {
		att, ok := attachmentOf(docMsg("application/pdf", name, 100))
		if !ok {
			t.Fatalf("documento com nome %q não foi reconhecido", name)
		}
		if att.Ext != ".pdf" {
			t.Errorf("nome %q produziu extensão %q; a extensão tem que vir do mimetype", name, att.Ext)
		}

		file := mediaFileName([]byte("conteúdo"), att.Ext)
		if strings.ContainsAny(file, `/\`) || strings.Contains(file, "..") {
			t.Errorf("nome de arquivo %q escapa do diretório", file)
		}
	}
}

func TestMediaFileNameIsContentAddressed(t *testing.T) {
	a := mediaFileName([]byte("mesmo conteúdo"), ".jpg")
	b := mediaFileName([]byte("mesmo conteúdo"), ".jpg")
	c := mediaFileName([]byte("outro conteúdo"), ".jpg")

	if a != b {
		t.Errorf("o mesmo conteúdo gerou nomes diferentes: %q e %q", a, b)
	}
	if a == c {
		t.Error("conteúdos diferentes geraram o mesmo nome")
	}
	if !strings.HasSuffix(a, ".jpg") {
		t.Errorf("nome %q perdeu a extensão", a)
	}
	if len(a) != 64+len(".jpg") {
		t.Errorf("nome %q não parece um sha256 hexa", a)
	}
}

// Trava, irmã de TestAlertFileIsPerHost: cada máquina só escreve arquivos com o
// próprio nome. Um diretório de mídia comum reabriria o lost update que o
// desenho inteiro evita — e impediria podar sem apagar o que é dos outros.
func TestMediaPathIsPerHost(t *testing.T) {
	a := export.MediaFile("maquina-a", "abc.jpg")
	b := export.MediaFile("maquina-b", "abc.jpg")

	if a == b {
		t.Fatalf("duas máquinas escreveriam no mesmo caminho: %q", a)
	}
	for host, path := range map[string]string{"maquina-a": a, "maquina-b": b} {
		if !strings.Contains(path, host) {
			t.Errorf("caminho %q não contém o host %q", path, host)
		}
		if !strings.HasPrefix(path, export.MediaDir+"/") {
			t.Errorf("caminho %q está fora de %s/", path, export.MediaDir)
		}
	}
}

func TestMediaJobFor(t *testing.T) {
	const famJID = "120363000000000000@g.us"

	liberado := func(cfg *config.Config) {
		cfg.Media = config.Media{
			Enabled:   true,
			MaxFileMB: 1,
			Chats:     []config.MediaChat{{Match: "Família", Kinds: []string{"image"}}},
		}
	}

	cases := []struct {
		name  string
		tweak func(*config.Config)
		msg   *waE2E.Message
		want  bool
	}{
		{"chat e kind autorizados", liberado, imageMsg("image/jpeg", 1000), true},
		{
			"trava mestra desligada",
			func(cfg *config.Config) {
				liberado(cfg)
				cfg.Media.Enabled = false
			},
			imageMsg("image/jpeg", 1000), false,
		},
		{"sem [media] nenhum", nil, imageMsg("image/jpeg", 1000), false},
		{"kind fora da política do chat", liberado, docMsg("application/pdf", "x.pdf", 1000), false},
		{
			"chat fora da política",
			func(cfg *config.Config) {
				liberado(cfg)
				cfg.Media.Chats = []config.MediaChat{{Match: "Trabalho", Kinds: []string{"image"}}}
			},
			imageMsg("image/jpeg", 1000), false,
		},
		{"acima do limite de tamanho", liberado, imageMsg("image/jpeg", 2*1024*1024), false},
		{"no limite exato passa", liberado, imageMsg("image/jpeg", 1024*1024), true},
		{"mensagem de texto", liberado, textMsg("oi"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(tc.tweak)
			c.setName(famJID, "Família")

			m := msgstore.Message{ID: "M1", ChatJID: famJID, Timestamp: time.Now()}
			job, ok := c.mediaJobFor(m, tc.msg, "Família")
			if ok != tc.want {
				t.Fatalf("mediaJobFor = %v, queria %v", ok, tc.want)
			}
			if ok && (job.chatJID != famJID || job.msgID != "M1") {
				t.Errorf("job aponta para %s/%s", job.chatJID, job.msgID)
			}
		})
	}
}

// Um job só pode nascer para uma mensagem que já existe no banco: o download
// termina num SetMedia, e sem linha o arquivo vira órfão na pasta.
func TestMediaJobNeedsIdentifiedMessage(t *testing.T) {
	c := testClient(func(cfg *config.Config) {
		cfg.Media = config.Media{Enabled: true, MaxFileMB: 20,
			Chats: []config.MediaChat{{Match: "g@g.us", Kinds: []string{"image"}}}}
	})

	for _, m := range []msgstore.Message{
		{ID: "", ChatJID: "g@g.us"},
		{ID: "M1", ChatJID: ""},
	} {
		if _, ok := c.mediaJobFor(m, imageMsg("image/jpeg", 100), "Família"); ok {
			t.Errorf("job criado para mensagem sem chave: %+v", m)
		}
	}
}

func TestWriteMediaFileIsAtomicAndIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "media")
	data := []byte("bytes do anexo")
	name := mediaFileName(data, ".jpg")

	for i := 0; i < 2; i++ {
		if err := writeMediaFile(dir, name, data); err != nil {
			t.Fatalf("escrita %d: %v", i, err)
		}
	}

	got, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Errorf("conteúdo = %q, queria %q", got, data)
	}

	// Nenhum temporário pode sobrar: a pasta é lida pelo ciclo de publicação.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("sobrou lixo na pasta de mídia: %v", names)
	}
}

// Regressão. O mapa de nomes nasce vazio a cada execução e só é preenchido pelo
// RefreshNames, alguns segundos depois do Connected. Antes desta correção, uma
// mensagem que chegasse nessa janela era avaliada com nome vazio — e uma
// política escrita como `match = "Família"` não casava com nada, sem que nada
// aparecesse no log. O banco sobrevive ao reinício e cobre o intervalo.
func TestChatNameFallsBackToDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := msgstore.Open(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const jid = "120363000000000000@g.us"
	if err := db.UpsertChat(ctx, msgstore.Chat{JID: jid, Name: "Família", IsGroup: true}); err != nil {
		t.Fatal(err)
	}

	c := testClient(func(cfg *config.Config) {
		cfg.Media = config.Media{Enabled: true, MaxFileMB: 20,
			Chats: []config.MediaChat{{Match: "Família", Kinds: []string{"image"}}}}
	})
	c.db = db

	// Memória vazia, como logo depois de subir o programa.
	if n := c.getName(jid); n != "" {
		t.Fatalf("o teste começou com o nome já em memória: %q", n)
	}
	if got := c.chatName(ctx, jid); got != "Família" {
		t.Fatalf("chatName = %q, queria \"Família\" vindo do banco", got)
	}

	// E com o nome resolvido, o anexo passa a ser baixado.
	m := msgstore.Message{ID: "M1", ChatJID: jid, Timestamp: time.Now()}
	if _, ok := c.mediaJobFor(m, imageMsg("image/jpeg", 1000), c.chatName(ctx, jid)); !ok {
		t.Error("anexo continua sendo pulado mesmo com o nome no banco")
	}

	// Memoriza: a próxima mensagem do mesmo chat não consulta o banco de novo.
	if n := c.getName(jid); n != "Família" {
		t.Errorf("nome não foi memorizado: %q", n)
	}
}

// Chat que ninguém nomeou não pode derrubar a decisão — casar por JID continua
// funcionando, que é o contorno recomendado quando o nome não é conhecido.
func TestJIDMatchWorksWithoutName(t *testing.T) {
	const jid = "120363000000000000@g.us"
	c := testClient(func(cfg *config.Config) {
		cfg.Media = config.Media{Enabled: true, MaxFileMB: 20,
			Chats: []config.MediaChat{{Match: jid, Kinds: []string{"image"}}}}
	})

	m := msgstore.Message{ID: "M1", ChatJID: jid, Timestamp: time.Now()}
	if _, ok := c.mediaJobFor(m, imageMsg("image/jpeg", 1000), ""); !ok {
		t.Error("match por JID exato falhou com o nome desconhecido")
	}
}
