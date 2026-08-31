// Package merge resolve o problema de várias máquinas escrevendo no mesmo
// destino em nuvem.
//
// O desenho evita o conflito em vez de tentar resolvê-lo:
//
//  1. Cada máquina só escreve o SEU shard (shards/<host>.jsonl). Duas máquinas
//     nunca escrevem no mesmo arquivo, então não existe "lost update".
//  2. Antes de publicar, a máquina funde o seu shard remoto com o que tem
//     localmente. Se o banco local foi apagado ou o pareamento refeito, o que
//     já estava na nuvem não é perdido.
//  3. A visão consolidada (latest/) é sempre construída lendo TODOS os shards.
//     A operação é idempotente e comutativa, então dois merges simultâneos
//     convergem para o mesmo resultado.
//  4. Antes de publicar o consolidado há três travas: um lease cooperativo,
//     a exigência de que todo shard conhecido tenha sido lido com sucesso, e a
//     monotonicidade do timestamp da mensagem mais recente.
package merge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bmar13/wapp-summarizer/internal/config"
	"github.com/bmar13/wapp-summarizer/internal/export"
	"github.com/bmar13/wapp-summarizer/internal/remote"
)

const (
	shardsDir    = "shards"
	latestDir    = "latest"
	leasePath    = "latest/.merge-lease.json"
	messagesFile = "latest/messages.jsonl"
	digestFile   = "latest/digest.md"
	indexFile    = "latest/index.json"
)

// guideFiles são os caminhos do guia para agentes.
//
// Dois nomes porque servem a leitores diferentes: LEIA-ME.md é óbvio para uma
// pessoa que abre a pasta, e AGENTS.md é a convenção que várias ferramentas de
// agente carregam sozinhas quando o diretório entra no contexto.
//
// Dois lugares porque a raiz é onde `shards/` fica visível — e somar shards,
// que duplica mensagens, é o erro mais fácil de cometer aqui.
var guideFiles = map[string]export.GuideLocation{
	"LEIA-ME.md":        export.GuideAtRoot,
	"AGENTS.md":         export.GuideAtRoot,
	"latest/LEIA-ME.md": export.GuideInLatest,
	"latest/AGENTS.md":  export.GuideInLatest,
}

func shardData(host string) string { return shardsDir + "/" + host + ".jsonl" }
func shardMeta(host string) string { return shardsDir + "/" + host + ".meta.json" }

// Combine funde dois conjuntos de registros. Para a mesma mensagem vence a
// versão de maior Prio; em empate, a de texto mais longo (mais informação).
// A operação é comutativa e idempotente.
func Combine(sets ...[]export.Record) []export.Record {
	best := make(map[string]export.Record)
	for _, set := range sets {
		for _, r := range set {
			cur, ok := best[r.Key()]
			if !ok || better(r, cur) {
				best[r.Key()] = r
			}
		}
	}
	out := make([]export.Record, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	export.Sort(out)
	return out
}

func better(candidate, current export.Record) bool {
	if candidate.Prio != current.Prio {
		return candidate.Prio > current.Prio
	}
	if len(candidate.Text) != len(current.Text) {
		return len(candidate.Text) > len(current.Text)
	}
	// Desempate determinístico para que máquinas diferentes cheguem ao mesmo
	// resultado a partir dos mesmos dados.
	return candidate.SenderName > current.SenderName
}

// Window descarta o que caiu fora da janela configurada.
func Window(recs []export.Record, from time.Time) []export.Record {
	out := recs[:0:0]
	for _, r := range recs {
		if !r.Timestamp.Before(from) {
			out = append(out, r)
		}
	}
	return out
}

// PublishShard grava o shard desta máquina, fundido com o que já está na nuvem.
func PublishShard(ctx context.Context, cfg *config.Config, be remote.Backend, local []export.Record, deviceJID string, from, now time.Time) (export.ShardMeta, error) {
	meta := export.ShardMeta{
		Host:        cfg.HostID,
		Schema:      export.SchemaVersion,
		DeviceJID:   deviceJID,
		GeneratedAt: now.UTC(),
	}

	merged := local
	if data, err := be.Get(ctx, shardData(cfg.HostID)); err == nil {
		published, skipped := export.UnmarshalJSONL(data)
		if skipped > 0 {
			return meta, fmt.Errorf("shard remoto %s tem %d linha(s) corrompida(s); "+
				"corrija ou apague o arquivo antes de publicar", shardData(cfg.HostID), skipped)
		}
		merged = Combine(local, published)
	} else if !errors.Is(err, remote.ErrNotExist) {
		return meta, fmt.Errorf("lendo shard remoto: %w", err)
	}

	merged = Window(merged, from)
	meta.Messages = len(merged)
	if len(merged) > 0 {
		meta.From = merged[0].Timestamp.UTC()
		meta.To = merged[len(merged)-1].Timestamp.UTC()
	}

	body, err := export.MarshalJSONL(merged)
	if err != nil {
		return meta, err
	}
	if err := be.Put(ctx, shardData(cfg.HostID), body); err != nil {
		return meta, fmt.Errorf("publicando shard: %w", err)
	}

	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return meta, err
	}
	if err := be.Put(ctx, shardMeta(cfg.HostID), append(metaJSON, '\n')); err != nil {
		return meta, fmt.Errorf("publicando meta do shard: %w", err)
	}
	return meta, nil
}

