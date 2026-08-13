# ADR-0021: ベンチマークスイートと CI 回帰ゲート

## Status

Accepted

## Context

### 背景

sloff は「大規模 monorepo でコード生成を速く回す」こと自体が製品価値であり、 実際に perf 専用の変更が積み重なっている ( #17 full-hit 高速化 / #47 digest・glob メモ化 / #49 `git ls-files` バッチ / #51 DynamoDB credential warming / #52 setup 短縮 / #53 `packages.Load` バッチ / #54 並行 Discover / #57 prewarm オーバーラップ / [ADR-0014](./0014-persistent-file-content-hash-cache.md) 永続 digest cache / [ADR-0020](./0020-scheduling-priority-downstream-height.md) downstream 高さ scheduling)。

一方で既存のテスト資産 ( E2E golden) は **正しさ ( どのファイルが生成され、 どの record が書かれるか) しか検証しない**。 将来のリファクタが上記の最適化のどれかを黙って壊しても、 全テストは green のまま通る。 この非対称を埋める自動の安全網が本 ADR の対象である。

### 制約 / 評価軸

- **R1 ( 感度・最重要)**: 各ガードは「守っている最適化を取り除いたら実際に動く」ことが実証されていること。 動かないベンチマークは偽の安心を与えるため、 無いより悪い
- **R2 ( ノイズ耐性)**: shared GitHub runner のノイズで偽陽性を出さないこと。 赤に慣れさせたら ( 3% のノイズを追ってメンテナが赤を無視するようになったら) ゲートは死ぬ。 実 30% 級の回帰を確実に捕まえる方に倒す
- **R3 ( 自動・非babysit)**: メンテナが PR ごとに手で benchstat を回す運用にしない。 baseline の手動更新も要求しない
- **R4 ( hermetic)**: CI 上で外部サービス・実クラウド認証に依存しない
- **R5 ( 可読性)**: macro の計測はメンテナが既に読んでいる `SLOFF_DEBUG_TIMING` ( [ADR-0018](./0018-otel-tracing.md)) のフェーズ分解と同じ軸で出すこと

### wall-clock 系最適化の観測問題

過去の perf 変更の多くは **CPU スループットではなく wall-clock / レイテンシ / 呼び出し回数** の勝ちであり、 素朴な `go test -bench` ( 単関数の ns/op) では観測できない:

- ADR-0020 の scheduling は「同じ総仕事を slot にどう詰めるか」の makespan 短縮で、 `depgraph.Build` の ns/op はほぼ不変
- #53 / #49 は「高価な呼び出し ( `packages.Load` / `git ls-files` spawn) の回数」の削減
- #57 は「prewarm を script 版本解決の裏に隠す」レイテンシ隠蔽で、 個別関数はどれも速くならない

ガードは各最適化の **メカニズムそのもの** を測る必要がある。

### References

- [ADR-0014](./0014-persistent-file-content-hash-cache.md) / [ADR-0018](./0018-otel-tracing.md) / [ADR-0020](./0020-scheduling-priority-downstream-height.md)
- perf PR 系譜: #17, #47, #49, #51, #52, #53, #54, #57, #64
- 実測記録: [.claude/skills/sloff-perf/references/lessons.md](../../.claude/skills/sloff-perf/references/lessons.md) ( 感度実験の生データとキャリブレーション。 perf 作業の方法論 skill に同梱)
- 運用ガイド: [docs/benchmarks/README.md](../benchmarks/README.md)

## Considered Options

### Comparison Table

| | A: benchstat 手動運用 | B: committed absolute baseline | **C: merge-base 相対比較 + 統計ゲート ( 採用)** | D: 外部の継続ベンチ基盤 / 専用ランナー |
|---|---|---|---|---|
| 自動でビルドを止める (R3) | × | ◎ | ◎ | ◎ |
| runner ノイズ耐性 (R2) | ○ ( 人間が判断) | × ( ランナー個体差で恒常的に flaky) | ○ ( 同一ランナー・interleave・有意性検定) | ◎ |
| baseline 管理コスト | — | × ( 手動 rebaseline が必要) | ◎ ( merge-base が常に baseline) | △ |
| 導入・維持コスト | ◎ | ○ | ○ | × ( 個人メンテ規模に過剰) |

### Option A: benchstat 手動運用

ベンチマークだけ足し、 比較はメンテナが手元で benchstat を回す。

棄却。 R3 に反する ( メンテナは「babysit しなくても信頼できるゲート」を明示的に選んでいる)。 レビュー時に回し忘れた PR がそのまま回帰を持ち込む。

### Option B: committed absolute baseline

`ns/op` の期待値をリポジトリにコミットし、 CI 実測との比を閾値判定する。

棄却。 GitHub の shared runner は世代・負荷で数十 % 単位の個体差があり、 絶対値 baseline は原理的に flaky ( R2 ×)。 flaky を避けようと閾値を広げると今度は実回帰を素通しする。 baseline 更新という手動運用も増える ( R3 ×)。

### Option C: merge-base 相対比較 + 統計ゲート ( 採用)

PR head と merge-base を **同一 job・同一ランナー上で時間的に interleave して** 実行し、 その場で比較する。 絶対値は一切コミットしない。 時間系メトリクスは「統計的有意 ( Mann-Whitney U) **かつ** 閾値超え」の二重条件でのみ fail。 決定的メトリクス ( 後述 D2) は増加即 fail。

- ◎ baseline は常に merge-base なので rebaseline という運用が存在しない ( 意図的な挙動変更はガード側の期待値を同 PR で更新すれば、 merge 後は自動的に新 baseline になる)
- ◎ 同一ランナー interleave で個体差・ドリフトが相殺される
- △ ジョブ時間が増える ( base 側もビルド・実行するため)

### Option D: 外部の継続ベンチ基盤 / 専用ランナー

bencher / codspeed 等の SaaS、 または self-hosted の専用ベンチランナー。

棄却 ( 現時点)。 ノイズ制御としては最強だが、 個人メンテの OSS に外部サービス依存・専用インフラ維持を持ち込むのは過剰。 Option C で捕まえ損ねる回帰クラスが実際に観測されたら再検討する。

## Decision

**Option C を採用し、 スイートを 3 層で構成する。**

### D1. スイートの 3 層構成

1. **micro-benchmark** ( `go test -bench`、 各パッケージの `bench_test.go`): 過去 perf PR が触った hot path を、 その最適化に感度がある形で測る。 `hash.FileCache` ( within-run / persistent warm / cold)、 `glob.Expander` ( shared-base cold / memoised repeat / per-pattern reference)、 `spec.Discover` ( 広いツリー + node_modules デコイ)、 `depgraph.Build`
2. **macro-benchmark** ( `internal/sloff/runner/bench_test.go`): `internal/sloff/benchgen` が生成する合成 monorepo ( 既定 501 task / 30,060 input file / chain 深さ 5 / 幅 400 の shallow fan + sink = ADR-0020 が対象にした導入先 monorepo 型の形状) に対する in-process フル `Run`。 シナリオは **cold** ( record 無し・output あり) / **warm-incremental** ( chain 先頭 1 file 変更) / **full-hit** ( 無変更) の 3 種。 warm / full-hit は `filehash=persist|memory` の 2 variant を常設し、 ADR-0014 の効果差 ( = `SLOFF_NO_FILE_HASH_CACHE` 相当の toggle) を毎回可視化する。 フェーズ内訳は ADR-0018 の span をそのまま集計し `discover/resolve/collect/prefetch/tasksrun/hashinputs/taskexec/fpload-ms/op` として報告する ( R5)
3. **決定的ガード** ( 通常テスト + カスタムメトリクス): wall-clock 系最適化はメカニズムを直接検証する ( D2)

なお macro の cold は「record 無し・**output あり**」と定義する。 sloff のモデルでは生成物は git 管理されており ( fresh clone でも存在する)、 output が無い clean 状態では下流 task の cross-task input glob が展開されず input_hash のキーが定常状態とずれるため、 「output も無い」状態は製品上の cold と一致しない。

### D2. wall-clock 系は ns/op ではなくメカニズムで守る

| 最適化 | ガード | 判定 |
|---|---|---|
| ADR-0020 scheduling | runner と同じ slot 意味論 ( emit 順 admission・待機中も slot 保持) の決定的シミュレータで makespan を算出し `makespan-ticks/op` として報告 + 「Build 順 < 辞書順」の比較テスト | メトリクス増加 = fail / テスト fail |
| #53 `packages.Load` バッチ | counting fake lister で `batchloads/op` ( = spec dir 数) / `listloads/op` ( = 0) を報告 + 同値の通常テスト | 増加 = fail |
| #49 `git ls-files` バッチ | counting enumerator で `enumcalls/op` ( = 1) を報告 + 通常テスト | 増加 = fail |
| #57 prewarm オーバーラップ | handshake する fake resolver による並行構造テスト ( prewarm 実行中に eager 解決が観測されること / gated 解決が prewarm 完了後であること) | テスト fail |

これらのメトリクスはコードの純関数で run-to-run 完全一致するため、 ゲートは統計検定なしで「増加 = 回帰」と扱える ( ノイズ 0 の最も強いゲート)。 意図的に設計を変える場合はガードの期待値と本 ADR を同じ PR で更新する。

### D3. CI ゲート ( internal/benchgate)

- `ci.yml` に独立 job `bench` を追加。 merge-base を `git worktree` に展開し、 **macro 3 round × count 2、 micro 5 round × count 1 を base / head 交互に** 実行して 2 つの生ログを作る ( 交互実行が遅いドリフトを相殺する)
- `internal/benchgate` ( `golang.org/x/perf` の benchfmt / benchmath) が単位でメトリクスを分類して判定する:
  - **時間系** ( `sec/op`, `*-ms/op`): Mann-Whitney U で p < 0.05 **かつ** median 悪化が +30% 超のときのみ fail。 二重条件により「有意だが微小」も「巨大だが不安定 ( 単発スパイク)」も素通しする ( R2)
  - **`*-ms/op` の絶対デルタ床 ( 25ms)**: 分母の小さいフェーズ ( resolve ≈ 数 ms / fpload ≈ 十数 ms) は、 較正実験で「~1ms のドリフト + タイマ量子化だけで有意 かつ +30% 超」に到達し得ることが実証された ( [lessons.md](../../.claude/skills/sloff-perf/references/lessons.md) の検証パス)。 そのため `*-ms/op` は悪化の絶対量が 25ms 以上のときのみ fail する ( resolve 4ms → 200ms のような実 blowup は依然捕まる)
  - **決定的** ( `makespan-ticks/op`, `batchloads/op`, `listloads/op`, `enumcalls/op`): 増加即 fail
  - **その他** ( `B/op`, `allocs/op`, 未知単位): 表示のみ
  - 片側にしか存在しないベンチマークは note 扱いで fail しない ( スイート導入 PR・ベンチ追加 PR が構造的に green)
  - 時間系で標本数が 4 未満なら **エラー** ( 検定が無力なまま silent pass するのは CI 設定ミス)
  - **fail-open 防止**: head 側に決定的ガード 4 単位と macro の `Run/scenario=*` が 1 つも見つからなければ **エラー**。 ベンチマークの rename / `-bench` regex のズレ / パッケージ移動でガードが消えても `go test` は green のままなので、 存在検査なしではゲート全体を無音で解除できてしまう ( ローカルの ad-hoc 比較は `-no-require` で外せる)
- 閾値 +30% は初期値。 [lessons.md](../../.claude/skills/sloff-perf/references/lessons.md) のキャリブレーション ( 同一コミット同士の比較で観測されたノイズ幅: ローカル / CI とも非有意 ±10% 以内、 有意到達は +7% 未満の微小系のみ) を上回るよう設定し、 CI 上での実測が溜まったら見直す

### D4. `-race` の分離

race detector は実行時間を大きく歪めるため、 **bench job は `-race` を使わない**。 既存 test job の `-race` は不変で、 決定的ガードの通常テスト ( call count / makespan 比較 / 並行構造) はそちらでも走る ( ゲートが動かない環境でも -race テストが最低限の回帰検出をする二重化)。

### D5. 感度の実証を要求する

ガードを追加・変更する PR は「最適化を無効化したらガードが動く」実験 ( 環境 toggle があれば toggle、 なければ scratch revert) の観測値を [lessons.md](../../.claude/skills/sloff-perf/references/lessons.md) に記録する。 動かないガードは追加しない ( R1)。

### D6. スコープ外: #51 ( DynamoDB credential warming)

`Warm` は実 AWS 認証チェーン ( SSO / STS の RTT ~1–2s) を裏で温める最適化で、 遅延源が外部サービスにある。 hermetic CI で fake を温めても守りたい量 ( 実 RTT の隠蔽) を測ったことにならないため、 **意図的に除外** し理由をここに記録する ( R4)。 既存の unit test ( Warm の転送・no-op 経路) は維持されている。

## Consequences

### 正の影響

- 過去の perf 資産すべてに「壊したら CI が止まる」ガードが付く。 E2E golden ( 正しさ) との役割分担が明確になる
- rebaseline 運用が存在しない: merge されたものが次の baseline
- 決定的メトリクスはノイズ 0 なので、 scheduling / バッチングの回帰は 1 サンプルでも確実に検出される

### 負の影響 / 注意点

- bench job で PR の CI 時間が増える ( 実測: GitHub hosted runner で **約 4 分**、 base 側のビルド・実行込み)。 round 数を減らす場合は検定の標本数要件 ( benchgate `-min-count`) を割らないこと
- 時間系ゲートの閾値 +30% は「実 30% 級を確実に、 3% を追わない」という R2 側への意図的な倒し。 小さな漸進的劣化 ( 例: 毎 PR +5%) は積もるまで検出できない。 これは macro のフェーズメトリクスを人間が時々眺めることで補う
- macro シナリオは `sync.OnceValues` の共有 fixture 上で順に走るため、 各シナリオの setup は前のシナリオが何を残しても状態を確立し直す規約 ( `converge`) を守る必要がある
- 合成 repo は script tool のみ ( go-local / pnpm-local の実 resolve は macro に含まれない)。 それらは micro + 決定的ガード側で守られている

### 撤回時の影響

`bench` job と `internal/benchgate` / `scripts/bench.sh` を消せば従来 CI に戻る。 ベンチマーク・ガードテスト自体は通常のテスト資産として残せる。

### 後続の更新

1. CI 上のランナー分散を数回の再実行で実測し、 閾値の妥当性を [lessons.md](../../.claude/skills/sloff-perf/references/lessons.md) に追記する
2. [architecture.md](../design/architecture.md) の関連リンクに本 ADR を追加する
