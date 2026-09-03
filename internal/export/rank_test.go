package export

import (
	"testing"
	"time"

	"github.com/bmar13/wappsync/internal/store"
)

// rankedSchema é o SchemaVersion vigente quando a tabela abaixo foi conferida à
// mão. Os dois precisam andar juntos.
//
// store.Message.Rank() vira Record.Prio, que viaja entre máquinas e é comparado
// com o prio de shards produzidos por OUTROS binários. Duas fórmulas diferentes
// produzem números que continuam comparáveis — nada estoura, o merge só passa a
// escolher a versão errada da mensagem, em silêncio. É por isso que mudar a
// fórmula exige bumpar o schema: é a única coisa que faz uma máquina
// desatualizada ser detectada em vez de ignorada.
const rankedSchema = "wappsync/2"

// TestRankFormulaIsPinnedToSchemaVersion trava a fórmula de precedência.
//
// Se você mudou Rank() de propósito: atualize a tabela, bumpe SchemaVersion e
// atualize rankedSchema. Se você não mudou, esta falha é um bug.
func TestRankFormulaIsPinnedToSchemaVersion(t *testing.T) {
	if SchemaVersion != rankedSchema {
		t.Errorf(`SchemaVersion = %q mas a fórmula de Rank() foi conferida contra %q.

Se o bump veio de uma mudança em Rank(), revise a tabela deste teste e atualize
rankedSchema. Se veio de outra coisa (campo novo em Record, por exemplo), basta
atualizar rankedSchema.`, SchemaVersion, rankedSchema)
	}

	full := func(m store.Message) store.Message {
		m.Body = "[imagem]"
		m.Source = "live"
		m.SenderName = "João"
		return m
	}

	cases := []struct {
		name string
		msg  store.Message
		want int
	}{
		{"vazia", store.Message{}, 0},
		{"só corpo", store.Message{Body: "oi"}, 100},
		{"corpo ao vivo", store.Message{Body: "oi", Source: "live"}, 110},
		{"corpo ao vivo com remetente", store.Message{Body: "oi", Source: "live", SenderName: "João"}, 111},
		{"do history sync", store.Message{Body: "oi", Source: "history", SenderName: "João"}, 101},
		{"marcador de mídia, sem anexo", full(store.Message{}), 111},
		{"anexo baixado", full(store.Message{Media: "media/a/ab12.jpg"}), 311},
		{"apagada", full(store.Message{Deleted: true}), 611},
		{"editada", full(store.Message{Revision: 1}), 1111},
		{"editada com anexo", full(store.Message{Revision: 1, Media: "media/a/ab12.jpg"}), 1311},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.msg.Rank(); got != tc.want {
				t.Errorf("Rank() = %d, queria %d — a fórmula mudou; ver o comentário acima", got, tc.want)
			}
		})
	}
}

// A ordem entre os casos é o que o merge realmente usa. A tabela acima trava os
// números; isto trava o significado deles.
func TestRankOrdering(t *testing.T) {
	full := func(m store.Message) store.Message {
		m.Body = "[imagem]"
		m.Source = "live"
		m.SenderName = "João"
		return m
	}

	semAnexo := full(store.Message{})
	comAnexo := full(store.Message{Media: "media/a/ab12.jpg"})
	apagada := full(store.Message{Deleted: true})
	editada := full(store.Message{Revision: 1})

	// O caso que motivou o bit: sem ele as duas empatam, e como o UPSERT é
	// `>` estrito, quem chegou primeiro fica — o download nunca entra.
	if !(comAnexo.Rank() > semAnexo.Rank()) {
		t.Error("a versão com anexo não vence o marcador: o download seria descartado no UPSERT")
	}
	if !(apagada.Rank() > comAnexo.Rank()) {
		t.Error("o anexo venceu uma revogação: a mensagem reapareceria depois de apagada")
	}
	if !(editada.Rank() > comAnexo.Rank()) {
		t.Error("o anexo venceu uma edição: o texto antigo voltaria")
	}
}

// Um registro com anexo tem que continuar vencendo depois de virar Record e
// passar pelo merge — é lá que a versão de outra máquina compete com a nossa.
func TestRecordCarriesMediaAndPrio(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	base := store.Message{
		ID: "M1", ChatJID: "fam@g.us", SenderJID: "joao@s.whatsapp.net",
		SenderName: "João", IsGroup: true, Timestamp: now,
		Kind: "image", Body: "[imagem] praia", Source: "live",
	}
	comAnexo := base
	comAnexo.Media = "media/maquina-b/ab12.jpg"

	recs := Build(baseCfg(), []store.Message{base}, nil, now.Add(-time.Hour))
	comRecs := Build(baseCfg(), []store.Message{comAnexo}, nil, now.Add(-time.Hour))

	if len(recs) != 1 || len(comRecs) != 1 {
		t.Fatalf("Build devolveu %d e %d registros, queria 1 e 1", len(recs), len(comRecs))
	}
	if comRecs[0].Media != comAnexo.Media {
		t.Errorf("Record.Media = %q, queria %q", comRecs[0].Media, comAnexo.Media)
	}
	if recs[0].Media != "" {
		t.Errorf("Record.Media = %q numa mensagem sem anexo", recs[0].Media)
	}
	if !(comRecs[0].Prio > recs[0].Prio) {
		t.Errorf("prio com anexo (%d) não vence a sem anexo (%d); o merge escolheria a errada",
			comRecs[0].Prio, recs[0].Prio)
	}
}
