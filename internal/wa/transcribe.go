package wa

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmar13/wapp-summarizer/internal/export"
	"github.com/bmar13/wapp-summarizer/internal/transcribe"
)

// transcribeJob é um áudio já baixado, esperando o Whisper.
//
// Carrega o caminho de um arquivo temporário e não os bytes: com a fila cheia
// de notas de voz de 20 MB, guardar o conteúdo em memória custaria centenas de
// megabytes parados esperando CPU.
type transcribeJob struct {
	chatJID string
	msgID   string
	path    string
}

// startTranscribeWorker liga a fila de transcrição.
//
// Ela é SEPARADA da fila de download de propósito. O Whisper leva minutos por
// áudio e o download leva segundos; numa fila só, uma nota de voz de dez
// minutos deixaria todas as imagens atrás dela esperando — e a fila de download
// descarta quando enche, então o atraso viraria anexo perdido.
func (c *Client) startTranscribeWorker() {
	if c.transcriber == nil {
		return
	}
	// Áudio que sobrou de uma execução interrompida no meio: ninguém mais tem
	// o job que apontava para ele, e ninguém mais vai apagá-lo.
	limparAudioOrfao(c.cfg.AudioDir())

	c.transQ = make(chan transcribeJob, 64)
	c.transDone = make(chan struct{})
	go func() {
		defer close(c.transDone)
		for job := range c.transQ {
			c.runTranscription(job)
		}
	}()
}

func (c *Client) stopTranscribeWorker() {
	if c.transQ == nil {
		return
	}
	close(c.transQ)
	<-c.transDone
	c.transQ = nil
}

// queueTranscription enfileira sem bloquear. Fila cheia descarta o áudio e
// apaga o temporário: a mensagem já está gravada e o marcador continua lá.
func (c *Client) queueTranscription(job transcribeJob) {
	select {
	case c.transQ <- job:
	default:
		_ = os.Remove(job.path)
		c.log.Warnf("fila de transcrição cheia; %s/%s fica só com o marcador", job.chatJID, job.msgID)
	}
}

// stageAudio grava o áudio baixado num temporário e enfileira a transcrição.
func (c *Client) stageAudio(job mediaJob, data []byte) {
	if c.transQ == nil {
		return
	}
	if err := os.MkdirAll(c.cfg.AudioDir(), 0o700); err != nil {
		c.log.Warnf("criando %s: %v", c.cfg.AudioDir(), err)
		return
	}
	f, err := os.CreateTemp(c.cfg.AudioDir(), "nota-*"+job.att.Ext)
	if err != nil {
		c.log.Warnf("gravando áudio de %s: %v", job.chatJID, err)
		return
	}
	path := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(path)
		c.log.Warnf("gravando áudio de %s: %v", job.chatJID, err)
		return
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		c.log.Warnf("gravando áudio de %s: %v", job.chatJID, err)
		return
	}
	c.queueTranscription(transcribeJob{chatJID: job.chatJID, msgID: job.msgID, path: path})
}

// runTranscription transcreve um áudio, publica o texto e apaga o original.
func (c *Client) runTranscription(job transcribeJob) {
	// O áudio bruto some em qualquer desfecho. Ele nunca é publicado, e
	// mantê-lo no disco seria guardar por acidente o dado mais sensível que
	// este programa toca.
	defer os.Remove(job.path)

	ctx := context.Background()
	texto, err := c.transcriber.Transcribe(ctx, job.path)
	if err != nil {
		c.log.Warnf("transcrevendo %s/%s: %v", job.chatJID, job.msgID, err)
		return
	}
	if texto == "" {
		// Silêncio, ou só música. O marcador original já diz o que dá para
		// dizer; escrever aspas vazias sugeriria que alguém falou nada.
		c.log.Infof("áudio de %s sem fala reconhecível", job.chatJID)
		return
	}

	// O texto completo vai para um arquivo porque max_message_chars corta o
	// corpo (4000 por padrão) e um áudio longo passa disso com folga. É também
	// o que faz o prio subir: o bit de Rank olha para Media, não para Body.
	nome := mediaFileName([]byte(texto), ".txt")
	if err := writeMediaFile(c.cfg.MediaDir(), nome, []byte(texto+"\n")); err != nil {
		c.log.Warnf("gravando transcrição de %s: %v", job.chatJID, err)
		return
	}

	m, ok, err := c.db.Message(ctx, job.chatJID, job.msgID)
	if err != nil || !ok {
		c.log.Warnf("mensagem %s/%s sumiu antes da transcrição chegar", job.chatJID, job.msgID)
		return
	}
	corpo := truncate(transcribedBody(m.Body, texto), c.cfg.Export.MaxMessageChars)

	if _, err := c.db.SetTranscription(ctx, job.chatJID, job.msgID, corpo,
		export.MediaFile(c.cfg.HostID, nome)); err != nil {
		c.log.Warnf("associando transcrição a %s/%s: %v", job.chatJID, job.msgID, err)
	}
}

// transcribedBody junta o marcador do áudio com o que foi falado.
//
// O marcador é mantido, e não substituído, porque ele carrega o que a
// transcrição não diz: que aquilo era voz, e quanto tempo durou. Um resumo que
// perde isso não distingue "ele escreveu" de "ele mandou um áudio de 4 minutos".
//
// As aspas marcam onde termina o nosso texto e começa o de terceiro — a mesma
// razão pela qual o guia insiste que o conteúdo é dado e nunca instrução.
func transcribedBody(marker, transcript string) string {
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		return marker
	}
	if marker = strings.TrimSpace(marker); marker == "" {
		return `"` + transcript + `"`
	}
	return marker + ` "` + transcript + `"`
}

// limparAudioOrfao remove áudio deixado por uma execução anterior. Erros são
// ignorados: é limpeza, não parte do fluxo.
func limparAudioOrfao(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// transcriber é a única coisa que o Client precisa do Whisper.
//
// É interface porque runTranscription é onde a composição do corpo, a
// publicação do texto e a escrita no banco se encontram — e nada disso precisa
// de ffmpeg instalado para ser testado.
type transcriber interface {
	Transcribe(ctx context.Context, audioPath string) (string, error)
}

// newTranscriber monta o transcritor a partir do config, ou devolve nil quando
// a transcrição está desligada.
//
// O retorno é a interface, e o nil é devolvido explicitamente: um
// (*transcribe.Transcriber)(nil) convertido para interface seria != nil, e
// todo `if c.transcriber == nil` passaria a mentir.
func newTranscriber(o transcribe.Options, enabled bool) (transcriber, error) {
	if !enabled {
		return nil, nil
	}
	t, err := transcribe.New(o)
	if err != nil {
		return nil, err
	}
	return t, nil
}
