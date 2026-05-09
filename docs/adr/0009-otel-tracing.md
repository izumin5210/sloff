# ADR-0009: sloff の観測モデルとして OpenTelemetry trace を採用する

## Context

sloff は cache-aware codegen orchestrator として、 1 run の中に多段のフェーズを走らせる:

1. spec discover ( `spec.Discover`)
2. tool registry の構築 ( ADR-0008)
3. preflight ( resolver 単位、 例: pnpm-local の install drift 検証)
4. resolver pre-pass ( `Inputs` / `Versions` を tool 単位で 1 回だけ走らせる、 ADR-0008 D6)
5. task collection と DAG 構築 ( `depgraph.Build`)
6. 並列 task 実行 ( errgroup でファンアウト → 各 task は cache load → ( miss なら exec → cache save) )

現状の観測手段は `runner.Logger` interface ( `log.Default()` 実装) 経由の平文 log 3 行 ( `SKIP cache hit` / `RUN ...` / preflight error) しかない。 これでは以下のような運用上の疑問に答えられない:

- 全体所要時間のうち **どのフェーズが支配的か** ( resolver か exec か cache I/O か)
- 並列 task の **ファンアウト形状**: どの task がクリティカルパス上に居るか
- `cache.hit` 率の分布: 「 cache hit のはずが exec まで降りた」 タスクの特定
- per-resolver / per-tool のコスト: `pnpm-local` BFS は何 ms / `go/packages.Load` は何 ms か
- 異常時の **subprocess の挙動**: どの cmd が長時間ブロックしているか

