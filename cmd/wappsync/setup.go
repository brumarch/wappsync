package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/bmar13/wapp-summarizer/internal/config"
	"github.com/bmar13/wapp-summarizer/internal/store"
	"github.com/bmar13/wapp-summarizer/internal/wa"
)

// setupChat é uma conversa como o assistente a apresenta: o nome é para a
// pessoa escolher, o JID é o que vai para o config. Grava-se o JID e não o
// nome de propósito — nome muda e depende de já ter sido visto (B14); JID é
// exato.
type setupChat struct {
	JID     string
	Name    string
	IsGroup bool
}

// label é o que aparece na lista numerada.
func (c setupChat) label() string {
	if c.Name == "" {
		return c.JID
	}
	return c.Name
}

// mediaException é uma conversa que fica de fora de um ou mais tipos de anexo.
type mediaException struct {
	Chat  setupChat
	Kinds []string
}

// setupAnswers é o questionário já respondido, resolvido em JIDs.
type setupAnswers struct {
	ExportAll   bool
	IncludeOnly []setupChat
	Exclude     []setupChat

	Transcribe bool
	Model      string

	// Kinds segue a ordem de config.MediaKinds.
	Kinds        []string
	MediaExclude []mediaException
}

// setupEnv isola do questionário o que depende da máquina, para o teste
// roteirizar sem whisper instalado e sem modelo em disco.
type setupEnv struct {
	fileExists func(string) bool
	haveBinary func(string) bool
}

func realSetupEnv() setupEnv {
	return setupEnv{
		fileExists: func(p string) bool { _, err := os.Stat(p); return err == nil },
		haveBinary: func(b string) bool { _, err := exec.LookPath(b); return err == nil },
	}
}

// errSetupCancelled é o usuário desistindo — não é falha, e o arquivo não muda.
var errSetupCancelled = errors.New("setup cancelado; o config.toml não foi alterado")

// kindLabels é como cada tipo aparece nas perguntas.
var kindLabels = map[string]string{
	"image":    "imagens",
	"document": "documentos",
	"audio":    "áudio (transcrição)",
}

// cmdSetup conecta só para listar as conversas e depois entrega tudo ao
// questionário. A conexão usa uma cópia do config com anexo e transcrição
// desligados: o setup existe para DECIDIR a política, então não deve executar
// a antiga enquanto pergunta — e um whisper ausente no config atual não pode
// impedir justamente o comando que serve para consertá-lo.
func cmdSetup(ctx context.Context, cfg *config.Config, verbose bool) error {
	quiet := *cfg
	quiet.Media.Enabled = false
	quiet.Transcribe.Enabled = false

	db, err := openDB(&quiet)
	if err != nil {
		return err
	}
	defer db.Close()

	client, err := wa.New(ctx, &quiet, db, verbose)
	if err != nil {
		return err
	}
	defer client.Close()

	if !client.IsPaired() {
		return errors.New("esta máquina não está pareada; rode `wappsync login` antes do setup")
	}
	if err := client.Connect(ctx); err != nil {
		return err
	}
	if err := client.RefreshNames(ctx); err != nil {
		fmt.Printf("aviso: nem todos os nomes puderam ser lidos (%v); a lista pode vir com JID no lugar do nome\n", err)
	}
	groups, err := client.Groups(ctx)
	if err != nil {
		return err
	}
	var gs []setupChat
	for _, g := range groups {
		gs = append(gs, setupChat{JID: g.JID.String(), Name: g.Name, IsGroup: true})
	}
	known, err := db.Chats(ctx)
	if err != nil {
		return err
	}
	chats := mergeSetupChats(gs, known)
	if len(chats) == 0 {
		return errors.New("nenhuma conversa conhecida ainda; deixe `wappsync run` capturar um pouco e tente de novo")
	}
	return setupFlow(os.Stdin, os.Stdout, chats, cfg, realSetupEnv())
}

