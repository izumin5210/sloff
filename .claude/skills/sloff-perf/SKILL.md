---
name: sloff-perf
description: >-
  sloff のパフォーマンス改善・ベンチマーク/ガード設計・CI bench ゲート運用の方法論。
  Use this skill whenever the task touches sloff performance in any way:
  「遅い」「速くしたい」「optimize」「perf」「ボトルネック」「レイテンシ」の調査・改善、
  ベンチマークやガードの追加・変更・レビュー、bench job / benchgate の失敗対応、
  しきい値やノイズのキャリブレーション、SLOFF_DEBUG_TIMING の読み解き、
  perf に影響しうるリファクタの事前確認(「この変更で遅くならない?」)まで含む。
  性能の数値を 1 つでも口にするタスクなら必ず先にこの skill を読むこと。
---

# sloff perf 作業の方法論

sloff は「大規模 monorepo でコード生成を速く回す」こと自体が製品価値であり、
perf 資産は ADR-0021 のベンチマークスイート + CI ゲートで保護されている。
この skill は、その資産を **壊さず・騙されず・再現可能に** 拡張するための手順書。

## 絶対原則

1. **実測なき数値を書かない。** 推定・記憶・外挿を実測のように報告しない。
   すべての性能主張は、この作業中に自分が実行した `go test -bench` / benchstat /
   benchgate / `SLOFF_DEBUG_TIMING` の生出力を根拠として提示する。
   根拠を出せない数値は「未計測」と明言する。
2. **感度なきガードは有害。** 「守っている最適化を無効化したら実際に動く」ことを
   実証できないベンチマーク・テストは、偽の安心を与えるので追加しない ( ADR-0021 D5)。
3. **wall-clock 系は ns/op では守れない。** sloff の perf 勝ち筋の多くは
   スケジューリング ( makespan) / 呼び出し回数 / レイテンシ隠蔽であり、
   単関数スループットの計測はこれらに盲目。ガード種別の決定表 ( 後述) に従う。
4. **ベンチマークは `-race` で走らせない。** 計測が大きく歪む。正しさの検証は
   既存 test job ( -race) 側の通常テストが担う。
5. **単発の数値で結論しない。** 比較は同一マシンで交互実行 ( interleave) した
   複数標本 ( 時間系は 4〜6 以上) を benchstat / benchgate に通す。

## 地図 — どこに何があるか

| 対象 | 場所 |
|---|---|
| 戦略・棄却案・ゲート仕様 | `docs/adr/0021-benchmark-suite-and-regression-gate.md` |
| 運用マニュアル ( 実行方法 / 失敗の読み方) | `docs/benchmarks/README.md` |
| 実測記録 ( 感度実証・キャリブレーション) | この skill の `references/lessons.md` |
| スイートの全ベンチ・メトリクス・ゲート規則の一覧 | この skill の `references/suite-map.md` |
| 実行スクリプト | `scripts/bench.sh <micro|macro> <out> [dir] [count]` |
| ゲート実装 | `internal/benchgate/` ( `-no-require` でローカル ad-hoc 比較可) |
| 合成 monorepo 生成 | `internal/sloff/benchgen/` |
| フェーズ計測 | `SLOFF_DEBUG_TIMING=1` ( CLI) / macro ベンチの `*-ms/op` メトリクス |

作業種別ごとの参照順:
- **改善作業・A/B 計測** → Workflow A + `references/suite-map.md` のコマンド集
- **ガード / ベンチ追加・変更** → Workflow B + `references/lessons.md` の過去事例
- **gate が赤い / ノイズ調整** → Workflow C + `docs/benchmarks/README.md`

## Workflow A: パフォーマンス改善

### 1. 計測してから触る

改変前に必ずボトルネックを実測で特定する。順に:

1. macro ベンチのフェーズメトリクスで当たりを付ける
   ( `discover / resolve / collect / prefetch / tasksrun / hashinputs / taskexec / fpload`。
   `SLOFF_DEBUG_TIMING` と同じ ADR-0018 span 軸なので実運用の体感と直結する):
   ```sh
   go test ./internal/sloff/runner/ -run '^$' -bench '^BenchmarkRun$' -benchtime 1x
   ```
2. 該当フェーズの micro ベンチ ( suite-map 参照) で絞り込む
3. どちらにも乗らない箇所なら pprof ( `-cpuprofile` / `-memprofile`) まで下りる

