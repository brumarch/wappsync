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
	"strings"
	"syscall"
	"time"

	"github.com/brumarch/wappsync/internal/config"
	"github.com/brumarch/wappsync/internal/export"
	"github.com/brumarch/wappsync/internal/merge"
	"github.com/brumarch/wappsync/internal/remote"
	"github.com/brumarch/wappsync/internal/store"
	"github.com/brumarch/wappsync/internal/wa"
)

const usage = `wappsync — ponte entre o seu WhatsApp e uma pasta privada em nuvem.

Uso:
  wappsync <comando> [flags]

Comandos:
  init      Cria um config.toml comentado no diretório atual.
  login     Pareia ESTA máquina como um novo aparelho conectado (QR ou código).
  setup     Questionário: quais conversas exportar, quais anexos baixar, se
            transcreve áudio. Grava [filter], [media] e [transcribe] no config.
  run       Captura mensagens e publica a cada intervalo. É o modo normal.
  export    Gera e publica uma vez, a partir do que já está no banco local.
  merge     Só consolida os shards das máquinas em latest/.
  status    Mostra o estado local e o que está publicado.
  groups    Lista os grupos da conta (útil para preencher os filtros).
  logout    Desvincula esta máquina da conta do WhatsApp.

Flags globais:
  -config <arquivo>   Padrão: ./config.toml, depois ~/.wappsync/config.toml
  -v                  Log detalhado do whatsmeow
`

// exitSessionLost é o código de saída quando o pareamento com o WhatsApp cai.
//
// Distinto de 1 de propósito: reiniciar não resolve — é preciso rodar
// `wappsync login` na máquina. Um supervisor configurado para reiniciar em
// qualquer falha (Docker `restart: on-failure`, systemd `Restart=always`)
// entraria em laço; com um código próprio dá para excluir só este caso
// (systemd: `RestartPreventExitStatus=3`).
const exitSessionLost = 3

// errSessionLost marca o erro que vira exitSessionLost.
var errSessionLost = errors.New("captura interrompida")

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "erro: %v\n", err)
		os.Exit(exitCodeFor(err))
	}
}

// exitCodeFor separa "o pareamento caiu" de qualquer outra falha.
func exitCodeFor(err error) int {
	if errors.Is(err, errSessionLost) {
		return exitSessionLost
	}
	return 1
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
	case "login", "setup", "run", "export", "merge", "status", "groups", "logout":
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
	case "setup":
		return cmdSetup(ctx, cfg, *verbose)
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
		fallback := filepath.Join(home, ".wappsync", "config.toml")
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
	// O JID é lido uma vez: ao desvincular, o whatsmeow zera o Store.ID, e o
	// ciclo final publicaria um shard sem device_jid.
	deviceJID := client.JID()
	fmt.Printf("Conectado como %s (máquina %q)\n", deviceJID, cfg.HostID)
	fmt.Printf("Janela: %d dia(s) · publicando a cada %d min · destino: %s\n",
		cfg.WindowDays, cfg.Export.IntervalMinutes, describeRemote(cfg))

	// Dá um tempo para o history sync inicial chegar antes do primeiro ciclo.
	select {
	case <-ctx.Done():
		return nil
	case loss := <-client.SessionLost():
		return finishSessionLost(cfg, db, deviceJID, loss)
	case <-time.After(20 * time.Second):
	}

	alertaLimpo := false
	for {
		if err := cycle(ctx, cfg, db, deviceJID, time.Now()); err != nil {
			fmt.Fprintf(os.Stderr, "ciclo falhou: %v\n", err)
		} else if !alertaLimpo {
			// Depois do primeiro ciclo que deu certo, e não no Connect:
			// conectar não é o mesmo que voltar a capturar e publicar. Apagar
			// no Connect faria o aviso sumir antes de a máquina provar que
			// voltou — e num laço de reconexão sumiria a cada tentativa.
			if err := clearAlert(ctx, cfg); err != nil {
				fmt.Fprintf(os.Stderr, "aviso: não consegui limpar o alerta anterior: %v\n", err)
			}
			alertaLimpo = true
		}
		if once {
			return nil
		}
		select {
		case <-ctx.Done():
			fmt.Println("\nEncerrando; publicando um último ciclo...")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if err := cycle(shutdownCtx, cfg, db, deviceJID, time.Now()); err != nil {
				fmt.Fprintf(os.Stderr, "ciclo final falhou: %v\n", err)
			}
			return nil
		case loss := <-client.SessionLost():
			return finishSessionLost(cfg, db, deviceJID, loss)
		case <-time.After(cfg.Interval()):
		}
	}
}

