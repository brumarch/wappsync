package remote

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bmar13/wapp-summarizer/internal/config"
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