改善余地の主張には「どのフェーズが何 ms で、全体の何割か」を添える。

### 2. 改変の A/B 実測

基準は常に**改変前のコミット**。絶対値の期待値をどこにも書かない ( ADR-0021 Option B 棄却):

```sh
git worktree add /tmp/perf-base HEAD   # 改変前を worktree に
# 改変を実装した working tree と交互に実行 ( ドリフト相殺)
for i in 1 2 3 4 5; do
  scripts/bench.sh micro /tmp/base.txt /tmp/perf-base 1
  scripts/bench.sh micro /tmp/head.txt . 1
done
go tool benchstat /tmp/base.txt /tmp/head.txt
# または gate と同じ判定で: go run ./internal/benchgate -no-require -base /tmp/base.txt -head /tmp/head.txt
```

計測中はビルドや別ベンチ等の重い処理を並走させない ( ノイズ源になる)。
macro が要る改善 ( setup phase / runner 全体) は `scripts/bench.sh macro` を同様に交互実行。

### 3. 勝ちをガードで固定する

改善が本物なら、その改善は将来のリファクタで黙って消え得る。landing する PR に
**その最適化のガード** ( Workflow B) を同梱する。既存ガードの期待値が変わる場合
( 決定的メトリクスなど) は同じ PR で更新し、`references/lessons.md` に実測を追記する。

### 4. PR 規約

perf PR は `perf(<scope>): ...`、本文に before/after の実測 ( 計測条件付き) を書く。
CI の bench job が merge-base と自動比較するので、改善は数値で可視化される。

## Workflow B: ガード / ベンチマーク設計

### ガード種別の決定表

最適化が「何を減らすか」でガードの形が決まる。時間を測るのは最後の手段:

| 最適化の性質 | ガード | ゲート判定 |
|---|---|---|
| CPU スループット ( 純関数の高速化) | 通常の ns/op ベンチ | 時間 ( 有意 かつ +30%) |
| 重複作業の削減 ( memoise / cache) | warm と cold ( = 最適化前相当) を**両方常設**する対比ベンチ | 時間 + 対比が常時の感度証明になる |
| 高価な呼び出しの回数削減 ( batch / subprocess) | counting fake を注入し `b.ReportMetric` で決定的単位を報告 + 同値の通常テスト ( -race 側でも守る) | exact ( 増加即 fail) |
| スケジューリング / makespan | runner と同じ slot 意味論の**決定的シミュレータ** proxy ( 例: `makespan-ticks/op`) + 旧アルゴリズムとの比較テスト | exact |
| レイテンシ隠蔽 ( 並行オーバーラップ) | handshake する fake による**並行構造テスト** ( タイムアウトで fail-closed) | テスト |
| 遅延源が外部サービス ( AWS 認証等) | **ガードしない**。hermetic CI の fake では守りたい量を測れない。除外理由を ADR / lessons に記録 | — |

### 感度実証プロトコル ( 必須)

ガードを追加・変更したら、入れる前に感度を証明する:

1. runtime toggle があればそれで on/off を実測 ( 例: `FileHashCachePath=""` = ADR-0014 off)
2. なければ **scratch revert**: 最適化を無効化する最小編集を production code に加え、
   ガードが実際に動く ( テストが fail する / メトリクスが跳ねる) ことを確認し、
   `git checkout -- <file>` で復元して green を再確認する
3. before/after の**生数値**を `references/lessons.md` に追記する ( ADR-0021 D5)。
   過去の全ガードの実証値が載っているので形式はそこに倣う
4. 動かなかったガードは設計を変える。動かないまま入れることは絶対にしない

### 新しい決定的メトリクスを足すとき

`internal/benchgate/gate.go` の `exactUnits` ( ゲート分類) と `requiredHeadUnits`
( fail-open 防止: ベンチが消えたらエラー) の両方に単位を登録し、`gate_test.go` と
ADR-0021 D2 の表を同じ PR で更新する。単位名は `<何を数えるか>/op` ( 例: `enumcalls/op`)。

### ベンチ実装の作法 ( 破ると計測が嘘をつく)

