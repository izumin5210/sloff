# ADR-0013: タスク間依存を spec に明示宣言する (`depends`)

## Context

### 背景

[architecture.md §タスク間依存](../design/architecture.md) は「依存関係は `inputs` / `outputs` から完全自動導出し、 手動 `depends` フィールドは設けない」 を設計判断として明記してきた。 自動導出は 全 task の glob を実ファイル集合に expand し、 `O_A ∩ I_B ≠ ∅` なら B → A のエッジを張る、 という規則 ( `depgraph.Build`) で実装されている。

この方式には構造的な欠陥がある。 **エッジの導出が「現在のファイルツリーに output ファイルが存在すること」 に依存する** ため、 生成物が存在しない状態では依存エッジが張れず、 実行順序が壊れる:

- 生成物を一括削除してから再生成する運用 ( `make clean && sloff run` 相当)
- 生成物を gitignore する運用 ( sloff は git 管理を推奨するが、 強制はしていない)
- リポジトリへの sloff 導入初回 ( まだ一度も生成していない task を追加した直後)

architecture.md はこの chicken-and-egg を「 generator output は git 管理されている前提のため通常起きない」 と棚上げしていたが、 上記の通り実務で普通に踏む。 しかも壊れ方が悪質で、 **エラーにならず黙って誤順序で実行される** ( 下流が上流の stale / 不在の出力を読んで間違った生成結果を出す、 あるいは generator が fail する)。 実行順序という根幹の性質が「 いつ実行したか」 で変わる non-determinism は、 deterministic ( R2) を掲げる sloff の設計思想と整合しない。

### 旧判断 ( 手動 depends を持たない) が守ろうとしていたもの

旧判断の rationale は fingerprint の健全性である。 sloff の fingerprint が信頼できる前提は「 generator は宣言された `inputs` 以外を読まず、 宣言された `outputs` 以外を書かない」 こと。 「 inputs / outputs に現れない論理依存」 を手動 `depends` で救済すると、 上流の変更が下流の `input_hash` に反映されないまま順序だけ正しくなり、 **fingerprint が嘘をつく状態を「 依存は明示してあるから大丈夫」 という偽の安心感で覆い隠す**、 というのが旧判断の核心だった。

本 ADR はこの懸念自体は正当と認める。 そのうえで、 旧設計が **「 実行順序の導出」 と「 spec 宣言の健全性強制」 という 2 つの責務を 1 つの機構 ( overlap 導出) に重ねていた** ことが問題の根本と整理する。 前者はファイルツリー状態に依存しない決定性が必要で、 後者はファイルが存在するときにしか検証できない。 責務を分離すれば両立できる。

### 評価軸

- **順序の決定性**: 実行順序がファイルツリーの状態 ( clean / 生成済み) に依存しないこと
- **fingerprint 健全性の防御線**: 「 inputs / outputs 宣言が実態とズレている spec」 を検出できる構造が維持されること ( 旧判断が守ろうとしたもの)
- **spec の可読性**: spec を読んだだけで実行順序が分かること
- **判定の実装可能性**: glob 同士の包含判定のような半解決問題を持ち込まないこと ( ADR-0004 D3 と同じ制約)
- **記述コスト**: 利用者が書く量・ミスの入りやすさ

### References

- [ADR-0002: fingerprint hit 判定モデル](./0002-fingerprint-hit-decision-model.md)
- [ADR-0004: Spec 検証と output 重複検出のポリシー](./0004-spec-validation-and-output-conflict-policy.md) ( D2 union semantics / D3 事後検知)
- [ADR-0008: tool を first-class spec entity とする](./0008-tool-as-first-class-spec-entity.md) ( D3: path 系フィールドは spec dir 相対)
- [Design Doc: sloff Architecture §タスク間依存](../design/architecture.md)

## Considered Options

### Comparison Table

| | A: 現状維持 | B: record からの導出補完 | C: pattern レベル overlap 判定 | D: 明示 `depends` のみ | **E: 明示 `depends` + overlap 検証 (採用)** |
|---|---|---|---|---|---|
| 順序の決定性 | × ( ツリー状態依存) | △ ( record 不在の初回は決まらない) | ◎ | ◎ | ◎ |
| fingerprint 健全性の防御線 | ◎ ( 導出 = 検証) | ◎ | ○ | × ( 宣言漏れを検出できない) | ◎ ( error / warning で検証) |
| spec の可読性 | × ( 暗黙) | × ( 暗黙) | × ( 暗黙) | ◎ | ◎ |
| 判定の実装可能性 | ◎ | ○ ( stale record の扱いが複雑) | × ( glob × glob は半解決問題) | ◎ | ◎ |
| 記述コスト | ◎ ( ゼロ) | ◎ ( ゼロ) | ◎ ( ゼロ) | △ | △ |

