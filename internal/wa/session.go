package wa

import (
	"fmt"
	"time"

	"go.mau.fi/whatsmeow/types/events"
)

// SessionLoss descreve por que esta máquina parou de capturar.
//
// Os dois campos vão parar no alerta publicado na nuvem, então são texto para
// pessoa: o motivo em uma linha e o que fazer para voltar a capturar.
type SessionLoss struct {
	Reason string
	Fix    string

	// At é quando o evento chegou, não quando o `run` reparou. Os dois podem
	// distar um ciclo inteiro se a queda cair no meio de uma publicação, e é
	// este horário que o alerta apresenta como início do buraco no histórico.
	At time.Time
}

// sessionLossFor traduz um evento do whatsmeow em perda de pareamento, ou
// devolve false se o evento não interrompe a captura.
//
// Vive fora do handler porque é a única parte desta lógica que dá para testar
// sem uma conta: entra um evento montado à mão, sai a descrição que o alerta
// publica. Ver "O que não é testável" no CLAUDE.md.
func sessionLossFor(rawEvt any) (SessionLoss, bool) {
	switch evt := rawEvt.(type) {
	case *events.LoggedOut:
		// Reason só é preenchido quando o evento veio de uma recusa de
		// conexão; num stream:error ele fica zerado e imprimiria
		// "0: unknown error", que confunde mais do que informa.
		reason := "o WhatsApp desvinculou este aparelho"
		if evt.OnConnect {
			reason = fmt.Sprintf("o WhatsApp recusou a conexão e desvinculou este aparelho (%s)", evt.Reason)
		}
		return SessionLoss{
			Reason: reason,
			Fix:    "rode `wappsync login` nesta máquina para parear de novo",
		}, true

	case *events.StreamReplaced:
		return SessionLoss{
			Reason: "outra sessão assumiu este pareamento",
			Fix:    "nunca use o mesmo session.db em duas máquinas; cada uma precisa do seu próprio `wappsync login`",
		}, true
	}
	return SessionLoss{}, false
}

// signalLoss avisa quem estiver esperando, sem nunca bloquear.
//
// Roda nas goroutines do whatsmeow, em paralelo com o ciclo de export: o canal
// é bufferizado em 1 e o envio é não-bloqueante de propósito. Se já houver um
// aviso na fila, o primeiro motivo é o que interessa — os eventos seguintes são
// consequência dele.
func (c *Client) signalLoss(loss SessionLoss) {
	if loss.At.IsZero() {
		loss.At = time.Now()
	}
	select {
	case c.lost <- loss:
	default:
	}
}

// SessionLost entrega o primeiro motivo pelo qual esta máquina deixou de
// capturar. Quem lê daqui deve tratar como terminal: nada volta a ser capturado
// sem intervenção.
func (c *Client) SessionLost() <-chan SessionLoss { return c.lost }
