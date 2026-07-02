# ADR-0016: 集約専用の group task を first-class にする

## Context

### 背景

[ADR-0013](./0013-explicit-task-dependencies.md) は実行順序を declared `depends` のみで決定し、 overlap 計算を検証に転用した。 その D3 「 inputs 漏れ」 warning は **「 depends エッジ = データ依存」 を暗黙の前提** にしており、 想定していた偽陽性は conditional outputs ( [ADR-0004 D2](./0004-spec-validation-and-output-conflict-policy.md) の union semantics) の 1 クラスのみだった。

実運用 ( 導入先の monorepo) で第 2 の正当な zero-overlap クラスが顕在化した: **fan-in の集約点 ( barrier / alias)** である。

- proto spec の集約 task が「 per-dir 生成タスク 64 個の完了」 を 1 つの名前で下流に提供する barrier を実 task ( 集約 codegen の実行) と兼務しており、 barrier 目的の 63 エッジが毎 run 63 件の D3 warning を出した。 上流が fingerprint hit で SKIP しても producedBy は cache record の paths から埋められるため、 warning は warm run でも毎回出る ( spec の性質を run の度に可視化する、 意図された挙動)
- 同 repo で sloff 以前に使われていた codegen ツールにも、 no-op cmd ( `echo done`) の集約 task で barrier を偽装する同型の hack が存在した。 集約点の需要はツールを跨いで再発している
- sloff spec は `cmd` / `inputs` / `outputs` / `tools` を必須とするため ( ADR-0004)、 純粋な集約点を書く正当な方法が無い。 実 task に barrier を兼務させるか、 no-op cmd + ダミー宣言で偽装するかになり、 どちらも D3 検証と衝突する

なお当該ケース自体は「 barrier を廃止し、 唯一の consumer を実生成タスクへ repoint + 集約 task を plugin 別に分割」 で解消できた。 しかし fan-in の大きい下流 (「 全 per-dir 生成の完了」 を待ちたい consumer) が現れるたびに、 各 consumer が N 本のエッジを列挙するのはスケールしない。 集約点を表現する語彙が spec に必要である。

### 評価軸

- **D3 不変条件の維持**: 「 実 task の depends エッジは observable であるべき」 という検証を弱めないこと
- **意味論の正確さ**: barrier の実態は「 後に実行せよ」 ( 順序制約) ではなく「 この名前はこれらの task 集合を指す」 ( 集約点の命名) であり、 それを正しく表現できること
- **誤用への耐性**: D3 warning を理解せずに黙らせる目的で乱用できないこと ( ADR-0013 の旧判断が警戒した「 偽の安心感」 の再来防止)
- **記述コスト / 可読性**
- **実装コスト**: runner / 検証の意味論への影響が小さいこと

### References

- [ADR-0004: Spec 検証と output 重複検出のポリシー](./0004-spec-validation-and-output-conflict-policy.md)
- [ADR-0013: タスク間依存を spec に明示宣言する (`depends`)](./0013-explicit-task-dependencies.md)
- [ADR-0015: command provider による動的タスク生成](./0015-dynamic-tasks-via-command-providers.md) ( OQ2: provider 出力スキーマの wire 互換)
- 前例: Ninja `phony` rule / Bazel `alias`・`test_suite` / Make `.PHONY` 集約 target

## Considered Options

### Comparison Table

| | A: 現状維持 | B: order-only depends flag | C: warning 抑制 flag | **D: group task (採用)** |
|---|---|---|---|---|
| D3 不変条件の維持 | ◎ ( 検証は無傷だが noise) | × ( エッジ単位で検証対象外を作る) | × ( 同左) | ◎ ( 実 task のエッジは全て検証対象のまま) |
| 意味論の正確さ | × ( barrier を実 task が兼務) | △ ( 順序制約ですらないものを順序と表現) | × ( 意味論を足さない) | ◎ ( 集約点をそのまま表現) |
| 誤用への耐性 | ◎ | × ( warning 黙らせ flag として乱用可能) | × ( 同左) | ◎ ( task 種別なので実 task の検証を逃れられない) |
| 記述コスト | × ( 毎 run の warning noise) | ○ | ○ | ◎ |
| 実装コスト | ◎ ( ゼロ) | ○ | ◎ | ○ ( task 種別の分岐追加) |

