package wa

import (
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// sessionLossFor é o ponto em que "o WhatsApp derrubou a sessão" vira um aviso
// publicável. Dá para exercitá-la com eventos montados à mão: entra o evento,
// sai o texto — sem conta, sem rede.

func TestSessionLossForLoggedOutViaStreamError(t *testing.T) {
	// OnConnect = false é o caso do stream:error, em que Reason vem zerado.
	// Citá-lo imprimiria "0: unknown error", que confunde mais do que informa.
	loss, ok := sessionLossFor(&events.LoggedOut{})
	if !ok {
		t.Fatal("LoggedOut não foi reconhecido como perda de sessão")
	}
	if !strings.Contains(loss.Reason, "desvinculou") {
		t.Errorf("motivo não diz que o aparelho foi desvinculado: %q", loss.Reason)
	}
	if strings.Contains(loss.Reason, "unknown error") || strings.Contains(loss.Reason, "0:") {
		t.Errorf("motivo vazou o código de falha zerado: %q", loss.Reason)
	}
	if !strings.Contains(loss.Fix, "wappsync login") {
		t.Errorf("correção não diz para rodar `wappsync login`: %q", loss.Fix)
	}
}

func TestSessionLossForLoggedOutOnConnectCitesReason(t *testing.T) {
	loss, ok := sessionLossFor(&events.LoggedOut{
		OnConnect: true,
		Reason:    events.ConnectFailureMainDeviceGone,
	})
	if !ok {
		t.Fatal("LoggedOut na conexão não foi reconhecido como perda de sessão")
	}
	// O código é o que diferencia "você desvinculou pelo celular" de "a conta
	// principal sumiu"; sem ele o alerta manda a pessoa tentar a coisa errada.
	if !strings.Contains(loss.Reason, "403") {
		t.Errorf("motivo não cita o código da recusa: %q", loss.Reason)
	}
}

func TestSessionLossForStreamReplaced(t *testing.T) {
	loss, ok := sessionLossFor(&events.StreamReplaced{})
	if !ok {
		t.Fatal("StreamReplaced não foi reconhecido como perda de sessão")
	}
	if !strings.Contains(loss.Fix, "session.db") {
		t.Errorf("correção não menciona o session.db duplicado: %q", loss.Fix)
	}
}

// O contrário importa tanto quanto: um falso positivo derruba o `run` de uma
// máquina saudável, que é pior do que o problema original.
func TestSessionLossIgnoresEventosNormais(t *testing.T) {
	cases := map[string]any{
		"mensagem":     &events.Message{},
		"conectado":    &events.Connected{},
		"desconectado": &events.Disconnected{}, // queda de rede: o whatsmeow reconecta
		"offline sync": &events.OfflineSyncCompleted{},
		"grupo":        &events.GroupInfo{},
		"history sync": &events.HistorySync{},
		"nil":          nil,
	}
	for name, evt := range cases {
		if _, ok := sessionLossFor(evt); ok {
			t.Errorf("%s foi tratado como perda de sessão", name)
		}
	}
}

// signalLoss roda nas goroutines do whatsmeow: não pode bloquear nem quando
// ninguém está lendo, e o primeiro motivo é o que vale — os seguintes são
// consequência dele.
func TestSignalLossNaoBloqueiaEGuardaOPrimeiro(t *testing.T) {
	c := &Client{lost: make(chan SessionLoss, 1)}

	c.signalLoss(SessionLoss{Reason: "primeiro"})
	c.signalLoss(SessionLoss{Reason: "segundo"})
	c.signalLoss(SessionLoss{Reason: "terceiro"})

	select {
	case got := <-c.SessionLost():
		if got.Reason != "primeiro" {
			t.Errorf("motivo entregue = %q, queria o primeiro", got.Reason)
		}
		// O horário vem de quem sinaliza, não de quem lê: entre os dois pode
		// caber um ciclo de publicação inteiro.
		if got.At.IsZero() {
			t.Error("o aviso saiu sem o horário do evento")
		}
	default:
		t.Fatal("nada foi sinalizado")
	}

	select {
	case got := <-c.SessionLost():
		t.Errorf("segundo aviso na fila: %q", got.Reason)
	default:
	}
}

// A costura entre o evento e o canal é o que de fato faz o `run` sair: sem ela,
// sessionLossFor continua correta e nada acontece — que é exatamente o silêncio
// que este item veio corrigir.
func TestHandleEventSinalizaPerdaDeSessao(t *testing.T) {
	c := &Client{log: waLog.Noop, names: map[string]string{}, lost: make(chan SessionLoss, 1)}

	c.handleEvent(&events.LoggedOut{})

	select {
	case loss := <-c.SessionLost():
		if !strings.Contains(loss.Reason, "desvinculou") {
			t.Errorf("motivo sinalizado = %q", loss.Reason)
		}
	default:
		t.Fatal("handleEvent não sinalizou a perda de sessão")
	}
}
