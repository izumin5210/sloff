# ADR-0016: タスク依存をパターンで宣言する (`depends[*].task` の glob)

## Context

### 背景

[ADR-0015](./0015-dynamic-tasks-via-command-providers.md) で、 1 つの generator を package / directory 単位の task に分割する per-dir codegen の **producer 側 task 集合**は、 `command_providers` が plan 時に directory 構成・ソースの import グラフから動的に導出できるようになった。 これにより producer 側の課題 C1〜C4 ( 生成物のコミットノイズ / 閉包ロジックの out-of-band 化 / ドリフト / 再現性の運用依存) は解消された。

しかし、 **その派生 task 群すべてを待つ下流 task ( consumer)** の依存は、 [ADR-0013](./0013-explicit-task-dependencies.md) により task ごとに literal な `depends` エントリとして列挙する必要がある。 per-dir で生成された N 個の producer を集約して読む 1 つの consumer ( 例: 全 package の生成物を 1 つにまとめる bundle / schema-build task) は、 N 本の `depends` を手書きする:

```yaml
# 下流の集約 task。 producer が増えるたびに depends も増える
commands:
  - name: bundle
    cmd: ...
    inputs: ["../gen/**/*"]
    outputs: ["dist/bundle.js"]
    tools: [bundler]
    depends:
      - {spec: ../gen, task: gen-a}
      - {spec: ../gen, task: gen-b}
      - {spec: ../gen, task: gen-c}
      # … producer の数だけ続く
```

この **consumer 側の fan-out** は、 producer の task 集合 ( = directory 構成) から機械的に決まる量であり、 ADR-0015 が producer 側で解消したのと同じ問題を consumer 側に残している:

- **C1' コミットノイズ**: 数十〜数百本の `depends` が spec に乗り、 dir 追加のたびに巨大 diff になる
- **C3' ドリフト**: 外部スクリプトで consumer の `depends` を生成してコミットする運用にすると、 「 スクリプト再実行を忘れた spec」 と「 実際の producer 集合」 がズレる。 新 dir 追加時に consumer がその producer を待たず、 誤順序になる

ADR-0015 の `command_providers` はこの consumer 側を直接は解けない。 provider は **task をまるごと emit する**機構で、 既存の静的 task の `depends` だけを動的に足すことはできない。 consumer task を provider 化すれば depends を動的算出できるが、 その代償として task の静的部分 ( `cmd` / `inputs` / `outputs`) が外部プログラムの文字列に沈み、 spec の可読性を損なう ( 95% 静的・5% 動的な task に対して割に合わない)。

本 ADR は、 consumer 側 fan-out を「 宣言コストなく・ドリフトなく」 表現する最小の spec 拡張を決定する。

### 評価軸

- **fan-out の表現**: 「 ある spec の特定グループの task すべてに依存する」 を、 producer の増減に追従する形で宣言できること
- **ADR-0013 の防御線の維持**: overlap 検証 ( depends 漏れ = error / inputs 漏れ = warning) が機能し続けること。 「 順序の導出」 と「 健全性の強制」 の分離 ( ADR-0013) を崩さないこと
- **scheduling-only の維持**: `depends` は実行順序のみに関与し、 `input_hash` に影響しない ( ADR-0013 D4) こと
- **determinism ( R2)**: 同じ task 集合に対して同じエッジ集合が決まること
- **後方互換**: 既存の literal `depends` が無変更で動くこと
- **実装可能性**: glob 同士の包含判定のような半解決問題を持ち込まないこと ( ADR-0004 D3 / ADR-0013 と同じ制約)

### References

- [ADR-0002: fingerprint hit 判定モデル](./0002-fingerprint-hit-decision-model.md)
- [ADR-0004: Spec 検証と output 重複検出のポリシー](./0004-spec-validation-and-output-conflict-policy.md) ( D3 静的 overlap 解析の棄却理由)
- [ADR-0008: tool を first-class spec entity とする](./0008-tool-as-first-class-spec-entity.md) ( D3: path 系フィールドは spec dir 相対)
- [ADR-0013: タスク間依存を spec に明示宣言する](./0013-explicit-task-dependencies.md) ( depends / overlap 検証)
- [ADR-0015: 動的タスクを外部 command provider で生成する](./0015-dynamic-tasks-via-command-providers.md) ( producer 側の動的生成)

## Considered Options