- **b.N ループと memoisation**: sloff のコンポーネントは run 内メモ化だらけ
  ( `lister.Memoized` / `pnpmlocal` の per-package cache / `glob.Expander` / `hash.FileCache`)。
  cold パスを測るベンチは **iteration ごとに fresh instance を構築**する。
  warm パスを測るベンチは逆に 1 instance を warm してからループする ( それが感度の設計)
- **結果を観測する**: 戻り値を package-level sink に代入するか assert する
  ( コンパイラ除去の防止 + 「違う値を返して速い」偽計測の防止)
- **シナリオの自己検証**: macro 系は iteration ごとに RUN/SKIP 件数を assert する。
  「full-hit のつもりが全 RUN だった」ようなベンチは黙って嘘をつく
- **fixture は timed region の外**で生成 ( `sync.OnceValues` 共有 + `b.ResetTimer`)、
  `b.ReportAllocs()` を付ける
- 新しいベンチが `scripts/bench.sh` の対象パッケージ / `-bench` regex に乗ることを
  確認する ( 乗らなければ CI で走らない)

### sloff 固有の罠 ( 実際に踏んだもの)

- **cold の定義は「record 無し・output あり」** ( fresh clone 相当)。生成物まで消すと
  下流 task の cross-task input glob ( `../d0/out.gen`) が collect 時に展開されず、
  input_hash キーが定常状態とずれて 2 回目の run が部分 miss する
- **ADR-0014 racy guard**: run 開始から 2s 以内の mtime/ctime を持つファイルの digest は
  Save で捨てられる。fixture 生成直後に persistent cache を温めると**黙って不完全な store**
  になる。settle sleep ( 2.1s) + store エントリ数の assert、または内部テストなら
  `saveAt( settled)` で回避する
- **小分母の時間メトリクスはノイズだけで「有意 かつ +30%」に達する** ( 実証済み:
  ~1ms のドリフトで p=0.022 / +42.9%)。フェーズメトリクスは小数 ms で報告し、
  ゲート側の 25ms 絶対床に頼る。数 ms オーダーの量を時間ゲートで守ろうとしない
  ( 決定的メトリクスにできないか先に考える)

## Workflow C: gate 対応・キャリブレーション

### bench job が赤いとき

`docs/benchmarks/README.md` の読み方に従う。要点:

- **class=exact の REGRESSION はノイズではない**。決定的な挙動変化。意図した設計変更なら
  ガードの期待値 + ADR-0021 を同じ PR で更新、意図していなければ回帰なので直す
- **class=time** は「有意 かつ +30% 超 ( ms 系はさらに絶対 25ms 超)」を満たしている。
  1 回の再実行は正当 ( ランナー異常の切り分け)、2 連続赤はほぼ実回帰。
  フェーズメトリクスで悪化箇所を特定してから直す
- **`required metric ... missing` エラー**は fail-open 防止。ガードのベンチが
  rename / regex ズレ / パッケージ移動で消えている。消したのが意図的なら
  `requiredHeadUnits` も同じ PR で更新する
- **手動 rebaseline は存在しない**。比較対象は常に merge-base。「baseline を更新する」
  という発想が出てきたらこの仕組みを誤解している

### ノイズ / しきい値を再キャリブレーションするとき

同一コミット同士を比較すれば観測デルタ = ノイズ幅そのもの:

```sh
git worktree add /tmp/calib HEAD
# CI と同じ round 構成で交互実行 → benchgate ( 期待: exit 0)
```

CI ランナー側は、bench job の merge-base 行を一時的に `HEAD` に変えた commit を
PR に積んで数回実行 → 必ず revert する。観測した最大デルタ・有意到達の有無を
`references/lessons.md` のキャリブレーション節に追記し、しきい値がノイズ帯の
上にあることを確認してから変更する。

## 検証チェックリスト ( perf 系 PR を出す前)

- [ ] 性能主張のすべてに自分で実行した計測の生出力が紐付いている
- [ ] 新規 / 変更ガードの感度実証を行い、`references/lessons.md` に実測を追記した
- [ ] 決定的メトリクスの追加・変更は `exactUnits` / `requiredHeadUnits` / ADR-0021 D2 に反映した
- [ ] `go test ./...` ( -race 込み) green / gofumpt / vet / tidy クリーン
- [ ] ベンチは `-race` なしで実行した。CI の bench job 構成 ( bench.sh の対象) に乗っている
