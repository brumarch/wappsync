package wa

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// allowedClientCalls é a lista COMPLETA de operações que este programa pode
// invocar no cliente do WhatsApp. Toda entrada é leitura, conexão ou
// pareamento. Adicionar qualquer coisa aqui é uma decisão consciente de
// ampliar o que o programa faz na conta — não faça sem entender o efeito.
var allowedClientCalls = map[string]string{
	"c.wa.AddEventHandler":               "registra handler local, sem tráfego de rede",
	"c.wa.Connect":                       "abre a conexão",
	"c.wa.Disconnect":                    "fecha a conexão",
	"c.wa.WaitForConnection":             "espera a conexão abrir",
	"c.wa.GetQRChannel":                  "canal de pareamento por QR",
	"c.wa.PairPhone":                     "pareamento por código, a pedido explícito do usuário",
	"c.wa.Logout":                        "desvincula este aparelho, a pedido explícito do usuário",
	"c.wa.GetJoinedGroups":               "leitura: lista de grupos",
	"c.wa.ParseWebMessage":               "conversão local de protobuf, sem rede",
	"c.wa.Store.Contacts.GetAllContacts": "leitura: nomes de contatos do banco local",
	"c.wa.Store.ID.String":               "leitura: JID do próprio aparelho, formatação local",
}

// forbiddenMethods são operações do whatsmeow que escrevem na conta: enviam,
// respondem, editam, apagam, marcam como lido, sinalizam presença ou alteram
// grupos e configurações. Nenhuma pode aparecer em lugar nenhum deste módulo.
var forbiddenMethods = []string{
	// Envio de mensagens
	"SendMessage", "SendFBMessage", "SendPeerMessage",
	// Construtores de mensagem: existir no código já indica intenção de enviar
	"BuildEdit", "BuildRevoke", "BuildReaction",
	"BuildPollCreation", "BuildPollVote",
	"BuildHistorySyncRequest", "BuildUnavailableMessageRequest",
	"RevokeMessage",
	// Sinalização visível para terceiros
	"MarkRead", "SendChatPresence", "SendPresence", "SubscribePresence",
	"SetForceActiveDeliveryReceipts", "SendProtocolMessageReceipt",
	"SendHistorySyncServerErrorReceipt",
	// Mutação de grupos
	"CreateGroup", "LeaveGroup", "JoinGroupWithLink", "JoinGroupWithInvite",
	"UpdateGroupParticipants", "UpdateGroupRequestParticipants",
	"SetGroupName", "SetGroupTopic", "SetGroupPhoto", "SetGroupAnnounce",
	"SetGroupLocked", "SetGroupDescription", "SetGroupJoinApprovalMode",
	"SetGroupMemberAddMode",
	// Mutação da conta
	"SendAppState", "SetStatusMessage", "SetPrivacySetting", "UpdateBlocklist",
	"SetDisappearingTimer", "SetDefaultDisappearingTimer",
	"CreateNewsletter", "FollowNewsletter", "UnfollowNewsletter",
	"AcceptTOSNotice", "RegisterForPushNotifications", "RejectCall",
	"MarkNotDirty",
}

// moduleRoot sobe até encontrar o go.mod.
func moduleRoot(t *testing.T) string {
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

// eachGoFile percorre todo o código-fonte do módulo.
func eachGoFile(t *testing.T, fn func(path string, fset *token.FileSet, file *ast.File)) {
	t.Helper()
	root := moduleRoot(t)
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		parsed, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		fn(filepath.ToSlash(rel), fset, parsed)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// dottedPath reduz uma cadeia de seletores a "a.b.c". Devolve "" se a expressão
// não for uma cadeia pura de identificadores.
func dottedPath(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		base := dottedPath(e.X)
		if base == "" {
			return ""
		}
		return base + "." + e.Sel.Name
	}
	return ""
}

// Toda chamada feita no cliente do WhatsApp precisa estar na allowlist.
// É a verificação mais forte: c.wa é o único *whatsmeow.Client do módulo, então
// nada alcança a conta sem passar por aqui.
func TestClientCallsAreAllowlisted(t *testing.T) {
	var unexpected []string

	eachGoFile(t, func(path string, fset *token.FileSet, file *ast.File) {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			p := dottedPath(call.Fun)
			if !strings.HasPrefix(p, "c.wa.") {
				return true
			}
			if _, allowed := allowedClientCalls[p]; !allowed {
				unexpected = append(unexpected,
					fset.Position(call.Pos()).String()+": "+p+"()")
			}
			return true
		})
	})

	sort.Strings(unexpected)
	for _, u := range unexpected {
		t.Errorf("chamada não autorizada no cliente do WhatsApp:\n  %s\n"+
			"  Se for realmente leitura, adicione em allowedClientCalls explicando o porquê.", u)
	}
}

// Nenhuma API de escrita do whatsmeow pode aparecer no módulo, sob qualquer
// nome de variável. Cobre o caso de alguém criar um segundo cliente.
func TestNoWriteAPIAnywhere(t *testing.T) {
	banned := make(map[string]bool, len(forbiddenMethods))
	for _, m := range forbiddenMethods {
		banned[m] = true
	}

	eachGoFile(t, func(path string, fset *token.FileSet, file *ast.File) {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if banned[sel.Sel.Name] {
				t.Errorf("API de escrita do WhatsApp invocada em %s: %s()\n"+
					"  Este programa é somente-leitura por desenho; ver enforceReadOnly.",
					fset.Position(call.Pos()), sel.Sel.Name)
			}
			return true
		})
	})
}

// enforceReadOnly precisa continuar sendo chamado na construção do cliente,
// senão a proteção contra reenvio induzido por retry receipt some silenciosamente.
func TestNewCallsEnforceReadOnly(t *testing.T) {
	found := false

	eachGoFile(t, func(path string, fset *token.FileSet, file *ast.File) {
		if path != "internal/wa/client.go" {
			return
		}
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "New" || fn.Recv != nil {
				return true
			}
			ast.Inspect(fn.Body, func(inner ast.Node) bool {
				if call, ok := inner.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "enforceReadOnly" {
						found = true
					}
				}
				return true
			})
			return true
		})
	})

	if !found {
		t.Error("wa.New não chama mais enforceReadOnly")
	}
}

// A postura somente-leitura tem que valer em comportamento, não só em intenção:
// o cliente precisa recusar qualquer pedido de reenvio.
func TestEnforceReadOnlyRefusesResends(t *testing.T) {
	cli := &whatsmeow.Client{}
	enforceReadOnly(cli)

	if cli.UseRetryMessageStore {
		t.Error("UseRetryMessageStore ligado: mensagens ficariam disponíveis para reenvio")
	}
	if cli.AutomaticMessageRerequestFromPhone {
		t.Error("AutomaticMessageRerequestFromPhone ligado: pediria mensagens ao celular")
	}
	if cli.GetMessageForRetry == nil {
		t.Fatal("GetMessageForRetry não foi definido")
	}
	if msg := cli.GetMessageForRetry(types.EmptyJID, types.EmptyJID, "qualquer-id"); msg != nil {
		t.Error("GetMessageForRetry devolveu uma mensagem: haveria o que reenviar")
	}
	if cli.PreRetryCallback == nil {
		t.Fatal("PreRetryCallback não foi definido")
	}
	if cli.PreRetryCallback(&events.Receipt{}, "qualquer-id", 1, nil) {
		t.Error("PreRetryCallback aceitou um pedido de reenvio")
	}
}
