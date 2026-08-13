# スイート全体マップ ( ベンチ・メトリクス・ゲート規則)

ベンチマークスイートの「何がどこにあり、どんな値を出すはずか」の一覧。
ガードの追加・変更時はこの表と `internal/benchgate/gate.go` を同期させること。

## Micro ベンチマーク

| パッケージ / ファイル | ベンチマーク | 守る最適化 | 期待メトリクス |
|---|---|---|---|
| `internal/sloff/depgraph/bench_test.go` | `BenchmarkBuild` | Build の O(V+E) コスト | ns/op |
| 同上 | `BenchmarkScheduleMakespan` | ADR-0020 downstream-height scheduling | **`makespan-ticks/op` = 37** ( 決定的) |
| `internal/sloff/depgraph/makespan_test.go` | `simulateMakespan` + `TestBuildOrderBeatsLexicographicMakespan` | 同上 ( slot 意味論シミュレータ: emit 順 admission・待機中も slot 保持) | テスト |
| `internal/sloff/hash/filecache_bench_test.go` ( 内部 package) | `BenchmarkFileCache/mode=cold\|withinrun-warm\|persistent-warm` | #47 run 内メモ化 / ADR-0014 永続 cache | 時間。warm ≪ cold の対比が感度証明 |
| `internal/sloff/glob/bench_test.go` | `BenchmarkExpander/mode=shared-base-cold\|memoised-repeat\|reference-per-pattern` | #52 single-base walk / #47 メモ化 | 時間。reference ( 最適化前相当) を常設 |
| `internal/sloff/spec/bench_test.go` | `BenchmarkDiscover` | #54 並行 walk + #17 node_modules pruning ( fixture 内に sloff.yml tripwire) | 時間 |
| `internal/sloff/toolresolver/golocal/bench_test.go` | `BenchmarkResolver/path=prewarmed` + `TestPrewarmedResolveCallCounts` | #53 packages.Load バッチ | **`batchloads/op` = 4, `listloads/op` = 0** |
| `internal/sloff/toolresolver/pnpmlocal/bench_test.go` | `BenchmarkResolver/path=inputs` + `TestBatchedEnumeratorCallCount` | #49 git ls-files バッチ | **`enumcalls/op` = 1** |
| `internal/sloff/runner/resolve_overlap_test.go` | `TestResolve_EagerResolutionOverlapsPrewarm` | #57 prewarm オーバーラップ ( handshake fake、 timeout 10s で fail-closed) | テスト |

## Macro ベンチマーク ( `internal/sloff/runner/bench_test.go`)

fixture: `benchgen.DefaultParams()` = 501 task ( wide 400 + chain 20×5 + sink) /
30,060 source file / 共有 script tool 1 つ / trivial `cat` cmd。
`sync.OnceValues` で process 内共有、bootstrap run で「output あり」状態を確立。

| シナリオ | 状態 | 期待 RUN/SKIP ( 自己検証 assert 済み) |
|---|---|---|
| `scenario=cold` | record 無し・output あり ( fresh clone) | RUN=501 / SKIP=0 |
| `scenario=warm-incremental/filehash=memory\|persist` | chain 先頭 1 file を毎 iteration 一意に書き換え | RUN=6 ( chain 5 + sink) / SKIP=495 |
| `scenario=full-hit/filehash=memory\|persist` | 無変更 | RUN=0 / SKIP=501 |

- `filehash=persist` は `Options.FileHashCachePath` 注入 ( ADR-0014 on)、
  `memory` は `""` ( `SLOFF_NO_FILE_HASH_CACHE` 相当)。両 variant の対比が常設の感度証明
- persist の setup は settle sleep ( 2.1s、racy guard 対策) + store エントリ数 ≥ 30,060 の assert
- フェーズメトリクス ( 小数 ms): `discover- / resolve- / collect- / prefetch- / tasksrun- /
  hashinputs- / taskexec- / fpload-ms/op` — ADR-0018 span と同一軸 ( `phaseCollector` が収集)

## ゲート規則 ( `internal/benchgate/gate.go`)

| 単位クラス | 対象 | 判定 |
|---|---|---|
| exact | `makespan-ticks/op`, `batchloads/op`, `listloads/op`, `enumcalls/op` ( = `exactUnits`) | 増加即 fail ( 許容 0.1%) |
| time | `sec/op`, `ns/op`, `*-ms/op` | Mann-Whitney p < 0.05 **かつ** +30% 超。`*-ms/op` はさらに絶対悪化 ≥ 25ms ( `msAbsFloor`) |
| info | `B/op`, `allocs/op`, 未知単位 | 表示のみ |

- 標本キー = ( `pkg`, ベンチ名, 単位)。golocal / pnpmlocal は同名 `BenchmarkResolver` を emit するため pkg が必須
- 片側のみに存在するベンチ = note ( fail しない)
- head に `requiredHeadUnits` ( exact 4 単位) と `Run/scenario=*` の sec/op が無ければ**エラー**
  ( fail-open 防止)。ローカル ad-hoc 比較は `-no-require`
- 時間系の標本 < `-min-count` ( 既定 4) は**エラー** ( 検定が無力なまま pass しない)
- フラグ: `-base` `-head` `-threshold 0.30` `-alpha 0.05` `-min-count 4` `-no-require`

## CI 構成 ( `.github/workflows/ci.yml` の `bench` job)

- pull_request のみ。merge-base を `git worktree add /tmp/bench-base $(git merge-base origin/main HEAD)`
- **交互実行**: macro 3 round × count 2 ( = 6 標本) → micro 5 round × count 1 ( = 5 標本)
- `-race` なし ( 正しさは test job)。実測ジョブ時間 ~4 分 ( 2026-07 時点)
- Gate step: `go run ./internal/benchgate -base ... -head ...`、raw 出力は最終 step で dump

## よく使うコマンド

```sh
# フェーズ内訳付き macro 1 回
go test ./internal/sloff/runner/ -run '^$' -bench '^BenchmarkRun$' -benchtime 1x

# 特定 micro を複数標本で
go test ./internal/sloff/depgraph/ -run '^$' -bench . -benchtime 300ms -count 5

# suite 一式 ( CI と同じ入口)
scripts/bench.sh micro /tmp/out.txt . 1
scripts/bench.sh macro /tmp/out.txt . 2

# A/B 比較 ( worktree と交互実行した 2 ファイルを比較)
go tool benchstat /tmp/base.txt /tmp/head.txt
go run ./internal/benchgate -no-require -base /tmp/base.txt -head /tmp/head.txt

# 実 CLI のフェーズ計測 ( 実 repo に対して)
SLOFF_DEBUG_TIMING=1 sloff run
```