### Option A: 現状維持 ( overlap 自動導出のみ)

棄却。 背景に記した通り、 clean state で黙って誤順序になる欠陥が残る。

### Option B: fingerprint record からの導出補完

output ファイルが存在しないとき、 git 管理されている fingerprint record の `output.files` から O_A を復元してエッジを張る案。 record は clean state でも git tree に残るため、 多くのケースで導出が成立する。

- ○ spec 変更ゼロで clean state 問題の大部分を救える
- × **完全初回 ( record がまだ無い task) では依然として順序が決まらない**。 問題が「 output が無い」 から「 record が無い」 に移るだけで、 構造は解決しない
- × stale record ( spec 変更後にまだ走っていない) が誤ったエッジを張る経路が生まれ、 「 record はあくまで skip 判定の材料」 という責務を超えてしまう

棄却。

### Option C: pattern レベル overlap 判定

実ファイルではなく glob pattern 同士 ( B の inputs pattern × A の outputs pattern) の交差可能性で判定する案。 ファイルツリー状態に依存しなくなる。

- ◎ spec 変更ゼロで決定性が得られる
- × glob 同士の包含 / 交差判定は半解決問題に近く、 完全実装は過剰投資 ( [ADR-0004 D3](./0004-spec-validation-and-output-conflict-policy.md) で静的 overlap 解析を棄却したのと同じ理由)
- × 保守的に倒すと偽エッジ ( 実際には交差しない pair の直列化・偽 cycle) が大量発生する

棄却。

### Option D: 明示 `depends` のみ ( overlap 検証なし)

順序決定も検証も declared `depends` だけにする案。

- ◎ 実装が最小
- × 「 B が A の生成物を読むのに depends 未宣言」 という spec を検出できない。 現行の自動導出が持っていた健全性の防御線 ( 宣言と実態の整合チェック) を失う。 誤順序は clean state での生成結果破壊として顕在化するまで気づけない

棄却。

### Option E: 明示 `depends` + overlap 検証 (採用)

実行順序は declared `depends` のみで決定し、 既存の overlap 計算は **検証** として存続させる。 「 順序の導出」 と「 健全性の強制」 の責務を分離する。

- ◎ 順序はファイルツリー状態に依存せず deterministic
- ◎ 旧判断が守ろうとした防御線は検証として維持される。 しかも後述の run 時検証により、 **旧実装では検出できなかった clean state での宣言漏れも必ず検出される** ( 検出力は純増)
- ◎ spec を読めば実行順序が分かる ( 旧設計の「 暗黙性の懸念」 も同時に解消)
- △ 依存がある task では `depends` の記述が必須になる。 書き忘れは検証 error が捕まえる

## Decision

**Option E を採用する。 タスク間の実行順序は spec の `depends` で明示宣言し、 inputs / outputs overlap の計算は順序導出から検証に役割を変えて存続させる。**

### D1. spec 文法: `depends` 構造体リスト

`commands[*]` に optional フィールド `depends` を追加する。 要素は構造体:

```yaml
# proto/svc/sloff.yml
commands:
  - name: protoc-gen-go
    cmd: buf generate --template buf.gen.yaml
    inputs: ["**/*.proto", "../../gen/**/*.options.pb.go"]
    outputs: ["../../gen/**/*.pb.go"]
    tools: [buf, protoc-gen-go]
    depends:
      - spec: ../options          # 依存先 task が属する spec dir ( この sloff.yml の dir 相対)
        task: options-codegen     # 依存先 task 名
      - task: lint-proto          # spec 省略時は同一 sloff.yml 内の task を指す
```

- `spec` は **spec dir 相対** で解釈する。 inputs / outputs glob および tool の path 系フィールド ( ADR-0008 D3) と同じ基準で、 spec 内の path 表現を一貫させる
- `task` は依存先 spec ファイル内の `commands[*].name`
- 文字列 shorthand ( `"../options:options-codegen"` 等) は提供しない。 区切り文字の解釈規則を増やさず、 構造体 1 形式に統一する

load 時 validation ( いずれも error):

- 参照先の spec / task が存在しない
- 自己参照
- 同一エッジの重複宣言
- `spec` の正規化結果が repoRoot を抜ける ( inputs / outputs glob と同じポリシー)

### D2. 実行順序は declared `depends` のみで決定する

`depgraph.Build` は declared エッジのみで DAG を構築する。 実ファイルの overlap は順序決定に使わない。 これにより実行順序は spec のみから決まり、 ファイルツリーの状態に依存しない。 循環依存は従来どおり DAG 構築時に error。 topological sort / 並列実行 / tie-break の規則は既存実装を踏襲する。

### D3. overlap 計算は検証として存続する

検証は 2 方向で、 厳しさを変える:

| 検証 | 意味 | タイミング | 挙動 |
|---|---|---|---|
| **depends 漏れ** | `O_A ∩ I_B ≠ ∅` なのに B が A への depends を宣言していない | (1) plan 時: 現ツリーで overlap を計算 (2) run 時: 各 task の実出力 ( ADR-0004 D3 の producedBy 集計を流用) と各 task の expanded inputs を突合 | **error** ( 不足している `depends` 記述を提示する)。 run 時検出は遅くとも run 終了時までに fail させ、 exit code 非ゼロ |
| **inputs 漏れ** | B が A への depends を宣言しているが、 A の実出力が B の inputs に 1 つもマッチしない | run 後 | **warning**。 「 depends はあるが上流の生成物が inputs に無い = 上流変更で invalidate されない」 疑いを知らせる |

- depends 漏れを error にするのは、 旧判断の防御線 ( 宣言と実態の整合強制) の継承。 plan 時検証は旧実装と同等の検出力で、 run 時検証が clean state をカバーする ( 上流が実際にファイルを生成した時点で必ず捕まる)
- inputs 漏れを warning に留めるのは、 ADR-0004 D2 ( union semantics) で許容した conditional outputs により正当な spec でも交差ゼロになり得るため。 error にすると正しい run を落とす false positive が発生する
- `sloff graph` では depends 漏れ検証を **warning に降格** し、 graph 自体は出力する ( graph は誤順序のデバッグに使う表示ツールであり、 検証 error で出力を止めると自己矛盾する)

### D4. `depends` は input_hash に含めない

`depends` は純粋な scheduling metadata であり、 fingerprint の invalidate には一切関与しない。 invalidate は従来どおり「 上流の出力ファイルが下流の inputs に含まれる → files_hash が変わる」 経路のみで成立する ( ADR-0002 の構造は不変)。 record schema にも変更はなく、 既存 record はそのまま有効。

### D5. 互換性

pre-1.0 のため breaking change として導入する ( ADR-0004 D1 の precedent)。 依存を持たない spec は無変更で動く。 依存を持つ spec は plan / run 時の depends 漏れ error が不足箇所と修正内容を提示するため、 機械的に追従できる。

## Consequences

### 正の影響

- 実行順序が spec のみから決定され、 clean state / 生成済みのどちらでも同一になる ( 本 ADR の動機の解消)
- 旧実装で検出不能だった「 clean state での依存宣言漏れ」 が run 時検証で必ず error になる。 健全性の検出力は純増
- spec を読むだけで実行順序が分かり、 旧設計が「 暗黙性の懸念と緩和策」 として抱えていたトレードオフが消える
- inputs 漏れ warning により、 「 順序は正しいが invalidate されない」 という従来は気づきようのなかった spec 不全に気づける

### 負の影響 / 注意点

- 依存がある task では `depends` の記述が必須になる。 inputs / outputs と depends の二重管理になるが、 両者の整合は D3 の検証が機械的に担保する
- depends 漏れの run 時検出は「 検出した run では既に誤順序で生成が走った後」 になり得る。 それでも黙って壊れる旧仕様より厳密に安全側であり、 git 管理された生成物は diff で復元できる
- conditional outputs を持つ正当な spec に inputs 漏れ warning が誤って出ることがある ( error にしない判断の裏面)。 warning 文言で conditional outputs の可能性に言及する

### 撤回時の影響

`depends` フィールドを撤去して自動導出に戻す場合、 spec の depends 記述は dead weight になるが、 record / fingerprint には影響しない ( D4 で hash に含めていないため)。 撤回判断は別途 ADR で行う。

### 後続の更新

1. [Design Doc: architecture.md](../design/architecture.md): spec ファイル形式 ( 文法ポイント) / §タスク間依存の全面改訂 ( 「 なぜ手動 depends を持たないか」 「 暗黙性の懸念と緩和策」 の置換)、 Resolver `Inputs` 節の output-overlap 連携記述 ( pnpm-local の旧設計の残骸) の整理、 Open Questions Q3 の文言調整
2. `internal/sloff/spec`: `Command.Depends` の追加と load 時 validation ( D1)
3. `internal/sloff/depgraph`: declared エッジベースの DAG 構築 + plan 時 overlap 検証 ( D2 / D3)
4. `internal/sloff/runner`: run 時 overlap 検証 ( producedBy 集計と expanded inputs の突合) と inputs 漏れ warning ( D3)
5. `internal/sloff/explain` / `cmd/sloff/graph.go`: declared エッジの表示 ( overlap が観測できればファイルサンプル、 できなければ declared のみの表示)、 検証の warning 降格
6. E2E test: clean state ( 生成物全削除) からの正順序実行 / depends 漏れの plan 時 error / clean state での run 時 error / inputs 漏れ warning / cross-spec depends / cycle / unknown 参照の各 case を追加、 既存 golden の更新
