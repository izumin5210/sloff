# ADR-0002: fingerprint hit 判定モデル

## Context

### 背景

[ADR-0001](./0001-fingerprint-aware-codegen-orchestrator-decision.md) で、 共有可能な fingerprint 機構を持つコード生成オーケストレーターを **自作する** ことが決定された。 自作する以上、 fingerprint hit 判定の論理そのものを自前で設計する必要がある。

ローカル単独運用の fingerprint store であれば「input が同じなら output も同じ」という deterministic generator の前提を素直に信用すれば足りるが、 **共有モデルでは別の開発者 / 別 OS で生成された record を信頼することになり**、 判定式の選定がそのまま fingerprint の健全性とリスクに直結する。 本 ADR では fingerprint hit の判定モデルそのものを 5 案で比較し、 採用案を確定する。

### 前提

- generator output は git 管理されている前提を採る ( typical な monorepo の運用)。 これは fingerprint 設計の制約というよりは、 「正しい状態」のリファレンスが git に常に存在する事実として効いてくる
- fingerprint の中に artifact 本体を持つかどうかは別の設計判断 ( record の置き場 / artifact 共有要否は [ADR-0003](./0003-fingerprint-storage-strategy.md) で扱う)
- ツールバージョンの整合は preflight (lockfile と install 状態の照合) などで別途担保する想定 (詳細は Design Doc 側で扱う)
- 共有 fingerprint は何らかの形で複数開発者 / CI 間で同じ内容を読み書きできる場所に置かれる

### References

- [ADR-0001: fingerprint ベースのコード生成オーケストレーターの選定](./0001-fingerprint-aware-codegen-orchestrator-decision.md)
- [ADR-0003: fingerprint のストレージ方式](./0003-fingerprint-storage-strategy.md)
- [Design Doc: sloff Architecture](../design/architecture.md)

## Considered Options

### Comparison Table

| | Option 1: input-only | **Option 2: output-comparison (採用)** | Option 3: artifact restore | Option 4: hybrid (input-only + 整合性 CI) | Option 5: input-only + output stat |
|---|---|---|---|---|---|
| 判定式 | `input_hash` 一致のみ | `input_hash` 一致 + record の `output_hash` と現状ツリーの `output_hash` 一致 | `input_hash` 一致 → record から output 復元 | 通常 input-only / 定期 CI でフル再生成比較 | `input_hash` 一致 + output ファイルの `mtime` / `size` 不変 |
| drift 検出 | × | ◎ | ◎ (上書き) | △ (遅延) | △ |
| per-task コスト | 最小 | 中 | 中 | 最小 + CI 負荷 | 小 |
| 共有運用適合性 | △ | ◎ | ◎ | △ | △ |
| artifact 共有要否 | 不要 | 不要 | 必要 | 不要 | 不要 |
| 実装複雑度 | 低 | 中 | 高 | 中 | 低 |

### Option 1: input-only

input_hash 一致のみで fingerprint hit と判定する。 record の output 情報は参照しない (そもそも保持しなくてもよい)。

👍 **Pros**

- per-task コストが最小 (output ファイルの hash 計算が不要)
- 実装が単純
- 個人ローカル単独運用なら deterministic generator の前提とも整合

👎 **Cons**

- 共有モデルでは「別の開発者 / 別 OS で作られた record の信頼性」を補完する手段がない
- 共有 record 取得後に手元で起きうる drift を一切検出できない
  - PR レビュー中に手で output を一時的に修正した
  - `go fix` / `goimports` / lefthook 等が走って output が書き換わった
  - 別ブランチの中途状態を残したまま checkout した
  - fingerprint だけ pull してまだ output 側を pull しきれていない
- 上記 drift があると正しくない状態のまま skip され、 後段のビルド / テストでようやく顕在化する

### Option 2: output-comparison (採用)

input_hash で record を引いて 1 段目、 record の output_hash と現状ツリーの output_hash を照合して 2 段目を通す。

👍 **Pros**

- drift があれば 2 段目で確実に検出して再生成へ落ちる
- record そのものを書き換えないため、「正しい input → 正しい output」のマップとしての健全性は保たれる
- 共有モデルで record の信頼性を独立検証する仕組みが組み込まれる

👎 **Cons**

- per-task で output ファイル群の SHA256 計算が発生する
  - ただし output は タスクあたり 数〜数十ファイル × 数 KB〜数百 KB のテキストで、計算コストは数十 ms〜数百 ms オーダー
  - generator 実行 (秒オーダー) に対しては誤差レベル
- spec で input ファイルと output ファイルを明示分離する必要がある (具体は Design Doc レベル)

### Option 3: artifact restore (turbo / bazel 流)

input_hash で record を引き、 record に含まれる output 本体で現状ツリーを上書き復元する。 Option 2 が drift 検出時に「再生成」に落ちるのに対し、 こちらは「上書き復元」に落ちる点で分かれる。

👍 **Pros**

- drift 検出時の復旧が速い (ファイル書き出しのみで完結、 generator 実行不要)
- **実行時間が長い generator が含まれる monorepo** では、 復元による時間短縮メリットは無視できない:
  - Go の静的解析を伴うツール (`gqlgen` / `wire` / `mockgen` / `genqlient` / 内製 protoc plugin など) は対象パッケージ規模に応じて秒〜分オーダーで重くなる
  - `buf generate` も対象 proto が多いと plugin 群の起動コストを含めて秒〜十数秒
