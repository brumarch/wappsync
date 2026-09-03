package merge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bmar13/wappsync/internal/config"
	"github.com/bmar13/wappsync/internal/export"
	"github.com/bmar13/wappsync/internal/remote"
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

	// Máquina B foi atualizada para um formato futuro. A versão é derivada da
	// atual de propósito: escrita à mão, ela vira a versão corrente no próximo
	// bump e o teste passa a não testar nada.
	h.writeShard(t, "maquina-b",
		export.ShardMeta{Host: "maquina-b", Schema: export.SchemaVersion + "-futuro", GeneratedAt: h.now.UTC()},
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

// Shard sem o campo Schema é tratado como wappsync/1, e daí segue a
// regra normal de compatibilidade.
//
// Enquanto o binário publicava /1, isso significava aceitar — era o ponto do
// campo, evitar um flag day na introdução dele. Com o binário em /2 significa
// recusar, e recusar é o comportamento certo: aquele shard traz Prio calculado
// por uma fórmula sem o bit de anexo, então fundir escolheria a versão errada
// da mensagem sem dar sinal nenhum. O preço é ter que atualizar as máquinas
// juntas, e é esse o preço que A4 escolheu pagar.
func TestLegacyShardIsTreatedAsSchemaOne(t *testing.T) {
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

	if export.SchemaCompatible("") {
		// legacySchema == SchemaVersion: o shard entra normalmente.
		if res.Skipped {
			t.Fatalf("shard legado foi recusado: %s", res.Reason)
		}
		if res.Messages != 1 {
			t.Errorf("consolidou %d mensagens, queria 1", res.Messages)
		}
		return
	}

	if !res.Skipped {
		t.Fatal("shard de formato anterior foi fundido em silêncio")
	}
	if !strings.Contains(res.Reason, "maquina-antiga") || !strings.Contains(res.Reason, "atualize") {
		t.Errorf("o motivo não diz qual máquina atualizar: %q", res.Reason)
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

// ---------------------------------------------------------------- anexos ---

// recMedia é um registro que aponta para um anexo publicado.
func recMedia(id, chat, media string, ts time.Time) export.Record {
	r := rec(id, chat, "[documento: x.pdf]", ts, 311)
	r.Kind = "document"
	r.Media = media
	return r
}

// writeLocalMedia simula o que o worker de download deixa no disco.
func writeLocalMedia(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPublishMediaUploadsWhatTheShardReferences(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	local := filepath.Join(t.TempDir(), "media")
	ts := h.now.Add(-time.Hour)

	writeLocalMedia(t, local, "abc.pdf", "conteúdo do pdf")
	if _, err := PublishShard(ctx, h.a, h.beA,
		[]export.Record{recMedia("A1", "g@g.us", export.MediaFile("maquina-a", "abc.pdf"), ts)},
		"", h.from, h.now); err != nil {
		t.Fatal(err)
	}

	res, err := PublishMedia(ctx, h.a, h.beA, local, h.from)
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded != 1 || res.Pruned != 0 || res.Missing != 0 {
		t.Fatalf("resultado = %+v, queria 1 publicado", res)
	}

	got, err := h.beA.Get(ctx, export.MediaFile("maquina-a", "abc.pdf"))
	if err != nil {
		t.Fatalf("anexo não foi publicado: %v", err)
	}
	if string(got) != "conteúdo do pdf" {
		t.Errorf("conteúdo publicado = %q", got)
	}

	// Idempotente: rodar de novo não republica nem apaga.
	again, err := PublishMedia(ctx, h.a, h.beA, local, h.from)
	if err != nil {
		t.Fatal(err)
	}
	if again.Uploaded != 0 || again.Pruned != 0 {
		t.Errorf("segunda rodada mexeu no destino: %+v", again)
	}
}

// Anexo que saiu da janela sai da pasta: sem isso a pasta da nuvem cresce para
// sempre, que é o motivo de Delete existir na interface.
func TestPublishMediaPrunesWhatLeftTheWindow(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	local := filepath.Join(t.TempDir(), "media")

	writeLocalMedia(t, local, "velho.pdf", "antigo")
	if _, err := PublishShard(ctx, h.a, h.beA,
		[]export.Record{recMedia("A1", "g@g.us", export.MediaFile("maquina-a", "velho.pdf"), h.now.Add(-time.Hour))},
		"", h.from, h.now); err != nil {
		t.Fatal(err)
	}
	if _, err := PublishMedia(ctx, h.a, h.beA, local, h.from); err != nil {
		t.Fatal(err)
	}

	// A janela avança e a mensagem cai fora; o shard é republicado sem ela.
	from := h.now.Add(time.Hour)
	if _, err := PublishShard(ctx, h.a, h.beA, nil, "", from, h.now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	res, err := PublishMedia(ctx, h.a, h.beA, local, from)
	if err != nil {
		t.Fatal(err)
	}
	if res.Pruned != 1 {
		t.Fatalf("resultado = %+v, queria 1 removido", res)
	}
	if _, err := h.beA.Get(ctx, export.MediaFile("maquina-a", "velho.pdf")); !errors.Is(err, remote.ErrNotExist) {
		t.Errorf("anexo fora da janela continua publicado (err = %v)", err)
	}
}

// Trava. A poda é a única operação destrutiva do programa. Ela só pode alcançar
// media/<host_id>/ — um shard alheio referencia anexos que aquela máquina ainda
// precisa, e apagá-los seria destruir dado de outro em cima de uma decisão
// tomada com informação incompleta.
func TestPublishMediaNeverTouchesAnotherHost(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	localA := filepath.Join(t.TempDir(), "a")
	ts := h.now.Add(-time.Hour)

	// Máquina B publica um anexo e some.
	localB := filepath.Join(t.TempDir(), "b")
	writeLocalMedia(t, localB, "deB.pdf", "arquivo da B")
	if _, err := PublishShard(ctx, h.b, h.beB,
		[]export.Record{recMedia("B1", "g@g.us", export.MediaFile("maquina-b", "deB.pdf"), ts)},
		"", h.from, h.now); err != nil {
		t.Fatal(err)
	}
	if _, err := PublishMedia(ctx, h.b, h.beB, localB, h.from); err != nil {
		t.Fatal(err)
	}

	// Máquina A publica um shard SEM anexo nenhum e roda a poda.
	if _, err := PublishShard(ctx, h.a, h.beA,
		[]export.Record{rec("A1", "g@g.us", "só texto", ts, 111)}, "", h.from, h.now); err != nil {
		t.Fatal(err)
	}
	res, err := PublishMedia(ctx, h.a, h.beA, localA, h.from)
	if err != nil {
		t.Fatal(err)
	}
	if res.Pruned != 0 {
		t.Errorf("a máquina A apagou %d arquivo(s) que não eram dela", res.Pruned)
	}

	if _, err := h.beA.Get(ctx, export.MediaFile("maquina-b", "deB.pdf")); err != nil {
		t.Errorf("o anexo da máquina B sumiu: %v", err)
	}
}

// Trava. Record.Media vem de um JSONL que mora na pasta compartilhada — dado de
// fora. Um caminho com ".." transformaria a poda num Delete no consolidado.
func TestMediaNameForRejectsEscapes(t *testing.T) {
	prefix := mediaPrefix("maquina-a")

	if name, ok := mediaNameFor(prefix, prefix+"/abc.jpg"); !ok || name != "abc.jpg" {
		t.Fatalf("caminho legítimo recusado: %q / %v", name, ok)
	}

	hostis := []string{
		prefix + "/../../latest/index.json",
		prefix + "/../maquina-b/deB.pdf",
		prefix + "/sub/abc.jpg",
		prefix + "/..",
		prefix + "/",
		prefix,
		"media/maquina-b/deB.pdf",
		"latest/index.json",
		"",
	}
	for _, rel := range hostis {
		if name, ok := mediaNameFor(prefix, rel); ok {
			t.Errorf("caminho %q foi aceito como o anexo %q", rel, name)
		}
	}
}

// O arquivo recém-baixado ainda não está em shard nenhum: o worker grava e só
// depois aponta a mensagem para ele. Podar por referência apenas apagaria
// justamente o que acabou de chegar.
func TestPublishMediaKeepsRecentLocalFiles(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	local := filepath.Join(t.TempDir(), "media")

	novo := writeLocalMedia(t, local, "recem-baixado.pdf", "acabou de chegar")
	velho := writeLocalMedia(t, local, "esquecido.pdf", "de outra era")
	antigo := h.from.Add(-24 * time.Hour)
	if err := os.Chtimes(velho, antigo, antigo); err != nil {
		t.Fatal(err)
	}

	if _, err := PublishShard(ctx, h.a, h.beA, nil, "", h.from, h.now); err != nil {
		t.Fatal(err)
	}
	if _, err := PublishMedia(ctx, h.a, h.beA, local, h.from); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(novo); err != nil {
		t.Errorf("arquivo recém-baixado foi apagado antes de ser publicado: %v", err)
	}
	if _, err := os.Stat(velho); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("arquivo antigo e sem referência continua no disco (err = %v)", err)
	}
}
