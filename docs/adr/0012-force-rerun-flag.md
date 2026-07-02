# ADR-0012: fingerprint を bypass して全 task を強制実行する `--force` flag

## Status

Accepted

## Context

### 背景

[ADR-0001](./0001-fingerprint-aware-codegen-orchestrator-decision.md) は sloff の根幹方針として「**fingerprint が `skip` を返したとき、 その出力が本当に正しいことを構造で保証する**」 を据え、 そのうえで「fingerprint を信じきれず手動で `--no-fingerprint` を打つ」 運用文化が共有 fingerprint store の存在意義を毀損する、 と警戒している。 2 防御線 ( OS 中立 logical version × output-comparison ヒット判定) はこの不信感を構造で消すための投資である。

一方で、 [storage-dynamodb.md §"キャッシュレイヤの ON/OFF"](../design/storage-dynamodb.md) は「 fingerprint そのものを bypass して全 task 強制実行」 の override は別途扱う、 と明示的に先送りしていた。 ローカル debug ( generator の non-deterministic 性を疑った時の確認)、 cache を温め直したい時 ( 古い CI が壊れた record を share してしまった等)、 generator 本体の変更後に全件を作り直したい時など、 「**fingerprint hit を信じない判断を、 利用者が明示的に下したい場面**」 は実務上存在し、 既存の `SLOFF_ALLOW_STALE_DEPS` ( preflight 失敗を warn 降格 + read-only にする escape hatch) ではこの責務を担えない。

本 ADR は ADR-0001 の警戒 ( 「`--no-fingerprint` を雑に打つ運用文化」 を生まない) を維持したまま、 この override を `--force` CLI flag として導入する判断を固める。

### 評価軸

- **ADR-0001 の 2 防御線を毀損しない**: 強制実行モードでも (1) OS 中立 logical version の取得経路 (2) output-comparison ヒット判定 ( 結果 record として残るもの) は構造上そのまま残ること
- **`--no-fingerprint` 文化を誘発しない**: 「常用したくなる」 形にしない ( 既定値 off、 CI でデフォルト ON にする標準手段は提供しない、 永続化される設定ファイル経由でなく明示の起動オプション)
- **`SLOFF_ALLOW_STALE_DEPS` と責務が混ざらない**: 既存 escape hatch は preflight failure → read-only 降格を担う。 本 flag は fingerprint hit の bypass を担う。 独立に動かせる
- **共有 fingerprint store の汚染を許容できる範囲に抑える**: 強制実行が record を「 上書きする」 のか「 書き込まない」 のかで、 汚染リスクとユースケース充足が分かれる
- **観測可能であること**: 強制実行された run は trace / log で識別できる ( 後から「 なぜ skip しなかったか」 を辿れる)

### References

- [ADR-0001: fingerprint ベースのコード生成オーケストレーターの選定](./0001-fingerprint-aware-codegen-orchestrator-decision.md)
- [ADR-0002: fingerprint hit 判定モデル](./0002-fingerprint-hit-decision-model.md)
- [ADR-0009: fingerprint の直列化形式 (protobuf binary)](./0009-fingerprint-binary-serialization.md)
- [Design Doc: sloff Architecture](../design/architecture.md)
- [Design Doc: DynamoDB storage backend](../design/storage-dynamodb.md)

## Considered Options

### Comparison Table

| | A: 提供しない | B: env var のみ (`SLOFF_FORCE_RUN=1`) | **C: `--force` CLI flag (採用)** | D: `--force` + write-skip ( read-only 化) |
|---|---|---|---|---|
| ADR-0001 の 2 防御線 | ◎ | ◎ | ◎ | ◎ |
| `--no-fingerprint` 文化抑制 | ◎ ( 構造で不可) | △ ( `.env` / CI 変数で常時 ON 化しやすい) | ○ ( 明示の起動 option) | ○ |
| `SLOFF_ALLOW_STALE_DEPS` との責務分離 | n/a | △ ( env var 同士で混同しやすい) | ◎ | ◎ |
| cache warm-up ユースケース | × | ◎ | ◎ | × ( 書き込まないので二度同じ事をする羽目) |
| 汚染 record 防衛 | ◎ | ○ | ○ | ◎ |
| 観測性 | n/a | △ ( env 起動なので shell 履歴に残らないことがある) | ◎ ( CLI 履歴 + span attribute) | ◎ |

### Option A: 提供しない