// mergeSetupChats junta os grupos (vindos do WhatsApp) com as conversas
// diretas que já têm mensagem no banco. Contatos sem mensagem ficam de fora:
// RefreshNames grava a agenda inteira em chats, e uma lista com centenas de
// nomes que nunca conversaram é pior que nenhuma. Grupo que está no banco mas
// não na conta (saiu) também não entra.
func mergeSetupChats(groups []setupChat, known map[string]store.Chat) []setupChat {
	out := append([]setupChat(nil), groups...)
	seen := map[string]bool{}
	for _, g := range groups {
		seen[g.JID] = true
	}
	var dms []setupChat
	for jid, c := range known {
		if c.IsGroup || seen[jid] || c.LastTS.IsZero() {
			continue
		}
		dms = append(dms, setupChat{JID: jid, Name: c.Name})
	}
	sort.Slice(dms, func(i, j int) bool {
		return strings.ToLower(dms[i].label()) < strings.ToLower(dms[j].label())
	})
	return append(out, dms...)
}

// setupFlow é o comando inteiro depois da conexão: pergunta, mostra o que vai
// mudar, pede confirmação e grava. Recebe leitor e escritor para o teste
// roteirizar de ponta a ponta.
func setupFlow(in io.Reader, out io.Writer, chats []setupChat, cur *config.Config, env setupEnv) error {
	w := &wizard{in: bufio.NewScanner(in), out: out}

	answers, err := w.run(chats, cur, env)
	if err != nil {
		return err
	}
	block := renderSetupSections(answers, cur)

	original, err := os.ReadFile(cur.SourcePath)
	if err != nil {
		return fmt.Errorf("lendo %s: %w", cur.SourcePath, err)
	}
	updated := spliceConfig(string(original), block)

	fmt.Fprintf(out, "\nVai substituir as seções [filter], [media] e [transcribe] de %s por:\n\n", cur.SourcePath)
	fmt.Fprintln(out, indent(block, "    "))
	if n := len(cur.Media.Chats); n > 0 {
		fmt.Fprintf(out, "As %d entrada(s) [[media.chat]] atuais serão removidas.\n", n)
	}
	fmt.Fprintf(out, "O resto do arquivo fica como está. Uma cópia do atual vai para %s.bak.\n", cur.SourcePath)
	ok, err := w.yesNo("Gravar?", false)
	if err != nil {
		return err
	}
	if !ok {
		return errSetupCancelled
	}
	if err := writeConfigValidated(cur.SourcePath, updated); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nGravado. Confira a política resolvida com `wappsync groups` e depois rode `wappsync run`.\n")
	return nil
}

// ---------------------------------------------------------- questionário ---

type wizard struct {
	in  *bufio.Scanner
	out io.Writer
}

