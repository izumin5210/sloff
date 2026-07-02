# ADR-0004: Spec 検証と output 重複検出のポリシー

## Status

Accepted

- Amended by [ADR-0017](./0017-barrier-tasks.md): `barrier: true` の task 種別が新設され、 必須フィールド ( `cmd` / `inputs` / `outputs` / `tools`) の要求は非 barrier task のみに適用される ( barrier task ではこれらの宣言が逆に load 時 error)

## Context

[ADR-0002](./0002-fingerprint-hit-decision-model.md) で決まった「output-comparison」の fingerprint 判定が正しく機能するためには、 fingerprint key を構成する各 component が **意味のある値** で埋められている必要がある。 また、 同一 output path を複数 task が produce する spec は fingerprint 依存配線が壊れるため、 早期に検出して fail させたい。

Codex のアドバサリアルレビューで以下 3 点の方針判断を求められた:

1. **`tools:` 未宣言 spec の扱い** — resolved_versions_hash が空のまま fingerprint key に混ざると、 generator binary upgrade (例: `buf` 1.x → 2.x) で fingerprint invalidate されない
2. **output pattern が 0 ファイルにマッチした場合の扱い** — generator が exit 0 で何も書かなかったケースをどう拾うか
3. **同一 output path を複数 task が produce する spec の検出方法** — 静的 (実行前) か事後 (実行時) か

本 ADR ではこの 3 点の方針を確定する。

### References

- [ADR-0001: fingerprint ベースのコード生成オーケストレーターの選定](./0001-fingerprint-aware-codegen-orchestrator-decision.md)
- [ADR-0002: fingerprint hit 判定モデル](./0002-fingerprint-hit-decision-model.md)

## Decision

### D1. `tools:` を spec の必須フィールドにする

`spec.validate` で `tools:` が空 / 未宣言の command を **拒否** する。 spec parse 段階で fail させ、 runner / registry のフォールバック経路 (= 警告のみで fingerprint 続行) は撤去する。

### D2. output pattern 検証は union semantics

実行後に declared output pattern を再展開し、 **全 pattern 合計が 0 ファイルのときだけ** fail させる。 個別 pattern が 0 マッチでも、 他 pattern が何かを produce していれば成功扱い。

### D3. 重複 output 検出は事後検知 (実行時集計) に倒す

`Runner` が各 task の resolved output paths を `producedBy map[string]string` に累積し、 **同一パスを別 task が produce した瞬間に fail** させる。 spec 文字列レベルでの静的解析 (両 glob 同士の overlap 判定など) は実装しない。 既存の `depgraph.Build` の重複検出 (実ファイル展開後の比較) はそのまま残す。

## Rationale

### D1: `tools:` 必須化

- sloff はコード生成 orchestrator であり、 何らかの generator binary を呼び出すことが前提。 spec に対応する tool が **存在しない** ケースは構造的に想定しづらい
- resolved_versions_hash が空のまま fingerprint key に混ざると、 binary 更新が fingerprint invalidate に効かず、 stale な generation 結果が serve され続ける。 これは ADR-0002 で確定した output-comparison の前提を破る
- 「警告だけ出して fingerprint を保存する」フォールバック経路は、 安全機構として機能しない (record はそのまま残り、 次回 fingerprint hit する) ため撤去
- pre-1.0 でユーザー spec の互換性ブレイクは許容範囲

### D2: union semantics

- output entry は **pattern** であり、 input や feature flag 次第で 0 ファイルに展開されるのは正当な generator 挙動
- 例: `outputs: ["**/*.pb.go", "**/*.connect.go"]` で connect.go を生成しない構成、 plugin を選んで生成物の一部だけが出るケース
- per-pattern 必須化は false positive で「成功した run を fail」させる事故が発生しやすい
- 一方、 元々防ぎたかった「generator が exit 0 で何も書かない」ケースは **全 pattern 合計 0** で検出できるため、 union check で意図は保たれる

### D3: 事後検知優先

- 静的解析の理想形は「2 つの output pattern が overlap しうるか」を spec から判定すること。 ただし両方 glob のケース (例: `**/*.go` と `*/*.go`) の包含判定は半解決問題に近く、 完全実装は過剰投資
- 一方、 sloff の典型ユースで何が問題になるかを場合分けすると以下のとおり:
  - 生成物を git commit するスタイル: 既存の `depgraph.Build` (実ファイル展開後の重複検出) で初回から検出可能
  - 生成物を gitignore するスタイル / 削除後の再生成: 実ファイルが空なので静的検出は機能しないが、 **実行時に 2 つ目の task が produce した瞬間に runner で集計検出可能**
- 事後検知は silent corruption を防ぐ (= 必ずエラーで止まり、 メッセージで両 task 名を提示する) ため、 ユーザーは spec を修正できる
- 実装も `map[string]string` 1 つ追加するだけで完結し、 静的 overlap 解析の数百行に対して圧倒的に小さい

## Consequences

### 正の影響

- fingerprint の正しさの最低保証が明確化される: tool version が必ず key に含まれ、 出力衝突は必ずエラーになる
- 「警告だけで先に進む」グレーな経路がなくなり、 fingerprint を信頼できる
- 実装が小さく保たれる (静的 overlap 解析を持ち込まない)

### 負の影響 / 注意点

- D1 により、 `tools:` を書いていない既存 spec は parse error になる。 pre-1.0 のため許容
- D2 により、 「declared した pattern が必ず 1 ファイル以上 produce する」という強い保証は失われる。 将来 `required_outputs` のような明示宣言を追加する余地は残す
- D3 は **1 度目の上書きが起きてからエラー** になる。 ファイルは git diff に現れるため silent ではないが、 完全な事前ガードではない。 両 glob 同士の overlap (例: `**/*.go` と `*/*.go`) は事前検出されない
- 将来、 ユースケースから「事前検出が必要」が示唆されたら、 文字列一致 + literal × glob マッチによる軽量な静的検出を追加する余地はある
