// Command wappsync captura o histórico do WhatsApp em uma máquina pessoal e
// publica uma janela recente numa pasta privada em nuvem, para que agentes de
// IA leiam sem receber acesso à conta.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bmar13/wapp-summarizer/internal/config"
	"github.com/bmar13/wapp-summarizer/internal/export"
	"github.com/bmar13/wapp-summarizer/internal/merge"
	"github.com/bmar13/wapp-summarizer/internal/remote"
	"github.com/bmar13/wapp-summarizer/internal/store"
	"github.com/bmar13/wapp-summarizer/internal/wa"
)

const usage = `wappsync — ponte entre o seu WhatsApp e uma pasta privada em nuvem.

Uso:
  wappsync <comando> [flags]

Comandos:
  init      Cria um config.toml comentado no diretório atual.
  login     Pareia ESTA máquina como um novo aparelho conectado (QR ou código).
  run       Captura mensagens e publica a cada intervalo. É o modo normal.
  export    Gera e publica uma vez, a partir do que já está no banco local.
  merge     Só consolida os shards das máquinas em latest/.
  status    Mostra o estado local e o que está publicado.
  groups    Lista os grupos da conta (útil para preencher os filtros).
  logout    Desvincula esta máquina da conta do WhatsApp.

Flags globais:
  -config <arquivo>   Padrão: ./config.toml, depois ~/.wapp-summarizer/config.toml
  -v                  Log detalhado do whatsmeow
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "erro: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		return errors.New("nenhum comando informado")
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("config", "", "caminho do config.toml")
	verbose := fs.Bool("v", false, "log detalhado")
	phone := fs.String("phone", "", "login: parear por número (ex: +5511999999999) em vez de QR")
	fullHistory := fs.Bool("full-history", false, "login: pedir histórico completo (~1 ano) em vez do recente")
	once := fs.Bool("once", false, "run: um único ciclo e sai")
	yes := fs.Bool("yes", false, "logout: não perguntar confirmação")

	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	case "init":
		_ = fs.Parse(args)
		path := *cfgPath
		if path == "" {
			path = "config.toml"
		}
		if err := config.WriteExample(path); err != nil {
			return err
		}
		fmt.Printf("Criado %s\n\nEdite pelo menos:\n"+
			"  - [remote.folder].path  (pasta sincronizada pelo Drive/OneDrive/Dropbox)\n"+
			"  - host_id               (um nome curto para esta máquina)\n\n"+
			"Depois rode: wappsync login\n", path)
		return nil
	case "login", "run", "export", "merge", "status", "groups", "logout":
	default:
		fmt.Print(usage)
		return fmt.Errorf("comando desconhecido: %q", cmd)
	}

	_ = fs.Parse(args)

	resolved, err := resolveConfigPath(*cfgPath)
	if err != nil {
		return err
	}
	cfg, err := config.Load(resolved)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "login":
		return cmdLogin(ctx, cfg, *verbose, *phone, *fullHistory)
	case "run":
		return cmdRun(ctx, cfg, *verbose, *once)
	case "export":
		return cmdExport(ctx, cfg)
	case "merge":
		return cmdMerge(ctx, cfg)
	case "status":
		return cmdStatus(ctx, cfg, *verbose)
	case "groups":
		return cmdGroups(ctx, cfg, *verbose)
	case "logout":
		return cmdLogout(ctx, cfg, *verbose, *yes)
	}
	return nil
}

func resolveConfigPath(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if _, err := os.Stat("config.toml"); err == nil {
		return "config.toml", nil
	}
	home, err := os.UserHomeDir()
	if err == nil {
		fallback := filepath.Join(home, ".wapp-summarizer", "config.toml")
		if _, err := os.Stat(fallback); err == nil {
			return fallback, nil
		}
	}
	return "", errors.New("config.toml não encontrado; rode `wappsync init` para criar um")
}

func openDB(cfg *config.Config) (*store.DB, error) {
	if err := os.MkdirAll(cfg.Paths.DataDir, 0o700); err != nil {
		return nil, err
	}
	return store.Open(cfg.MessagesDBPath())
}

// --------------------------------------------------------------- comandos ---

func cmdLogin(ctx context.Context, cfg *config.Config, verbose bool, phone string, fullHistory bool) error {
	db, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	if fullHistory {
		wa.SetFullSync(true)
		fmt.Println("Pedindo histórico completo — o primeiro sync pode demorar e ocupar bastante espaço.")
	}

	client, err := wa.New(ctx, cfg, db, verbose)
	if err != nil {
		return err
	}
	defer client.Close()

	if err := client.Login(ctx, phone); err != nil {
		return err
	}

	fmt.Println("\nAguardando o histórico inicial (até 2 minutos)...")
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	<-waitCtx.Done()

	st, _ := db.Stats(ctx, time.Now().Add(-cfg.Window()))
	fmt.Printf("Recebidas %d mensagens (%d na janela de %d dia(s)).\n", st.Messages, st.InWindow, cfg.WindowDays)
	fmt.Println("\nPronto. Agora rode: wappsync run")
	return nil
}

func cmdLogout(ctx context.Context, cfg *config.Config, verbose bool, yes bool) error {
	db, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	client, err := wa.New(ctx, cfg, db, verbose)
	if err != nil {
		return err
	}
	defer client.Close()

	if !client.IsPaired() {
		fmt.Println("Esta máquina não está pareada.")
		return nil
	}
	if !yes {
		fmt.Printf("Desvincular %s desta máquina? O banco local de mensagens é preservado. [s/N]: ", client.JID())
		var answer string
		fmt.Scanln(&answer)
		if answer != "s" && answer != "S" {
			return errors.New("cancelado")
		}
	}
	if err := client.Connect(ctx); err != nil {
		return err
	}
	if err := client.Logout(ctx); err != nil {
		return err
	}
	fmt.Println("Desvinculado.")
	return nil
}

func cmdRun(ctx context.Context, cfg *config.Config, verbose, once bool) error {
	db, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	client, err := wa.New(ctx, cfg, db, verbose)
	if err != nil {
		return err
	}
	defer client.Close()

	if err := client.Connect(ctx); err != nil {
		return err
	}
	fmt.Printf("Conectado como %s (máquina %q)\n", client.JID(), cfg.HostID)
	fmt.Printf("Janela: %d dia(s) · publicando a cada %d min · destino: %s\n",
		cfg.WindowDays, cfg.Export.IntervalMinutes, describeRemote(cfg))

	// Dá um tempo para o history sync inicial chegar antes do primeiro ciclo.
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(20 * time.Second):
	}

	for {
		if err := cycle(ctx, cfg, db, client.JID(), time.Now()); err != nil {
			fmt.Fprintf(os.Stderr, "ciclo falhou: %v\n", err)
		}
		if once {
			return nil
		}
		select {
		case <-ctx.Done():
			fmt.Println("\nEncerrando; publicando um último ciclo...")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if err := cycle(shutdownCtx, cfg, db, client.JID(), time.Now()); err != nil {
				fmt.Fprintf(os.Stderr, "ciclo final falhou: %v\n", err)
			}
			return nil
		case <-time.After(cfg.Interval()):
		}
	}
}

func cmdExport(ctx context.Context, cfg *config.Config) error {
	db, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	return cycle(ctx, cfg, db, "", time.Now())
}

func cmdMerge(ctx context.Context, cfg *config.Config) error {
	be, err := remote.New(cfg)
	if err != nil {
		return err
	}
	if be == nil {
		return errors.New(`remote.backend = "none": nada para consolidar`)
	}
	now := time.Now()
	res, err := merge.Consolidate(ctx, cfg, be, now.Add(-cfg.Window()), now)
	if err != nil {
		return err
	}
	reportMerge(res)
	return nil
}

func cmdStatus(ctx context.Context, cfg *config.Config, verbose bool) error {
	db, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	from := time.Now().Add(-cfg.Window())
	st, err := db.Stats(ctx, from)
	if err != nil {
		return err
	}

	fmt.Printf("Config:      %s\n", cfg.SourcePath)
	fmt.Printf("Máquina:     %s\n", cfg.HostID)
	fmt.Printf("Dados:       %s\n", cfg.Paths.DataDir)
	fmt.Printf("Destino:     %s\n", describeRemote(cfg))
	fmt.Printf("Janela:      %d dia(s)  ·  retenção local: %d dia(s)\n\n", cfg.WindowDays, cfg.RetentionDays)

	fmt.Printf("Banco local: %d mensagens em %d conversas\n", st.Messages, st.Chats)
	if !st.OldestTS.IsZero() {
		fmt.Printf("             de %s até %s\n",
			st.OldestTS.Format("2006-01-02 15:04"), st.NewestTS.Format("2006-01-02 15:04"))
	}
	fmt.Printf("Na janela:   %d mensagens\n\n", st.InWindow)

	client, err := wa.New(ctx, cfg, db, verbose)
	if err == nil {
		defer client.Close()
		if client.IsPaired() {
			fmt.Printf("Pareamento:  %s\n", client.JID())
		} else {
			fmt.Printf("Pareamento:  NÃO pareado — rode `wappsync login`\n")
		}
	}

	be, err := remote.New(cfg)
	if err != nil || be == nil {
		return nil
	}
	data, err := be.Get(ctx, "latest/index.json")
	if err != nil {
		fmt.Println("Publicado:   nada ainda em latest/")
		return nil
	}
	var idx export.Index
	if err := json.Unmarshal(data, &idx); err != nil {
		fmt.Printf("Publicado:   index.json ilegível (%v)\n", err)
		return nil
	}
	fmt.Printf("\nPublicado em latest/:\n")
	fmt.Printf("  %d mensagens · %d conversas · consolidado por %q em %s\n",
		idx.Messages, len(idx.Chats), idx.GeneratedBy, idx.GeneratedAt.Local().Format("2006-01-02 15:04"))
	for _, s := range idx.Shards {
		age := time.Since(s.GeneratedAt).Round(time.Minute)
		fmt.Printf("  shard %-16s %6d msgs · atualizado há %s\n", s.Host, s.Messages, age)
	}
	return nil
}

func cmdGroups(ctx context.Context, cfg *config.Config, verbose bool) error {
	db, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	client, err := wa.New(ctx, cfg, db, verbose)
	if err != nil {
		return err
	}
	defer client.Close()

	if err := client.Connect(ctx); err != nil {
		return err
	}
	groups, err := client.Groups(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%d grupo(s):\n\n", len(groups))
	for _, g := range groups {
		fmt.Printf("  %-45s  %3d membros  %s\n", g.Name, g.ParticipantCount, g.JID.String())
	}
	fmt.Println("\nUse os nomes (ou JIDs) em [filter].include_only / [filter].exclude no config.toml")
	return nil
}

// ------------------------------------------------------------------ ciclo ---

// cycle é uma rodada completa: lê o banco, gera os artefatos, salva uma cópia
// local, publica o shard desta máquina e consolida.
//
// now entra por parâmetro em vez de vir de time.Now(): é o que torna um ciclo
// inteiro reproduzível nos testes de integração.
func cycle(ctx context.Context, cfg *config.Config, db *store.DB, deviceJID string, now time.Time) error {
	from := now.Add(-cfg.Window())

	msgs, err := db.Since(ctx, from)
	if err != nil {
		return fmt.Errorf("lendo mensagens: %w", err)
	}
	chats, err := db.Chats(ctx)
	if err != nil {
		return fmt.Errorf("lendo conversas: %w", err)
	}

	recs := export.Build(cfg, msgs, chats, from)
	idx := export.BuildIndex(cfg, recs, from, now, cfg.HostID)

	if err := writeLocal(cfg, idx, recs); err != nil {
		return err
	}

	if n, err := db.Prune(ctx, now.Add(-cfg.Retention())); err != nil {
		fmt.Fprintf(os.Stderr, "aviso: prune falhou: %v\n", err)
	} else if n > 0 {
		fmt.Printf("[%s] retenção: %d mensagens antigas removidas do banco local\n",
			now.Format("15:04"), n)
	}

	be, err := remote.New(cfg)
	if err != nil {
		return err
	}
	if be == nil {
		fmt.Printf("[%s] %d mensagens em %d conversas → %s (backend none)\n",
			now.Format("15:04"), len(recs), len(idx.Chats), cfg.OutDir())
		return nil
	}

	meta, err := merge.PublishShard(ctx, cfg, be, recs, deviceJID, from, now)
	if err != nil {
		return err
	}
	fmt.Printf("[%s] shard %q publicado: %d mensagens em %d conversas\n",
		now.Format("15:04"), cfg.HostID, meta.Messages, len(idx.Chats))

	if !cfg.Merge.Enabled {
		return nil
	}
	res, err := merge.Consolidate(ctx, cfg, be, from, now)
	if err != nil {
		return err
	}
	reportMerge(res)
	return nil
}

func writeLocal(cfg *config.Config, idx export.Index, recs []export.Record) error {
	dir := cfg.OutDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if cfg.HasFormat("jsonl") {
		data, err := export.MarshalJSONL(recs)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "messages.jsonl"), data, 0o600); err != nil {
			return err
		}
	}
	if cfg.HasFormat("markdown") {
		md := export.MarshalMarkdown(idx, recs, time.Local)
		if err := os.WriteFile(filepath.Join(dir, "digest.md"), md, 0o600); err != nil {
			return err
		}
	}
	data, err := export.MarshalIndex(idx)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "index.json"), data, 0o600)
}

func reportMerge(res *merge.Result) {
	if res.Skipped {
		fmt.Printf("         consolidação pulada: %s\n", res.Reason)
		return
	}
	fmt.Printf("         latest/ atualizado: %d mensagens em %d conversas (%d máquina(s))\n",
		res.Messages, res.Chats, len(res.Shards))
}

func describeRemote(cfg *config.Config) string {
	be, err := remote.New(cfg)
	if err != nil {
		return "inválido: " + err.Error()
	}
	if be == nil {
		return "nenhum (backend none)"
	}
	return be.Describe()
}
