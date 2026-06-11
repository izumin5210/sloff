# ADR-0013: sloff の観測モデルとして OpenTelemetry trace を採用する

## Context

sloff は fingerprint-aware codegen orchestrator として、 1 run の中に多段のフェーズを走らせる:

1. spec discover ( `spec.Discover`)
2. tool registry の構築 ( ADR-0008)
3. preflight ( resolver 単位、 例: pnpm-local の install drift 検証)
4. resolver pre-pass ( `Inputs` / `Versions` を tool 単位で 1 回だけ走らせる、 ADR-0008 D6)
5. task collection と DAG 構築 ( `depgraph.Build`)
6. 並列 task 実行 ( errgroup でファンアウト → 各 task は fingerprint load → ( miss なら exec → fingerprint save) )

現状の観測手段は `runner.Logger` interface ( `log.Default()` 実装) 経由の平文 log 3 行 ( `SKIP fingerprint hit` / `RUN ...` / preflight error) しかない。 これでは以下のような運用上の疑問に答えられない:

- 全体所要時間のうち **どのフェーズが支配的か** ( resolver か exec か fingerprint I/O か)
- 並列 task の **ファンアウト形状**: どの task がクリティカルパス上に居るか
- `fingerprint.hit` 率の分布: 「 fingerprint hit のはずが exec まで降りた」 タスクの特定
- per-resolver / per-tool のコスト: `pnpm-local` BFS は何 ms / `go/packages.Load` は何 ms か
- 異常時の **subprocess の挙動**: どの cmd が長時間ブロックしているか

