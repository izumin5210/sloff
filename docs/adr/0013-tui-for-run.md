# ADR-0013: `sloff run` の進捗を bubbletea ベースの簡易 TUI で表示する

## Context

### 背景

`sloff run` は依存グラフ上の独立した task を `NumCPU` 並列で fan-out する ( [`runner.taskConcurrency`](../../internal/sloff/runner/runner.go) )。 結果として stdout / stderr には複数 task の出力がインターリーブして流れ、 「**いま何が走っていて、 何が終わり、 何が失敗したのか**」 が読み取りにくい。 fingerprint hit が増えるほど run 全体は高速になるが、 cache miss した数本の重い task の進行を覗くために `tail -f` 相当の手段が欲しい、 という体感も出ている。

一方で `sloff` は monorepo 向け CLI であり、 CI 上では「`go test` のような行指向ログ」 として機能する必要がある。 既存 ADR は CI / non-tty 環境を主要利用シーンとして扱っている ([ADR-0001](./0001-fingerprint-aware-codegen-orchestrator-decision.md))。 TUI 化は 「**開発者の interactive run で進捗を読みやすくする**」 ことを目的とするのであり、 CI ログのフォーマットを置き換えるものではない。

加えて、 失敗 task の generator 出力を見たいとき、 現状は終了後の stderr 全体を遡るしかない。 task 単位でログをファイル化し、 TUI から `less` で開ければ、 「**走っている generator の生ログを覗き、 検索する**」 操作が常套手段化できる。

### 評価軸

- **CI / non-tty 環境では従来の行指向出力を変えない** ( ADR-0001 が前提とする運用)
- **interactive run で進捗の可読性が上がる** ( 並列 task の状態 / どこで詰まっているか / 失敗の有無)
- **失敗時の調査経路を構造化する** ( task ごとのログファイル + pager 起動)
- **fingerprint / depgraph / preflight の挙動には影響しない** ( runner は表示層の変化に依存しない)
- **観測性 ( OTel span / 既存 logger) を毀損しない** ( TUI モードでも span は今までどおり)
- **撤回コストが低い** ( TUI を取り下げた場合に runner / cmd の側に痕跡が残らない)

### References

- [ADR-0001: fingerprint ベースのコード生成オーケストレーターの選定](./0001-fingerprint-aware-codegen-orchestrator-decision.md)
- [ADR-0009: OpenTelemetry トレーシング](./0009-otel-tracing.md)
- [ADR-0012: `--force` flag](./0012-force-rerun-flag.md) ( CLI flag 設計の参考)
- [Design Doc: sloff Architecture](../design/architecture.md)

## Considered Options

### Comparison Table

| | A: 維持 ( 何もしない) | B: 行指向 progress を整形 (例: `task-spinner`) | **C: bubbletea TUI ( 採用)** | D: bubbletea + 常時 TUI ( non-tty 含む) |
|---|---|---|---|---|
| CI / non-tty で従来挙動を維持 | ◎ | ◎ ( 行指向のまま) | ◎ ( tty 判定で切替) | × ( CI ログを破壊) |
| 並列 task の進捗可読性 | × | △ ( interleave が完全には解けない) | ◎ | ◎ |
| 失敗ログの即時調査 | × | × | ◎ ( `l` で pager) | ◎ |
| runner / depgraph への影響 | n/a | 低 | 低 ( EventSink 経由) | 低 |
| 撤回コスト | n/a | 中 ( 出力フォーマット定着後の撤回は破壊的) | 低 ( EventSink を nil にすれば即座に従来挙動) | 低 |
| 実装規模 | 0 | 小 | 中 ( bubbletea 依存 +) | 中 |

### Option A: 維持

並列 run の体感は今のままだが、 stdout interleave の不読性は構造として残る。 ADR-0001 が想定する「日常使いに耐える orchestrator」 の体験面で機会損失が大きい。 棄却。

### Option B: 行指向 progress を整形

各 task の状態遷移 ( start / finish / skip / fail) を `[1/12] RUN spec/task` のような行 で流す。 既存ロガーの上位互換だが、 並列 fan-out 下で行が時間順に飛び交う問題は解消できない。 また pager 起動による失敗ログ調査の動線が欠ける。 棄却。

### Option C: bubbletea TUI ( 採用)

stdout が tty のとき bubbletea ベースの TUI を起動し、 depgraph topo 順固定のリストに各 task の状態 (`pending` / `running` / `succeeded` / `skipped` / `failed`) を表示する。 stdout が tty でない、 または `--no-tui` 明示時は **従来のロガー出力にフォールバック**する ( EventSink を nil にする経路)。 task 出力は `.sloff/logs/<spec>/<task>.log` に逃がし、 リスト上で `l` 押下時に `$PAGER` ( なければ `less`) で開く。

