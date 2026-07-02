# Architecture Decision Records

sloff の設計判断の記録。 ADR は日本語で書く ( コード・コミットメッセージ・PR は英語)。

## 運用ルール

- ファイル名は `NNNN-slug.md`。 番号は採択順の連番で、 欠番・重複を作らない
- 過去の決定を覆す・改訂するときは、 過去の ADR 本文を書き換えるのではなく、 新しい ADR に経緯と判断を記録したうえで **過去側の Status を更新する**。 ADR は「その時点で何をなぜ決めたか」 の記録であり、 本文は immutable に保つ ( 誤字や参照リンクの修正は除く)

## Status

各 ADR は H1 直後に `## Status` セクションを持つ。

| Status | 意味 |
|---|---|
| `Accepted` | 決定は現在も有効 |
| `Superseded by [ADR-NNNN](./NNNN-slug.md)` | 決定全体が後続 ADR で置き換えられた。 本文は歴史的記録としてのみ読む |

決定の**一部だけ**が後続 ADR で改訂された場合は `Accepted` のまま、 何がどう変わったかを `Amended by` の箇条書きで併記する:

```markdown
## Status

Accepted

- Amended by [ADR-0010](./0010-fingerprint-filename-timestamp-prefix.md): R5 ( conflict-free) の担保が write-skip 規約から filename の timestamp prefix ( path uniqueness) へ移行 ( D 単位で何が変わったかをここに書く)
```

読者への契約: **Status セクションだけ読めば「この ADR の決定を今も信じてよいか」 が分かる** 状態を保つ。 後続 ADR を書いて過去の決定に触れたら、 触れられた側の Status 更新を同じ PR に含める。