- 並列実行時の **CPU 専有時間** も削減できる ( generator を再実行しない分、 同時に走る他タスクに CPU を回せる。 monorepo の全 task 並列実行時にはこのスループット向上効果が大きい)

👎 **Cons**

- record が output 本体を含むためサイズが大きくなる ( Option 2 が KB レンジに対し、 こちらは MB 〜 GB レンジになりうる)
- 上書き復元は手元の作業ツリーを **無条件で書き換える** ため、 開発者が一時的に手で修正した output / debugging 中の出力 / 未コミットの変更を破壊する可能性がある (drift = 必ずしも "間違った状態" とは限らず、 開発者が意図的に置いている状態のこともある)
- generator が deterministic である限り、 再生成 ( Option 2) と上書き復元 ( Option 3) の最終結果は同じ。 違いは「実行時間 / CPU 専有時間」と「現状ツリーを無条件上書きするかどうか」
- 復元するための artifact 配信が別途必要 (リポジトリ内なら容量肥大、 外部ストレージなら依存追加。 [ADR-0003](./0003-fingerprint-storage-strategy.md) で Hybrid として将来拡張案として議論)

### Option 4: hybrid (input-only + 整合性 CI)

通常運用は input-only で速く動かし、 定期 CI で全タスクをフル再生成して record と diff を取り drift を検出する。

👍 **Pros**

- 通常時の per-task コストが最小
- drift 検出の責務を CI に集約できる

👎 **Cons**

- drift が CI ジョブで表面化するまで時間がかかる
- その間 共有 record が歪んだ前提で他の開発者が動く期間が生じる
- invalidate 安全性は **fail-fast** であってこそ意味があり、 遅延検出はリスク補償としては弱い

### Option 5: input-only + output stat

input_hash 一致 + output ファイルの `mtime` / `size` 不変で fingerprint hit と判定する。

👍 **Pros**

- stat 取得は SHA256 計算より安価
- 内容 hash まで取らないため per-task コストは小さい

👎 **Cons**

- `git checkout` がブランチ切替時にファイル `mtime` を更新するため false miss が頻発する (再生成が走り続け fingerprint の意味が薄れる)
- 同サイズで内容だけ変わった改変は false hit のまま素通り
- `mtime` を判定材料にする方式は git ベースの開発フローと根本的に相性が悪い

## Decision

**Option 2: output-comparison を採用する。**

採用根拠は以下の連鎖で論証される:

1. Option 1 (input-only) の性能優位は事実だが、 共有モデルでは別環境で作られた record の信頼性を補完する仕組みが必要になる。 ツールバージョンの整合は preflight で担保できるとして、 手元の output が drift していないことは別途検証する必要があり、 その検証は record の output_hash と現状の output_hash を比べる以外に軽量な方法がない
2. Option 3 (artifact restore) は drift 時の挙動が Option 2 と分かれる ( 再生成 vs 上書き復元) が、 deterministic な generator では最終結果は同じ。 実行時間が長い generator が含まれる monorepo では復元による時間短縮 / CPU 専有時間削減のメリット自体は実在する。 ただし、 (a) 上書き復元が手元の未コミット変更や debugging 中の状態を無条件で破壊するリスク、 (b) record が output 本体を含むためサイズが KB → MB〜GB に膨らむこと、 (c) artifact 配信のための追加ストレージ依存、 という 3 点のコストが上回る。 時間短縮 / CPU 削減を本気で取りに行く場合は、 Option 2 を採用したうえで [ADR-0003](./0003-fingerprint-storage-strategy.md) の Hybrid (Option E) で artifact 共有を別レイヤとして追加する形が筋が良く、 本 ADR ではまず Option 2 を採用して将来拡張パスを残す
3. Option 4 (hybrid) は遅延検出が fail-fast invalidate と整合せず、 Option 5 (stat) は git ワークフローと相性が悪い。 除外
4. 残った Option 2 のコスト (per-task 数十〜数百 ms の hash 計算) は、 generator 実行時間に対して誤差レベルで、 性能上の決定的な不利にはならない

判定シーケンス:

- record が見つからなければ即時 generator 実行 → record 書込み
- record があるが output が drift していれば再生成 → record 上書き ( deterministic なら no-op で同一 record になる)
- いずれの分岐でも record の正当性は壊れない

## Consequences

### 正の影響

- 共有された record を別環境で再利用する際、 record の output_hash と現状ツリーの output_hash を比較することで drift を fail-fast に検出できる
- 開発者が手元で output を修正した / formatter が走った / lefthook が書き換えた等の状態でも、 誤った skip が起きず必ず再生成される
- record 自体が「正しい input → 正しい output」のマップとして自律的に健全性を維持する

### 負の影響

- per-task で output ファイル群の SHA256 計算が必要。 タスクあたり 数十〜数百 ms の追加コスト
- spec 側に input / output の明示分離が必要になる (具体表現は Design Doc レベル)
- record に output ファイル一覧と各 hash を持つ必要があり、 record 自体のサイズが Option 1 より大きくなる

### 後続の詳細設計

- record のストレージ方式 (どこに置くか) → [ADR-0003](./0003-fingerprint-storage-strategy.md)
- record の具体 schema、 OS 横断 invalidate 戦略、 preflight、 マイグレーション計画 → [Design Doc](../design/architecture.md)