| | A: literal 列挙 / 外部生成 ( 現状) | B: consumer を provider 化 | C: provider ごと暗黙 barrier | D: overlap から depends 自動推論 | **E: パターン depends (採用)** |
|---|---|---|---|---|---|
| fan-out の表現 | △ ( 手書き / 外部生成) | ○ | ○ | ◎ | ◎ |
| C1' ノイズ解消 | × | ○ | ○ | ◎ | ◎ |
| C3' ドリフト解消 | × | ○ | ○ | ◎ | ◎ |
| ADR-0013 overlap 検証の維持 | ◎ | ◎ | **× ( 推移対応の改修要)** | △ ( 検証 = 導出に逆戻り) | **◎ ( 無改修)** |
| scheduling-only / fingerprint 不変 | ◎ | ◎ | ◎ | ◎ | ◎ |
| spec 可読性 | ○ | × ( 静的部分が program に沈む) | ○ | ◎ | ◎ |
| ADR-0013 の明示性思想との整合 | ◎ | ◎ | △ ( 仮想ノード) | × ( 暗黙導出へ反転) | ◎ |
| 実装コスト | ゼロ | 中 ( consumer ごとに program) | 大 ( 仮想ノード + 検査推移化) | 大 | 小 |

- **Option A ( 現状維持)**: C1' / C3' が残る。 棄却。
- **Option B ( consumer を provider 化)**: consumer task を `command_providers` で emit し、 depends を plan 時に算出する。 fan-out は解けるが、 静的な `cmd` / `inputs` / `outputs` が外部プログラムに移り可読性を失う。 consumer の数だけ provider プログラムが要る。 棄却。
- **Option C ( provider ごとの暗黙 barrier)**: sloff が provider の emit した task 群を束ねる合成 barrier ノードを生成し、 consumer はそれ 1 本を待つ。 直感的だが、 barrier は出力を持たないため ADR-0013 D3 の 2 つの overlap 検証 ( いずれも **直接エッジ**前提) が破綻する ( consumer→barrier は overlap ゼロで inputs 漏れ warning が誤発火し、 consumer→barrier→producer は間接エッジなので depends 漏れが誤検知)。 検証を barrier 透過 ( 推移的に満たされたとみなす) に改修する必要があり、 仮想ノード概念とあわせて変更が大きい。 また 1 provider が異種 task を emit する場合 ( 例: 言語別生成を 1 provider で) barrier の粒度が粗すぎ、 consumer が不要な producer まで待つ。 棄却。
- **Option D ( overlap から depends 自動推論)**: consumer の `inputs` glob と他 task の `outputs` glob の交差から depends を plan 時に自動生成する。 fan-out を完全に消せるが、 ADR-0013 が明確に分離した「 順序の導出」 と「 健全性の検証」 を再び 1 機構に重ねることになり ( overlap = 検証 → overlap = 導出 への逆戻り)、 ADR-0013 の中核判断に反する。 opt-in にすれば緩和できるが本 ADR の射程を超える。 棄却 ( 将来の別 ADR 候補)。
- **Option E ( パターン depends)**: `depends[*].task` に glob を許し、 plan 時に対象 spec の task 名集合に対して展開して **literal なエッジ群**に落とす。 採用。 fan-out を 1 行で表現でき、 展開後は通常の literal depends と区別なく ADR-0013 の overlap 検証・depgraph・determinism を **無改修で**流れる。 ADR-0015 で producer を動的化したのと対称に、 consumer の依存を動的化する。

## Decision

**Option E を採用する。 `depends[*].task` に glob パターンを許可し、 plan 時に ( ADR-0015 の provider 展開後の) 対象 spec の task 名へ展開して literal なエッジ集合に正規化する。 展開後は ADR-0013 の既存経路 ( depgraph 構築 / overlap 検証 / determinism) をそのまま流れる。**

### D1. spec 文法: `task` フィールドの glob

`depends[*].task` の値に glob メタ文字 ( `*` / `?` / `[...]`) を含められる。 メタ文字を含まない値は従来どおり literal な task 名として扱う ( 後方互換)。 ADR-0008 D4 の task 名規則 ( `^[a-z0-9][a-z0-9_-]*$`) は glob メタ文字を含まないため、 値にメタ文字が現れたら一意に「 pattern」 と判定でき、 新たな区切り規則を導入せずに済む。 この一意性は task 名が実際に slug に強制されていて初めて成立するため、 `ValidateCommands` が ( static / provider 生成を問わず) 全 task 名へ D4 の規則を load 時に強制する。 これがないと `gen-*` のようなメタ文字入り task 名を受理してしまい、 それを参照する literal depends が誤って pattern 判定され、 無関係なエッジ追加や cycle を生む。

```yaml
commands:
  - name: bundle
    cmd: ...
    inputs: ["../gen/**/*"]
    outputs: ["dist/bundle.js"]
    tools: [bundler]
    depends:
      - {spec: ../gen, task: "gen-*"}        # ../gen 内の gen-* に一致する全 task に依存
      - {spec: ../gen, task: "client-*"}     # 別グループも同様に
      - {task: lint}                          # literal は従来どおり共存できる
```

