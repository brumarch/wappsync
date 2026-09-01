package transcribe

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A transcrição em si depende de dois binários que esta máquina não tem. O que
// dá para testar de verdade é o que o nosso código controla: a linha de comando
// que montamos e o que fazemos com a saída. Os dois binários viram scripts
// falsos no PATH, que é a mesma tática que o item B4 propõe para o rclone.

func TestParseOutput(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"texto simples", "chego às 19h\n", "chego às 19h"},
		{
			"várias linhas viram texto corrido",
			"bom dia\ntudo certo por aí?\n",
			"bom dia tudo certo por aí?",
		},
		{
			// Se uma build ignorar o -nt, o texto continua aproveitável.
			"timestamps são removidos",
			"[00:00:00.000 --> 00:00:02.480]   chego às 19h\n[00:00:02.480 --> 00:00:04.000]   avisa a Maria\n",
			"chego às 19h avisa a Maria",
		},
		{
			// Num resumo, "[BLANK_AUDIO]" parece informação e não é.
			"marcador de silêncio não é fala",
			"[BLANK_AUDIO]\n",
			"",
		},
		{"marcador de música não é fala", "(música)\n", ""},
		{
			"marcador no meio some, fala fica",
			"olha só\n[BLANK_AUDIO]\né isso\n",
			"olha só é isso",
		},
		{
			"diagnóstico do whisper não é fala",
			"whisper_init_from_file: loading model\nsystem_info: n_threads = 4\nchego às 19h\n",
			"chego às 19h",
		},
		{"espaços em excesso são colapsados", "  chego   às    19h  \n", "chego às 19h"},
		{"saída vazia", "", ""},
		{"só espaço", "   \n\n  \n", ""},
		{"CRLF", "chego às 19h\r\navisa a Maria\r\n", "chego às 19h avisa a Maria"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseOutput([]byte(tc.raw)); got != tc.want {
				t.Errorf("ParseOutput(%q) = %q, queria %q", tc.raw, got, tc.want)
			}
		})
	}
}

// O whisper.cpp lê um formato só. Errar qualquer um destes três parâmetros
// produz "não consegui abrir o arquivo" bem longe daqui.
func TestFFmpegArgsProduceWhisperInputFormat(t *testing.T) {
	args := ffmpegArgs("/tmp/nota.ogg", "/tmp/saida.wav")
	linha := strings.Join(args, " ")

	for _, obrigatorio := range []string{
		"-ar 16000", // 16 kHz
		"-ac 1",     // mono
		"pcm_s16le", // 16 bits
		"-nostdin",  // não trava esperando terminal
		"-i /tmp/nota.ogg",
	} {
		if !strings.Contains(linha, obrigatorio) {
			t.Errorf("faltou %q em: %s", obrigatorio, linha)
		}
	}
	if args[len(args)-1] != "/tmp/saida.wav" {
		t.Errorf("o destino tem que ser o último argumento: %v", args)
	}
}

func TestWhisperArgs(t *testing.T) {
	base := Options{Model: "/m/ggml.bin", Language: "pt", Threads: 4}
	linha := strings.Join(whisperArgs(base, "/tmp/a.wav"), " ")

	for _, obrigatorio := range []string{"-m /m/ggml.bin", "-f /tmp/a.wav", "-nt", "-l pt", "-t 4"} {
		if !strings.Contains(linha, obrigatorio) {
			t.Errorf("faltou %q em: %s", obrigatorio, linha)
		}
	}

	// Threads 0 significa "deixa o whisper decidir": passar -t 0 travaria.
	semThreads := strings.Join(whisperArgs(Options{Model: "/m/g.bin", Language: "auto"}, "/tmp/a.wav"), " ")
	if strings.Contains(semThreads, "-t ") {
		t.Errorf("threads 0 não pode virar -t: %s", semThreads)
	}
}