参考として [Goアプリケーションを observability する話](https://blog.kengo-toda.jp/entry/2026/02/18/082231) と [OpenTelemetry CLI による observability](https://mackerel.io/ja/blog/entry/tech/opentelemetry-cli-observability) は、 短命な CLI process でも OpenTelemetry の trace を吐けば「 phase ごとの所要時間」 「 並列 task の Gantt」 「 cache hit/miss 分布」 を後から可視化できるとしている。 sloff は cache-aware という性質上、 trace ベースで初めて見える運用課題が大きい。

## Decision

### D1. 観測モデルとして OpenTelemetry trace を採用する

- **trace のみ** 導入する。 metrics / logs は本 ADR の scope 外 ( D8 / Future work で再考余地)。
- export 系統は OpenTelemetry SDK 標準の **環境変数** ( `OTEL_EXPORTER_OTLP_ENDPOINT` 等) で完結させる。 sloff 独自の CLI フラグは追加しない。

### D2. 環境変数で auto-detect する ( CLI フラグ追加なし)

`OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` / `OTEL_TRACES_EXPORTER` のいずれかが set されていれば SDK を初期化する。 未設定なら **global provider をそのまま** にする ( `setupTracing` が disabled パスで何もしない、 SDK もエクスポーターも組まない)。 in-process な host が既に独自 TracerProvider を設定済みなら、 sloff の span はその host の provider を経由して host のバックエンドに届く ( 「 disable は exporter 設定をしない」 という意味であって、 「 sloff の spans を絶対に送らない」 とは扱わない)。

理由:

- Mackerel 記事は「 OTLP のデフォルト送信先 ( `localhost:4317`) を勝手に叩きに行く挙動は驚きを生むので、 明示的に有効化フラグを与えるのが望ましい」 と指摘している。 sloff では **endpoint の指定自体が user の明示的意思表示** とみなすことで、 フラグの追加を避けつつ「 何もしなければ何も送らない」 を担保する。
- `OTEL_TRACES_EXPORTER=none` または `OTEL_SDK_DISABLED=true` を尊重する ( 上記 endpoint がたまたま set されていても、 これらが立っていれば disable する)。
- フラグを足さないことで、 後で metrics / logs を有効化する際にも環境変数だけで完結する ( `--otel-metrics` / `--otel-logs` を増殖させない)。

### D2'. `SLOFF_` prefix の override 規則を提供する

`OTEL_*` 系の env var に対し、 同じ key に `SLOFF_` prefix を付けたもの ( `SLOFF_OTEL_EXPORTER_OTLP_ENDPOINT` / `SLOFF_OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` / `SLOFF_OTEL_EXPORTER_OTLP_PROTOCOL` / `SLOFF_OTEL_EXPORTER_OTLP_HEADERS` / `SLOFF_OTEL_TRACES_EXPORTER` / `SLOFF_OTEL_SERVICE_NAME` / `SLOFF_OTEL_RESOURCE_ATTRIBUTES` / `SLOFF_OTEL_SDK_DISABLED`) が set されていれば、 同名の `OTEL_*` を **process 起動直後に in-process で上書き** してから SDK を初期化する。

ユースケース:

- shell 全体で `OTEL_EXPORTER_OTLP_ENDPOINT` を別 backend に向けているが、 sloff だけ別 endpoint に投げたい
- 上記の環境で sloff だけ trace を完全に止めたい ( `SLOFF_OTEL_TRACES_EXPORTER=none` か `SLOFF_OTEL_SDK_DISABLED=true`)
- 逆に他ツールには触らせず sloff だけに trace を有効化したい ( `SLOFF_OTEL_EXPORTER_OTLP_ENDPOINT=...` のみ set)

実装上は `os.Setenv` で in-process 適用する。 sloff から起動される child process ( task の cmd) には伝搬しない ( child は元の `OTEL_*` を見るか、 sloff が `os.Setenv` で書き換えた値を `os.Environ()` 経由で継承するかは Go runtime の挙動に従う)。 child の trace は sloff の関心外。

**Restore 規律**: `applySloffPrefixOverrides` は touch した key のスナップショット ( `wasSet` / `prevValue`) を取り、 `restore func()` を返す。 `setupTracing` が返す shutdown は最後に必ずこの restore を呼ぶ。 これにより、 同一 process で `newRootCmd().Execute()` を複数回呼ぶ ( テスト・ embedding host) ケースで、 1 回目の `SLOFF_*` 設定が 2 回目以降の `OTEL_*` を汚染しない。

### D3. Exporter は `autoexport` で動的選択する

`go.opentelemetry.io/contrib/exporters/autoexport` の `NewSpanExporter` を採用する。 これは `OTEL_TRACES_EXPORTER` を読み `otlp` ( gRPC または HTTP、 さらに `OTEL_EXPORTER_OTLP_PROTOCOL` で切替) / `console` ( stdout) / `none` を出し分ける標準パターン。

利点:

- ローカル動作確認が `OTEL_TRACES_EXPORTER=console` 一発でできる ( Collector / Jaeger 不要)
- gRPC と HTTP の選択が env で完結
- 後で他 exporter ( Zipkin 等) を追加したくなっても autoexport の対応に追従できる

### D4. Resource は OTel SDK 標準フィールドを尊重する

Resource attributes:

- `service.name`: デフォルト `sloff` ( `OTEL_SERVICE_NAME` で override)
- `service.version`: package-level 変数 `buildVersion` ( デフォルト `dev`、 release pipeline で `-ldflags "-X main.buildVersion=v0.x.y"` 注入予定)
- `OTEL_RESOURCE_ATTRIBUTES` ( `host.name=...,user.name=...` 等) は `resource.WithFromEnv()` 経由で取り込む
- process / OS / host 情報は `resource.WithProcess() / WithOS() / WithHost()` で標準 attribute を埋める

### D5. Span 粒度: phase + per-tool resolver + per-task の 3 階層

```
sloff.run | sloff.graph                   [root, cmd/sloff]
├─ spec.discover
├─ runner.preflight                       (run のみ)
├─ runner.resolve.inputs
│   └─ resolver.<channel>[<tool>]         per referenced tool
├─ runner.resolve.versions                (run のみ)
│   └─ resolver.<channel>[<tool>]         per referenced tool
├─ runner.collect_tasks
├─ runner.depgraph.build
└─ runner.tasks.run                       (run のみ)
    └─ runner.task.run                    per task
        ├─ runner.cache.load
        ├─ runner.task.exec               miss 時のみ
        └─ runner.cache.save              miss かつ ReadOnly=false 時のみ
```

主な span attribute:

- `runner.task.run`: `sloff.spec` ( spec dir) / `sloff.task.name` / `sloff.cache.hit` ( bool) / `sloff.input.hash` ( 12 桁 hex に切詰) / `sloff.tool.count`
- `runner.cache.load`: `sloff.cache.state` ( hit / stale / miss / not_found)
- `runner.task.exec`: `sloff.cmd` ( argv 全体は冗長なので argv[0] のみ) / `process.exit_code`
- `runner.cache.save`: `sloff.output.file_count`
- `resolver.<channel>[<tool>]`: `sloff.tool.name` / `sloff.resolver.channel`

Span のエラー時は必ず `RecordError` + `SetStatus(codes.Error, ...)` をセットする ( runner package に共通 helper を置く)。

#### 設計判断の補足

- **resolver 実装は無変更**。 instrumentation は呼び出し側 ( `runner.resolveInputContribs` / `resolveVersionContribs`) の loop 内で span を貼る。 resolver パッケージに otel 依存を持ち込まないことで、 将来 resolver を追加するときの認知負荷を増やさない。
- **cache `Storage` interface は無変更**。 `Storage.Load` / `Storage.Save` の呼び出し箇所を span で囲む ( これも runner 側責任)。
- **`spec.Discover` は cmd/sloff 側でラップ**。 spec パッケージに otel 依存を持たせない。 phase span として cmd / runner の境界に span 開始 / 終了が分散するが、 trace tree としては parent-child で正しく繋がる。

### D6. Shutdown 規律 ( global state restore も含む)

- `sdktrace.WithBatcher` ( BatchSpanProcessor) を採用。 短命 CLI なので `Shutdown` で必ず flush する。
- 各 subcommand の `RunE` ( `runE` / `graphE`) で `defer shutdown(ctx)` する。 shutdown 失敗時は stderr に warn を 1 行出すだけで **exit code には影響させない**。 exporter 障害が CLI の primary 機能 ( codegen 実行成否) を壊さない原則。
- shutdown は `context.Background()` ベースの短い deadline を持たせる ( 親 ctx が cancel された後でも flush が走るようにするため)。
- **enabled パスの shutdown は global state を restore する**: 起動時に snapshot した `prevTracerProvider` / `prevTextMapPropagator` に戻し、 `applySloffPrefixOverrides` が返した env restore も呼ぶ。 これがないと、 in-process な host が事前に設定していた provider / propagator が sloff の shut-down 済み TP に置換されたまま残り、 host 側の以後のトレーシングが壊れる。
- **disabled パスは global provider に一切触らない**。 host の provider をそのまま尊重し、 shutdown では env restore のみを行う ( SDK は組まない)。

### D7. Propagator は TraceContext + Baggage のみ

`propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})` を global propagator に設定する。

- CI 環境などで親 span を `TRACEPARENT` env から受ける拡張 ( `otelchildspan` 的な inject) は **本 PR では入れない**。 必要に迫られたら別 ADR で再検討。
- task の cmd ( child process) に `TRACEPARENT` を伝搬する仕組みも本 PR では入れない ( 同上)。

### D8. metrics / logs / TRACEPARENT 受信は scope 外

- **metrics**: cache hit rate / task duration histogram / resolver duration 等は OTel metrics で表現したい候補だが、 trace の attribute だけでも当面の運用課題には足りる ( backend 側で集計可能)。 別 ADR で扱う。
- **logs**: 現 `runner.Logger` は `log.Default()` 直書き。 OTel log bridge ( slog 経由) への移行は 「 logger interface のリファクタ + `slog` 導入」 を伴う別 issue。
- **TRACEPARENT 受信**: CI から親 span を継承して sloff の trace を child として繋ぐのは有用だが、 単独で sloff の運用課題に対する優先度は低い。 上 2 つと同様、 必要に迫られたら別 ADR で。

## Consequences

### 正の影響

- **observability の足場**: 本 PR で trace tree が見える状態になり、 「 どのフェーズが遅いか」 「 cache hit 率」 「 並列 task 形状」 を Jaeger / Tempo / Honeycomb 等で観測できるようになる。
- **0 cost when disabled**: env を何も set しなければ `noop` provider で短絡。 CI fixture や手元実行は今までと挙動 / 性能が変わらない。
- **既存 E2E goldens は無変更**: `internal/sloff/runner/runner_test.go` の golden 比較は test process が default `noop` のままなので emit が起きず、 `testdata/e2e/runner/.../expected/` は touch されない。
- **将来の metrics / logs 拡張余地**: SDK / exporter / resource はすべて OTel 標準に乗っているので、 metrics / logs 追加時に provider 横並びで追加できる。

### 負の影響 / 注意点

- **依存追加** ( `go.opentelemetry.io/otel` 系 + `autoexport` + semconv)。 binary size と build time に影響するが、 必要なコストと判断。
- **resource 取得** ( `resource.WithProcess() / WithOS() / WithHost()`) で `os/exec` 系の system call が発生する。 startup latency に数 ms 載る可能性。 ただし trace 有効時のみ。
- `os.Setenv` で `SLOFF_*` を `OTEL_*` に上書きする副作用が child process に波及する可能性 ( task の cmd が `OTEL_*` を読む場合)。 task 実行は通常 codegen subprocess ( buf / protoc / pnpm 等) で OTel 環境を読まないものが大半なので実害は低いが、 ADR-0009 の知識として明記しておく。

### 将来再考の余地

- **metrics 導入** ( task duration histogram / cache hit rate counter / resolver duration histogram)
- **`slog` への logger 移行 + OTel log bridge** ( runner.Logger interface のリファクタを伴う)
- **TRACEPARENT 受信** ( CI から親 span を継承)
- **child process への TRACEPARENT 伝搬** ( codegen tool 側が OTel 対応していれば trace tree が垂直に繋がる)
- **service.version の build pipeline injection** ( release workflow が整ったタイミングで ldflags 注入を整備)
- **per-glob.Expand / per-hash 単位など更に細かい span** ( 必要に迫られたら追加)
