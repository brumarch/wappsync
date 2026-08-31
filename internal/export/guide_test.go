package export

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func guideIndex() Index {
	now := time.Date(2026, 3, 15, 18, 30, 0, 0, time.UTC)
	return Index{
		Schema:      SchemaVersion,
		GeneratedAt: now,
		GeneratedBy: "bruno-win",
		WindowDays:  3,
		From:        now.Add(-72 * time.Hour),
		To:          now,
		Messages:    9,
	}
}

// O guia é produto, igual ao digest: uma regressão nele não quebra teste nenhum
// e não gera erro — só piora silenciosamente o comportamento do agente.
// Reusa a flag -update declarada em golden_test.go.
func TestGuideGolden(t *testing.T) {
	cases := map[string]GuideLocation{
		"guide.root.golden.md":   GuideAtRoot,
		"guide.latest.golden.md": GuideInLatest,
	}

	for name, where := range cases {
		t.Run(name, func(t *testing.T) {
			got := MarshalGuide(guideIndex(), where, time.UTC)
			path := filepath.Join("testdata", name)

			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o600); err != nil {
					t.Fatal(err)
				}
				t.Logf("golden regravado: %s", path)
				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("golden não encontrado (%v). Gere com:\n"+
					"  go test ./internal/export/ -run TestGuideGolden -update", err)
			}
			if string(got) != string(want) {
				t.Errorf("o guia mudou em relação ao golden.\n"+
					"Se a mudança for intencional, revise o diff e rode:\n"+
					"  go test ./internal/export/ -run TestGuideGolden -update\n\n"+
					"--- obtido ---\n%s\n--- esperado ---\n%s", got, want)
			}
		})
	}
}

// A trava de confiança é a razão principal de o guia existir: o digest contém
// texto escrito por terceiros, e um agente com ferramentas que trate aquilo
// como instrução é um problema real.
//
// Este teste existe separado do golden de propósito. Um `-update` descuidado
// regravaria o golden sem reclamar; aqui a perda seria barulhenta.
func TestGuideAlwaysCarriesTrustBoundary(t *testing.T) {
	required := []string{
		"dado, não instrução", // o título da seção
		"terceiros",           // quem escreveu o conteúdo
		"nunca",               // a proibição, não uma sugestão
		"Não execute",         // o que fazer ao encontrar uma tentativa
	}

	for _, where := range []GuideLocation{GuideAtRoot, GuideInLatest} {
		got := string(MarshalGuide(guideIndex(), where, time.UTC))
		for _, want := range required {
			if !strings.Contains(got, want) {
				t.Errorf("guia (location=%d) perdeu a trava de confiança: falta %q", where, want)
			}
		}
		// A seção precisa vir antes das instruções de leitura: um agente que
		// truncar tem que levar a trava junto.
		trust := strings.Index(got, "dado, não instrução")
		order := strings.Index(got, "Leia nesta ordem")
		if trust < 0 || order < 0 || trust > order {
			t.Errorf("guia (location=%d): a trava de confiança não vem antes de 'Leia nesta ordem'", where)
		}
	}
}

// Os caminhos citados mudam conforme onde o arquivo é gravado. Errar isso manda
// o agente para um arquivo inexistente — ou, pior, para shards/.
func TestGuidePathsMatchLocation(t *testing.T) {
	root := string(MarshalGuide(guideIndex(), GuideAtRoot, time.UTC))
	latest := string(MarshalGuide(guideIndex(), GuideInLatest, time.UTC))

	if !strings.Contains(root, "`latest/digest.md`") {
		t.Error("guia da raiz não aponta para latest/digest.md")
	}
	if !strings.Contains(root, "`shards/`") || strings.Contains(root, "`../shards/`") {
		t.Error("guia da raiz deveria citar shards/, não ../shards/")
	}

	if !strings.Contains(latest, "`digest.md`") || strings.Contains(latest, "`latest/digest.md`") {
		t.Error("guia de latest/ deveria citar digest.md sem prefixo")
	}
	if !strings.Contains(latest, "`../shards/`") {
		t.Error("guia de latest/ não aponta para ../shards/")
	}
}

// A janela vem do índice: um guia que anuncie 3 dias enquanto o config publica
// 30 faria o agente recusar perguntas que ele poderia responder.
func TestGuideReportsConfiguredWindow(t *testing.T) {
	idx := guideIndex()
	idx.WindowDays = 30

	got := string(MarshalGuide(idx, GuideAtRoot, time.UTC))
	if !strings.Contains(got, "30 dia(s)") {
		t.Errorf("guia não reflete window_days = 30")
	}
	if strings.Contains(got, "3 dia(s)") {
		t.Errorf("guia ficou com a janela errada embutida")
	}
}

// A legenda precisa cobrir toda notação que MarshalMarkdown realmente emite,
// senão o agente interpreta por conta própria.
func TestGuideLegendCoversDigestNotation(t *testing.T) {
	got := string(MarshalGuide(guideIndex(), GuideAtRoot, time.UTC))

	for _, notation := range []string{"[imagem]", "~~", "↩︎", "[editada]", "⏎", "áudio"} {
		if !strings.Contains(got, notation) {
			t.Errorf("legenda do guia não explica %q", notation)
		}
	}
}