func (w *wizard) run(chats []setupChat, cur *config.Config, env setupEnv) (setupAnswers, error) {
	var a setupAnswers

	groups := 0
	for _, c := range chats {
		if c.IsGroup {
			groups++
		}
	}
	fmt.Fprintf(w.out, "Conversas conhecidas: %d grupo(s) e %d conversa(s) direta(s).\n", groups, len(chats)-groups)
	fmt.Fprintln(w.out, "Responda com Enter para aceitar o padrão entre colchetes.")

	// 1. O que sai daqui.
	fmt.Fprintln(w.out, "\n== Conversas ==")
	all, err := w.yesNo("Exportar TODAS as conversas?", true)
	if err != nil {
		return a, err
	}
	a.ExportAll = all
	if all {
		a.Exclude, err = w.pick("Alguma conversa que NÃO deve sair desta máquina?", chats)
		if err != nil {
			return a, err
		}
	} else {
		for len(a.IncludeOnly) == 0 {
			a.IncludeOnly, err = w.pick("Quais conversas exportar? (pelo menos uma)", chats)
			if err != nil {
				return a, err
			}
		}
	}
	exported := a.exportedChats(chats)

	// 2. Transcrição vem antes dos anexos porque "audio" só existe com ela.
	fmt.Fprintln(w.out, "\n== Transcrição ==")
	fmt.Fprintln(w.out, "Whisper roda NESTA máquina; o áudio não sai daqui, só o texto vai para a pasta.")
	fmt.Fprintln(w.out, "Exige whisper-cli e ffmpeg no PATH, e um modelo ggml-*.bin em disco.")
	a.Transcribe, err = w.yesNo("Transcrever notas de voz?", false)
	if err != nil {
		return a, err
	}
	if a.Transcribe {
		a.Model, err = w.modelPath(cur.Transcribe.Model, env)
		if err != nil {
			return a, err
		}
		for _, bin := range []string{cur.Transcribe.Binary, cur.Transcribe.FFmpeg} {
			if !env.haveBinary(bin) {
				fmt.Fprintf(w.out, "aviso: %q não está no PATH; o `wappsync run` não vai subir até estar\n", bin)
			}
		}
	}

	// 3. Anexos. O padrão de cada pergunta é NÃO: um assistente que facilita
	// ligar tudo é pior que nenhum, porque anexo é a maior mudança de
	// exposição do projeto.
	fmt.Fprintln(w.out, "\n== Anexos ==")
	fmt.Fprintln(w.out, "Sem anexos, só TEXTO vai para a nuvem. Cada tipo abaixo vale para todas as")
	fmt.Fprintln(w.out, "conversas exportadas; em seguida você marca as exceções.")
	for _, kind := range config.MediaKinds() {
		if kind == config.KindAudio && !a.Transcribe {
			continue
		}
		yes, err := w.yesNo(fmt.Sprintf("Baixar %s de todas as conversas exportadas?", kindLabels[kind]), false)
		if err != nil {
			return a, err
		}
		if !yes {
			continue
		}
		a.Kinds = append(a.Kinds, kind)
		skip, err := w.pick(fmt.Sprintf("Conversas onde NÃO baixar %s:", kindLabels[kind]), exported)
		if err != nil {
			return a, err
		}
		for _, c := range skip {
			a.addMediaException(c, kind)
		}
	}
	return a, nil
}

// exportedChats é a lista sobre a qual as exceções de anexo fazem sentido:
// não se pergunta sobre anexo de conversa que nem vai sair.
func (a setupAnswers) exportedChats(all []setupChat) []setupChat {
	if !a.ExportAll {
		return a.IncludeOnly
	}
	excluded := map[string]bool{}
	for _, c := range a.Exclude {
		excluded[c.JID] = true
	}
	var out []setupChat
	for _, c := range all {
		if !excluded[c.JID] {
			out = append(out, c)
		}
	}
	return out
}

func (a *setupAnswers) addMediaException(c setupChat, kind string) {
	for i := range a.MediaExclude {
		if a.MediaExclude[i].Chat.JID == c.JID {
			a.MediaExclude[i].Kinds = append(a.MediaExclude[i].Kinds, kind)
			return
		}
	}
	a.MediaExclude = append(a.MediaExclude, mediaException{Chat: c, Kinds: []string{kind}})
}

// modelPath insiste até receber um arquivo que existe. Vazio, sem default,
// cancela: um caminho errado aqui faria o Load do fim recusar o arquivo
// inteiro, e a pessoa perderia as outras respostas.
func (w *wizard) modelPath(def string, env setupEnv) (string, error) {
	for {
		prompt := "Caminho do modelo Whisper (.bin)"
		if def != "" {
			prompt += " [" + def + "]"
		}
		line, err := w.ask(prompt + ": ")
		if err != nil {
			return "", err
		}
		if line == "" {
			line = def
		}
		if line == "" {
			return "", errSetupCancelled
		}
		if env.fileExists(line) {
			return line, nil
		}
		fmt.Fprintf(w.out, "não encontrado: %s\n", line)
	}
}

func (w *wizard) ask(prompt string) (string, error) {
	fmt.Fprint(w.out, prompt)
	if !w.in.Scan() {
		if err := w.in.Err(); err != nil {
			return "", err
		}
		return "", errSetupCancelled
	}
	return strings.TrimSpace(w.in.Text()), nil
}

