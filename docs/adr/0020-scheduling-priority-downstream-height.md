# ADR-0020: ready task の tie-break を downstream 高さ優先にする (scheduling priority)

## Status

Accepted

## Context

### 背景

runner は [depgraph](../design/architecture.md) が返す topological order を `runTasks` の goroutine submit 順として使う。同時実行は `errgroup.SetLimit(NumCPU)` で slot 数に制限され、**predecessor 待ちの goroutine も slot を保持したまま待つ** ( slot は submit 順に付与される)。ゆえに **emit 順 ≒ 実行優先度** である。

[ADR-0013](./0013-explicit-task-dependencies.md) D2 は実行順序を declared `depends` のみで決定し、独立 task 間の tie-break を `(SpecRelpath, Name)` の辞書順に固定した。順序の *決定性* はこれで担保されるが、tie-break が **依存構造の深さを一切見ない**ため、導入先 monorepo ( layerone、約 500 task) で深い依存 chain が飢餓する病理が顕在化した:

- sink である `graphql-gateway:generate` は `buf-pothos-*` ( 42 個) / `buf-es-*` / `buf-custom-node` / `build-protoc-plugins` 等に依存する
- critical path は `buf-protoc-plugins-es → build-protoc-plugins → buf-custom-node → generate` ( chain 長 4)
- ところが `build-protoc-plugins` は辞書順で全 `buf-*` ( 約 400) の **後ろ** に並ぶ ( `buf` < `build`)
- 14 slot が幅広い shallow task ( pothos wave 等、いずれも generate への直接入力で chain 長 2) に先に埋められ、chain 起点が slot を得られず **~12s 飢餓** → generate 開始が ~17s まで遅延する

`SLOFF_DEBUG_TIMING` ( [ADR-0018](./0018-otel-tracing.md) の span を stderr 集計) の実測で、auth novel シナリオの tail が `build-protoc-plugins [12s..] → buf-custom-node → generate` の直列 chain であり、`runner.tasks.run` の wall ( ~21-25s) の相当部分がこの **chain の遅延起動** に帰属することを確認した。task の総 exec work を slot 数で割った理論下限は ~7s だが、critical chain が末尾に直列化されて wall を押し上げている。

これは「実行順序が spec 宣言や依存構造ではなく **task 名の辞書順** に依存し、depth を無視する」問題である。scheduling は健全性 ( SKIP/RUN 判定・生成物・error) には影響しないが、wall-clock には直結する。

### 評価軸

- **決定性 (R2)**: 同一 task 集合 → 同一 emit 順。ツリー状態や計測タイミングに依存しないこと
- **既存挙動の不変性**: SKIP/RUN 判定・生成物・error・依存の semantics を変えず、tie-break のみを変えること
- **fingerprint 不参加**: [ADR-0013](./0013-explicit-task-dependencies.md) D4 ( `depends` は input_hash に不参加) と同じく、優先度も input_hash に影響しないこと
- **実装コスト / 複雑さ**: `runTasks` の slot 保持構造 ( ADR-0013 が採る単純な errgroup) を変えないこと
- **判定の実装可能性**: exec 時間の推定のような非決定的・プロファイル依存の入力を持ち込まないこと ( ADR-0013 が排したツリー状態依存の non-determinism を再導入しない)

### References

- [ADR-0013: タスク間依存を spec に明示宣言する](./0013-explicit-task-dependencies.md) ( D2: declared エッジのみで順序決定 / D4: depends は input_hash 不参加)
- [ADR-0017: 集約専用の barrier task を first-class にする](./0017-barrier-tasks.md)
- [ADR-0018: OTel トレーシング](./0018-otel-tracing.md) ( 病理の実測に使用)

## Considered Options

### Comparison Table

| | A: 現状維持 ( 辞書順) | B: critical-path weighting ( exec 推定) | C: runtime work-stealing | **D: downstream 高さ優先 (採用)** |
|---|---|---|---|---|
| 深い chain の早期起動 | × | ◎ | ◎ | ○ ( 起点の submit を前倒し) |
| 決定性 | ◎ | × ( 計測タイミング依存) | △ ( 実行時 race に依存) | ◎ |
| 既存挙動の不変性 | ◎ | ○ | × ( slot 保持を変える) | ◎ |
| 実装コスト | ◎ | △ | × | ◎ ( tie-break の差し替えのみ) |
| fingerprint 不参加 | ◎ | ◎ | ◎ | ◎ |

### Option A: 現状維持 ( 辞書順 tie-break)

棄却。背景の通り、`build-*` が `buf-*` の後ろに並ぶだけで深い chain が飢餓し、wall が伸びる。順序が task 名という無関係な属性に依存する。

### Option B: critical-path weighting ( exec 時間推定で重み付け)

各 task の推定 exec 時間を重みとした critical path ( 最長 *時間* 経路) で優先度を付ける案。理論上は最適な packing に近づく。

- ◎ 実 exec 時間を反映できれば wall 最小化に最も近い
- × 推定 exec 時間の出所が無い。fingerprint record は exec 時間を持たず、初回 / 変更後の task は推定不能。過去 record やプロファイルに頼ると **優先度が「いつ・どの環境で計測したか」に依存**し、ADR-0013 が排除したツリー状態依存の non-determinism を scheduling に再導入する
- × 計測の無い環境 ( CI 初回、clean checkout) で優先度が退化する