### Option A: 現状維持 ( 実 task に barrier を兼務させる)

- × barrier エッジ数ぶんの warning が毎 run 出続け、 正当な警告 ( conditional outputs の告知や真の inputs 漏れ) を noise に埋める
- × 回避策として上流 outputs を inputs に足すのは「 読んでいないファイルを読む」 という嘘の宣言で、 hash 計算コストも増える

棄却。

### Option B: order-only depends flag ( Ninja の `||` / Make の order-only prerequisites 相当)

`depends: [{task: x, order-only: true}]` を追加し、 D3 warning の判定対象から外す案。

- ○ barrier 以外の純順序制約 ( 共有 scratch dir の直列化等) も表現できる汎用性がある
- × barrier の実態は順序制約ではない ( 動機となった実例では、 集約 task は上流の生成物を一切読まず、 順序自体も不要だった)。 エッジ属性として表現するのは意味がズレる
- × warning の意味を理解できない利用者が貼って黙らせる誘惑を、 エッジ単位で開いてしまう。 「 依存は明示してあるから大丈夫」 という偽の安心感 ( ADR-0013 §旧判断) の再来経路になる
- × 「 実 task の depends エッジは observable」 という不変条件が「 一部のエッジは除く」 に弱まる

棄却。 真の順序制約 ( データを共有しないが同時実行できない task pair) が実例として現れたら、 その時に実例ベースで再検討する。

### Option C: warning 抑制 flag ( エッジ or task 単位の suppress)

- × 意味論を追加せず検証だけ黙らせる。 B と同じ誤用経路を持ち、 B が持つ表現力の獲得すら無い

棄却。

### Option D: group task ( 採用)

`group: true` の task 種別を新設する。 `cmd` / `inputs` / `outputs` / `tools` を持たず、 `depends` のみを持つ純粋な DAG ノード。

- ◎ 「 この名前はこれらの task 集合を指す」 という barrier の実態をそのまま表現する
- ◎ 実 task のエッジ不変条件は無傷。 group は inputs を持たないため D3 warning の判定対象そのものにならず、 「 判定を弱める」 のではなく「 判定すべきものが構造的に無い」
- ◎ 実行も fingerprint も持たないため runner の意味論への影響が最小
- △ task 種別が 2 つになり spec / depgraph / runner / explain に分岐が増える

## Decision

**Option D を採用する。 `group: true` を宣言した task は depends のみを持つ集約点となり、 実行・fingerprint・overlap 検証の対象外とする。**

### D1. spec 文法

```yaml
commands:
  - name: gen-all
    group: true
    depends:
      - {task: gen-foo}
      - {spec: ../other, task: gen-bar}
```

load 時 validation ( いずれも error):

- group task が `cmd` / `inputs` / `outputs` / `tools` のいずれかを持つ ( 「 group は集約点であり仕事を持たない」 を構造で強制する)
- group task の `depends` が空 ( 何も集約しない group は無意味であり、 書き間違いとして扱う)
- `depends` の既存 validation ( 参照先不在 / 自己参照 / 重複 / repoRoot 逸脱、 ADR-0013 D1) は同様に適用

非 group task の必須フィールドは従来通り ( ADR-0004)。

### D2. 実行モデル

group は実行されず fingerprint も持たない。 スケジューラ上は「 全 depends の完了で即完了」 となるノードで、 depends のいずれかが fail すれば group も fail する ( 下流に伝播する)。

- `RUN` / `SKIP` ログは出さない ( 仕事が無いので状態遷移として無意味)
- producedBy に登録しない ( outputs が無く、 登録すべきものが無い)
- fingerprint record を書かない ( ADR-0002 の hit 判定の対象外)

### D3. 検証との関係

