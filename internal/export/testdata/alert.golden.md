# ALERTA — a máquina `bruno-win` parou de capturar

Escrito pelo `wapp-summarizer` quando uma máquina perde o pareamento com o
WhatsApp. Enquanto este alerta valer, o histórico publicado tem um buraco:
nada que chegou depois do horário abaixo foi capturado por `bruno-win`.

- Máquina: `bruno-win`
- Aparelho: `5511900000000.0:12@s.whatsapp.net`
- Parou em: 2026-03-15T18:30:00Z
- Motivo: o WhatsApp desvinculou este aparelho
- Alerta publicado em: 2026-03-15T18:31:30Z

## Este alerta ainda vale?

Ele não é apagado sozinho. Compare o `generated_at` de
`shards/bruno-win.meta.json` com o "Alerta publicado em" acima:

- **anterior** → a máquina continua parada e o alerta vale;
- **posterior** → `bruno-win` voltou a publicar e este arquivo é histórico.

## Para quem resume estes dados

Se o alerta ainda vale, **diga no resumo** que `bruno-win` parou de capturar em 2026-03-15T18:30:00Z.
A ausência de mensagens depois desse horário não é prova de que nada aconteceu:
pode ser só o buraco. Se outras máquinas continuaram publicando, a cobertura do
período é parcial, não nula — o que esta máquina veria sozinha é que falta.

## Para quem administra a máquina

Rode `wappsync login` nesta máquina para parear de novo.

O `wappsync run` encerrou ao detectar isto, em vez de continuar de pé sem
capturar nada. Reiniciar o processo não resolve sozinho.
