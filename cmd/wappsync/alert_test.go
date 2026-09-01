package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bmar13/wapp-summarizer/internal/config"
	"github.com/bmar13/wapp-summarizer/internal/export"
)

// O código de saída é o que um supervisor enxerga. Confundir "o pareamento
// caiu" com uma falha qualquer faz o systemd/Docker reiniciar em laço uma
// máquina que só volta com `wappsync login`.
func TestExitCodeForSeparaPareamentoDeOutrasFalhas(t *testing.T) {
	if got := exitCodeFor(errors.New("config.toml não encontrado")); got != 1 {
		t.Errorf("falha comum saiu com %d, queria 1", got)
	}
	lost := fmt.Errorf("%w: desvinculado", errSessionLost)
	if got := exitCodeFor(lost); got != exitSessionLost {
		t.Errorf("perda de pareamento saiu com %d, queria %d", got, exitSessionLost)
	}
	// Precisa sobreviver a mais um nível de embrulho: o erro sobe por cmdRun.
	if got := exitCodeFor(fmt.Errorf("run: %w", lost)); got != exitSessionLost {
		t.Errorf("perda de pareamento embrulhada saiu com %d, queria %d", got, exitSessionLost)
	}
}

// publishAlert é a costura entre o texto do alerta e o backend. Duas máquinas
// caídas ao mesmo tempo precisam terminar com DOIS arquivos, não com uma
// sobrescrevendo a outra.
func TestPublishAlertEscreveUmArquivoPorMaquina(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	now := time.Now()

	for _, name := range []string{"casa", "trabalho"} {
		m := newMachine(t, root, name, "")
		_, err := publishAlert(ctx, m.cfg, export.Alert{
			Host:   m.cfg.HostID,
			Reason: "o WhatsApp desvinculou este aparelho",
			Fix:    "rode `wappsync login` nesta máquina",
			LostAt: now,
			At:     now.Add(time.Minute),
		})
		if err != nil {
			t.Fatalf("publicando alerta de %s: %v", name, err)
		}
	}

	drive := filepath.Join(root, "drive", "wapp", export.AlertDir)
	entries, err := os.ReadDir(drive)
	if err != nil {
		t.Fatalf("lendo %s: %v", drive, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 {
		t.Fatalf("alertas publicados = %v, queria um por máquina", names)
	}

	body, err := os.ReadFile(filepath.Join(drive, "casa.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "`casa`") {
		t.Error("o alerta de casa não identifica a máquina")
	}
	if strings.Contains(string(body), "trabalho") {
		t.Error("o alerta de casa foi sobrescrito pelo de trabalho")
	}
}

// A cópia local existe para o backend "none", em que não há nuvem nenhuma —
// e é a única coisa que resta se o destino estiver fora do ar na hora da queda.
func TestPublishAlertGravaCopiaLocalMesmoSemBackend(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data-solo")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "host_id = \"solo\"\n[paths]\ndata_dir = '" +
		filepath.ToSlash(dataDir) + "'\n[remote]\nbackend = \"none\"\n"
	cfgPath := filepath.Join(root, "solo.toml")
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	path, err := publishAlert(context.Background(), cfg, export.Alert{
		Host: cfg.HostID, Reason: "motivo", Fix: "conserte", LostAt: time.Now(), At: time.Now(),
	})
	if err != nil {
		t.Fatalf("publicando alerta: %v", err)
	}
	if want := filepath.Join(cfg.OutDir(), "ALERTA.md"); path != want {
		t.Errorf("caminho devolvido = %q, queria %q", path, want)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("cópia local não foi escrita: %v", err)
	}
}

// Trava. Voltar a capturar apaga o aviso DESTA máquina e só o dela. O alerta de
// outra continua valendo: quem caiu foi ela, e daqui não há como saber se
// voltou. Apagar o alheio seria afirmar, sem base, que o buraco no histórico
// dela acabou.
func TestClearAlertSoRemoveODaPropriaMaquina(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	now := time.Now()

	casa := newMachine(t, root, "casa", "")
	trabalho := newMachine(t, root, "trabalho", "")

	for _, m := range []*machine{casa, trabalho} {
		if _, err := publishAlert(ctx, m.cfg, export.Alert{
			Host: m.cfg.HostID, Reason: "desvinculado", Fix: "rode login",
			LostAt: now, At: now,
		}); err != nil {
			t.Fatalf("publicando alerta de %s: %v", m.name, err)
		}
	}

	if err := clearAlert(ctx, casa.cfg); err != nil {
		t.Fatalf("clearAlert: %v", err)
	}

	drive := filepath.Join(root, "drive", "wapp", export.AlertDir)
	if _, err := os.Stat(filepath.Join(drive, "casa.md")); !os.IsNotExist(err) {
		t.Errorf("o alerta da própria máquina continua publicado (err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(drive, "trabalho.md")); err != nil {
		t.Errorf("o alerta da outra máquina foi apagado: %v", err)
	}

	// A cópia local também sai: senão o `status` e a pasta out/ seguem
	// mostrando um aviso que já não vale.
	if _, err := os.Stat(localAlertPath(casa.cfg)); !os.IsNotExist(err) {
		t.Errorf("a cópia local sobreviveu (err = %v)", err)
	}
	if _, err := os.Stat(localAlertPath(trabalho.cfg)); err != nil {
		t.Errorf("a cópia local da outra máquina foi apagada: %v", err)
	}
}

// Limpar sem nada para limpar é o caso normal: a máquina nunca caiu. Não pode
// virar erro, senão todo primeiro ciclo imprimiria um aviso falso.
func TestClearAlertSemAlertaNaoEErro(t *testing.T) {
	root := t.TempDir()
	m := newMachine(t, root, "casa", "")

	for i := 0; i < 2; i++ {
		if err := clearAlert(context.Background(), m.cfg); err != nil {
			t.Fatalf("clearAlert %d: %v", i, err)
		}
	}
}