- ◎ tty 自動切替で CI / non-tty 環境への影響をゼロにできる
- ◎ runner 側は `EventSink` interface を呼ぶだけで、 TUI を取り下げても呼出側 ( cmd) の差し替えだけで済む
- ◎ ログがファイル化されることで、 run 終了後にも `.sloff/logs/<spec>/<task>.log` を直接 `less` で開ける ( TUI 外でも調査可能)
- △ bubbletea / bubbles / lipgloss を direct dependency に追加する。 既に indirect で入っているため依存グラフ上の増分は限定的

### Option D: bubbletea + 常時 TUI

non-tty 環境でも TUI を起動する。 CI ログが壊れ、 ADR-0001 が前提とする運用と直接衝突する。 棄却。

## Decision

**Option C を採用する。 stdout が tty なら TUI を起動し、 そうでなければ従来挙動。 task ログは `.sloff/logs/<spec>/<task>.log` に書き出し、 TUI から `l` で pager を起動する。**

採用根拠:

1. **tty 自動切替で CI 影響をゼロにできる**。 ADR-0001 が前提とする「CI で行指向ログとして動く」 性質を構造で保ったまま、 interactive run の体験だけを置き換えられる
2. **runner と TUI の責務分離が `EventSink` interface で完結する**。 runner は表示層を知らず、 TUI を取り下げる場合は cmd 側で EventSink を nil にするだけ
3. **失敗ログのファイル化は TUI と独立した価値を持つ**。 TUI が無い ( non-tty / `--no-tui`) 場合でも `.sloff/logs/` は書かれるため、 開発者は run 後にも `less` でログを読み返せる
4. **OTel span は不変**。 表示層の追加は runner の span 構造に手を入れない

### CLI 仕様

```sh
sloff run                # stdout が tty なら TUI、 そうでなければ従来出力
sloff run --no-tui       # 常に従来出力 ( pipe / less / log redirect 用)
```

- 既定値は「tty 判定で自動切替」
- 環境変数 mirror は提供しない ( ADR-0012 の議論と同じく、 「常時 ON / 常時 OFF を `.env` で固定する」 形を構造で避ける)
- `--force` / `SLOFF_ALLOW_STALE_DEPS` との併用は許可 ( TUI は表示層なので、 fingerprint / preflight の動作とは直交する)

### TUI 仕様

- **リスト順序**: depgraph topo 順固定。 状態だけが更新される ( 「実行中を先頭に詰める」 並べ替えは状態追跡を難しくするため避ける)
- **状態表記**:
  - `pending` ( 未開始): 灰色のドット
  - `running` ( 実行中): braille spinner ( 時刻ベース、 並列 task で位相が揃う)
  - `succeeded` ( cmd 実行成功): チェック + 灰色行
  - `skipped` ( fingerprint hit): チェック + 「(cached)」 補記 + 灰色行
  - `failed` ( cmd エラー): バツ + 赤色
- **キーバインド**: `j` / `k` / `↑` / `↓` でカーソル移動、 `l` で選択中の task のログを pager で開く、 `q` / `Ctrl+C` で cancel ( ctx cancel 経由で runner を停止)
- **終了挙動**: 全 task が `succeeded` / `skipped` / `failed` のいずれかになった時点で自動退出。 失敗時は退出後に stderr に失敗 task と log path のサマリを 1 行ずつ出力する

### ログファイル仕様

- 出力先: `<RepoRoot>/.sloff/logs/<spec>/<task>.log`
- 毎 run truncate-create ( 履歴は残さない)。 ローテーション / GC は不要。 同一 spec 配下の `<task>.log` は次回の run でそのまま上書きされる
- TUI 非利用時 ( `--no-tui` / non-tty) でも書き出す ( 失敗後に開発者が `less` で読めるよう一貫させる)
- `.gitignore` に `.sloff/logs/` を追加する ( fingerprint record は引き続き git 管理、 log は ephemeral)

### Pager 仕様

- 解決順: `$PAGER` ( 環境変数) → `less -R +F` → 解決不能なら TUI 内に inline エラー表示
- `less -R +F` で起動した場合のオプションは [ADR-0001 の運用前提](./0001-fingerprint-aware-codegen-orchestrator-decision.md) に揃え、 ANSI 色を保ち ( `-R`)、 follow モードで起動する ( `+F`)。 プロンプトの文言は最下行 hint として `q: quit  Ctrl+C: pause` 相当を出す
- pager 起動中は `tea.ExecProcess` で TUI altscreen を suspend し、 ユーザーが pager から抜けると TUI に復帰する
- `$PAGER` が空文字、 または bubbletea が pager 起動に失敗した場合は TUI 内に「pager not found」 を 1 行 inline で表示し、 run の継続を妨げない

### Runner 仕様

`runner.Options` に以下を追加する:

- `LogDir string`: 空ならば従来通り stdout/stderr を直接 cmd に渡す。 非空ならば task ごとに `<LogDir>/<spec>/<task>.log` を truncate-create し cmd.Stdout / Stderr に差し替える
- `EventSink EventSink`: 状態通知の hook。 nil ならば既存 logger 出力を維持。 非 nil の場合は既存の `RUN` / `SKIP` log 出力を抑止し、 EventSink にのみ流す ( 二重出力を避ける)

`EventSink` interface:

```go
type EventSink interface {
    PhaseChanged(phase Phase)
    RunStarted(tasks []TaskRef)
    TaskStarted(ref TaskRef, logPath string)
    TaskFinished(ref TaskRef, result TaskResult)
}
```

- `PhaseChanged` は run の前半 ( preflight → resolve-inputs → resolve-versions → planning → prefetch-fingerprints → running-tasks) で各フェーズの **開始時** に発火する。 TUI はリストが空の間 ( = `RunStarted` 前) は上部に「`<spinner> <phase>`」 を 1 行表示し、 `RunStarted` 以降は行ごとの状態表示に切り替えて準備行を消す
- フェーズ名は表示文字列が API 表面なので破壊的に変更しない ( 既存テストの phase order assertion を再利用する形で固定する)
- `PhaseRunningTasks` は `RunStarted` の **直前** に必ず発火する。 表示層が「準備中表示 → 行表示」の切替フリッカーを起こさないための順序保証

- `TaskRef` は `{ SpecRelpath, Name }` の値オブジェクト
- `TaskResult` は `{ Outcome (succeeded|skipped|failed), Err error }`
- 失敗時の `Err` は EventSink 実装 ( TUI) で表示用に整形する。 run 全体の error 集約 ( `errgroup.Wait`) は runner 内に残る
- TUI 利用時 ( cmd 側で EventSink を bind したとき) のみ runner 内の `RUN` / `SKIP` ロガー出力を抑止する。 これは TUI と logger の二重出力 ( TUI altscreen 外で logger が混入する) を避ける構造的措置

### 観測性

- OTel span ( `runner.task.run` 等) は変更しない。 TUI は表示層なので span 構造には介在しない
- TUI モードでも `runner.task.exec` の `process.exit_code` は記録される
- TUI から `l` で pager を起動した行為は trace に出さない ( 表示層の操作はログ層に持ち込まない)

### 撤回時の影響

TUI を取り下げる場合、 `cmd/sloff/run.go` で EventSink を bind せず `--no-tui` を default にすればよい。 `runner.Options.EventSink` / `LogDir` は EventSink 非 nil / LogDir 非空 のときだけ挙動が変わるため、 互換性を毀損せずに残せる。 撤回コストは小さい。

## Consequences

### 正の影響

- interactive run の進捗 / 失敗位置 / 完了タスクの可読性が上がる
- 失敗 task のログがファイル化されるため、 run 後にも `less` / エディタ / `grep` で調査できる
- runner と表示層が `EventSink` で分離され、 将来別の表示層 ( e.g. structured JSON for AI agents) を追加するときも同じ hook 経路に乗せられる

### 負の影響

- direct dependency に bubbletea / bubbles / lipgloss / go-isatty が追加される。 ただし lipgloss は既に indirect で入っているため、 binary size の増分は限定的
- `.sloff/logs/` を `.gitignore` から漏らした場合に意図せず log が git に乗る ( 本 ADR 採用と同時に gitignore 更新を必須化する)
- task 出力が stdout に流れなくなる ( TUI モード時)。 これは仕様上の意図 ( 並列出力を整理する) だが、 「`go run` の出力をそのまま grep したい」 ような操作は `--no-tui` か `cat .sloff/logs/<spec>/<task>.log` に置き換わる

### 後続の更新

本 ADR の決定を受けて以下を更新する:

1. [Design Doc: architecture.md](../design/architecture.md): runner と CLI の節に「表示層は EventSink で分離」「 task ログは `.sloff/logs/`」 を加筆
2. `internal/sloff/runner/runner.go`: `Options.LogDir` / `Options.EventSink` 追加、 `runTask` でログファイルへの redirect、 EventSink 非 nil 時の既存 logger 抑止
3. `internal/sloff/runner/events.go` ( 新規): `EventSink` interface と `TaskRef` / `TaskResult` 型
4. `internal/sloff/tui/` ( 新規): bubbletea TUI 実装
5. `cmd/sloff/run.go`: `--no-tui` flag、 tty 判定、 `signal.NotifyContext` での SIGINT / SIGTERM hookup、 runner と TUI の goroutine 連携
6. `go.mod`: direct dependency に bubbletea / bubbles / lipgloss / go-isatty
7. `.gitignore`: `.sloff/logs/` を追記
8. E2E test ( `internal/sloff/runner` ): `LogDir` 指定時にログファイルが期待通り書かれるケースを追加。 既存 E2E は `LogDir=""` / `EventSink=nil` で従来挙動が保たれることを引き続き検証する