// finishSessionLost fecha o `run` quando o pareamento cai: publica o que já foi
// capturado, deixa o alerta no destino e devolve o erro que vira exitSessionLost.
//
// A ordem importa. O ciclo final vem PRIMEIRO para que o "publicado em" do
// alerta seja posterior ao generated_at do shard — é essa comparação que diz,
// meses depois, se o alerta ainda vale. Invertida, todo alerta nasceria
// parecendo já resolvido.
//
// O contexto é novo de propósito: o ctx do comando pode já estar cancelado
// (SIGTERM junto com a queda), e mesmo assim é preciso conseguir publicar.
func finishSessionLost(cfg *config.Config, db *store.DB, deviceJID string, loss wa.SessionLoss) error {
	fmt.Fprintf(os.Stderr, "\nCAPTURA INTERROMPIDA: %s\n%s\n", loss.Reason, loss.Fix)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := cycle(ctx, cfg, db, deviceJID, time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "ciclo final falhou: %v\n", err)
	}

	alert := export.Alert{
		Host:      cfg.HostID,
		DeviceJID: deviceJID,
		Reason:    loss.Reason,
		Fix:       loss.Fix,
		LostAt:    loss.At,
		At:        time.Now(),
	}
	if path, err := publishAlert(ctx, cfg, alert); err != nil {
		fmt.Fprintf(os.Stderr, "aviso: não consegui publicar o alerta: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "alerta publicado em %s\n", path)
	}

	return fmt.Errorf("%w: %s — %s", errSessionLost, loss.Reason, loss.Fix)
}

// publishAlert grava o alerta desta máquina no destino e uma cópia local.
// Devolve o caminho publicado, para o log.
func publishAlert(ctx context.Context, cfg *config.Config, a export.Alert) (string, error) {
	body := export.MarshalAlert(a, time.Local)

	if err := os.MkdirAll(cfg.OutDir(), 0o700); err != nil {
		return "", err
	}
	local := localAlertPath(cfg)
	if err := os.WriteFile(local, body, 0o600); err != nil {
		return "", err
	}

	be, err := remote.New(cfg)
	if err != nil {
		return local, err
	}
	if be == nil {
		return local, nil
	}
	rel := export.AlertFile(cfg.HostID)
	if err := be.Put(ctx, rel, body); err != nil {
		return local, err
	}
	return rel, nil
}

// localAlertPath é a cópia local do alerta desta máquina.
func localAlertPath(cfg *config.Config) string {
	return filepath.Join(cfg.OutDir(), "ALERTA.md")
}

// clearAlert remove o alerta desta máquina, no destino e na cópia local.
//
// Um aviso que fica para sempre é indistinguível de um aviso atual: quem lê a
// pasta meses depois não tem como saber se aquela máquina voltou. Até B10 não
// dava para apagar — o backend não tinha Delete —, e o alerta passou a carregar
// o próprio critério de validade (comparar o generated_at do shard com a hora
// do aviso). Isso continua valendo como segunda linha, mas exige que o leitor
// faça a conta. Apagar é melhor sinal.
//
// Só toca no arquivo DESTA máquina. O alerta de outra continua de pé: quem caiu
// foi ela, e daqui não há como saber se voltou.
func clearAlert(ctx context.Context, cfg *config.Config) error {
	if err := os.Remove(localAlertPath(cfg)); err != nil && !os.IsNotExist(err) {
		return err
	}
	be, err := remote.New(cfg)
	if err != nil || be == nil {
		return err
	}
	return be.Delete(ctx, export.AlertFile(cfg.HostID))
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
	if err != nil {
		// Este é o comando de diagnóstico: engolir o erro aqui esconde
		// justamente o que se veio ver. Um whisper ausente, por exemplo,
		// impede o `run` de subir e não aparecia em lugar nenhum.
		fmt.Printf("Pareamento:  não consegui abrir a sessão: %v\n", err)
	} else {
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

	// Alertas de sessão caída. Uma máquina parada é a diferença entre "não
	// aconteceu nada" e "ninguém estava ouvindo" — e é o que o status existe
	// para mostrar sem que você precise abrir a pasta.
	if names, err := be.List(ctx, export.AlertDir); err == nil && len(names) > 0 {
		fmt.Printf("\nALERTAS de sessão caída (%d):\n", len(names))
		for _, n := range names {
			fmt.Printf("  %s/%s — abra o arquivo; se o shard da máquina for mais recente, ela voltou\n",
				export.AlertDir, n)
		}
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
		fmt.Printf("  %-40s  %3d membros  %-28s  anexos: %s\n",
			g.Name, g.ParticipantCount, g.JID.String(), describeMediaPolicy(cfg, g.JID.String(), g.Name))
	}
	fmt.Println("\nUse os nomes (ou JIDs) em [filter].include_only / [filter].exclude no config.toml")
	fmt.Println("A coluna \"anexos\" mostra a política de [media] JÁ RESOLVIDA para cada")
	fmt.Println("conversa: é o que este binário faria hoje, não o que o TOML parece dizer.")
	return nil
}

// describeMediaPolicy resume o que seria baixado num chat.
func describeMediaPolicy(cfg *config.Config, jid, name string) string {
	if !cfg.Media.Enabled {
		return "nenhum ([media].enabled desligado)"
	}
	kinds := cfg.MediaKindsFor(jid, name)
	if len(kinds) == 0 {
		return "nenhum"
	}
	return strings.Join(kinds, ", ")
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

	// Anexo é acessório: se a sincronização deles falhar, as mensagens já
	// foram publicadas e a consolidação não tem por que parar.
	if med, err := merge.PublishMedia(ctx, cfg, be, cfg.MediaDir(), from); err != nil {
		fmt.Fprintf(os.Stderr, "aviso: anexos: %v\n", err)
	} else if med.Uploaded > 0 || med.Pruned > 0 || med.Missing > 0 {
		fmt.Printf("[%s] anexos: %d publicados, %d removidos da janela, %d sem arquivo local\n",
			now.Format("15:04"), med.Uploaded, med.Pruned, med.Missing)
	}

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