ADR-0001 の警戒に忠実な選択。 ただし「ローカル debug で一回だけ全 task 再実行したい」 「 壊れた record を共有してしまった後の集合的 cache warm-up」 は実務上のニーズとして残り、 利用者は `.sloff/fingerprints/` の rm + 通常 run で代替することになる。 これは結果的に **record を一旦消す** という破壊的操作を伴い、 並行 run / 共有 store の運用と衝突する。 棄却。

### Option B: env var のみ (`SLOFF_FORCE_RUN=1`)

`SLOFF_ALLOW_STALE_DEPS` と同じ世界観 ( env var による escape hatch) で揃える案。

- ◎ 既存の escape hatch と並べやすく、 学習コスト低
- △ env var は `.env` / CI の env 設定で **常時 ON 化されやすい**。 ADR-0001 が警戒する「`--no-fingerprint` 文化」 が定着するリスクがこの flag の中では最も高い
- △ `SLOFF_ALLOW_STALE_DEPS` と `SLOFF_FORCE_RUN` で 「 どっちが何だっけ」 の混同が起きやすい

「 一回きりの bypass」 という意図と env var の常駐性が噛み合わない。 棄却。

### Option C: `--force` CLI flag (採用)

`sloff run --force` で起動した run のみ fingerprint hit を bypass し、 全 task を強制実行する。 record の書き込みは通常通り行う。 preflight は通常通り走らせる ( `SLOFF_ALLOW_STALE_DEPS` とは独立)。

- ◎ 「 一回ごとの明示判断」 を構造化できる。 shell 履歴 / Make target / CI job に残り、 後から監査できる
- ◎ env var による常時 ON 化を構造で抑える ( `--force` を自動でつけ続ける CI 設定は記述コストがかかる + PR review で目に入る)
- ◎ record を書き込むため、 「cache warm-up」 ユースケースを満たす ( 次回以降は通常 run で skip 可能)
- ◎ preflight は独立に動くため、 lockfile drift で実行を止める安全網はそのまま機能する
- △ 強制再生成中に generator が壊れていると、 壊れた output で record を上書きする可能性がある。 ただし ADR-0001 の (2) output-comparison は **次回以降の skip 判定** で fail-fast に検出できる ( 強制 run 自体は skip 判定を bypass するが、 後続の通常 run が drift を捕まえる)

### Option D: `--force` + 書き込みも止める ( read-only 化)

「 壊れた record を上書きしてしまうかも」 の懸念に対し、 強制実行時は record を書き込まないとする案。

- ◎ 強制実行による record 汚染 リスクが構造で 0 になる
- × 「cache を温め直したい」 「 generator 修正後に全件作り直して record を更新したい」 ユースケースが満たせない ( 強制 run 後にもう一度通常 run を回す必要があり、 そこで初めて record が更新される)
- × 「強制実行モードでは何も書かない」 は ADR-0001 が警戒する「fingerprint 信頼の侵食」 を逆方向から強化してしまう ( 利用者は `--force` 後に「 また走らせ直さないと共有されない」 と知り、 結果的に `--force` をデフォルトにする CI を組みやすくなる)

Option C の代替として残せるが、 ユースケース充足度で劣る。 棄却。

## Decision

**Option C: `--force` CLI flag を導入する。 強制実行モードでも fingerprint 書き込みと preflight は通常通り行う。**

採用根拠:

1. **`--no-fingerprint` 文化への構造的抑止が CLI flag では成立する**。 env var による常時 ON 化を防ぎ、 shell 履歴 / CI 設定 / Make rule のレベルで明示判断が残る
2. **既存 escape hatch (`SLOFF_ALLOW_STALE_DEPS`) と責務が分離される**。 前者は preflight failure → warn 降格 + read-only。 後者は fingerprint hit の bypass。 直交する
3. **ユースケース充足**: ローカル debug、 record warm-up、 generator 変更後の全件再生成のいずれも満たす
4. **ADR-0001 の 2 防御線は不変**: input_hash の計算経路 ( (1)) は変えない。 output-comparison ヒット判定 ( (2)) は 「 強制 run の skip 判定では使わない」 だけで、 record 構造としては残り、 次回以降の通常 run で fail-fast に効く

### CLI 仕様

```sh
sloff run --force
```

- 既定値: `false` ( 通常 run と同じ動作)
- 環境変数 mirror は **提供しない** ( ADR-0001 警戒のための意図的判断)
- `SLOFF_ALLOW_STALE_DEPS` との併用は許可 ( preflight failure 下でも強制 run したいケース、 例えば lockfile drift を承知で generator 変更を検証したい時)。 この場合 read-only 化が優先され、 record は書かれない