- **D3 warning ( inputs 漏れ)**: consumer が group の場合は判定を明示的に skip する。 inputs が空のため機械的には全エッジが「 overlap 無し」 に該当してしまうが、 それは group の定義そのものであり告知する意味が無い。 group が upstream の場合は何も produce しないため、 既存ロジック ( producedBy 不在 → skip) のまま対象外になる
- **depends 漏れ ( error)**: group への depends は **データ読み取りの免罪符にならない**。 consumer が group メンバーの生成物を読む場合、 実 producer への直接エッジを従来通り要求する。 group を透過扱いすると「 大きな group に depends しておけば何を読んでも通る」 抜け道になり、 D3 の error 側が骨抜きになる
- **cycle 検証**: 既存どおり ( group もノードとして参加する)
- **output 衝突 ( ADR-0004 D3)**: outputs を持たないため対象外

### D4. invalidation は group を流れない

group への depends は scheduling ( とラベリング) のみで、 fingerprint の invalidate には一切関与しない。 これは新しい制約ではなく ADR-0013 D4 ( depends は input_hash に含めない) の帰結である。 上流の生成物を読む consumer は、 実 producer への直接エッジ + inputs 宣言が引き続き必要で、 それは D3 の depends 漏れ error が強制する。

### D5. command provider との関係

当面、 provider ( ADR-0015) は group を emit できない ( 出力スキーマ v1 の写像対象は従来フィールドのみ)。 provider から group が必要になった時点で、 `schema_version` bump とセットで拡張する ( ADR-0015 OQ2 の運用)。

なお「 旧 sloff × group を emit する新 provider」 の組み合わせは、 JSON の未知フィールド無視により group task が通常 command として validate され `cmd is required` で loudly fail する。 静かな誤動作にはならないため、 移行順序の事故は検出可能である。

### D6. explain / graph 表示

`sloff graph` / explain には group をノードとして表示する ( エッジの可視化が主目的のため省略しない)。 実 task と区別できる表示形式にする ( 具体形は実装時に決める)。

### D7. 互換性

追加的変更であり、 既存 spec は無変更で動く。

## Consequences

### 正の影響

- barrier / alias を偽装なしで書ける。 D3 warning の第 2 の偽陽性クラス ( grouping エッジ) が、 warning の緩和ではなく **モデルの表現力追加** によって構造的に消える
- fan-in の大きい task 集合を待つ consumer の記述コストが O(N) → O(1) になる
- 「 実 task の depends エッジは全て observable であるべき」 という検証は一切弱まらない

### 負の影響 / 注意点

- task 種別が 2 つになり、 spec / depgraph / runner / explain に group 分岐が増える
- group への depends では invalidation が流れない ( D4)。 「 group に依存しているから上流変更で再実行される」 という誤解の余地はあるが、 データを読む場合は depends 漏れ error が直接エッジを強制するため、 誤解が silent stale に到達する経路は無い
- 空 depends を error にするため、 「 とりあえず名前だけ予約する」 用途には使えない ( 意図的な制約)

### 撤回時の影響

group を使う spec は、 group の depends を各 consumer に展開する機械的な書き換えで実 task 構成に戻せる。 fingerprint record には何も書いていないため、 record / storage への影響は無い。

### 後続の更新

1. [Design Doc: architecture.md](../design/architecture.md): spec ファイル形式 / §タスク間依存に group を追記
2. `internal/sloff/spec`: `Command.Group` の追加と D1 の load 時 validation
3. `internal/sloff/depgraph`: group ノードの DAG 参加 ( cycle 検証含む)
4. `internal/sloff/runner`: スケジューラの即完了ノード化、 fingerprint / producedBy / RUN・SKIP ログの skip、 D3 warning の consumer 側 skip
5. `internal/sloff/explain` / `cmd/sloff/graph.go`: group の表示
6. E2E test: group 経由の実行順序 ( clean state 含む) / group メンバーの生成物を読む consumer に対する depends 漏れ error が group 経由で満たされないこと / group の validation error 各種 ( cmd 等の混在 / 空 depends / 参照不在) / group を含む graph 表示 golden