参考として [Goアプリケーションを observability する話](https://blog.kengo-toda.jp/entry/2026/02/18/082231) と [OpenTelemetry CLI による observability](https://mackerel.io/ja/blog/entry/tech/opentelemetry-cli-observability) は、 短命な CLI process でも OpenTelemetry の trace を吐けば「 phase ごとの所要時間」 「 並列 task の Gantt」 「 fingerprint hit/miss 分布」 を後から可視化できるとしている。 sloff は fingerprint-aware という性質上、 trace ベースで初めて見える運用課題が大きい。

## Decision

### D1. 観測モデルとして OpenTelemetry trace を採用する

- **trace のみ** 導入する。 metrics / logs は本 ADR の scope 外 ( D8 / Future work で再考余地)。
- export 系統は OpenTelemetry SDK 標準の **環境変数** ( `OTEL_EXPORTER_OTLP_ENDPOINT` 等) で完結させる。 sloff 独自の CLI フラグは追加しない。

### D2. 環境変数で auto-detect する ( CLI フラグ追加なし)

`OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` / `OTEL_TRACES_EXPORTER` のいずれかが set されていれば SDK を初期化する。 disable signals ( `OTEL_SDK_DISABLED=true` / `OTEL_TRACES_EXPORTER=none`、 `SLOFF_OTEL_*` 経由含む) は enable signal を **常に上書き** する。

**`setupTracing` は sloff-local な `TracerProvider` を返すだけで、 otel-go の global ( `otel.SetTracerProvider`) には一切触らない。** runner / cmd は返ってきた TP から `Tracer` を導出して使う。 enable signal が立っていない、 または explicit disable signal が立っているケースでは noop TP を返す。

| 状態 | 返値 |
|---|---|
| **enabled** | sloff 専用 sdktrace TracerProvider ( exporter は OTLP HTTP/gRPC または stdout) |
| **disabled** ( passive または explicit) | `noop.NewTracerProvider()` ( 何も emit しない) |

理由 ( global を一切触らない設計の根拠) :

- in-process な host が独自の global TracerProvider を入れている場合、 sloff が global を mutate すると host の instrumentation が壊れる
- 同一プロセスで `run` / `graph` を **並行実行** する場合、 global 経由の挙動だと一方の shutdown が他方の TracerProvider を tearing down してしまう ( codex review round 6 の指摘) 。 sloff-local TP ならインスタンスごとに独立
- explicit disable も「 sloff-local が noop」 で実現できるので、 global を一時的に noop に置換する必要がない ( 副次的に host の lazy `otel.Tracer()` を狂わせない)
- 「 host が独自 TP を入れている状況で sloff の span を host backend に流したい」 ユースケースは、 host が `runner.Options.TracerProvider` に自分の TP を渡せば達成できる ( library 層の API 設計として明示的)

理由:

- Mackerel 記事は「 OTLP のデフォルト送信先 ( `localhost:4317`) を勝手に叩きに行く挙動は驚きを生むので、 明示的に有効化フラグを与えるのが望ましい」 と指摘している。 sloff では **endpoint の指定自体が user の明示的意思表示** とみなすことで、 フラグの追加を避けつつ「 何もしなければ何も送らない」 を担保する。
- `OTEL_TRACES_EXPORTER=none` または `OTEL_SDK_DISABLED=true` を尊重する ( 上記 endpoint がたまたま set されていても、 これらが立っていれば disable する)。
- フラグを足さないことで、 後で metrics / logs を有効化する際にも環境変数だけで完結する ( `--otel-metrics` / `--otel-logs` を増殖させない)。

### D2'. `SLOFF_` prefix の override 規則を提供する

`OTEL_*` 系の env var に対し、 同じ key に `SLOFF_` prefix を付けたもの ( `SLOFF_OTEL_EXPORTER_OTLP_ENDPOINT` / `SLOFF_OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` / `SLOFF_OTEL_EXPORTER_OTLP_PROTOCOL` / `SLOFF_OTEL_EXPORTER_OTLP_HEADERS` / `SLOFF_OTEL_TRACES_EXPORTER` / `SLOFF_OTEL_SERVICE_NAME` / `SLOFF_OTEL_RESOURCE_ATTRIBUTES` / `SLOFF_OTEL_SDK_DISABLED`) が set されていれば、 設定値として優先する。

ユースケース:

- shell 全体で `OTEL_EXPORTER_OTLP_ENDPOINT` を別 backend に向けているが、 sloff だけ別 endpoint に投げたい
- 上記の環境で sloff だけ trace を完全に止めたい ( `SLOFF_OTEL_TRACES_EXPORTER=none` か `SLOFF_OTEL_SDK_DISABLED=true`)
- 逆に他ツールには触らせず sloff だけに trace を有効化したい ( `SLOFF_OTEL_EXPORTER_OTLP_ENDPOINT=...` のみ set)

**実装**: `effectiveEnv(otelKey)` ヘルパで「 `SLOFF_OTEL_*` が set ならそちら、 そうでなければ `OTEL_*`」 を読み取り、 SDK 構築時には **explicit な options** ( `otlptracehttp.WithEndpointURL`, `otlptracehttp.WithHeaders`, `semconv.ServiceName`, `resource.WithAttributes` 等) として渡す。 `os.Setenv` は一切呼ばない。

**禁則**: 「 `os.Setenv` で `SLOFF_*` の値を `OTEL_*` に書き戻して SDK に env 経由で読ませる」 アプローチは採らない。 そうすると以下の問題が出る:

- env mutation が `setupTracing` 〜 `shutdown` の間続くと、 runner が `exec.Command + os.Environ()` で task を spawn するときに SLOFF_ 由来の `OTEL_*` を child process が継承してしまう ( 「 sloff だけ別 endpoint」 という方針に反する)
- 「 SDK 構築直後に restore」 する短時間 mutate でも、 process env を一時的にせよ書き換える設計はテスト不能・並行不能・読みづらく、 否応なく fragile

代わりに、 **SDK が options を受ける** という事実を全面的に活用する。 OTel exporter / resource 系 API は env 読みと options が両方サポートされており、 options が常に env を override するので、 effective 値を options として渡せば env を一切触らずに SLOFF_ 優先を実現できる。

**Subprocess env scrub**: 加えて、 runner が task の cmd を spawn する際 ( `runner.execCmd`) には `os.Environ()` から `SLOFF_OTEL_*` 接頭辞を持つ entry を **`childEnv` ヘルパで除外** する。 task の cmd 自体が別の `sloff` ( または同じ prefix を honor するツール) を invoke した場合、 child は親 run の SLOFF_ 設定を引き継ぐべきではない ( child は独立した sloff プロセスとして自身の env を持つべき)。 `OTEL_*` ( prefix なし) は通常通り child に届く。

### D3. Exporter は SLOFF_ 値を options で渡せるよう自前 dispatch する

autoexport (`go.opentelemetry.io/contrib/exporters/autoexport`) は env だけで exporter を選び設定する設計で、 options による override の口が無い。 このため D2' で求められる「 env 不変で SLOFF_ override」 を実現できない。 代わりに sloff 側で `effectiveEnv("OTEL_TRACES_EXPORTER")` の値を見て自前 dispatch する:

| OTEL_TRACES_EXPORTER | dispatch 先 | options |
|---|---|---|
| `console` | `stdouttrace.New(stdouttrace.WithWriter(stderr))` | **stderr 固定** ( 後述) |
| `""` / `otlp` ( default) | OTLP HTTP/gRPC を protocol で振り分け | `WithEndpointURL` / `WithHeaders` |
| `none` | -- ( `envOTelEnabled` が false を返すので到達しない) | -- |
| その他 | 起動エラー ( unsupported) | -- |

OTLP の protocol は `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL` > `OTEL_EXPORTER_OTLP_PROTOCOL` > default `http/protobuf` の順に解決し、 `grpc` / `http/protobuf` のみ受け付ける。 OTel spec 上は `http/json` も列挙されているが otel-go の `otlptracehttp` は **常に protobuf を吐く** 実装であり、 「 JSON を選んだのに protobuf を送る」 silent な不整合を避けるため `http/json` は **起動時に明示的に拒否** する ( 実装が追従したらサポート再開)。 endpoint と headers は signal-specific ( `_TRACES_`) を generic より優先 ( OTel spec 準拠) し、 effective 値が非空なら `WithEndpointURL` / `WithHeaders` で渡す ( exporter 内部の env 読みは options で必ず上書きされる)。

**HTTP endpoint の path 解決**: OTel spec は generic と signal-specific で path 規則を区別する:

- `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` ( signal-specific) は **as-is** で使う。 例: `http://traces.example.com/api/v1/traces` がそのまま POST 先になる。
- `OTEL_EXPORTER_OTLP_ENDPOINT` ( generic) は **base URL** とみなし、 traces 信号には `/v1/traces` を **append** する。 例: `http://collector:4318` は `http://collector:4318/v1/traces` として POST される。

`otlptracehttp.WithEndpointURL` は渡した URL をそのまま使うので、 path append は sloff 側 ( `resolveOTLPHTTPEndpoint`) で実装する。 これがないと最も一般的な `OTEL_EXPORTER_OTLP_ENDPOINT=http://collector:4318` 設定で `/` に POST されて collector に拒否される。 gRPC は path を持たないので append 不要。

OTel spec に書かれているが本実装が **直接サポートしていない** OTEL_ 変数 ( `OTEL_EXPORTER_OTLP_TIMEOUT`, `OTEL_EXPORTER_OTLP_COMPRESSION`, TLS 関連等) は exporter が env を直接読むので shell 設定はそのまま効く。 ただし SLOFF_ prefix の override は届かない。 必要になれば options 経由のサポートを足す。

**Unsupported な `OTEL_TRACES_EXPORTER` は warn + noop fallback** ( fail-fast しない) :

shell が他ツール用に `OTEL_TRACES_EXPORTER=zipkin` / `jaeger` / `otlp,zipkin` ( カンマ区切り multi-export) を export しているケースで、 sloff は OTel spec [SDK Configuration] の規定 ( 「 If a configured exporter is not implemented, the SDK MUST issue a warning and use the noop tracer」) に従い stderr に 1 行 warn を出して noop TP に fallback する。 sloff のコマンド自身は fail させない。 user が sloff だけ OTLP を使いたければ `SLOFF_OTEL_TRACES_EXPORTER=otlp` で override 可能。

これに対し **`OTEL_EXPORTER_OTLP_PROTOCOL` の unknown 値は fail-fast** を維持する: protocol は user が `otlp` を明示的に選んだあとの sub-config であり、 そこが壊れているのは user 自身の OTLP 設定の誤り、 silently noop に落とす方が診断を困難にする。 `http/json` の reject 理由 ( D3 既述) と同じ判断軸。

**Console exporter の出力先は stderr 固定**: `stdouttrace.New()` のデフォルトは `os.Stdout` だが、 sloff は `stdouttrace.WithWriter(os.Stderr)` を渡して **stderr に振り向ける**。 理由は `sloff graph` のような machine-readable な出力 ( Mermaid / DOT) を吐くサブコマンドが stdout を持つため、 trace JSON が stdout に混ざるとパイプライン下流のパーサが壊れる。 `OTEL_TRACES_EXPORTER=console` での local 検証は便利機能なので、 「 trace 有効化が通常コマンド出力を汚さない」 ことを CLI コントラクトとして守る。

**Header / attribute の percent-decode は `url.PathUnescape` を使う**: `url.QueryUnescape` を使うとクエリ文字列流の `+` → space 変換が走り、 `Authorization: Bearer <base64>` のように `+` を含む値が黙って壊れる。 PathUnescape は `%XX` のみをデコードし `+` をリテラルとして残すので、 OTel spec が要求する percent-encoding の意味論と OTLP header 系の実用 ( base64 auth) の双方に合致する。

### D4. Resource は SLOFF_ 値を attribute で組み立てる

`resource.WithFromEnv()` も env-only で options 経路が無い。 そこで:

- `OTEL_SERVICE_NAME` → `effectiveEnv` で読み `semconv.ServiceName(...)` として attribute 化
- `OTEL_RESOURCE_ATTRIBUTES` → `effectiveEnv` で読み、 自前パーサで `key=value,...` を `attribute.KeyValue` slice に展開 ( URL-decode を含む、 OTel spec 準拠)
- 上記を `resource.WithAttributes(...)` で渡す
- process / OS / host 情報は `resource.WithProcess() / WithOS() / WithHost()` で標準 attribute を埋める ( これらは OTEL_ env を読まず system info を取るだけなので env mutation の懸念なし)

`buildVersion` ( ldflags で injectable な package-level 変数) を `service.version` として常に attribute に追加する ( `OTEL_SERVICE_NAME` で上書き可能だが `service.version` は OTEL_ env で出ないので常に sloff 側で埋める)。

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
        ├─ runner.fingerprint.load
        ├─ runner.task.exec               miss 時のみ
        └─ runner.fingerprint.save              miss かつ ReadOnly=false 時のみ
```

主な span attribute:

- `runner.task.run`: `sloff.spec` ( spec dir) / `sloff.task.name` / `sloff.fingerprint.hit` ( bool) / `sloff.input.hash` ( 12 桁 hex に切詰) / `sloff.tool.count`
- `runner.fingerprint.load`: `sloff.fingerprint.state` ( hit / stale / miss / not_found)
- `runner.task.exec`: `sloff.cmd` ( argv 全体は冗長なので argv[0] のみ) / `process.exit_code`
- `runner.fingerprint.save`: `sloff.output.file_count`
- `resolver.<channel>[<tool>]`: `sloff.tool.name` / `sloff.resolver.channel`

Span のエラー時は必ず `RecordError` + `SetStatus(codes.Error, ...)` をセットする ( runner package に共通 helper を置く)。

#### 設計判断の補足

- **resolver 実装は無変更**。 instrumentation は呼び出し側 ( `runner.resolveInputContribs` / `resolveVersionContribs`) の loop 内で span を貼る。 resolver パッケージに otel 依存を持ち込まないことで、 将来 resolver を追加するときの認知負荷を増やさない。
- **fingerprint `Storage` interface は無変更**。 `Storage.Load` / `Storage.Save` の呼び出し箇所を span で囲む ( これも runner 側責任)。
- **`spec.Discover` は cmd/sloff 側でラップ**。 spec パッケージに otel 依存を持たせない。 phase span として cmd / runner の境界に span 開始 / 終了が分散するが、 trace tree としては parent-child で正しく繋がる。

### D6. Shutdown 規律

- `sdktrace.WithBatcher` ( BatchSpanProcessor) を採用。 短命 CLI なので `Shutdown` で必ず flush する。
- 各 subcommand の `RunE` ( `runE` / `graphE`) で `defer shutdown(ctx)` する。 shutdown 失敗時は stderr に warn を 1 行出すだけで **exit code には影響させない**。 exporter 障害が CLI の primary 機能 ( codegen 実行成否) を壊さない原則。
- shutdown は `context.Background()` ベースの短い deadline を持たせる ( 親 ctx が cancel された後でも flush が走るようにするため)。
- **shutdown は sloff-local TP の `Shutdown` を呼ぶだけ**。 global state の restore は不要 ( D2 で global を一切触らないと決めたため)。
- **env も touch しない** ( D2' / D3 / D4)。 SLOFF_ override は effectiveEnv → options で SDK に渡るので、 `os.Setenv` を使わず process env は不変。 shutdown で env restore する必要も無い。
- **disabled パス ( passive / explicit いずれも) は noop TP を返すだけ**。 shutdown は no-op。

### D7. Propagator は global を touch しない

global TextMapPropagator も `setupTracing` から **触らない**。 sloff 自身は propagator を使う経路 ( HTTP server / TRACEPARENT 受信等) を持たないので、 global を上書きする利得が無い。 host の propagator 設定が温存される。

- CI 環境などで親 span を `TRACEPARENT` env から受ける拡張は **本 PR では入れない**。 必要に迫られたら別 ADR で再検討 ( その場合は sloff-local な propagator を `runner.Options` に渡す形になる)。
- task の cmd ( child process) に `TRACEPARENT` を伝搬する仕組みも本 PR では入れない ( 同上)。

### D8. metrics / logs / TRACEPARENT 受信は scope 外

- **metrics**: fingerprint hit rate / task duration histogram / resolver duration 等は OTel metrics で表現したい候補だが、 trace の attribute だけでも当面の運用課題には足りる ( backend 側で集計可能)。 別 ADR で扱う。
- **logs**: 現 `runner.Logger` は `log.Default()` 直書き。 OTel log bridge ( slog 経由) への移行は 「 logger interface のリファクタ + `slog` 導入」 を伴う別 issue。
- **TRACEPARENT 受信**: CI から親 span を継承して sloff の trace を child として繋ぐのは有用だが、 単独で sloff の運用課題に対する優先度は低い。 上 2 つと同様、 必要に迫られたら別 ADR で。

## Consequences

### 正の影響

- **observability の足場**: 本 PR で trace tree が見える状態になり、 「 どのフェーズが遅いか」 「 fingerprint hit 率」 「 並列 task 形状」 を Jaeger / Tempo / Honeycomb 等で観測できるようになる。
- **0 cost when disabled**: env を何も set しなければ `noop` provider で短絡。 CI fixture や手元実行は今までと挙動 / 性能が変わらない。
- **既存 E2E goldens は無変更**: `internal/sloff/runner/runner_test.go` の golden 比較は test process が default `noop` のままなので emit が起きず、 `testdata/e2e/runner/.../expected/` は touch されない。
- **将来の metrics / logs 拡張余地**: SDK / exporter / resource はすべて OTel 標準に乗っているので、 metrics / logs 追加時に provider 横並びで追加できる。

### 負の影響 / 注意点

- **依存追加** ( `go.opentelemetry.io/otel` 系 ( otel / sdk / trace / exporters/otlp/otlptrace/{otlptracehttp,otlptracegrpc} / exporters/stdout/stdouttrace) + semconv)。 autoexport は採用しなかった ( D3 参照) ので prometheus / log bridge 系のトランジティブ依存が削れている。
- **resource 取得** ( `resource.WithProcess() / WithOS() / WithHost()`) で `os/exec` 系の system call が発生する。 startup latency に数 ms 載る可能性。 ただし trace 有効時のみ。
- **SLOFF_ override は env を一切汚染しない**。 child process ( task の cmd) が `OTEL_*` を読んでも、 user の shell が設定した値だけを見る ( D2' の childEnv による subprocess scrub も併用)。 「 sloff だけ別 endpoint」 という方針が subprocess に対しても矛盾なく成り立つ。
- **global OpenTelemetry state を一切 mutate しない**。 同一プロセスで複数の sloff invocation を直列 / 並列に走らせても干渉しない ( codex review round 6 P2 #2 で指摘された並行実行時の TP 巻き戻りリスクを構造的に排除)。 host が独自の global TracerProvider / propagator を入れていても影響を受けない。 in-process embedder が sloff spans を自分のバックエンドに流したい場合は `runner.Options.TracerProvider` に host TP を渡す。

### 将来再考の余地

- **metrics 導入** ( task duration histogram / fingerprint hit rate counter / resolver duration histogram)
- **`slog` への logger 移行 + OTel log bridge** ( runner.Logger interface のリファクタを伴う)
- **TRACEPARENT 受信** ( CI から親 span を継承)
- **child process への TRACEPARENT 伝搬** ( codegen tool 側が OTel 対応していれば trace tree が垂直に繋がる)
- **service.version の build pipeline injection** ( release workflow が整ったタイミングで ldflags 注入を整備)
- **per-glob.Expand / per-hash 単位など更に細かい span** ( 必要に迫られたら追加)
