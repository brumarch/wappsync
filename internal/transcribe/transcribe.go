// Package transcribe converte áudio em texto com Whisper rodando localmente.
//
// Local, e não uma API: o ponto do projeto é que o conteúdo das conversas não
// sai da máquina sem decisão explícita. Mandar o áudio para um serviço de
// transcrição desfaria isso justamente no tipo de mensagem mais íntimo que o
// WhatsApp tem.
//
// São dois processos externos, e nenhum é Go. O whisper.cpp lê um único
// formato de entrada — WAV PCM 16 bits, 16 kHz, mono — e nota de voz do
// WhatsApp é Opus dentro de OGG, então o ffmpeg converte antes.
package transcribe

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Options são os caminhos e parâmetros vindos do config.
type Options struct {
	Binary   string // whisper.cpp: "whisper-cli", ou "main" nas builds antigas
	Model    string // caminho do ggml-*.bin
	FFmpeg   string
	Language string // código ISO, ou "auto"
	Threads  int    // 0 deixa o whisper decidir
	Timeout  time.Duration
}

// Transcriber roda os dois binários. Criá-lo já resolve os caminhos, para que
// um whisper ausente falhe ao subir o programa e não horas depois, no primeiro
// áudio, dentro de um aviso de log que ninguém lê.
type Transcriber struct {
	opts    Options
	whisper string
	ffmpeg  string
}

func New(o Options) (*Transcriber, error) {
	whisper, err := exec.LookPath(o.Binary)
	if err != nil {
		return nil, fmt.Errorf("binário do whisper não encontrado no PATH (%q): %w", o.Binary, err)
	}
	ffmpeg, err := exec.LookPath(o.FFmpeg)
	if err != nil {
		return nil, fmt.Errorf("ffmpeg não encontrado no PATH (%q): %w", o.FFmpeg, err)
	}
	if _, err := os.Stat(o.Model); err != nil {
		return nil, fmt.Errorf("modelo do whisper não acessível em %s: %w", o.Model, err)
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Minute
	}
	return &Transcriber{opts: o, whisper: whisper, ffmpeg: ffmpeg}, nil
}

// ffmpegArgs converte qualquer entrada para o único formato que o whisper.cpp
// aceita. `-nostdin` importa: sem ele o ffmpeg tenta ler o terminal e trava um
// processo que ninguém está olhando.
func ffmpegArgs(in, out string) []string {
	return []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-i", in,
		"-ar", "16000", // 16 kHz
		"-ac", "1", // mono
		"-c:a", "pcm_s16le",
		"-f", "wav",
		out,
	}
}

// whisperArgs monta a linha do whisper.cpp. `-nt` tira os timestamps da saída:
// o que interessa aqui é o texto corrido, não a legenda.
func whisperArgs(o Options, wav string) []string {
	args := []string{"-m", o.Model, "-f", wav, "-nt"}
	if lang := strings.TrimSpace(o.Language); lang != "" {
		args = append(args, "-l", lang)
	}
	if o.Threads > 0 {
		args = append(args, "-t", strconv.Itoa(o.Threads))
	}
	return args
}

var (
	// Timestamp no formato do whisper, para o caso de uma build ignorar o -nt.
	timestampRe = regexp.MustCompile(`^\[\d{2}:\d{2}:\d{2}\.\d{3} --> \d{2}:\d{2}:\d{2}\.\d{3}\]\s*`)
	// Marcador de ausência de fala: "[BLANK_AUDIO]", "(música)", "[MUSIC]".
	// O whisper emite isso como se fosse conteúdo, e num resumo vira ruído que
	// parece informação.
	markerRe = regexp.MustCompile(`^[\[\(][^\]\)]*[\]\)]$`)
	// Ruído de diagnóstico que algumas builds mandam para stdout em vez de
	// stderr. Nenhuma fala humana começa assim.
	noiseRe = regexp.MustCompile(`^(whisper_|ggml_|main\s*:|system_info:)`)
	spaceRe = regexp.MustCompile(`\s+`)
)

// ParseOutput limpa a saída do whisper e devolve o texto corrido.
//
// Devolve "" quando não sobrou fala nenhuma — silêncio, só música, ou só
// diagnóstico. Quem chama trata isso como "não transcreveu", que é diferente de
// "transcreveu vazio": no primeiro caso o marcador original continua valendo.
func ParseOutput(raw []byte) string {
	var partes []string
	for _, linha := range strings.Split(string(raw), "\n") {
		linha = strings.TrimSpace(strings.TrimSuffix(linha, "\r"))
		linha = timestampRe.ReplaceAllString(linha, "")
		linha = strings.TrimSpace(linha)

		if linha == "" || markerRe.MatchString(linha) || noiseRe.MatchString(linha) {
			continue
		}
		partes = append(partes, linha)
	}
	return strings.TrimSpace(spaceRe.ReplaceAllString(strings.Join(partes, " "), " "))
}

// Transcribe converte o áudio em audioPath e devolve o texto.
//
// Texto vazio sem erro significa que não havia fala. É um resultado legítimo,
// não uma falha.
func (t *Transcriber) Transcribe(ctx context.Context, audioPath string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, t.opts.Timeout)
	defer cancel()

	dir, err := os.MkdirTemp("", "wappsync-wav-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	wav := filepath.Join(dir, "audio.wav")
	if _, err := run(ctx, t.ffmpeg, ffmpegArgs(audioPath, wav)); err != nil {
		return "", fmt.Errorf("convertendo o áudio: %w", err)
	}

	out, err := run(ctx, t.whisper, whisperArgs(t.opts, wav))
	if err != nil {
		return "", fmt.Errorf("transcrevendo: %w", err)
	}
	return ParseOutput(out), nil
}

// run executa um dos binários e devolve o stdout. O stderr entra no erro
// porque é lá que os dois explicam o que deu errado.
func run(ctx context.Context, bin string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Sem isto o timeout não termina nada. CommandContext mata só o processo
	// filho; um neto (o ffmpeg que o wrapper chamou, por exemplo) herda o pipe
	// de stdout e mantém Wait bloqueado até ELE terminar. WaitDelay põe teto
	// nisso: passado o prazo, os pipes são fechados à força e Wait devolve.
	cmd.WaitDelay = 2 * time.Second

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 500 {
			msg = msg[:500] + "…"
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s excedeu o tempo limite: %w", filepath.Base(bin), ctx.Err())
		}
		return nil, fmt.Errorf("%s falhou: %w: %s", filepath.Base(bin), err, msg)
	}
	return stdout.Bytes(), nil
}
