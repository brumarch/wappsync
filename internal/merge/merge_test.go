package merge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bmar13/wapp-summarizer/internal/config"
	"github.com/bmar13/wapp-summarizer/internal/export"
	"github.com/bmar13/wapp-summarizer/internal/remote"
)

// harness monta duas "máquinas" apontando para a mesma pasta de nuvem.
type harness struct {
	drive string
	a, b  *config.Config
	beA   remote.Backend
	beB   remote.Backend
	now   time.Time
	from  time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	drive := t.TempDir()

	mk := func(host string) (*config.Config, remote.Backend) {
		cfg := &config.Config{
			WindowDays: 3,
			HostID:     host,
			Export:     config.Export{Formats: []string{"jsonl", "markdown"}},
			Remote: config.Remote{
				Backend: "folder",
				Prefix:  "wapp",
				Folder:  config.FolderRemote{Path: drive},
			},
			Merge: config.Merge{Enabled: true, GuardMonotonic: true, LeaseMinutes: 15},
		}
		be, err := remote.New(cfg)
		if err != nil {
			t.Fatalf("remote.New(%s): %v", host, err)
		}
		return cfg, be
	}

	a, beA := mk("maquina-a")
	b, beB := mk("maquina-b")
	now := time.Now()
	return &harness{drive: drive, a: a, b: b, beA: beA, beB: beB, now: now, from: now.Add(-72 * time.Hour)}
}

func rec(id, chat, text string, ts time.Time, prio int) export.Record {
	return export.Record{
		ID: id, Chat: chat, ChatName: "Família", IsGroup: true,
		Sender: "u@s.whatsapp.net", SenderName: "João",
		Timestamp: ts.UTC(), Kind: "text", Text: text, Prio: prio,
	}
}

func (h *harness) readIndex(t *testing.T) export.Index {
	t.Helper()
	data, err := h.beA.Get(context.Background(), "latest/index.json")
	if err != nil {
		t.Fatalf("lendo index: %v", err)
	}
	var idx export.Index
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatalf("index inválido: %v", err)
	}
	return idx
}

func (h *harness) readMessages(t *testing.T) []export.Record {
	t.Helper()
	data, err := h.beA.Get(context.Background(), "latest/messages.jsonl")
	if err != nil {
		t.Fatalf("lendo messages: %v", err)
	}
	recs, skipped := export.UnmarshalJSONL(data)
	if skipped > 0 {
		t.Fatalf("%d linhas corrompidas", skipped)
	}
	return recs
}

// A fusão é comutativa e a maior prioridade vence — é o que garante que duas
// máquinas cheguem ao mesmo resultado independentemente da ordem.
func TestCombineIsCommutativeAndPrioWins(t *testing.T) {
	ts := time.Now()
	low := rec("A1", "g@g.us", "", ts, 10)
	high := rec("A1", "g@g.us", "texto completo", ts, 1110)

	ab := Combine([]export.Record{low}, []export.Record{high})
	ba := Combine([]export.Record{high}, []export.Record{low})

	if len(ab) != 1 || len(ba) != 1 {
		t.Fatalf("dedupe falhou: %d vs %d", len(ab), len(ba))
	}
	if ab[0].Text != "texto completo" || ba[0].Text != "texto completo" {
		t.Errorf("prio menor venceu: %q / %q", ab[0].Text, ba[0].Text)
	}
	if ab[0] != ba[0] {
		t.Error("Combine não é comutativa")
	}

	// Idempotência: aplicar de novo não muda nada.
	again := Combine(ab, ab)
	if len(again) != 1 || again[0] != ab[0] {
		t.Error("Combine não é idempotente")
	}
}

