# ADR-0022: task 自身の outputs を input hash から除く

## Status

Accepted

## Context

### 背景

pnpm-local resolver は、 宣言された workspace package 配下の git 管理ファイルをすべて ExtraInputs として返す ( [resolver-pnpm-local.md](../design/resolver-pnpm-local.md))。 生成物を commit する運用 ( [ADR-0002](./0002-fingerprint-hit-decision-model.md) 前提) で、 **生成先の package 自身を pnpm-local の tool に宣言した task** は、 自分の committed outputs も入力に含めることになる。 「 生成物が置かれる package のソース全体を入力にしたい」 ( inputs を手で列挙すると漏れが古い生成物の見逃しに直結する) という動機で実際に使われている形である。

input hash は cmd 実行前のツリーで計算され、 record はその hash を key に書かれる。 outputs が書き換わる run では次のことが起きる:

1. 1 回目: 古い outputs を含む input hash で record を書く。 その後のツリーでは outputs が新しくなっている
2. 現ツリーの input hash は 1 回目の key と一致しないため、 次の `sloff run` は record miss で再実行し、 `sloff check` ( [ADR-0021](./0021-check-command-for-ci-drift-verification.md)) は record miss の drift を報告する
3. 2 回目の run で初めて「 新しい outputs を含む input hash」 の record が書かれ、 落ち着く

つまり「 1 回 run すれば check が緑になる」 が成り立たない。 正しく生成して commit した人にも check が「 gen 忘れ or record commit 忘れ」 を報告し、 CI は毎回もう 1 回 generator を回す。

同じことは宣言 inputs の glob ( `**/*` など) が自分の outputs にも一致する場合にも起きる。

### 自分の outputs は入力として意味を持たない

sloff は generator を「 宣言された inputs から outputs を決める純粋な関数」 として扱う ( [architecture.md](../design/architecture.md) の overlap 検証の節)。 決定的な generator にとって、 実行前に置いてあった自分の outputs は結果に影響しない。 実行前の outputs に結果が依存する generator は冪等ではなく、 もともと sloff のスコープ外である。 また実行前の outputs の正しさは、 input hash ではなく output-comparison ( ADR-0002) の 2 段目がすでに検証している。

したがって自分の outputs を input hash に含めても、 invalidate の健全性は何も増えず、 上記の不安定さだけが残る。

## Considered Options

### Option 1: pnpm-local resolver が返す ExtraInputs から、 その task の outputs を除く

resolver は task を知らない ( tool は複数 task から参照される) ため、 除外は runner 側でしかできない。 また pnpm-local に限定する理由がなく、 go-local ・ 今後の resolver ・ 宣言 inputs の glob で同じ問題が残る。

### Option 2: tool / task に除外 path を設定できるようにする

spec にすでにある `outputs` を利用者にもう一度書かせる二重管理になる。 書き漏らせば同じ問題が黙って起きる。

### Option 3: 実行後のツリーで input hash を計算し直して record を書く

1 回で落ち着くが、 実行前と実行後で key が分かれる。 outputs を手で書き換えた状態が「 output mismatch」 ではなく「 record miss」 に分類されるようになり、 ADR-0002 の 2 段判定と ADR-0021 の drift 分類が崩れる。

### Option 4: runner が全 task の入力から、 その task 自身の outputs pattern に一致するファイルを除く ( 採用)

宣言 inputs と resolver の contribution を合わせた後、 hash 計算の前に除く。 spec の変更は要らない。

## Decision

**Option 4 を採用する。**

- **D1**: task の入力集合 ( 宣言 inputs の展開結果 ∪ tools の ExtraInputs) から、 その task 自身の `outputs` pattern ( spec dir 基準) に一致するファイルを除いたものを input hash に使う。 除外はファイルの存在に依存せず pattern 照合で行うため、 clean state でも結果は変わらない。 run ・ check ・ deferred tool の再解決 ( ADR-0019 D4) のすべてが同じ集合を使う
- **D2**: 除くのは **自分の** outputs だけ。 他 task の outputs は従来どおり入力に残り、 上流の変更で下流を invalidate する経路 ( ADR-0013) は変わらない
- **D3**: resolver の contribution との重なりは黙って除く ( 利用者は resolver の列挙を制御できない)。 宣言 inputs の glob との重なりは除いたうえで warning を出す ( spec の書き間違いである可能性が高い)。 error にはしない ( `**/*` のような広い glob を使う既存 spec を壊さないため)

## Consequences

### 正の影響

- 生成先の package 自身を pnpm-local の tool にした task でも、 1 回の run で record が落ち着き、 直後の check が緑になる
- CI の gen ジョブで、 正しく commit された PR に対する generator の再実行が無くなる

### 負の影響

- 自分の outputs を入力に含んでいた task は input hash が変わるため、 導入後の初回 run で 1 度だけ record miss になる ( record の書き直しが発生する)
- 冪等でない generator ( 自分の前回 outputs を読んで結果を変える) は、 その依存が input hash に乗らなくなる。 ただしこれは従来からスコープ外であり、 従来も 2 回目以降で結果が揺れていた