func (w *wizard) yesNo(prompt string, def bool) (bool, error) {
	hint := "[s/N]"
	if def {
		hint = "[S/n]"
	}
	for {
		line, err := w.ask(prompt + " " + hint + " ")
		if err != nil {
			return false, err
		}
		switch strings.ToLower(line) {
		case "":
			return def, nil
		case "s", "sim", "y", "yes":
			return true, nil
		case "n", "nao", "não", "no":
			return false, nil
		}
		fmt.Fprintln(w.out, "responda s ou n")
	}
}

// pick mostra a lista numerada e lê uma seleção. Enter é "nenhuma".
func (w *wizard) pick(prompt string, chats []setupChat) ([]setupChat, error) {
	fmt.Fprintln(w.out, prompt)
	for i, c := range chats {
		tag := "  "
		if c.IsGroup {
			tag = "grupo"
		}
		fmt.Fprintf(w.out, "  %3d. %-40s %-6s %s\n", i+1, c.label(), tag, c.JID)
	}
	for {
		line, err := w.ask("Números (ex.: 1 3 5-7), ou Enter para nenhuma: ")
		if err != nil {
			return nil, err
		}
		idx, err := parseSelection(line, len(chats))
		if err != nil {
			fmt.Fprintln(w.out, err)
			continue
		}
		var out []setupChat
		for _, i := range idx {
			out = append(out, chats[i])
		}
		return out, nil
	}
}

// parseSelection lê "1 3 5-7" (ou com vírgulas) e devolve índices zero-based,
// sem repetição, em ordem. Fora do intervalo é erro, não silêncio: marcar a
// conversa errada é o que este comando existe para evitar.
func parseSelection(s string, n int) ([]int, error) {
	seen := map[int]bool{}
	var out []int
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' }) {
		lo, hi := tok, tok
		if a, b, ok := strings.Cut(tok, "-"); ok {
			lo, hi = a, b
		}
		from, err1 := strconv.Atoi(lo)
		to, err2 := strconv.Atoi(hi)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("não entendi %q: use números, como 1 3 5-7", tok)
		}
		if from < 1 || to > n || from > to {
			return nil, fmt.Errorf("%q está fora da lista (1 a %d)", tok, n)
		}
		for i := from; i <= to; i++ {
			if !seen[i] {
				seen[i] = true
				out = append(out, i-1)
			}
		}
	}
	sort.Ints(out)
	return out, nil
}

// ------------------------------------------------------------ renderizar ---

// renderSetupSections escreve as três seções que o setup governa. O que ele
// não pergunta ([media].max_file_mb, os caminhos e limites de [transcribe])
// é copiado do config atual, para não voltar ao default sem ninguém pedir.
func renderSetupSections(a setupAnswers, cur *config.Config) string {
	var b strings.Builder

	b.WriteString("[filter]\n")
	b.WriteString("# Gerado por `wappsync setup`. JID em vez de nome: nome muda, JID não.\n")
	writeChatList(&b, "include_only", a.IncludeOnly)
	writeChatList(&b, "exclude", a.Exclude)

	b.WriteString("\n[media]\n")
	b.WriteString("# Gerado por `wappsync setup`. `kinds` vale para toda conversa exportada;\n")
	b.WriteString("# [[media.exclude]] tira tipos de conversas específicas.\n")
	fmt.Fprintf(&b, "enabled = %t\n", len(a.Kinds) > 0)
	fmt.Fprintf(&b, "max_file_mb = %d\n", cur.Media.MaxFileMB)
	fmt.Fprintf(&b, "kinds = %s\n", tomlStrings(a.Kinds))
	for _, e := range a.MediaExclude {
		fmt.Fprintf(&b, "\n# %s\n[[media.exclude]]\nmatch = %s\nkinds = %s\n",
			commentText(e.Chat.label()), tomlString(e.Chat.JID), tomlStrings(e.Kinds))
	}

	t := cur.Transcribe
	b.WriteString("\n[transcribe]\n")
	fmt.Fprintf(&b, "enabled = %t\n", a.Transcribe)
	fmt.Fprintf(&b, "binary = %s\n", tomlString(t.Binary))
	fmt.Fprintf(&b, "ffmpeg = %s\n", tomlString(t.FFmpeg))
	model := t.Model
	if a.Transcribe {
		model = a.Model
	}
	fmt.Fprintf(&b, "model = %s\n", tomlString(model))
	fmt.Fprintf(&b, "language = %s\n", tomlString(t.Language))
	fmt.Fprintf(&b, "threads = %d\n", t.Threads)
	fmt.Fprintf(&b, "timeout_minutes = %d\n", t.TimeoutMinutes)
	fmt.Fprintf(&b, "max_seconds = %d\n", t.MaxSeconds)
	return b.String()
}

