package config

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// O .gitignore é a única coisa que separa o config real do repositório, e ele
// já falhou uma vez: a regra era `config.toml`, o editor deixou um
// `config.toml~`, e o arquivo com caminhos e nomes de conversas foi versionado.
//
// O teste chama o próprio git em vez de conferir o texto do arquivo. Padrão de
// .gitignore não se lê de cabeça: a ordem importa, negação anula regra anterior,
// e é justamente aí que mora o erro.

func moduleRootDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod não encontrado")
		}
		dir = parent
	}
}

// ignorado devolve o que o git de verdade decide sobre um caminho.
func ignorado(t *testing.T, root, path string) bool {
	t.Helper()
	cmd := exec.Command("git", "check-ignore", "-q", "--no-index", path)
	cmd.Dir = root
	err := cmd.Run()
	if err == nil {
		return true
	}
	// check-ignore sai com 1 quando o caminho NÃO é ignorado; qualquer outro
	// código é falha de verdade e não pode virar "não ignorado" silencioso.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false
	}
	t.Fatalf("git check-ignore %s: %v", path, err)
	return false
}

func TestGitignoreCobreOConfigEOsBackups(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git não está no PATH")
	}
	root := moduleRootDir(t)
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		t.Skip("não é uma cópia de trabalho do git")
	}

	// Nada aqui pode ser versionado: são dados da máquina de quem roda.
	proibidos := []string{
		"config.toml",
		"config.toml~",     // o caso que de fato aconteceu
		"config.toml.bak",  // "salvar antes de mexer"
		"config.toml.orig", // sobra de merge
		"config.local.toml",
		"anotacoes~",
		"rascunho.bak",
		"messages.db",
		"session.db",
	}
	for _, p := range proibidos {
		if !ignorado(t, root, p) {
			t.Errorf("%q NÃO está ignorado — um arquivo desses no repositório vaza "+
				"caminhos locais e nomes de conversas", p)
		}
	}

	// E nada disso pode ser ignorado por acidente: uma regra ampla demais some
	// com fixture nova sem avisar ninguém.
	obrigatorios := []string{
		// Este é o caso que a negação `!**/testdata/**` existe para proteger:
		// `*.jsonl` engoliria a fixture. Não existe arquivo assim hoje, e é de
		// propósito que o caminho seja hipotético — a regra tem que valer ANTES
		// de alguém criar a fixture, senão ela some sem aviso.
		"internal/export/testdata/messages.jsonl",
		"internal/config/example.toml",
		"internal/export/testdata/digest.golden.md",
		"internal/export/testdata/guide.root.golden.md",
		"README.md",
		"cmd/wappsync/main.go",
	}
	for _, p := range obrigatorios {
		if ignorado(t, root, p) {
			t.Errorf("%q está sendo ignorado; ele TEM que ser versionado", p)
		}
	}
}
