package wa

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"

	"github.com/bmar13/wapp-summarizer/internal/config"
	"github.com/bmar13/wapp-summarizer/internal/export"
	msgstore "github.com/bmar13/wapp-summarizer/internal/store"
)

// attachment descreve um anexo que este binário sabe baixar e publicar.
type attachment struct {
	Kind string // "image" | "document" — os mesmos valores de media.chat.kinds
	Ext  string // extensão derivada do mimetype, com ponto
	// Length é o tamanho DECLARADO pelo remetente. Serve para desistir antes
	// de baixar; não é confiável, então o tamanho real é conferido de novo
	// depois do download.
	Length uint64
	// Seconds é a duração declarada, usada só para áudio: o custo de CPU da
	// transcrição cresce com ela, e um áudio de uma hora não pode segurar a
	// fila.
	Seconds uint32
	Source  whatsmeow.DownloadableMessage
}

// mediaExts mapeia mimetype para extensão.
//
// A extensão sai daqui e NUNCA do nome do arquivo declarado na mensagem: aquele
// campo é texto de terceiro, e vira nome de arquivo no disco e na pasta da
// nuvem. Um "../../.bashrc" ou um ".exe" não podem chegar lá por esse caminho.
//
// Mimetype fora desta lista não é baixado. O anexo existe para o agente ler; um
// formato que ele não abre só ocuparia espaço na pasta.
var mediaExts = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
	"image/gif":  ".gif",

	"application/pdf":    ".pdf",
	"application/msword": ".doc",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": ".docx",
	"application/vnd.ms-excel": ".xls",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         ".xlsx",
	"application/vnd.ms-powerpoint":                                             ".ppt",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": ".pptx",
	"text/plain": ".txt",
	"text/csv":   ".csv",
	"text/rtf":   ".rtf",
}

// audioExts fica separado de mediaExts porque a semântica é outra: nada aqui
// vira arquivo publicado. O áudio é baixado para um temporário, transcrito e
// apagado — o que sai da máquina é o texto. Publicar um .ogg na nuvem seria
// peso que nenhum agente lê.
//
// A extensão só serve para o ffmpeg reconhecer a entrada.
var audioExts = map[string]string{
	"audio/ogg":   ".ogg",
	"audio/opus":  ".opus",
	"audio/mpeg":  ".mp3",
	"audio/mp4":   ".m4a",
	"audio/aac":   ".aac",
	"audio/amr":   ".amr",
	"audio/wav":   ".wav",
	"audio/x-wav": ".wav",
	"audio/webm":  ".webm",
	"audio/flac":  ".flac",
}

// extForMime devolve a extensão de um mimetype, ou "" se não for suportado.
func extForMime(mime string) string {
	// O mimetype vem com parâmetros com frequência ("image/jpeg; codecs=...").
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	return mediaExts[strings.ToLower(strings.TrimSpace(mime))]
}

// audioExtForMime devolve a extensão de um áudio transcritível, ou "".
func audioExtForMime(mime string) string {
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	return audioExts[strings.ToLower(strings.TrimSpace(mime))]
}

// unwrapOnce devolve o conteúdo de um invólucro (efêmera, documento com
// legenda, enviada por outro aparelho, ...) ou nil se msg não for um invólucro.
//
// Existe compartilhada entre describe e attachmentOf porque as duas precisam
// descascar exatamente a mesma lista. Duas listas separadas divergiriam, e a
// divergência apareceria como "a mensagem foi descrita mas o anexo não veio".
func unwrapOnce(msg *waE2E.Message) *waE2E.Message {
	for _, wrapped := range []*waE2E.FutureProofMessage{
		msg.GetEphemeralMessage(),
		msg.GetViewOnceMessage(),
		msg.GetViewOnceMessageV2(),
		msg.GetViewOnceMessageV2Extension(),
		msg.GetDocumentWithCaptionMessage(),
		msg.GetGroupMentionedMessage(),
		msg.GetLottieStickerMessage(),
	} {
		if wrapped.GetMessage() != nil {
			return wrapped.GetMessage()
		}
	}
	if dsm := msg.GetDeviceSentMessage(); dsm.GetMessage() != nil {
		return dsm.GetMessage()
	}
	return nil
}

