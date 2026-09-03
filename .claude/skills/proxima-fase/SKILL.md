---
name: proxima-fase
description: Executa um item do backlog de PLANO-APRIMORAMENTOS.md do wappsync, do planejamento ao commit. Use quando o usuário pedir para avançar o backlog, rodar a próxima fase, ou executar um item por id (ex. "/proxima-fase B4").
---

# Próxima fase

Executa um item do backlog em `PLANO-APRIMORAMENTOS.md`, do planejamento ao
commit, mantendo o plano atualizado.

## 1. Escolher o item

Leia `PLANO-APRIMORAMENTOS.md`.

- Se o usuário passou um id (`B4`), é esse.
- Se passou uma descrição, case com o item mais próximo e **confirme antes de
  começar** — nunca adivinhe em silêncio.
- Sem argumento: proponha o primeiro pendente, ordenando por impacto e depois
  por esforço. Diga qual escolheu e por quê, em uma linha.

Se o backlog não tiver pendentes, diga isso e pare. Não invente trabalho.

## 2. Avaliar antes de codar

Leia `CLAUDE.md` — em especial os dois invariantes — e o código que o item toca.

Depois **verifique se o item ainda faz sentido**. O backlog envelhece: o
problema pode já ter sido resolvido, ou a premissa pode não valer mais. Se for o
caso, diga isso e proponha atualizar o plano em vez de executar por obediência.

Antes de escrever código, diga em 2–3 linhas o que vai fazer. Se o item tiver
uma decisão que muda materialmente o resultado — sobretudo privacidade, ou
qualquer coisa que amplie o que o programa faz na conta do WhatsApp — pergunte
ao usuário em vez de escolher sozinho.

## 3. Implementar

Siga as convenções do `CLAUDE.md`: pt-BR, comentário explica o porquê, código
parecido com o que já existe ao redor.

Dois pontos de atenção que valem mais que o resto:

- **Se tocar em `internal/wa`**, prefira extrair lógica nova para uma função
  pura e testá-la com protobufs montados à mão, em vez de enterrá-la no handler
  de eventos.
- **Se o item exigir uma chamada nova ao cliente do WhatsApp**, isso não é
  detalhe de implementação. Traga para o usuário: diga qual chamada, por que é
  necessária e por que continua sendo leitura. Só então adicione à allowlist de
  `readonly_test.go`, com justificativa no mapa.

## 4. Verificar

```bash
gofmt -l . && go vet ./... && go test ./... -count=1
```

Tudo tem que passar. Se o item mudou o formato do digest de propósito, regrave o
golden (`go test ./internal/export/ -run TestDigestGolden -update`) e **revise o
diff** — um golden regravado sem leitura não protege nada.

**Se o item criou um teste de trava, valide por mutação**: quebre
deliberadamente o que ele protege, confirme que ele falha apontando o problema,
e restaure. Um teste de trava que passa nas duas situações é pior que nenhum.
Mostre o resultado dessa checagem ao usuário — é a evidência de que o teste
serve para algo.

Lembre: `-race` não roda nesta máquina (exige cgo). Quem cobre isso é a CI.

## 5. Fechar

1. Mova o item para a tabela **Concluídos** do `PLANO-APRIMORAMENTOS.md`, com
   uma coluna de resultado que diga o que mudou de fato (números, não adjetivos).
2. Atualize a linha de cobertura se ela mudou.
3. Se durante o trabalho apareceu um problema novo fora de escopo, **adicione
   como item pendente** em vez de resolver por conta própria.
4. Atualize `CLAUDE.md` apenas se um invariante, comando ou armadilha mudou.
   Ele é lido a cada sessão: crescer sem necessidade o torna ignorado.
5. Commite em branch (nunca direto em `main`), mensagem em pt-BR explicando o
   porquê. Confira `git status` antes — binários e bancos não podem entrar.

## 6. Relatar

Em poucas linhas: o que mudou, a evidência (números de teste e cobertura, e o
resultado da checagem por mutação se houve), e o que **não** foi feito e por quê.

Se algo ficou pela metade, diga explicitamente. Não relate como concluído um
item cuja verificação você não rodou.