- glob は **task 名のみ**に適用する。 `spec` はパターン化しない ( 単一 spec dir を指す。 ADR-0013 D1 と同じ)。 マッチ対象は `spec` で指定した 1 つの spec の task 集合
- マッチャは sloff が既に使う doublestar を流用する。 task 名は区切り文字 `/` を含まないため、 実質 `*` が「 名前内の任意の連続」 にマッチする ( `**` は意味を持たない)
- 文字列 shorthand は導入しない ( ADR-0013 D1 の方針を踏襲)

load 時 validation:

- glob 構文として不正なパターンは error ( doublestar の構文エラーをそのまま提示)
- `spec` の正規化結果が repoRoot を抜ける場合は error ( literal と同じポリシー)

### D2. 解決タイミング: provider 展開後・depgraph 構築前に literal エッジへ展開

パターンは **`command_providers` の展開 ( ADR-0015 D2, `expandProviders`) が完了し、 全 task 集合が確定した後**、 かつ `depgraph.Build` の前に、 対象 spec の task 名に対して展開する。 これにより:

- パターンは **provider が動的に emit した task にもマッチする**。 producer 側 ( ADR-0015) と consumer 側 ( 本 ADR) の動的化が噛み合い、 dir 追加で producer task が増えれば consumer のエッジも自動で増える ( C3' 解消)
- 展開後のエッジは literal な `{spec, task}` と完全に同一表現になり、 以降の depgraph 構築・overlap 検証・cycle 検出・topological sort は **無改修**で適用される

### D3. 一致規則: self 除外・0 件 error・sort + dedupe

- **self 除外**: パターンが宣言元 task 自身にマッチしても、 自己エッジは張らない ( パターンは「 一致する *他* の task すべてに依存する」 の意味)。 ADR-0013 の自己参照 error と矛盾させない
- **0 件マッチは error**: パターンが対象 spec の 1 つの task にもマッチしない場合は error ( typo / 対象 spec 取り違え / 想定 producer 不在の検出)。 literal の「 参照先 task が存在しない = error」 ( ADR-0013 D1) と整合する。 「 0 件を許容したい」 要求は現時点で想定しないため strict に倒す ( Open Question)
- **determinism**: 展開結果は ( spec, task) で sort してからエッジ集合に加える。 パターンの記述順・マッチ列挙順に依存しない ( R2)
- **dedupe**: 複数パターン間、 またはパターンと literal の間で同一エッジが生じた場合は **union ( 重複排除)** とし error にしない ( 利用者がパターンの重なりを常に避けられるとは限らないため)。 ただし ADR-0013 D1 の「 同一エッジの literal 重複宣言は error」 は、 source の literal エントリが文字どおり重複する場合に限り維持する

### D4. 既存の overlap 検証との関係 ( ADR-0013 D3 の継承)

パターンは展開後 literal エッジになるため、 ADR-0013 D3 の 2 検証は基本的に無改修で適用される。 ただし「 グループ一括依存」 という意図に対して inputs 漏れ warning が過剰発火しないよう、 後者のみ集計単位を調整する:

| 検証 | パターンでの扱い |
|---|---|
| **depends 漏れ** ( `O_A ∩ I_B ≠ ∅` なのに depends 未宣言 = **error**) | **無改修で継承**。 consumer が読むファイルの producer は、 literal か pattern 展開かを問わず必ずいずれかのエッジで覆われていなければならない。 パターンはグループ内 producer を覆うので、 consumer がそのグループの出力を読む限り自然に充足する。 グループ外の producer の出力を読むのに対応エッジが無ければ従来どおり error ( 安全側の防御線は不変) |
| **inputs 漏れ** ( depends 先の出力が inputs に 1 つも現れない = **warning**) | **パターン単位に集計する**。 パターンが展開したエッジ群のうち **どの producer の出力も consumer の inputs に現れない**場合に限り、 パターン 1 本につき warning を 1 回出す ( = パターンが consumer の読むものに全く対応していない疑い)。 一部の producer 出力でも overlap すれば、 そのパターンによるグループ依存は妥当とみなし warning を出さない。 これにより「 全部待つが一部しか読まない」 という正当なグループ依存での per-edge ノイズを避けつつ、 「 完全に的外れなパターン」 は検出する |

depends 漏れ ( error) を素通しで維持するのが要点である。 これがある限り、 パターンを使っても「 consumer が読むのに順序が保証されない producer」 は plan / run 時に必ず捕まり、 ADR-0013 が確立した順序健全性の防御線は損なわれない。

### D5. fingerprint: `depends` は input_hash に含めない ( ADR-0013 D4 の継承)

パターンも含め `depends` は純粋な scheduling metadata であり、 invalidate には関与しない。 パターンは plan 時にエッジへ展開されるだけで、 `cmd_hash` / `files_hash` / `resolved_versions_hash` のいずれにも寄与しない。 record schema は不変で、 既存 record はそのまま有効。

### D6. 互換性

pre-1.0 のため additive change として導入する。 メタ文字を含まない `task` は従来の literal として完全に同じ挙動になり、 既存 spec は無変更で動く。 record / fingerprint への影響もない。

## Consequences

### 正の影響

- consumer 側 fan-out を 1 行で表現でき、 producer の増減 ( dir 追加 / ADR-0015 の provider が emit する task の変化) に **spec 変更なしで追従**する。 C1' ( ノイズ) / C3' ( ドリフト) を consumer 側でも解消する
- 展開後は literal depends と区別がないため、 ADR-0013 の overlap 検証・depgraph・determinism が無改修で効く。 「 健全性は sloff が機械的に保証する」 という ADR-0013 / ADR-0015 の性質を維持したまま記述コストだけを下げる
- ADR-0015 ( producer 動的化) と対称な consumer 動的化となり、 per-dir codegen の spec 全体が directory 構成から導出可能になる
- depends 漏れ error を維持するため、 パターンによる順序健全性の後退はない

### 負の影響 / 注意点

- パターンが意図より広く ( または狭く) マッチする設計ミスはあり得る。 広すぎる場合は不要な直列化 ( 過剰な待ち) が、 狭すぎる ( 0 件) 場合は D3 の error が、 グループ外を読む場合は depends 漏れ error が、 それぞれ顕在化させる
- inputs 漏れ warning をパターン単位に集計する ( D4) ため、 「 パターンが一部 producer としか overlap しない」 ケースの per-edge な気づきは弱まる。 これは「 グループ一括依存」 を一級市民にするための意図的なトレードオフ
- 実行順序を読むとき、 パターンは展開しないと最終的なエッジが分からない。 `sloff graph` / 診断出力で展開後のエッジを提示してこれを補う ( 後続の更新 5)

### 撤回時の影響

パターン機能を撤去する場合、 パターンを使う spec は literal な列挙へ戻す必要があるが、 record / fingerprint には影響しない ( D5 で hash に含めていないため)。 撤回判断は別途 ADR で行う。

### Open Questions

- **OQ1**: 0 件マッチを許容する opt-in ( 例: 「 まだ producer が 1 つも無い段階で先に consumer を置く」) が必要になるか。 現時点は strict ( error) で start し、 要求が出たら別途設計する
- **OQ2**: `spec` 自体のパターン化 ( 複数 spec dir 横断のグループ依存) の需要。 本 ADR は `task` のみに限定し、 spec はパターン化しない
- **OQ3**: overlap から depends を導出する Option D ( opt-in inference) を将来別 ADR として再評価するか。 パターンより記述コストは下がるが ADR-0013 の明示性思想との整合の議論が要る

### 後続の更新

1. `internal/sloff/spec`: `Depend.Task` の pattern 判定 ( メタ文字検出) と load 時の glob 構文 validation ( D1)。 ADR-0013 の load 時 validation ( 存在 / 自己参照 / 重複 / repoRoot escape) のうち、 存在検査は展開後へ移す
2. `internal/sloff/runner` ( または depgraph 構築前の専用フェーズ): provider 展開後・`depgraph.Build` 前にパターンを対象 spec の task 名へ展開し、 self 除外・0 件 error・sort + dedupe で literal エッジ集合へ正規化 ( D2 / D3)
3. `internal/sloff/depgraph` / overlap 検証: 展開後 literal エッジに対する depends 漏れ error は無改修。 inputs 漏れ warning をパターン単位に集計するよう調整 ( D4)
4. `internal/sloff/spec` の cross-spec depends 検証 ( ADR-0013 の参照解決): 展開後のエッジ集合に対して実行する
5. `cmd/sloff/graph.go` / 診断出力: パターン由来エッジを展開後の形で表示し、 元パターンも併記できるようにする
6. [Design Doc: architecture.md](../design/architecture.md): `depends` 文法へのパターン追記、 解決順序 ( provider 展開 → パターン展開 → depgraph) の明文化
7. E2E test: パターンが provider 生成 task にマッチして展開される / 0 件 error / self 除外 / 複数パターンと literal の dedupe / depends 漏れ error がパターンでも効く / inputs 漏れ warning のパターン単位集計 / determinism ( マッチ順非依存) の各 case
