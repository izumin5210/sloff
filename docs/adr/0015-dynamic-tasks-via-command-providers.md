# ADR-0015: 動的タスクを外部 command provider で生成する (`command_providers`)

## Status

Accepted

## Context

### 背景

sloff の task は静的である。 `**/sloff.yml` を discover し、 各 `commands[*]` をそのまま 1 task として materialize する ( `runner.collectTasks`)。 task の集合・各 task の `inputs` / `outputs` / `depends` は spec ファイルに literal に書かれた内容で完全に確定する ( = applicative build system)。

monorepo の codegen を高速化する典型手法は、 1 つの generator を **package / directory 単位の task に分割** ( per-dir incremental) し、 未変更 dir を fingerprint hit で SKIP することである。 さらに generator によっては、 per-dir task の `inputs` に **自 dir だけでなく、 そのソースが推移的に import している他 dir のソースまで含める**必要がある ( import 先の型定義が変われば自 dir の出力も変わるため。 例: IDL の forward import 閉包)。

この **task 集合**と各 task の **inputs 閉包**は、 ディレクトリ構成とソースの import 文から機械的に決まる量であり、 `sloff.yml` に手書きするのは非現実的である。 現状の回避策は「外部スクリプトで per-dir の `sloff.yml` を生成して commit する」だが、 次の課題を抱える:

- **C1 生成物のコミットノイズ**: 数十〜数百 task ぶんの `sloff.yml` が git に乗り、 dir 追加 / import 変更のたびに巨大な diff になる
- **C2 閉包ロジックの out-of-band 化**: 閉包を計算するスクリプトが sloff の外にあり、 sloff から見えない
- **C3 ドリフト**: 「スクリプトを再実行し忘れた `sloff.yml`」と「実際の import グラフ」がズレても sloff は気づけず、 古い inputs のまま hit してしまう
- **C4 再現性が運用依存**: 生成スクリプトの実行が CI / 開発者の手順に埋め込まれ、 sloff の deterministic ( R2) / OS 非依存 ( R3) 保証の外側にある

本 ADR は、 この task 生成を sloff の関心事として first-class に取り込む方式を決定する。 方式の網羅的な比較・調査は [Design Doc: dynamic-tasks.md](../design/dynamic-tasks.md) にある。 本 ADR はその結論を確定する。

### 要件の 2 軸

- **軸 A ( parametric fan-out)**: 1 つの task テンプレートを、 ファイルシステム列挙で得た集合にわたって展開する ( task の数 = データ由来)
- **軸 B ( content-derived inputs)**: 各 task の `inputs` を、 ソースの中身 ( import 文) を parse した推移閉包にする ( inputs の中身 = データ由来)

重要な観察: 本ユースケースの閉包は **チェックイン済みソースの中身**だけから決まり、 **どの task の出力にも依存しない**。 したがって閉包は plan フェーズ ( 実行前) に純粋関数として計算でき、 sloff を「実行中に依存を発見する monadic build system」( Buck2 `dynamic_output` / Ninja `dyndep` / Shake `need`) に作り変える必要は **ない**。 必要なのは plan 時の "グラフ導出フェーズ" だけである ( Nx Project Crystal / Pants dependency-inference / CMake configure と同クラス)。

### 評価軸

- **軸 A / 軸 B の充足**: fan-out と推移閉包の両方を表現できること
- **fingerprint 健全性**: 生成 task も「出力に影響する全入力」を `input_hash` に捕捉すること ( ADR-0002)
- **determinism ( R2) / OS 非依存 ( R3)**: 2 開発者が独立に生成しても同一の task 集合・同一 record になること
- **既存資産の流用**: 現状の閉包計算ロジックを転用でき、 移行コストが小さいこと
- **sloff の設計指針との整合**: buf / proto を special-case しない ( ADR-0006 / ADR-0007)、 declared-only ( ADR-0005)、 既存の overlap 検証 ( ADR-0013) を活かす

### References

