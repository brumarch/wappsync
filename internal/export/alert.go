package export

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

// AlertDir é a pasta dos alertas no destino, ao lado de shards/ e latest/.
const AlertDir = "alertas"

// AlertFile é o caminho do alerta de uma máquina no destino.
//
// Um arquivo POR MÁQUINA, como os shards, e pelo mesmo motivo: um
// `latest/ALERTA.md` compartilhado seria o único arquivo mutável comum do
// desenho, e duas máquinas caídas ao mesmo tempo apagariam o alerta uma da
// outra. Ver o invariante anti-sobrescrita no CLAUDE.md.
func AlertFile(host string) string { return AlertDir + "/" + host + ".md" }

// Alert é o que uma máquina publica ao perder o pareamento com o WhatsApp.
type Alert struct {
	Host      string
	DeviceJID string
	// Reason é o motivo em uma linha; Fix é o que fazer para voltar a capturar.
	Reason string
	Fix    string
	// LostAt é quando a captura parou. At é quando este arquivo foi escrito —
	// sempre DEPOIS do último ciclo de publicação, para que a comparação com o
	// generated_at do shard diga se o alerta ainda vale.
	LostAt time.Time
	At     time.Time
}

// MarshalAlert gera o aviso que fica no destino quando uma máquina para de
// capturar.
//
// O arquivo precisa se sustentar sozinho por muito tempo: o backend não tem
// Delete, então ninguém o apaga quando a máquina volta. Daí a seção que ensina
// a decidir se ele ainda vale — um alerta que não sabe expirar vira ou pânico
// permanente, ou ruído ignorado.
func MarshalAlert(a Alert, loc *time.Location) []byte {
	if loc == nil {
		loc = time.Local
	}
	stamp := func(t time.Time) string {
		if t.IsZero() {
			return "desconhecido"
		}
		return t.In(loc).Format(time.RFC3339)
	}

	var b strings.Builder

	fmt.Fprintf(&b, "# ALERTA — a máquina `%s` parou de capturar\n\n", a.Host)
	b.WriteString("Escrito pelo `wapp-summarizer` quando uma máquina perde o pareamento com o\n")
	b.WriteString("WhatsApp. Enquanto este alerta valer, o histórico publicado tem um buraco:\n")
	fmt.Fprintf(&b, "nada que chegou depois do horário abaixo foi capturado por `%s`.\n\n", a.Host)

	fmt.Fprintf(&b, "- Máquina: `%s`\n", a.Host)
	if a.DeviceJID != "" {
		fmt.Fprintf(&b, "- Aparelho: `%s`\n", a.DeviceJID)
	}
	fmt.Fprintf(&b, "- Parou em: %s\n", stamp(a.LostAt))
	fmt.Fprintf(&b, "- Motivo: %s\n", a.Reason)
	fmt.Fprintf(&b, "- Alerta publicado em: %s\n\n", stamp(a.At))

	b.WriteString("## Este alerta ainda vale?\n\n")
	b.WriteString("Ele não é apagado sozinho. Compare o `generated_at` de\n")
	fmt.Fprintf(&b, "`shards/%s.meta.json` com o \"Alerta publicado em\" acima:\n\n", a.Host)
	b.WriteString("- **anterior** → a máquina continua parada e o alerta vale;\n")
	fmt.Fprintf(&b, "- **posterior** → `%s` voltou a publicar e este arquivo é histórico.\n\n", a.Host)

	b.WriteString("## Para quem resume estes dados\n\n")
	fmt.Fprintf(&b, "Se o alerta ainda vale, **diga no resumo** que `%s` parou de capturar em %s.\n",
		a.Host, stamp(a.LostAt))
	b.WriteString("A ausência de mensagens depois desse horário não é prova de que nada aconteceu:\n")
	b.WriteString("pode ser só o buraco. Se outras máquinas continuaram publicando, a cobertura do\n")
	b.WriteString("período é parcial, não nula — o que esta máquina veria sozinha é que falta.\n\n")

	b.WriteString("## Para quem administra a máquina\n\n")
	fmt.Fprintf(&b, "%s.\n\n", sentence(a.Fix))
	b.WriteString("O `wappsync run` encerrou ao detectar isto, em vez de continuar de pé sem\n")
	b.WriteString("capturar nada. Reiniciar o processo não resolve sozinho.\n")

	return []byte(b.String())
}

// sentence transforma o Fix, que é fragmento ("rode `wappsync login` ..."), na
// frase que abre uma seção.
func sentence(s string) string {
	s = strings.TrimSuffix(strings.TrimSpace(s), ".")
	if s == "" {
		return s
	}
	r := []rune(s)
	return string(unicode.ToUpper(r[0])) + string(r[1:])
}