棄却。決定性の要件と両立しない。

### Option C: runtime work-stealing / slot 動的再配分

predecessor 待ちの goroutine が slot を手放し、ready な task に動的再配分する案。emit 順に依存しなくなる。

- ◎ chain 中間 task の slot 保持が無くなり packing が最適化される
- × `runTasks` の errgroup ベースの単純な構造を大改造する必要があり、複雑度・race の面積が跳ね上がる
- × 今回の病理は「chain *起点* の submit 順」で解けるので過剰投資

棄却 ( 将来、起点前倒しでも詰まる場合の選択肢としては残す)。

### Option D: downstream 高さ優先 (採用)

tie-break を「**downstream 高さ ( そのタスクを起点に依存 *される* 方向へ辿った最長 chain 長) 降順 → (SpecRelpath, Name) 昇順**」に変える。高さは **依存エッジを反転した DP** で決まり、exec 時間ではなく **グラフ構造** の関数なので決定的。

- ○ 深い chain の *起点* が同一 ready wave の shallow fan より先に slot を得る → chain が早期起動する
- ◎ 決定性・既存挙動不変・実装単純さをすべて満たす ( tie-break 関数の差し替えのみ、`runTasks` は不変)
- ◎ exec 推定を持ち込まない ( Option B の欠点を回避)

## Decision

**Option D を採用する。depgraph の tie-break を downstream 高さ降順 → (SpecRelpath, Name) 昇順に変更する。**

### D1. tie-break の定義

Kahn 法の ready / unblocked キューの並べ替えを次の全順序で行う:

1. **downstream 高さ 降順** ( 高いものが先)
2. 同高さは **(SpecRelpath, Name) 昇順** ( 従来の安定キー)

downstream 高さ `height(t)` = t に ( 推移的に) 依存する task の最長 chain 長。sink ( 何にも依存されない task) は 1、依存する task が 1 段増えるごとに +1。

### D2. 高さの計算 ( O(V+E)、cycle-safe)

依存エッジ ( 「i depends on j」 = j before i) を反転した `consumers[j] = { i : i が j に依存 }` を構築し、`height(j) = 1 + max_{i∈consumers[j]} height(i)` を memoized DFS で求める。

- 再帰スタック上の node ( back-edge) は高さ 0 を返す。これにより **cyclic な入力でも有限・決定的な高さを返して hang しない** ( 循環は Build が高さ計算の直後に既存の cycle 検出で error にするので、cyclic 時の高さの正しさは問わない — 停止性と決定性だけを保証すればよい)
- 再帰深さは最長依存 chain に等しく、実グラフでは十分浅い

### D3. `runTasks` は不変

slot 保持構造 ( predecessor 待ち goroutine が slot を保持) は変えない。優先 submit された chain の中間 task が slot を保持して待つのは chain 深さ分 ( 実測で ~4 段) のみで、slot 数 ( NumCPU) に対し十分小さく許容範囲。runtime の work-stealing ( Option C) は導入しない。

### D4. fingerprint 意味論・依存 semantics は不変

高さは **scheduling 専用の派生量** であり、エッジを追加・削除しない。input_hash・record schema・overlap 検証 ( ADR-0013 D3)・SKIP/RUN 判定・`depends` の意味は一切変わらない ( ADR-0013 D4 と同じ立場: 順序 metadata は健全性に不参加)。`sloff graph` の DAG 構造も不変で、変わりうるのは emit 順の表示だけである。

### D5. 決定性

高さは純粋にグラフ構造の関数であり、sort は stable かつ全順序 ( 高さ → SpecRelpath → Name、後二者は task の一意キー) なので、**同一 task 集合 → byte 同一の emit 順** が保たれる ( R2)。

## Consequences

### 正の影響

- 深い依存 chain の起点が同一 ready wave の shallow fan より先に slot を得て早期起動する。auth novel シナリオでは `build-protoc-plugins` 起点の chain が前倒しされ、それに gate される `generate` の開始が早まる
- exec work の slot への packing が改善し、tasks.run の wall が短縮する
- 実行順が「task 名の辞書順」ではなく「依存構造の深さ」で決まるため、順序が意図と揃う

### 負の影響 / 注意点

- 実行順が変わるため、`depends` 未宣言の **暗黙順序に依存していた spec** があれば顕在化しうる。ただし ADR-0013 D3 の overlap 検証 ( plan 時 + run 時) が未宣言依存を error にするので原則安全であり、健全性の防衛線は不変
- 高さ配列は task 数に対して線形のメモリを使う ( 数百〜数千 task で無視できる)

### 撤回時の影響

tie-break を `(SpecRelpath, Name)` 単独に戻せば旧挙動に復帰する。fingerprint record・スケジューリング以外のロジックには一切影響しない ( 高さは永続化しない)。

### 後続の更新

1. [architecture.md](../design/architecture.md) の `depgraph.Build` / topological order の記述に priority tie-break ( 本 ADR) を追記
2. `internal/sloff/depgraph`: `downstreamHeights` / `sortByPriority` の追加と、Build の Kahn ループでの適用 ( D1-D2)
3. depgraph の unit test: 深い chain が shallow sibling より先に emit される / 等高さは (spec, name) に fallback / 決定性の各 case を追加