// Duas máquinas com dados parcialmente sobrepostos devem produzir a união.
func TestConsolidateMergesHosts(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	ts := h.now.Add(-time.Hour)

	// A viu A1 e A2; B viu A2 (com metadado melhor) e A3.
	recsA := []export.Record{
		rec("A1", "g@g.us", "primeira", ts, 111),
		rec("A2", "g@g.us", "segunda", ts.Add(time.Minute), 110),
	}
	recsB := []export.Record{
		rec("A2", "g@g.us", "segunda", ts.Add(time.Minute), 111),
		rec("A3", "g@g.us", "terceira", ts.Add(2*time.Minute), 111),
	}

	if _, err := PublishShard(ctx, h.a, h.beA, recsA, "a@s.whatsapp.net", h.from, h.now); err != nil {
		t.Fatalf("shard A: %v", err)
	}
	if _, err := PublishShard(ctx, h.b, h.beB, recsB, "b@s.whatsapp.net", h.from, h.now); err != nil {
		t.Fatalf("shard B: %v", err)
	}

	res, err := Consolidate(ctx, h.a, h.beA, h.from, h.now)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if res.Skipped {
		t.Fatalf("consolidação pulada sem motivo: %s", res.Reason)
	}
	if res.Messages != 3 {
		t.Errorf("consolidou %d mensagens, queria 3", res.Messages)
	}
	if len(res.Shards) != 2 {
		t.Errorf("viu %d shards, queria 2", len(res.Shards))
	}

	got := h.readMessages(t)
	if len(got) != 3 {
		t.Fatalf("messages.jsonl tem %d linhas, queria 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Timestamp.Before(got[i-1].Timestamp) {
			t.Errorf("saída fora de ordem cronológica em %d", i)
		}
	}
}