### Runner 仕様

`runner.Options` に `Force bool` を追加し、 既存 `ReadOnly` の隣に置く。 `runTask` 内の fingerprint hit 判定経路で、 `Force=true` のとき:

- prefetched / live storage いずれの record 取得経路も変更しない ( ADR-0009 §"byte stability" の write-skip ルールで `existing` を引き続き使うため)
- output-comparison ヒット判定の結果に関わらず `hit=false` 扱いで cmd を実行する
- output 計算 / record 書き込み経路は通常 run と同じ。 ADR-0009 §"byte stability の担保" §4 の write-skip ルールはそのまま効くため、 output が変わらないなら record は実質上書きされない ( informational field の first-observed value が保持される)

### preflight との関係

preflight は `--force` の有無に関わらず通常通り走る。 lockfile drift / install drift は record 汚染の最大要因の一つであり、 ここを bypass すると ADR-0001 の (1) 防御線を毀損するため。 利用者が drift を承知で強制 run したい場合は `SLOFF_ALLOW_STALE_DEPS=1` と組み合わせる ( 上述、 この場合 record は書かれない)。

### 観測性

- `cmd/sloff/run.go` の root span に `sloff.force` ( bool) attribute を追加
- `runner.runTask` の task span にも同じ attribute を追加し、 trace 解析側で「 hit を bypass したか」 が後から判別できるようにする
- log には強制実行であることを 1 行 INFO で出す ( cmd 毎の "RUN" log を変えるかは実装側の判断、 trace の attribute が一次情報源)

### CI でのデフォルト ON は推奨しない

CI で `sloff run --force` を常時打つ運用は、 共有 fingerprint store の skip 機能を CI から見て無効化する。 これは ADR-0001 が警戒する「`--no-fingerprint` 文化」 そのものになるため、 sloff としては推奨しない。 CI で record が hit しない事象が頻発する場合は (a) preflight drift の修正 (b) generator non-deterministic 性の修正 (c) 古い壊れた record の GC のどれかを行うべきで、 `--force` は症状を覆い隠す手段としては使わない。

## Consequences

### 正の影響

- ローカル debug / cache warm-up / generator 修正後の全件再生成という実務ニーズに、 record 削除という破壊的操作を伴わずに応えられる
- `SLOFF_ALLOW_STALE_DEPS` の責務 ( preflight 降格 + read-only) と混同せず、 独立に動かせる
- ADR-0001 の 2 防御線は構造上不変。 強制 run で書かれた record も次回以降の通常 run で output-comparison に晒される

### 負の影響

- 強制 run 中に generator が壊れていると、 壊れた output で record を上書きする可能性がある。 ただし ADR-0009 §"byte stability の担保" §4 の write-skip ルールで output が同一なら overwrite されないため、 「`--force` が誤って record を破壊する」 経路は output が実際に変わる場合に限る
- CLI 表面が一つ増える。 利用者の学習コストは +1

### 撤回時の影響

`--force` を後から削除する場合、 利用者は `.sloff/fingerprints/` の rm + 通常 run に逆戻りする。 record format / schema 側に副作用は無いため、 撤回コストは小さい。 ただし「 一度提供した escape hatch を取り上げる」 ことの利用者影響は大きいため、 撤回判断は別途 ADR で行う。

### 後続の更新

本 ADR の決定を受けて以下を更新する:

1. [Design Doc: storage-dynamodb.md](../design/storage-dynamodb.md) §"キャッシュレイヤの ON/OFF": 「DEV-22 で別途扱う」 の記述を本 ADR への参照に置き換える
2. [Design Doc: architecture.md](../design/architecture.md): preflight 節の escape hatch 説明 ( `SLOFF_ALLOW_STALE_DEPS`) の隣に `--force` を併記する
3. `internal/sloff/runner/runner.go`: `Options.Force bool` を追加、 `runTask` の hit 判定 bypass、 span attribute 追加
4. `cmd/sloff/run.go`: `--force` flag を追加、 `runner.Options.Force` に伝搬、 起動 span に `sloff.force` attribute
5. E2E test (`internal/sloff/runner` / `cmd/sloff`): 既存 record がある状態で `--force` を渡し、 全 task が再実行されること、 output 同一なら write-skip ルールで record の bytes が変わらないことを確認する case を追加