type Result struct {
	Skipped  bool   // true quando a trava impediu a publicação
	Reason   string // motivo, quando Skipped
	Messages int
	Chats    int
	Shards   []export.ShardMeta
}

type lease struct {
	Host  string    `json:"host"`
	Until time.Time `json:"until"`
}

// Consolidate lê todos os shards e publica a visão unificada em latest/.
func Consolidate(ctx context.Context, cfg *config.Config, be remote.Backend, from, now time.Time) (*Result, error) {
	res := &Result{}

	// 1. Lease cooperativo: evita duas máquinas consolidando ao mesmo tempo.
	//    Não é um lock distribuído de verdade — é uma redução de trabalho
	//    redundante. A corretude vem da idempotência do merge, não daqui.
	if data, err := be.Get(ctx, leasePath); err == nil {
		var l lease
		if json.Unmarshal(data, &l) == nil && l.Host != cfg.HostID && now.Before(l.Until) {
			res.Skipped = true
			res.Reason = fmt.Sprintf("consolidação em andamento por %q até %s",
				l.Host, l.Until.Local().Format(time.RFC3339))
			return res, nil
		}
	} else if !errors.Is(err, remote.ErrNotExist) {
		return nil, fmt.Errorf("lendo lease: %w", err)
	}

	held, _ := json.Marshal(lease{Host: cfg.HostID, Until: now.Add(time.Duration(cfg.Merge.LeaseMinutes) * time.Minute).UTC()})
	if err := be.Put(ctx, leasePath, held); err != nil {
		return nil, fmt.Errorf("gravando lease: %w", err)
	}
	// O lease cobre apenas a duração do trabalho. Liberar em TODA saída,
	// inclusive quando uma trava aborta: senão um shard temporariamente
	// ausente congelaria a consolidação de todas as máquinas por lease_minutes.
	defer func() {
		free, _ := json.Marshal(lease{Host: cfg.HostID, Until: now.UTC()})
		_ = be.Put(ctx, leasePath, free)
	}()

	// 2. Índice anterior: define quais shards DEVEM estar legíveis agora.
	var prev export.Index
	hasPrev := false
	if data, err := be.Get(ctx, indexFile); err == nil {
		if json.Unmarshal(data, &prev) == nil {
			hasPrev = true
		}
	} else if !errors.Is(err, remote.ErrNotExist) {
		return nil, fmt.Errorf("lendo índice anterior: %w", err)
	}

	// 3. Lê todos os shards presentes.
	names, err := be.List(ctx, shardsDir)
	if err != nil {
		return nil, fmt.Errorf("listando shards: %w", err)
	}

	var (
		sets  [][]export.Record
		metas []export.ShardMeta
		read  = map[string]bool{}
	)
	for _, name := range names {
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		host := strings.TrimSuffix(name, ".jsonl")

		meta := export.ShardMeta{Host: host}
		if md, err := be.Get(ctx, shardMeta(host)); err == nil {
			_ = json.Unmarshal(md, &meta)
		}

		// Schema incompatível significa que aquela máquina roda um binário de
		// outra geração. Fundir formatos diferentes produziria um consolidado
		// silenciosamente errado — em especial porque Record.Prio, calculado
		// por fórmulas distintas, continuaria "funcionando". Melhor travar e
		// dizer exatamente qual máquina precisa ser atualizada.
		if !export.SchemaCompatible(meta.Schema) {
			res.Skipped = true
			res.Reason = fmt.Sprintf("shard %q usa o formato %q e este binário usa %q; "+
				"atualize a máquina desatualizada. Mantendo o consolidado anterior",
				host, meta.Schema, export.SchemaVersion)
			return res, nil
		}

		data, err := be.Get(ctx, shardData(host))
		if err != nil {
			// Um shard ilegível é exatamente o caso em que consolidar
			// significaria publicar uma visão incompleta. Abortamos.
			res.Skipped = true
			res.Reason = fmt.Sprintf("shard %q ilegível (%v); mantendo o consolidado anterior", host, err)
			return res, nil
		}
		recs, skipped := export.UnmarshalJSONL(data)
		if skipped > 0 {
			res.Skipped = true
			res.Reason = fmt.Sprintf("shard %q tem %d linha(s) corrompida(s); mantendo o consolidado anterior", host, skipped)
			return res, nil
		}
		read[host] = true
		sets = append(sets, recs)

		meta.Messages = len(recs)
		metas = append(metas, meta)
	}

	// 4. Trava de completude: todo shard que participou do consolidado
	//    anterior precisa estar legível agora.
	if cfg.Merge.GuardMonotonic && hasPrev {
		for _, s := range prev.Shards {
			if !read[s.Host] {
				res.Skipped = true
				res.Reason = fmt.Sprintf("shard %q participou do consolidado anterior mas sumiu; "+
					"mantendo o consolidado anterior", s.Host)
				return res, nil
			}
		}
	}

	merged := Window(Combine(sets...), from)
	idx := export.BuildIndex(cfg, merged, from, now, cfg.HostID)
	idx.Shards = metas

	// 5. Trava de monotonicidade: o consolidado novo nunca pode ter uma
	//    mensagem mais recente ANTERIOR à do consolidado publicado.
	//    (A contagem total pode cair legitimamente — mensagens saem da janela.)
	if cfg.Merge.GuardMonotonic && hasPrev &&
		!prev.LastMessageTS.IsZero() && idx.LastMessageTS.Before(prev.LastMessageTS) {
		res.Skipped = true
		res.Reason = fmt.Sprintf("mensagem mais recente regrediria de %s para %s; mantendo o consolidado anterior",
			prev.LastMessageTS.Local().Format(time.RFC3339), idx.LastMessageTS.Local().Format(time.RFC3339))
		return res, nil
	}

	jsonl, err := export.MarshalJSONL(merged)
	if err != nil {
		return nil, err
	}
	if err := be.Put(ctx, messagesFile, jsonl); err != nil {
		return nil, fmt.Errorf("publicando %s: %w", messagesFile, err)
	}
	if cfg.HasFormat("markdown") {
		if err := be.Put(ctx, digestFile, export.MarshalMarkdown(idx, merged, time.Local)); err != nil {
			return nil, fmt.Errorf("publicando %s: %w", digestFile, err)
		}
	}
	idxJSON, err := export.MarshalIndex(idx)
	if err != nil {
		return nil, err
	}
	if err := be.Put(ctx, indexFile, idxJSON); err != nil {
		return nil, fmt.Errorf("publicando %s: %w", indexFile, err)
	}
	for path, where := range guideFiles {
		if err := be.Put(ctx, path, export.MarshalGuide(idx, where, time.Local)); err != nil {
			return nil, fmt.Errorf("publicando %s: %w", path, err)
		}
	}

	res.Messages = len(merged)
	res.Chats = len(idx.Chats)
	res.Shards = metas
	return res, nil
}
