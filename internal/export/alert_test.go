package export

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testAlert() Alert {
	lost := time.Date(2026, 3, 15, 18, 30, 0, 0, time.UTC)
	return Alert{
		Host:      "bruno-win",
		DeviceJID: "5511900000000.0:12@s.whatsapp.net",
		Reason:    "o WhatsApp desvinculou este aparelho",
		Fix:       "rode `wappsync login` nesta máquina para parear de novo",
		LostAt:    lost,
		At:        lost.Add(90 * time.Second),
	}
}

// O alerta é produto, igual ao digest e ao guia: uma regressão nele não quebra
// teste nenhum e não gera erro — só faz o aviso chegar pior a quem precisa dele.
// Reusa a flag -update declarada em golden_test.go.
func TestAlertGolden(t *testing.T) {
	got := MarshalAlert(testAlert(), time.UTC)
	path := filepath.Join("testdata", "alert.golden.md")

	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("golden regravado: %s", path)
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden não encontrado (%v). Gere com:\n"+
			"  go test ./internal/export/ -run TestAlertGolden -update", err)
	}
	if string(got) != string(want) {
		t.Errorf("o alerta mudou em relação ao golden.\n"+
			"Se a mudança for intencional, revise o diff e rode:\n"+
			"  go test ./internal/export/ -run TestAlertGolden -update\n\n"+
			"--- obtido ---\n%s\n--- esperado ---\n%s", got, want)
	}
}

// TRAVA DO INVARIANTE ANTI-SOBRESCRITA.
//
// O alerta precisa continuar sendo um arquivo POR MÁQUINA. Um caminho fixo
// (latest/ALERTA.md, que foi a proposta original) seria o único arquivo mutável
// compartilhado do desenho: duas máquinas caídas ao mesmo tempo apagariam o
// alerta uma da outra, e a que escrevesse por último decidiria o que a pessoa
// vê. Ver "Nunca sobrescrever com dado pior" no CLAUDE.md.
func TestAlertFileIsPerHost(t *testing.T) {
	a, b := AlertFile("bruno-win"), AlertFile("servidor-casa")
	if a == b {
		t.Fatalf("duas máquinas escrevem no mesmo arquivo de alerta (%q): "+
			"é exatamente o lost update que o desenho evita", a)
	}
	for host, want := range map[string]string{
		"bruno-win":     "alertas/bruno-win.md",
		"servidor-casa": "alertas/servidor-casa.md",
	} {
		if got := AlertFile(host); got != want {
			t.Errorf("AlertFile(%q) = %q, queria %q", host, got, want)
		}
	}
	if !strings.HasPrefix(AlertFile("x"), AlertDir+"/") {
		t.Errorf("o alerta saiu de %s/: %q", AlertDir, AlertFile("x"))
	}
}

// TRAVA DE EXPIRAÇÃO.
//
// Ninguém apaga este arquivo: o backend não tem Delete, e quem cairia é
// justamente a máquina que não está mais rodando. Sem o critério de validade
// escrito dentro dele, um alerta de três meses atrás é indistinguível de um de
// agora — e vira ou pânico permanente, ou ruído ignorado.
//
// Separado do golden de propósito: um `-update` descuidado regravaria o golden
// sem reclamar; aqui a perda é barulhenta.
func TestAlertSempreDizComoSaberSeAindaVale(t *testing.T) {
	a := testAlert()
	got := string(MarshalAlert(a, time.UTC))

	required := map[string]string{
		"## Este alerta ainda vale?": "a seção que ensina a decidir se o alerta expirou",
		"shards/bruno-win.meta.json": "o arquivo com que a data do alerta deve ser comparada",
		"generated_at":               "o campo dessa comparação",
		"Alerta publicado em":        "o lado do alerta na comparação",
		"2026-03-15T18:31:30Z":       "o horário de publicação, que é a referência",
		"não é prova":                "o aviso de que ausência de mensagem não conclui nada",
		"`wappsync login`":           "o que a pessoa precisa fazer",
		"parou de capturar":          "o que de fato aconteceu",
	}
	for want, why := range required {
		if !strings.Contains(got, want) {
			t.Errorf("o alerta perdeu %s: falta %q", why, want)
		}
	}

	// Os dois horários não podem trocar de lugar. É a publicação — posterior ao
	// ciclo final — que se compara com o shard; com a queda no lugar dela, todo
	// alerta nasceria parecendo já resolvido, e em silêncio.
	line := func(prefix string) string {
		for _, l := range strings.Split(got, "\n") {
			if strings.HasPrefix(l, prefix) {
				return l
			}
		}
		return ""
	}
	if l := line("- Parou em:"); !strings.Contains(l, a.LostAt.Format(time.RFC3339)) {
		t.Errorf("a linha da queda não traz LostAt: %q", l)
	}
	if l := line("- Alerta publicado em:"); !strings.Contains(l, a.At.Format(time.RFC3339)) {
		t.Errorf("a linha de publicação não traz At: %q", l)
	}
}

// Sem aparelho pareado (ou sem horário conhecido) o alerta ainda precisa sair
// legível: ele é escrito justamente quando as coisas deram errado.
func TestAlertToleraCamposVazios(t *testing.T) {
	got := string(MarshalAlert(Alert{Host: "casa", Reason: "motivo", Fix: "conserte"}, time.UTC))

	if strings.Contains(got, "Aparelho:") {
		t.Error("linha de aparelho apareceu sem JID")
	}
	if !strings.Contains(got, "desconhecido") {
		t.Error("horário zerado não virou 'desconhecido'")
	}
	if !strings.Contains(got, "`casa`") {
		t.Error("o alerta não identifica a máquina")
	}
}
