package remote

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/brumarch/wappsync/internal/config"
)

// Este arquivo cobre apenas Delete, que é a operação destrutiva introduzida
// junto com os anexos. A cobertura completa de `remote` (escrita atômica, List,
// e o rclone com um binário falso no PATH) é o item B4 do plano.

func folderBackend(t *testing.T) (Backend, string) {
	t.Helper()
	root := t.TempDir()
	be, err := New(&config.Config{
		Remote: config.Remote{
			Backend: "folder",
			Prefix:  "wapp",
			Folder:  config.FolderRemote{Path: root},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return be, filepath.Join(root, "wapp")
}

func TestFolderDelete(t *testing.T) {
	ctx := context.Background()
	be, root := folderBackend(t)

	if err := be.Put(ctx, "media/maquina-a/abc.pdf", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := be.Delete(ctx, "media/maquina-a/abc.pdf"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := be.Get(ctx, "media/maquina-a/abc.pdf"); !errors.Is(err, ErrNotExist) {
		t.Errorf("arquivo continua legível (err = %v)", err)
	}

	// Apagar o que não existe não é erro: a poda roda a cada ciclo e não pode
	// falhar porque outra máquina já removeu o mesmo arquivo.
	if err := be.Delete(ctx, "media/maquina-a/abc.pdf"); err != nil {
		t.Errorf("Delete de arquivo ausente devolveu erro: %v", err)
	}

	if _, err := os.Stat(root); err != nil {
		t.Errorf("a raiz do destino sumiu: %v", err)
	}
}

// Trava. Delete é a única operação destrutiva desta interface, e os caminhos
// que chegam nela derivam de arquivos lidos da pasta compartilhada. Um ".."
// apagaria o consolidado — ou algo fora do destino inteiro.
func TestDeleteRefusesEscapingPaths(t *testing.T) {
	ctx := context.Background()
	be, root := folderBackend(t)

	vizinho := filepath.Join(filepath.Dir(root), "nao-mexa.txt")
	if err := os.WriteFile(vizinho, []byte("intacto"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := be.Put(ctx, "latest/index.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}

	hostis := []string{
		"../nao-mexa.txt",
		"media/maquina-a/../../latest/index.json",
		"media/../../nao-mexa.txt",
		"/etc/passwd",
		`media\maquina-a\abc.pdf`,
		"",
	}
	for _, rel := range hostis {
		if err := be.Delete(ctx, rel); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("Delete(%q) = %v, queria ErrUnsafePath", rel, err)
		}
	}

	if _, err := os.Stat(vizinho); err != nil {
		t.Errorf("um arquivo fora do destino foi apagado: %v", err)
	}
	if _, err := be.Get(ctx, "latest/index.json"); err != nil {
		t.Errorf("o consolidado foi apagado: %v", err)
	}
}

// O rclone valida o caminho antes de montar a linha de comando: um ".." não
// pode nem chegar a virar argumento de `rclone deletefile`.
func TestRcloneDeleteRefusesEscapingPaths(t *testing.T) {
	r := &rclone{bin: "/bin/false", root: "gdrive:wapp"}
	if err := r.Delete(context.Background(), "media/a/../../latest/index.json"); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("err = %v, queria ErrUnsafePath", err)
	}
}

// rclone travado não pode segurar o ciclo de publicação para sempre.
//
// O script falso sobe um `sleep` em segundo plano, que herda o stdout: é o neto
// que mantém o Wait preso depois de o filho ter sido morto pelo contexto. Sem
// cmd.WaitDelay, run só volta quando o sleep termina — 30 segundos aqui, cinco
// minutos de timeout mais o que o neto quiser em produção.
func TestRcloneTimeoutIsNotHeldByGrandchild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("o rclone falso é um script de shell")
	}
	bin := filepath.Join(t.TempDir(), "rclone")
	script := "#!/bin/sh\nsleep 30 &\nwait\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	be, err := New(&config.Config{Remote: config.Remote{
		Backend: "rclone",
		Rclone:  config.RcloneRemote{Binary: bin, Remote: "falso:pasta"},
	}})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	inicio := time.Now()
	if _, err := be.Get(ctx, "latest/index.json"); err == nil {
		t.Fatal("Get devolveu sucesso de um rclone que nunca respondeu")
	}
	if d := time.Since(inicio); d > 10*time.Second {
		t.Errorf("Get só voltou depois de %s: o neto segurou o Wait", d)
	}
}
