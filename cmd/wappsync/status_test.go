package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// capturaSaida executa fn com o stdout redirecionado e devolve o que foi escrito.
func capturaSaida(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	fn()

	w.Close()
	os.Stdout = orig
	return <-done
}

// Trava. `status` é o comando de diagnóstico: quando a sessão não abre, ele tem
// que DIZER por quê. Engolir o erro — que era o comportamento antigo — deixava
// o usuário com um status aparentemente saudável e um `run` que não sobe.
//
// O gatilho aqui é transcrição ligada com o whisper ausente, que é o motivo
// mais comum de wa.New falhar depois de B1. A máquina de testes não tem whisper
// nem ffmpeg, então a falha é real, não simulada.
func TestStatusReportsSessionFailure(t *testing.T) {
	root := t.TempDir()
	modelo := filepath.Join(root, "ggml.bin")
	if err := os.WriteFile(modelo, []byte("modelo"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := newMachine(t, root, "maquina-a",
		"[transcribe]\nenabled = true\nbinary = \"whisper-que-nao-existe\"\nmodel = "+
			"'"+filepath.ToSlash(modelo)+"'\n")

	saida := capturaSaida(t, func() {
		if err := cmdStatus(context.Background(), m.cfg, false); err != nil {
			t.Errorf("cmdStatus devolveu erro: %v", err)
		}
	})

	if !strings.Contains(saida, "whisper-que-nao-existe") {
		t.Errorf("o status não disse qual binário faltou:\n%s", saida)
	}
	// E continua útil: o resto do diagnóstico não pode sumir junto.
	if !strings.Contains(saida, "Banco local:") {
		t.Errorf("o status parou no erro em vez de seguir:\n%s", saida)
	}
}