// fakeBins põe um ffmpeg e um whisper falsos no PATH e devolve as Options que
// apontam para eles. saida é o que o whisper falso imprime; codigo é a saída
// dele (0 = sucesso).
func fakeBins(t *testing.T, saida string, codigo int, extra string) (Options, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("os binários falsos são scripts de shell")
	}

	dir := t.TempDir()
	escrever := func(nome, corpo string) {
		p := filepath.Join(dir, nome)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+corpo), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// O ffmpeg falso registra os argumentos e cria o WAV que o whisper espera:
	// o arquivo precisa existir para o encadeamento ser real.
	escrever("ffmpeg", `echo "$@" > `+filepath.Join(dir, "ffmpeg.args")+`
for a in "$@"; do last="$a"; done
printf 'RIFF' > "$last"
`)
	escrever("whisper-cli", `echo "$@" > `+filepath.Join(dir, "whisper.args")+`
`+extra+`
cat <<'FIM'
`+saida+`
FIM
exit `+strconv.Itoa(codigo)+`
`)

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	modelo := filepath.Join(dir, "ggml-fake.bin")
	if err := os.WriteFile(modelo, []byte("modelo"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Options{
		Binary: "whisper-cli", FFmpeg: "ffmpeg", Model: modelo,
		Language: "pt", Timeout: 10 * time.Second,
	}, dir
}

func TestTranscribeRunsBothBinaries(t *testing.T) {
	opts, dir := fakeBins(t, "chego às 19h, avisa a Maria", 0, "")

	tr, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}

	audio := filepath.Join(t.TempDir(), "nota.ogg")
	if err := os.WriteFile(audio, []byte("opus"), 0o600); err != nil {
		t.Fatal(err)
	}

	texto, err := tr.Transcribe(context.Background(), audio)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if texto != "chego às 19h, avisa a Maria" {
		t.Errorf("texto = %q", texto)
	}

	// O ffmpeg recebeu o áudio original; o whisper recebeu o WAV convertido,
	// e não o .ogg — inverter isso é o erro clássico deste encadeamento.
	ffArgs := ler(t, filepath.Join(dir, "ffmpeg.args"))
	wArgs := ler(t, filepath.Join(dir, "whisper.args"))

	if !strings.Contains(ffArgs, audio) {
		t.Errorf("o ffmpeg não recebeu o áudio original: %s", ffArgs)
	}
	if !strings.Contains(wArgs, ".wav") || strings.Contains(wArgs, ".ogg") {
		t.Errorf("o whisper não recebeu o WAV convertido: %s", wArgs)
	}
	if !strings.Contains(wArgs, opts.Model) {
		t.Errorf("o whisper não recebeu o modelo: %s", wArgs)
	}
}

// Silêncio é resultado legítimo, não erro: quem chama mantém o marcador.
func TestTranscribeReturnsEmptyOnSilence(t *testing.T) {
	opts, _ := fakeBins(t, "[BLANK_AUDIO]", 0, "")
	tr, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}

	audio := filepath.Join(t.TempDir(), "nota.ogg")
	if err := os.WriteFile(audio, []byte("opus"), 0o600); err != nil {
		t.Fatal(err)
	}

	texto, err := tr.Transcribe(context.Background(), audio)
	if err != nil {
		t.Fatalf("silêncio virou erro: %v", err)
	}
	if texto != "" {
		t.Errorf("texto = %q, queria vazio", texto)
	}
}

// O stderr do whisper é onde ele explica o que deu errado (modelo corrompido,
// arquivo ilegível). Engolir isso deixaria o usuário sem nada para agir.
func TestTranscribeSurfacesStderr(t *testing.T) {
	opts, _ := fakeBins(t, "", 1, "echo 'error: failed to load model' >&2")
	tr, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}

	audio := filepath.Join(t.TempDir(), "nota.ogg")
	if err := os.WriteFile(audio, []byte("opus"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = tr.Transcribe(context.Background(), audio)
	if err == nil {
		t.Fatal("saída 1 do whisper não virou erro")
	}
	if !strings.Contains(err.Error(), "failed to load model") {
		t.Errorf("o erro não carrega o stderr do whisper: %v", err)
	}
}

// Whisper travado não pode segurar a fila para sempre.
func TestTranscribeRespectsTimeout(t *testing.T) {
	opts, _ := fakeBins(t, "tarde demais", 0, "sleep 30")
	opts.Timeout = 200 * time.Millisecond

	tr, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}

	audio := filepath.Join(t.TempDir(), "nota.ogg")
	if err := os.WriteFile(audio, []byte("opus"), 0o600); err != nil {
		t.Fatal(err)
	}

	inicio := time.Now()
	if _, err := tr.Transcribe(context.Background(), audio); err == nil {
		t.Fatal("o timeout não interrompeu o whisper")
	}
	if d := time.Since(inicio); d > 10*time.Second {
		t.Errorf("Transcribe só voltou depois de %s", d)
	}
}

func TestNewReportsMissingDependencies(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("depende de PATH de shell")
	}
	t.Setenv("PATH", t.TempDir())

	_, err := New(Options{Binary: "whisper-nao-existe", FFmpeg: "ffmpeg", Model: "/nao/existe"})
	if err == nil {
		t.Fatal("New aceitou um whisper que não existe")
	}
	if !strings.Contains(err.Error(), "whisper-nao-existe") {
		t.Errorf("o erro não diz qual binário faltou: %v", err)
	}
}

func ler(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("lendo %s: %v", p, err)
	}
	return string(b)
}
