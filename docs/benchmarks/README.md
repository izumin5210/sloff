# Benchmarks 運用ガイド

方針・設計判断は [ADR-0021](../adr/0021-benchmark-suite-and-regression-gate.md)、
感度実験とキャリブレーションの生データは [LESSONS.md](./LESSONS.md) を参照。

## ローカルで回す

```sh
# micro ( depgraph / hash / glob / spec / toolresolver)
scripts/bench.sh micro /tmp/bench.txt . 3

# macro ( runner の cold / warm-incremental / full-hit、 1 回で数十秒かかる)
scripts/bench.sh macro /tmp/bench.txt . 2

# 個別に絞る場合は普通の go test でよい
go test ./internal/sloff/depgraph/... -run '^$' -bench . -benchtime 300ms -count 5
go test ./internal/sloff/runner/ -run '^$' -bench '^BenchmarkRun$' -benchtime 1x
```

注意:

- **`-race` を付けない** ( 計測が歪む。 CI の bench job も付けていない)
- 比較したいときは main を worktree に出して同じコマンドを交互に回し、
  `go tool benchstat old.txt new.txt` か `go run ./internal/benchgate -base old.txt -head new.txt` で見る

## ゲートが赤くなったら

CI の `bench` job は PR head と merge-base を同一ランナーで交互実行し、
`internal/benchgate` が比較する。 fail の読み方:

1. job ログのテーブルで `REGRESSION` 行を見る
   - **class=exact** ( `makespan-ticks/op` / `batchloads/op` / `listloads/op` / `enumcalls/op`):
     ノイズではない。 スケジューリング / バッチングの決定的な挙動が変わっている。
     意図した設計変更なら、 該当ガード ( bench / テスト) の期待値と ADR-0021 を同じ PR で更新する。
     意図していなければ回帰なので直す
   - **class=time**: 「統計的有意 かつ +30% 超」 の二重条件を満たしている
     ( `*-ms/op` はさらに絶対悪化 25ms 以上が必要)。 まず再実行してみる価値はある
     ( ランナー異常のケース) が、 2 回連続で赤ならほぼ確実に実回帰。 `*-ms/op` の
     フェーズ内訳 ( `SLOFF_DEBUG_TIMING` と同じ軸) でどのフェーズが悪化したかを特定する
2. `error:` 行は 2 種類ある:
   - `insufficient samples`: CI 設定の問題 ( round 数を減らした場合などに出る)
   - `required metric ... missing from head` / `macro suite vanished`: ガードの
     ベンチマークが head から消えている ( rename / `-bench` regex / パッケージ移動)。
     ゲートは fail-open を防ぐため、 ガード消失を pass ではなくエラーにする

## rebaseline について

**手動 rebaseline は存在しない。** 比較対象は常に PR の merge-base なので、
意図的に性能特性を変える PR は ( ガードの期待値更新を含んだ上で) merge されれば
それが次の baseline になる。 絶対値をどこにもコミットしない設計は ADR-0021 の
Option B 棄却理由を参照。

## ガードを足す・変えるときの規約 ( ADR-0021 D5)

- 新しいガードは「守りたい最適化を無効化したら数値 / テストが実際に動く」ことを
  実証してから入れる ( toggle があれば toggle、 なければ scratch branch で revert 実験)
- 観測値 ( before / after の生数値) を [LESSONS.md](./LESSONS.md) に追記する
- 決定的メトリクスの期待値を変える場合は ADR-0021 D2 の表も更新する