// Se o shard de outra máquina ainda não sincronizou, consolidar publicaria uma
// visão incompleta. Precisa abortar e preservar o consolidado anterior.
func TestConsolidateAbortsWhenShardDisappears(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	ts := h.now.Add(-time.Hour)

	if _, err := PublishShard(ctx, h.a, h.beA, []export.Record{rec("A1", "g@g.us", "de A", ts, 111)}, "", h.from, h.now); err != nil {
		t.Fatal(err)
	}
	if _, err := PublishShard(ctx, h.b, h.beB, []export.Record{rec("B1", "g@g.us", "de B", ts, 111)}, "", h.from, h.now); err != nil {
		t.Fatal(err)
	}
	if res, err := Consolidate(ctx, h.a, h.beA, h.from, h.now); err != nil || res.Skipped {
		t.Fatalf("consolidação inicial falhou: %v / %+v", err, res)
	}
	before := h.readIndex(t)
	if before.Messages != 2 {
		t.Fatalf("baseline tem %d mensagens, queria 2", before.Messages)
	}

	// O shard de B some: Drive ainda não sincronizou, arquivo em trânsito etc.
	if err := os.Remove(filepath.Join(h.drive, "wapp", "shards", "maquina-b.jsonl")); err != nil {
		t.Fatal(err)
	}

	res, err := Consolidate(ctx, h.a, h.beA, h.from, h.now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if !res.Skipped {
		t.Fatal("consolidou mesmo com shard faltando — publicaria uma visão parcial")
	}

	after := h.readIndex(t)
	if after.Messages != before.Messages || !after.GeneratedAt.Equal(before.GeneratedAt) {
		t.Errorf("consolidado anterior foi alterado: %d/%v -> %d/%v",
			before.Messages, before.GeneratedAt, after.Messages, after.GeneratedAt)
	}
}

// O caso que motivou o projeto: um shard com dados velhos não pode sobrescrever
// um consolidado mais novo.
func TestConsolidateAbortsOnTimeRegression(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	recent := rec("A1", "g@g.us", "mensagem de agora", h.now.Add(-time.Minute), 111)
	if _, err := PublishShard(ctx, h.a, h.beA, []export.Record{recent}, "", h.from, h.now); err != nil {
		t.Fatal(err)
	}
	if res, err := Consolidate(ctx, h.a, h.beA, h.from, h.now); err != nil || res.Skipped {
		t.Fatalf("consolidação inicial falhou: %v / %+v", err, res)
	}
	before := h.readIndex(t)

	// Uma máquina desatualizada reescreve o shard só com dado antigo.
	old := rec("Z9", "g@g.us", "mensagem de ontem", h.now.Add(-24*time.Hour), 111)
	oldJSONL, err := export.MarshalJSONL([]export.Record{old})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.drive, "wapp", "shards", "maquina-a.jsonl"), oldJSONL, 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := Consolidate(ctx, h.a, h.beA, h.from, h.now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if !res.Skipped {
		t.Fatal("consolidou uma regressão temporal — era exatamente o que a trava deve impedir")
	}

	after := h.readIndex(t)
	if !after.LastMessageTS.Equal(before.LastMessageTS) {
		t.Errorf("LastMessageTS regrediu de %v para %v", before.LastMessageTS, after.LastMessageTS)
	}
	if got := h.readMessages(t); len(got) != 1 || got[0].ID != "A1" {
		t.Errorf("messages.jsonl foi sobrescrito com dado antigo: %+v", got)
	}
}

// O shard próprio se auto-recupera: perder o banco local não apaga o que já
// estava publicado por esta mesma máquina.
func TestPublishShardHealsFromRemote(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	ts := h.now.Add(-time.Hour)

	if _, err := PublishShard(ctx, h.a, h.beA,
		[]export.Record{rec("A1", "g@g.us", "antes", ts, 111)}, "", h.from, h.now); err != nil {
		t.Fatal(err)
	}

	// Banco local zerado: só a mensagem nova é conhecida localmente.
	meta, err := PublishShard(ctx, h.a, h.beA,
		[]export.Record{rec("A2", "g@g.us", "depois", ts.Add(time.Minute), 111)}, "", h.from, h.now)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Messages != 2 {
		t.Errorf("shard tem %d mensagens, queria 2 (a antiga deveria ter sido preservada)", meta.Messages)
	}
}

// Um lease válido de outra máquina evita consolidação redundante.
func TestLeaseSkipsOtherHost(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	if _, err := PublishShard(ctx, h.a, h.beA,
		[]export.Record{rec("A1", "g@g.us", "x", h.now.Add(-time.Hour), 111)}, "", h.from, h.now); err != nil {
		t.Fatal(err)
	}

	held, _ := json.Marshal(map[string]any{
		"host":  "maquina-b",
		"until": h.now.Add(10 * time.Minute).UTC(),
	})
	if err := h.beA.Put(ctx, "latest/.merge-lease.json", held); err != nil {
		t.Fatal(err)
	}

	res, err := Consolidate(ctx, h.a, h.beA, h.from, h.now)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped {
		t.Error("ignorou o lease de outra máquina")
	}

	// Depois que o lease expira, consolida normalmente.
	res, err = Consolidate(ctx, h.a, h.beA, h.from, h.now.Add(11*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped {
		t.Errorf("não consolidou após o lease expirar: %s", res.Reason)
	}
}

// Uma consolidação abortada por uma trava não pode deixar o lease retido: isso
// congelaria a consolidação de todas as máquinas até o lease expirar.
func TestAbortedConsolidateReleasesLease(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	ts := h.now.Add(-time.Hour)

	if _, err := PublishShard(ctx, h.a, h.beA, []export.Record{rec("A1", "g@g.us", "de A", ts, 111)}, "", h.from, h.now); err != nil {
		t.Fatal(err)
	}
	if _, err := PublishShard(ctx, h.b, h.beB, []export.Record{rec("B1", "g@g.us", "de B", ts, 111)}, "", h.from, h.now); err != nil {
		t.Fatal(err)
	}
	if res, err := Consolidate(ctx, h.a, h.beA, h.from, h.now); err != nil || res.Skipped {
		t.Fatalf("consolidação inicial falhou: %v / %+v", err, res)
	}

	// A aborta: o shard de B sumiu.
	shardB := filepath.Join(h.drive, "wapp", "shards", "maquina-b.jsonl")
	saved, err := os.ReadFile(shardB)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(shardB); err != nil {
		t.Fatal(err)
	}
	res, err := Consolidate(ctx, h.a, h.beA, h.from, h.now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped {
		t.Fatal("deveria ter abortado")
	}

	// B volta e tenta consolidar logo em seguida, dentro da janela do lease.
	if err := os.WriteFile(shardB, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = Consolidate(ctx, h.b, h.beB, h.from, h.now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped {
		t.Fatalf("lease de uma consolidação abortada bloqueou a outra máquina: %s", res.Reason)
	}
	if res.Messages != 2 {
		t.Errorf("consolidou %d mensagens, queria 2", res.Messages)
	}
}

// writeShard grava um shard cru na nuvem, com o meta que o teste quiser.
// Serve para simular uma máquina rodando outro binário.
func (h *harness) writeShard(t *testing.T, host string, meta export.ShardMeta, recs []export.Record) {
	t.Helper()
	body, err := export.MarshalJSONL(recs)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(h.drive, "wapp", "shards")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, host+".jsonl"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, host+".meta.json"), metaJSON, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPublishShardStampsSchema(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	meta, err := PublishShard(ctx, h.a, h.beA,
		[]export.Record{rec("A1", "g@g.us", "x", h.now.Add(-time.Hour), 111)}, "", h.from, h.now)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Schema != export.SchemaVersion {
		t.Errorf("Schema = %q, queria %q", meta.Schema, export.SchemaVersion)
	}

	// E o schema tem que chegar ao arquivo, não só ao valor devolvido.
	raw, err := h.beA.Get(ctx, "shards/maquina-a.meta.json")
	if err != nil {
		t.Fatal(err)
	}
	var onDisk export.ShardMeta
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.Schema != export.SchemaVersion {
		t.Errorf("meta publicado sem schema: %+v", onDisk)
	}
}

// Uma máquina rodando outro formato não pode ser fundida em silêncio: o
// Record.Prio dela viria de outra fórmula e o merge escolheria errado sem
// dar nenhum sinal.
func TestConsolidateAbortsOnIncompatibleSchema(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	ts := h.now.Add(-time.Hour)

	if _, err := PublishShard(ctx, h.a, h.beA,
		[]export.Record{rec("A1", "g@g.us", "de A", ts, 111)}, "", h.from, h.now); err != nil {
		t.Fatal(err)
	}
	if res, err := Consolidate(ctx, h.a, h.beA, h.from, h.now); err != nil || res.Skipped {
		t.Fatalf("consolidação inicial falhou: %v / %+v", err, res)
	}
	before := h.readIndex(t)

	// Máquina B foi atualizada para um formato futuro.
	h.writeShard(t, "maquina-b",
		export.ShardMeta{Host: "maquina-b", Schema: "wapp-summarizer/2", GeneratedAt: h.now.UTC()},
		[]export.Record{rec("B1", "g@g.us", "de B", ts, 999)})

	res, err := Consolidate(ctx, h.a, h.beA, h.from, h.now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped {
		t.Fatal("fundiu shards de formatos diferentes")
	}
	if !strings.Contains(res.Reason, "maquina-b") || !strings.Contains(res.Reason, "atualize") {
		t.Errorf("o motivo não diz qual máquina atualizar: %q", res.Reason)
	}

	after := h.readIndex(t)
	if after.Messages != before.Messages {
		t.Errorf("consolidado anterior foi alterado: %d -> %d", before.Messages, after.Messages)
	}
}

// Shard publicado antes de o campo Schema existir continua sendo aceito:
// não queremos um flag day que trave a consolidação de todo mundo.
func TestLegacyShardWithoutSchemaIsAccepted(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	ts := h.now.Add(-time.Hour)

	h.writeShard(t, "maquina-antiga",
		export.ShardMeta{Host: "maquina-antiga", GeneratedAt: h.now.UTC()}, // sem Schema
		[]export.Record{rec("V1", "g@g.us", "de uma versão antiga", ts, 111)})

	res, err := Consolidate(ctx, h.a, h.beA, h.from, h.now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped {
		t.Fatalf("shard legado foi recusado: %s", res.Reason)
	}
	if res.Messages != 1 {
		t.Errorf("consolidou %d mensagens, queria 1", res.Messages)
	}
}

// Mensagens fora da janela configurada não são publicadas.
func TestWindowDropsOldMessages(t *testing.T) {
	now := time.Now()
	recs := []export.Record{
		rec("velha", "g@g.us", "x", now.Add(-96*time.Hour), 111),
		rec("nova", "g@g.us", "y", now.Add(-time.Hour), 111),
	}
	got := Window(recs, now.Add(-72*time.Hour))
	if len(got) != 1 || got[0].ID != "nova" {
		t.Errorf("janela não aplicada: %+v", got)
	}
}