// isViewOnce informa se a mensagem está embrulhada em "ver uma vez".
func isViewOnce(msg *waE2E.Message) bool {
	return msg.GetViewOnceMessage().GetMessage() != nil ||
		msg.GetViewOnceMessageV2().GetMessage() != nil ||
		msg.GetViewOnceMessageV2Extension().GetMessage() != nil
}

// attachmentOf devolve o anexo baixável de uma mensagem, se houver um.
func attachmentOf(msg *waE2E.Message) (attachment, bool) {
	return attachmentAt(msg, 0)
}

func attachmentAt(msg *waE2E.Message, depth int) (attachment, bool) {
	if msg == nil || depth > 4 {
		return attachment{}, false
	}

	// "Ver uma vez" é a única expectativa explícita que o remetente registra
	// sobre a permanência do que mandou. Descrever como "[imagem]" já é o
	// limite; guardar o arquivo para sempre numa pasta de nuvem seria outra
	// coisa. O texto continua saindo — só o arquivo não.
	if isViewOnce(msg) {
		return attachment{}, false
	}
	if inner := unwrapOnce(msg); inner != nil {
		return attachmentAt(inner, depth+1)
	}

	switch {
	case msg.GetImageMessage() != nil:
		im := msg.GetImageMessage()
		ext := extForMime(im.GetMimetype())
		if ext == "" {
			return attachment{}, false
		}
		return attachment{Kind: "image", Ext: ext, Length: im.GetFileLength(), Source: im}, true

	case msg.GetDocumentMessage() != nil:
		dm := msg.GetDocumentMessage()
		ext := extForMime(dm.GetMimetype())
		if ext == "" {
			return attachment{}, false
		}
		return attachment{Kind: "document", Ext: ext, Length: dm.GetFileLength(), Source: dm}, true

	case msg.GetAudioMessage() != nil:
		am := msg.GetAudioMessage()
		ext := audioExtForMime(am.GetMimetype())
		if ext == "" {
			return attachment{}, false
		}
		return attachment{
			Kind: config.KindAudio, Ext: ext,
			Length: am.GetFileLength(), Seconds: am.GetSeconds(), Source: am,
		}, true
	}
	return attachment{}, false
}

// mediaFileName é o nome do arquivo de um anexo: o sha256 do conteúdo já
// decifrado, em hexa, mais a extensão.
//
// O hash é calculado aqui e não copiado do FileSHA256 da mensagem porque aquele
// campo é declarado por terceiro. Como efeito colateral, dois envios do mesmo
// arquivo convergem para o mesmo nome: republicar é reescrever bytes idênticos,
// nunca um lost update.
func mediaFileName(data []byte, ext string) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) + ext
}

// mediaJob é um anexo autorizado e ainda não baixado.
type mediaJob struct {
	chatJID string
	msgID   string
	att     attachment
}

// mediaJobFor decide se a mensagem m tem um anexo que ESTA máquina deve baixar.
//
// É a função que concentra a decisão inteira, e é pura em tudo que importa:
// dado o mesmo config, o mesmo nome de chat e o mesmo protobuf, devolve sempre
// a mesma coisa. Quem baixa é fetchMedia; aqui só se decide.
func (c *Client) mediaJobFor(m msgstore.Message, msg *waE2E.Message) (mediaJob, bool) {
	if !c.cfg.Media.Enabled || m.ID == "" || m.ChatJID == "" {
		return mediaJob{}, false
	}
	att, ok := attachmentOf(msg)
	if !ok {
		return mediaJob{}, false
	}
	if !c.cfg.MediaAllowed(m.ChatJID, c.getName(m.ChatJID), att.Kind) {
		return mediaJob{}, false
	}
	// Tamanho declarado pelo remetente: serve para desistir ANTES de gastar
	// banda. O real é conferido de novo depois, porque este número não é
	// verificado por ninguém.
	if att.Length > c.mediaMaxBytes() {
		c.log.Infof("anexo de %s ignorado: %d MB declarados, limite é %d MB",
			m.ChatJID, att.Length/(1024*1024), c.cfg.Media.MaxFileMB)
		return mediaJob{}, false
	}
	// Áudio tem um segundo teto, em duração: o custo é de CPU, não de banda,
	// e transcrever uma hora de gravação seguraria a fila por muito mais tempo
	// do que o conteúdo justifica.
	if att.Kind == config.KindAudio {
		if c.transcriber == nil {
			return mediaJob{}, false
		}
		if max := uint32(c.cfg.Transcribe.MaxSeconds); att.Seconds > max {
			c.log.Infof("áudio de %s ignorado: %ds acima do limite de %ds",
				m.ChatJID, att.Seconds, max)
			return mediaJob{}, false
		}
	}
	return mediaJob{chatJID: m.ChatJID, msgID: m.ID, att: att}, true
}