- [ADR-0002: fingerprint hit 判定モデル](./0002-fingerprint-hit-decision-model.md) ( output-comparison)
- [ADR-0004: Spec 検証と output 重複検出のポリシー](./0004-spec-validation-and-output-conflict-policy.md) ( D3 producer 一意性)
- [ADR-0005: resolver auto-dispatch の排除](./0005-eliminate-resolver-auto-dispatch.md) ( declared-only)
- [ADR-0006: buf を special-case しない](./0006-no-buf-specific-resolver-or-preflight.md)
- [ADR-0008: tool を first-class spec entity とする](./0008-tool-as-first-class-spec-entity.md) ( D3: path 系フィールドは spec dir 相対)
- [ADR-0013: タスク間依存を spec に明示宣言する](./0013-explicit-task-dependencies.md) ( overlap 検証)
- [Design Doc: dynamic-tasks.md](../design/dynamic-tasks.md) ( 全方式の比較・競合調査)

## Considered Options

| | A: 外部生成 + commit ( 現状) | B: 宣言的 matrix / template | **C: 外部 command provider (採用)** | D: ネイティブ inference | E: staged spec 再生成 |
|---|---|---|---|---|---|
| 軸 A ( fan-out) | ○ | ○ | ○ | ○ | ○ |
| 軸 B ( 推移閉包) | ○ | **×** | ○ | ○ | ○ |
| commit ノイズ解消 ( C1) | × | ○ | ○ | ○ | △ |
| 閉包ロジックを fingerprint ( C2/C3) | × | n/a | ○ | ○ | ○ |
| 既存ロジック流用 | ○ | × | **○** | × | ○ |
| ADR-0006 ( proto を special-case しない) | ○ | ○ | **○** | **×** | ○ |
| sloff core を applicative 維持 | ○ | ○ | ○ | ○ | △ |
| 実装コスト | ゼロ | 小 | 中 | 大 | 中 |

- **Option A ( 現状維持)**: C1〜C4 が残る。 棄却。
- **Option B ( matrix / template)**: load 時に template を FS 列挙で展開する ( Bazel macro / Make pattern rule 相当)。 軸 A は満たすが、 import 閉包 ( 軸 B) を表現できない。 単独では不足。 棄却。
- **Option C ( 外部 command provider)**: 利用者が宣言したプログラムを plan 時に実行し、 stdout の task 定義一覧を取り込む ( Nx Project Crystal / Ninja の build.ninja 生成器 / CMake 相当)。 軸 A+B を単一機構で満たし、 既存の閉包ロジックをそのまま転用でき、 proto を special-case しない。 **採用**。
- **Option D ( ネイティブ inference)**: sloff 本体に proto / ts / go の parser を抱える ( Pants 相当)。 ADR-0006 / ADR-0007 の指針に正面から反し、 実装が最も重い。 棄却。
- **Option E ( staged 再生成)**: 普通の codegen task が spec 断片を出力し読み直す ( Make `.d` / CMake regen 相当)。 移行パスとしては有効だが、 「plan は 1 回」モデルを崩すか 2 回起動運用を要する。 Option C の下位互換。 棄却。

## Decision

**Option C を採用する。 spec に `command_providers` ブロックを追加し、 各 provider を plan 時に実行して stdout の versioned JSON ( task 定義一覧) を task 集合に取り込む。 生成 task は通常 task と同一経路 ( glob 展開 / depgraph / overlap 検証 / fingerprint) を流れる。**

### D1. spec 文法: `command_providers` ブロック

`sloff.yml` の top-level に optional ブロック `command_providers` を追加する。 各 entry は名前と実行コマンドのみ:

```yaml
tools:
  buf: { exec: ["buf", "--version"] }

command_providers:
  - name: proto-perdir
    exec: ["go", "run", "./tools/emit-proto-tasks"]

commands:        # 静的 task も従来どおり共存できる
  - name: registry
    cmd: "buf build -o registry.binpb"
    inputs: ["**/*.proto"]
    outputs: ["registry.binpb"]
    tools: [buf]
```

