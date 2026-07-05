# Benchmark suite — lessons & evidence log

ベンチマークスイート構築時に確認した各最適化のメカニズム、 sensitivity 検証の
生データ、 CI ランナーの分散キャリブレーション記録。 [ADR-0021](../adr/0021-benchmark-suite-and-regression-gate.md)
D5 により、 ガードの追加・変更時はここに実測を追記する。 数値はすべて実測
( 捏造・推定は書かない)。

## Coverage checklist ( git 履歴から再構成、 2026-07-04)

| # | 最適化 | PR / ADR | ガード | 感度実証 |
|---|---|---|---|---|
| 1 | downstream-height scheduling | #64 / ADR-0020 | makespan proxy ( `makespan-ticks/op`) + 辞書順比較テスト | ✅ 下記 |
| 2 | 永続 file-digest cache | ADR-0014 ( #52) | macro warm/full-hit の `filehash=persist\|memory` variant + micro persistent-warm | ✅ 下記 |
| 3 | run 内 digest / glob memoisation | #47 | hash withinrun-warm / glob memoised-repeat | ✅ 下記 |
| 4 | setup-phase 短縮 ( single-base glob walk / merged resolve) | #52 | glob shared-base-cold + macro cold のフェーズメトリクス | ✅ 下記 ( glob) |
| 5 | go-local `packages.Load` バッチ | #53 | `batchloads/op`=4 / `listloads/op`=0 + 通常テスト | ✅ 下記 |
| 6 | pnpm-local `git ls-files` バッチ | #49 | `enumcalls/op`=1 + 通常テスト | ✅ 下記 |
| 7 | 並行 `spec.Discover` | #54 | BenchmarkDiscover ( 広ツリー + node_modules デコイ) | ✅ 下記 |
| 8 | prewarm/resolve オーバーラップ | #57 | 並行構造テスト ( handshake fake resolver) | ✅ 下記 |
| 9 | full-cache-hit fast path | #17 / #14 | macro full-hit + Discover デコイ ( pruning) | ✅ macro 実測 |
| — | DynamoDB credential warming | #51 | **除外**: 遅延源が実 AWS 認証チェーン ( SSO/STS RTT) にあり、 hermetic CI の fake では守りたい量を測れない ( ADR-0021 D6)。 既存 unit test ( Warm 転送 / no-op 経路) は維持 | — |

## Sensitivity evidence ( 2026-07-04/05, Apple M4, darwin/arm64)

各実験は「最適化を scratch で無効化 → ガードが動くことを確認 → `git checkout` で復元」の手順。
CI ( linux/amd64, 4 vCPU) では絶対値は変わるが、 いずれも倍率ベースの巨大な差なので判定は保たれる。

### 1. ADR-0020 scheduling ( makespan proxy)

`sortByPriority` から高さを外す ( 辞書順のみ = pre-#64) と:

- `BenchmarkScheduleMakespan` ( 501 task / slots=14): **37 → 52 makespan-ticks/op ( +40.5%、 3 count とも完全に決定的)**
- `TestBuildOrderBeatsLexicographicMakespan`: fail ( Build 順 35 ticks = 辞書順 35 ticks)

### 2–3. hash.FileCache ( #47 within-run / ADR-0014 persistent)

N=2000 file × 1KiB。 `digest()` の cache lookup を無効化すると:

- withinrun-warm: **2.5–3.1ms → 21.1–23.4ms ( cold と同速に崩壊、 ~8x)**
- 正常時: cold ~21.3ms / withinrun-warm ~2.7ms ( 7.8x) / persistent-warm ~3.3ms ( 6.5x)
- persistent-warm は cold と digest 一致を毎回 assert ( 嘘の計測防止)。 racy guard ( ADR-0014) により
  「書いた直後の fixture は Save で捨てられる」ため、 テスト同様 `saveAt( settled)` で回避している

### 3–4. glob.Expander ( #47 memoise / #52 single-base walk)

300 svc dir、 shared-base 3 pattern。 `computeMatches` を常に per-pattern `doublestar.Glob` に落とすと:

- shared-base-cold: **16.0–19.6ms → 91.3–102.4ms ( reference-per-pattern と同速に退行、 ~5.5x)**
- 正常時: shared-base-cold ~17.5ms / memoised-repeat ~0.125ms ( ~140x) / reference-per-pattern ~101ms
- reference-per-pattern ( pre-#47/#52 相当) をベンチとして常設し、 勝ち幅を毎回可視化

### 5. golocal batch prewarm ( #53)

64 tool / 4 spec dir、 counting fake lister。 `Prewarm` を no-op にすると:

- メトリクス: **batchloads/op 4 → 0、 listloads/op 0 → 64**
- `TestPrewarmedResolveCallCounts`: fail ( -race の test job でも検出される)

### 6. pnpmlocal batched enumerator ( #49)

1 tool が 13 workspace dir を transitively link。 `collectFiles` を per-dir ループに戻すと:

- メトリクス: **enumcalls/op 1 → 13**
- `TestBatchedEnumeratorCallCount`: fail

### 7. spec.Discover 並行化 ( #54) + pruning ( #17)

16 top-level dir / ~9.6k file + node_modules デコイ ( 計 ~2.5k file、 デコイ内に
sloff.yml tripwire を配置し pruning 退行は件数 assert でも捕まる)。 `g.SetLimit(1)` で直列化すると:

- BenchmarkDiscover: **7.6–8.6ms → 20.4–26.0ms ( ~2.9x @ 10 core)**

### 8. prewarm オーバーラップ ( #57)

`startPrewarm` を同期実行 ( pre-#57 相当) に変えると:

- `TestResolve_EagerResolutionOverlapsPrewarm`: **fail** ( "prewarm never observed an eager
  resolver call while in flight")。 正常時は 0.01s で pass、 退行時のみ handshake timeout ( 10s) を踏む

### 9. full-hit fast path ( #17) + ADR-0014 macro 効果

macro ( 501 task / 30,060 file、 M4、 -benchtime=1x):

| シナリオ | ns/op | prefetch-ms | tasksrun-ms |
|---|---|---|---|
| cold | 2.20s | 837 | 1148 |
| warm-incremental / memory | 728ms | 539 | 80 |
| warm-incremental / persist | **236ms** | **37** | 85 |
| full-hit / memory | 662ms | 528 | 33 |
| full-hit / persist | **218ms** | **42** | 35 |

- **persist vs memory = full-hit で 3.0x、 warm-incremental で 3.1x**。 prefetch 528ms → 42ms は
  ADR-0014 本文の実測 ( ~545ms → 数十 ms) と整合。 この対比が毎 CI 実行で出るため、
  ADR-0014 の退行は full-hit/persist の悪化として時間ゲートに掛かる
- 各シナリオは iteration ごとに RUN/SKIP 件数を assert しており、 「全部 RUN になっていた
  full-hit」のような嘘の計測は bench 自体が fail する

## 設計上の学び

- **cold の定義**: 生成物まで消した clean 状態では、 下流 task の cross-task input glob
  ( `../d0/out.gen`) が collect 時に展開されず input_hash キーが定常状態とずれ、 2 回目の run が
  部分 miss する。 sloff のモデル ( 生成物は git 管理) に合わせ、 macro の cold は
  「record 無し・output あり ( fresh clone)」と定義した ( benchgen のテストがこの性質を固定)
- **racy guard と fixture**: ADR-0014 の racy guard ( run 開始から 2s 以内の mtime/ctime は
  Save で捨てる) のため、 fixture 生成直後に persistent cache を温めると **黙って空 store になる**。
  macro は settle sleep ( 2.1s) + store サイズ assert、 micro は `saveAt( settled)` で対処
- **決定的メトリクスが最強のゲート**: makespan / call count はノイズ 0 なので閾値 1 サンプルで
  gate できる。 時間ゲートは「有意 かつ +30%」の二重条件でノイズに倒す ( ADR-0021 D3)

## CI runner variance calibration

### ローカル ( M4) での事前キャリブレーション ( 2026-07-05)

同一コミット ( HEAD vs HEAD の worktree) を CI と同じ手順 ( macro 3 round × count 2 = 6 標本、
micro 5 round × count 1 = 5 標本、 交互実行) で比較。 差分ゼロなので観測デルタ = ノイズ幅そのもの。
結果: **benchgate exit 0 ( 偽陽性なし)**。

- `sec/op` の |delta| 最大: **+9.2%** ( FileCache/mode=cold、 p=0.421 で非有意)。
  macro の sec/op は全シナリオ ±3.6% 以内
- フェーズメトリクスの |delta| 最大: **±20% 前後** ( `fpload-ms/op` = 5〜16ms と分母が小さい系列。
  いずれも p ≥ 0.16 で非有意)。 分母の大きい `prefetch-ms/op` / `tasksrun-ms/op` は ±6% 以内
- 決定的メトリクス ( makespan / batchloads / listloads / enumcalls): **全て完全一致**
- p < 0.05 に達した時間系メトリクスは 0 件

初期閾値 +30% ( かつ有意性必須) は観測ノイズ幅 ( 非有意な ±20%) の上に立っており妥当。
分母の小さいフェーズ ( fpload / resolve / collect / discover) は相対ノイズが大きいが、
二重条件 ( 有意 かつ 閾値超) が防波堤になることを確認した。

### GitHub Actions 上の実測

( PR の bench job を複数回実行して追記する)