func writeChatList(b *strings.Builder, key string, chats []setupChat) {
	if len(chats) == 0 {
		fmt.Fprintf(b, "%s = []\n", key)
		return
	}
	fmt.Fprintf(b, "%s = [\n", key)
	for _, c := range chats {
		fmt.Fprintf(b, "  %s,  # %s\n", tomlString(c.JID), commentText(c.label()))
	}
	b.WriteString("]\n")
}

// tomlString prefere a string literal (aspas simples), que não tem escape —
// é o que o example.toml usa para caminhos do Windows. Só cai para a básica
// quando o valor tem aspas simples.
func tomlString(s string) string {
	if !strings.ContainsAny(s, "'\n\r") {
		return "'" + s + "'"
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`).Replace(s) + `"`
}

func tomlStrings(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = tomlString(s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// commentText mantém um nome de conversa numa linha só de comentário.
func commentText(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n")
}

// --------------------------------------------------------------- gravar ---

// setupSections são as famílias de tabela que o setup governa. Tudo com esse
// primeiro segmento — [media], [[media.chat]], [[media.exclude]] — é trocado
// em bloco; qualquer outra seção, e todo comentário nelas, fica intacto.
var setupSections = map[string]bool{"filter": true, "media": true, "transcribe": true}

var tomlHeaderRe = regexp.MustCompile(`^\s*\[\[?\s*([A-Za-z0-9_-]+)`)

// spliceConfig substitui as seções do setup dentro do texto original,
// preservando o resto byte a byte. O bloco novo entra onde estava a primeira
// seção substituída, para o arquivo manter a ordem que a pessoa conhece; se
// nenhuma existia, vai para o fim.
func spliceConfig(original, block string) string {
	crlf := strings.Contains(original, "\r\n")
	if crlf {
		original = strings.ReplaceAll(original, "\r\n", "\n")
	}

	var kept []string
	insertAt := -1
	family := ""
	for _, line := range strings.Split(original, "\n") {
		if m := tomlHeaderRe.FindStringSubmatch(line); m != nil {
			family = m[1]
		}
		if setupSections[family] {
			if insertAt < 0 {
				insertAt = len(kept)
			}
			continue
		}
		kept = append(kept, line)
	}
	if insertAt < 0 {
		insertAt = len(kept)
	}

	before := strings.TrimRight(strings.Join(kept[:insertAt], "\n"), "\n")
	after := strings.TrimLeft(strings.Join(kept[insertAt:], "\n"), "\n")
	var parts []string
	if before != "" {
		parts = append(parts, before)
	}
	parts = append(parts, strings.TrimRight(block, "\n"))
	if after != "" {
		parts = append(parts, strings.TrimRight(after, "\n"))
	}
	out := strings.Join(parts, "\n\n") + "\n"
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	return out
}

// writeConfigValidated grava num temporário ao lado, carrega ESSE arquivo
// pelo mesmo Load do programa, e só então guarda o backup e troca. Um config
// que o próprio wappsync não aceitaria nunca chega a substituir o que estava
// funcionando.
func writeConfigValidated(path, content string) error {
	tmp := path + ".setup-tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	if _, err := config.Load(tmp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("o config gerado não passou na validação, nada foi alterado: %w", err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.WriteFile(path+".bak", original, 0o600); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("gravando backup: %w", err)
	}
	return os.Rename(tmp, path)
}