- `name` は provider 識別子 ( エラーメッセージ用)。 同一 file 内で一意
- `exec` は plan 時に実行するコマンド ( argv リスト)
- `tools` / `commands` / `command_providers` のうち最低 1 つを持てば valid ( provider のみの file も可。 tool は repo-wide flat namespace なので他 sloff.yml 定義を参照できる)

### D2. 実行モデル: plan 時に exec → versioned JSON → commands に merge

provider は **`collectTasks` の手前 ( `expandProviders` フェーズ)** で実行され、 出力 task が `commands[*]` 相当に展開されてから既存経路に合流する。

- **cwd / path 基準**: provider は宣言元 `sloff.yml` の dir を cwd に実行する。 provider が emit する `inputs` / `outputs` / `depends` の path も同 dir 相対で解釈する ( 手書き task と同一基準、 ADR-0008 D3)。 生成 task は宣言元 spec の relpath に属し、 fingerprint Key のパス `.sloff/fingerprints/<spec_relpath>/<task_id>/` が安定する
- **出力スキーマ**: `{ "schema_version": "v1", "tasks": [ {name, cmd, inputs, outputs, tools, depends} ] }`。 各 task は `commands[*]` を 1:1 で写像した形。 `cmd` は文字列または argv リスト。 未知 `schema_version` は load error ( ADR-0009 と同じ「跨ぎ互換読み込みはしない」方針)
- **合流後**: 生成 task は通常 task と区別なく `collectTasks` → `depgraph.Build` → `runTasks` を流れる。 sloff core の改修は `expandProviders` フェーズに局所化される

### D3. fingerprint: 生成 task は自己完結。 provider の version は含めない

生成 task `T` の `input_hash` は既存の式そのまま ( `T.tools` の version のみ):

```
input_hash(T) = H( files_hash(inputs), cmd_hash(cmd), resolved_versions_hash(T.tools) )
```

**provider 自身の version は `input_hash` に含めない。** 理由: provider は plan 時に「task 定義を選ぶ」だけで、 実行時に走るのは `cmd` であって provider ではない。 task 定義 `(cmd, inputs, tools)` がいったん確定すれば、 その出力は provider の version とは因果的に無関係である ( ADR-0002 の「generator は宣言 inputs の純関数」前提下)。 provider のロジック変更を 2 ケースに分けると:

| provider ロジック変更の結果 | 検知経路 |
|---|---|
| T に**別の** `(cmd, inputs)` を吐くようになった | cmd_hash / files_hash が変わり **既存経路で自然に invalidate** |
| T に**同一の** `(cmd, inputs)` を吐く | T の出力は不変 ( 純関数) なので **再生成不要が正しい** |

どちらも provider version の注入を必要としない。 ソースや閉包の変化はすべて `files_hash` 経路で捕捉される。

### D4. メモ化なし ( v1): provider は毎 run 実行する純関数

C3 ドリフト ( 閉包ロジックを直したのに古い inputs で hit) を塞ぐ鍵は、 **provider を毎 `sloff run` 実行して現状から task 集合を再 emit すること**である。 v1 では provider 出力の run 跨ぎメモ化は持たない。

- run 跨ぎメモ化は「provider が読む全ソース」の宣言 ( メモ化キー) を要求し、 その over-approx が外れると新 dir を取りこぼす新たな failure mode を生む。 健全性の surface を増やすため v1 では入れない
- 閉包計算は import 行の scan で済む見込みで、 毎回走らせても安い。 plan-time コストが実測で問題化したら opt-in メモ化を別途 ADR で設計する ( まず always-rerun で sound に作る規律)

### D5. 既存の検証機構を生成 task にも適用する

生成 task は merge 後に通常 task と同一の検証を通る。 動的生成特有の追加は **determinism の強制**のみ:

- **provider 出力を name 昇順に sort**してから取り込む ( provider の出力順に非依存、 R2)
- 生成 task の **required フィールド / spec dir 内 name 一意性**は、 静的 task と同じ `spec.ValidateCommands` を merge 後の集合に適用して検証する
- **tools 参照** ( `ValidateToolReferences`) / **depends 参照** ( `ValidateDependReferences`) / **inputs・outputs の repo-root escape** ( `glob.Expand`) / **output 衝突** ( ADR-0004 D3) / **depends 漏れ** ( ADR-0013 overlap 検証) は、 生成 task が通常経路に載ることで **無改修で適用**される

これにより、 利用者は「正しい task を吐く」ことだけに集中でき、 「健全な fingerprint になっているか」は sloff が機械的に保証する。 これは inference を信頼するだけの Nx / Bazel に対する sloff の差別化点である。

### D6. 互換性

pre-1.0 のため additive change として導入する。 `command_providers` を持たない spec は無変更で動く。 record schema にも変更はなく、 既存 record はそのまま有効。

## Consequences

### 正の影響

- per-dir codegen の task 集合と inputs 閉包を `sloff.yml` に手書きせず、 provider プログラムから導出できる。 課題 C1〜C4 を一括解消する
- 既存の閉包計算ロジックをそのまま provider に転用でき、 移行コストが小さい
- proto / 言語を sloff 本体に取り込まない ( ADR-0006 / ADR-0007 と整合)。 sloff は「versioned JSON で task を受け取る」汎用プリミティブのみを持つ
- 生成 task が既存の overlap 検証 ( ADR-0013) / producer 一意性 ( ADR-0004 D3) を通るため、 provider のバグ ( depends 漏れ / 出力衝突) が機械的に検出される

### 負の影響 / 注意点

- plan 時に毎回 provider subprocess + ソース parse のコストを払う ( v1 はメモ化しない)。 setup への上乗せは実測で評価する ( Open Question)
- provider が non-deterministic だと R2 が崩れる。 provider 自身の再現性 ( toolchain / 依存の固定) は利用者責務。 sort と name 一意性検証で部分的に担保するが、 出力内容の決定性までは強制できない
- provider が undeclared input を読んで閉包を計算すると、 その入力変化は invalidate されない。 これは ADR-0002 の前提と同クラスで、 現状方式 A も持つリスク ( 純増ではない)。 取りこぼした入力が他 task の output なら overlap 検証が、 純ソースなら down-stream の compile が顕在化させる

### Open Questions

- **OQ1**: 「DSL → proto 生成 → その生成 proto の import から閉包」のように閉包が **task 出力由来**になる要求が出たら、 monadic 機構 ( Buck2 `dynamic_output` / staged re-plan) の限定導入を再評価する。 現時点はソース由来のみ想定し applicative で確定
- **OQ2**: provider 出力スキーマ ( `schema_version`) の wire 互換を破る変更時の運用
- **OQ3**: provider の plan-time コストの実測。 import 行 scan で十分安いか、 メモ化が要るほどか
- **OQ4**: plan-time コストが問題化した場合の run 跨ぎメモ化の opt-in 設計 ( メモ化キーの over-approx 健全性 / 置き場)

### 後続の更新

1. `internal/sloff/spec`: `File.CommandProviders` ( `CommandProviderDecl`) の追加と parse 時 validation、 `ValidateCommands` の export ( merge 後の再検証用)
2. `internal/sloff/provider` ( 新規): provider の exec / versioned JSON parse / `spec.Command` への変換 / name sort
3. `internal/sloff/runner`: `expandProviders` フェーズの追加 ( `prepareRegistry` の手前)。 生成 task の merge と既存経路への合流
4. [Design Doc: dynamic-tasks.md](../design/dynamic-tasks.md): Status を本 ADR 確定に更新
5. E2E test: provider の first-run / second-run-hits / task-set-changes / input-change-invalidates / logic-change-reemits / exec 失敗 / 不正 schema_version / output 衝突 / name 衝突 の各 case