func (c *Client) mediaMaxBytes() uint64 {
	return uint64(c.cfg.Media.MaxFileMB) * 1024 * 1024
}

// startMediaWorker liga a fila de download.
//
// O download NÃO pode acontecer no handler de eventos: ele roda nas goroutines
// do whatsmeow, e segurar uma delas por alguns megabytes atrasaria a recepção
// de todo o resto.
func (c *Client) startMediaWorker() {
	if !c.cfg.Media.Enabled {
		return
	}
	c.mediaQ = make(chan mediaJob, 256)
	c.mediaDone = make(chan struct{})
	go func() {
		defer close(c.mediaDone)
		for job := range c.mediaQ {
			c.fetchMedia(job)
		}
	}()
}

func (c *Client) stopMediaWorker() {
	if c.mediaQ == nil {
		return
	}
	close(c.mediaQ)
	<-c.mediaDone
	c.mediaQ = nil
}

// queueMediaJob enfileira sem nunca bloquear quem chamou.
//
// Fila cheia descarta o anexo, de propósito: a mensagem já foi gravada e o
// marcador continua lá, então o que se perde é o arquivo. Bloquear aqui
// pararia a captura inteira, que é um estrago bem maior.
func (c *Client) queueMediaJob(job mediaJob) {
	if c.mediaQ == nil {
		return
	}
	select {
	case c.mediaQ <- job:
	default:
		c.log.Warnf("fila de anexos cheia; %s/%s fica só com o marcador", job.chatJID, job.msgID)
	}
}

// fetchMedia baixa um anexo, grava no disco local e aponta a mensagem para ele.
func (c *Client) fetchMedia(job mediaJob) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	data, err := c.wa.Download(ctx, job.att.Source)
	if err != nil {
		c.log.Warnf("baixando anexo de %s/%s: %v", job.chatJID, job.msgID, err)
		return
	}
	if uint64(len(data)) > c.mediaMaxBytes() {
		c.log.Infof("anexo de %s descartado: %d MB reais acima do limite de %d MB",
			job.chatJID, len(data)/(1024*1024), c.cfg.Media.MaxFileMB)
		return
	}

	// Áudio não vira arquivo publicado: segue para a fila de transcrição, e o
	// que sai da máquina é o texto.
	if job.att.Kind == config.KindAudio {
		c.stageAudio(job, data)
		return
	}

	name := mediaFileName(data, job.att.Ext)
	if err := writeMediaFile(c.cfg.MediaDir(), name, data); err != nil {
		c.log.Warnf("gravando anexo %s: %v", name, err)
		return
	}
	if _, err := c.db.SetMedia(ctx, job.chatJID, job.msgID, export.MediaFile(c.cfg.HostID, name)); err != nil {
		c.log.Warnf("associando anexo %s a %s/%s: %v", name, job.chatJID, job.msgID, err)
	}
}

// writeMediaFile grava o anexo de forma atômica.
//
// O nome é o hash do conteúdo, então reescrever é sempre reescrever bytes
// idênticos — mas o ciclo de export pode estar lendo o arquivo para publicar,
// e um leitor não pode encontrar meio arquivo.
func writeMediaFile(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	dst := filepath.Join(dir, name)
	if _, err := os.Stat(dst); err == nil {
		return nil // mesmo hash, mesmo conteúdo: nada a fazer
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}
