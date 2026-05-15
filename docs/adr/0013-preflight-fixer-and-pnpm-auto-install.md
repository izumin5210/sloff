# ADR-0013: preflight Fixer interface による install drift の自動修復

## Context

### 背景

[ADR-0008](./0008-tool-as-first-class-spec-entity.md) D7 と [resolver-pnpm-local.md](../design/resolver-pnpm-local.md#install-drift-check-pnpm-install-忘れ検出--preflight-経由) の決定により、 `pnpm-local` channel は `pnpm-lock.yaml` と `node_modules/.pnpm/lock.yaml` の byte 比較で install drift を検知し、 `preflight.Issue` を返して `sloff run` を fail させる。 escape hatch として `SLOFF_ALLOW_STALE_DEPS=1` を立てれば warn 降格 + read-only モードで通過する ( [ADR-0012](./0012-force-rerun-flag.md) §"preflight との関係" でこの分離は確認済み)。

実運用では「 `git pull` → `pnpm install` を忘れて `sloff run`」 の sequence が日常的に発生し、 そのたびに利用者が手動で `pnpm install` を打って sloff を再起動する。 `Issue.Suggestion` フィールドに `"pnpm install"` という文字列が入っているが、 sloff 自身がそれを実行する経路は無く、 UX 上の摩擦になっている。

`Checker` interface には **「 Implementations must be read-only」** という制約 ( `preflight.go` の doc comment) が明記されており、 副作用を伴う remediation を Checker に直接持たせると、 既存実装が依拠している invariant ( drift detection は read-only / hash 入力には混ぜない / ADR-0007 の責務境界) を曖昧にする。

### 評価軸

- **`SLOFF_ALLOW_STALE_DEPS` escape hatch を壊さない**: 既存利用者の運用 ( experimental edit を一時的に通すケース) を維持する
- **ADR-0008 D7 の境界 ( build/run は cmd 責務) を維持する**: install と build を概念的に区別し、 install のみを sloff が面倒見ても D7 の趣旨は壊れないことを明文化する
- **Checker は read-only という invariant を壊さない**: 副作用付き処理は別 interface で受け、 言語仕様レベルで責務を分離する
- **テスト容易性**: 実 `pnpm` バイナリへの依存を unit / E2E test の通常経路から外せる ( CI で `pnpm install` を実行する build を増やしたくない)
- **後方互換**: 既存の `SLOFF_ALLOW_STALE_DEPS=1` 経路 / drift なし経路 / Fixer 未実装 channel の挙動を変えない

### References

- [ADR-0007: no external dependency resolver](./0007-no-external-dependency-resolver.md) ( SSoT は lockfile / preflight は state validation)
- [ADR-0008: tool を first-class spec entity とする](./0008-tool-as-first-class-spec-entity.md) ( D7: build/run は cmd 責務)
- [ADR-0012: `--force` flag](./0012-force-rerun-flag.md) ( `SLOFF_ALLOW_STALE_DEPS` との責務分離)
- [Design Doc: resolver-pnpm-local.md](../design/resolver-pnpm-local.md) ( install drift check の実装)
- [Design Doc: architecture.md](../design/architecture.md) ( preflight 章 / 拡張点 interface)

## Considered Options

### Comparison Table

| | A: 提供しない ( 現状) | B: `Issue.FixCmd []string` | **C: `Fixer` interface ( 採用)** | D: 並列 `autofix.Registry` |
|---|---|---|---|---|
| Checker の read-only invariant | ◎ | ○ ( Issue は data のまま) | ◎ ( 別 interface で文法レベル分離) | ◎ |
| ADR-0008 D7 との整合 | ◎ | △ ( 文字列を runner が `exec` する形は build 系拡張への悪い導線) | ◎ ( interface 拡張で意図を限定) | ○ |
| テスト容易性 ( fake 注入) | n/a | △ ( PATH 経由 fake binary しかない) | ◎ ( installer 関数を Option 注入) | ◎ |
| 既存 escape hatch との両立 | ◎ | ○ | ◎ ( `ReadOnly=true` で Fix を呼ばない構造的分岐) | ◎ |
| 拡張性 ( 将来の channel 追加) | × | ○ ( exec しか出来ない) | ◎ ( Go 関数で任意の remediation を書ける) | ○ ( registry 二重実装) |
| 既存実装からの diff 量 | 0 | 中 ( runner で exec / 安全性レビュー) | **小** ( interface 追加 + runner 数十行) | 大 ( scopeCheckers を再実装) |
| UX 改善 | × | ◎ | ◎ | ◎ |

### Option A: 提供しない ( 現状維持)

`Issue.Suggestion` の文字列のまま、 利用者が手動で `pnpm install` を打つ。 ADR-0008 D7 「 build/run は cmd 責務」 を最も strict に解釈する立場。

ただし install は build と概念的に区別できる ( 詳細は §Decision §D2)。 install drift は SSoT (`pnpm-lock.yaml`) と runtime state ( `node_modules/`) の同期問題で、 source rebuild と無関係。 D7 の趣旨を厳格に解釈しても install までは含まないと整理可能で、 UX 摩擦を放置するメリットが薄い。 棄却。

### Option B: `Issue.FixCmd []string` を追加して runner が `exec.Command` する

最もシンプルな data 拡張。 Checker は `Issue.FixCmd = []string{"pnpm", "install"}` を詰めるだけ、 runner が `exec.CommandContext(ctx, FixCmd[0], FixCmd[1:]...)` で起動する。

- ○ data class 拡張なので diff が小さい
- △ runner が文字列ベースで任意コマンドを起動する形になり、 「 install 以外の remediation ( 例: 将来 `cargo metadata` を triggers) を追加するとき、 何を実行できて何を実行できないかを runner レイヤで判別する」 責務が肥大化する
- △ テスト時の差し替えが PATH 経由 fake binary 配置になり、 並列テストでの隔離が脆くなる ( `$PATH` を temp dir に差し替える等のテクニックが必要)
- △ ADR-0008 D7 が警戒する「 cmd 領域の責務を sloff に持ち込む」 流れに将来的に乗りやすい ( `FixCmd` で build もできる、 となれば D7 の境界が緩む)

UX は満たすが、 拡張性とテスト容易性で C に劣る。 棄却。

### Option C: Checker に optional `Fixer` interface を持たせる ( 採用)

`preflight.Fixer` interface を新設し、 `pnpmlocal.Checker` がこれを satisfy する形。 runner は `if fixer, ok := checker.(preflight.Fixer); ok { fixer.Fix(ctx, repoRoot) }` で type assert する。

```go
// internal/sloff/preflight/fixer.go
type Fixer interface {
    // Fix attempts to remediate the drift surfaced by the preceding Check.
    // Implementations MAY have side effects ( exec processes, write files);
    // this is the explicit distinction from Checker.Check, which must be read-only.
    Fix(ctx context.Context, repoRoot string) error
}
```

- ◎ Checker の "must be read-only" invariant を壊さない ( Fix は別メソッド、 別 interface 名で副作用を明示)
- ◎ installer 関数 ( `func(ctx, repoRoot) error`) を Checker の Option で注入できる → test で実 `pnpm` 不要、 fake で完全 mockable
- ◎ runner の分岐は type assert + Fix 呼び出しの数十行で済み、 既存 `runPreflight` の骨格を保ったまま追加できる
- ◎ 将来 `bun` / `cargo` 等の channel が同じパターン ( drift 検出 → install) で Fixer を実装する余地が widening する

### Option D: 並列 `autofix.Registry` を新設

`preflight.Registry` と並列に `autofix.Registry` を持ち、 preflight が Issue を出した後に autofix が name 一致で remediation を起動する。

- ◎ Checker / Fixer の責務が完全分離する
- × `runner.scopeCheckers` ( referenced resolver name 集合との交差) を autofix 側でも再実装する必要があり、 cognitive overhead が増える
- × 「 1 channel に Checker と Fixer 両方を持たせる」 のが基本ケースなので、 別 Registry にする meaning が薄い

実装コストに見合うメリットが無い。 棄却。

## Decision

**Option C: `preflight.Fixer` interface を新設し、 `pnpmlocal.Checker` に Fix を実装する。 runner は drift 検出時に default で Fix を呼び、 失敗時は run 全体を fail させる。 `SLOFF_ALLOW_STALE_DEPS=1` のときは Fix を呼ばず現状どおりの read-only 降格を維持する。**

採用根拠:

1. **Checker の read-only invariant が文法レベルで担保される**: Fix は別 interface のメソッドで、 Check と signature レベルで区別される。 「 副作用を伴う処理は Fixer」 「 検証は Checker」 が読み手から自明
2. **`SLOFF_ALLOW_STALE_DEPS` escape hatch と完全に直交**: ReadOnly=true ( = `SLOFF_ALLOW_STALE_DEPS=1`) のときは Fix を呼ばないという 1 行の分岐で既存挙動を完全保存
3. **テスト容易性**: installer を Option (`WithInstaller`) で注入できるため、 unit / E2E test の通常経路から実 `pnpm` 依存を切り離せる
4. **段階的な拡張に耐える**: 将来 `bun` / `cargo` 等が同じパターン ( drift 検出 → install) を踏むとき、 同じ Fixer interface を実装するだけで runner 側の変更不要

### D1. CLI フラグは増やさない

`SLOFF_ALLOW_STALE_DEPS=1` ( = `Runner.ReadOnly`) の有無が auto-fix を切り替える唯一のスイッチ。 新フラグ ( `--auto-install` 等) は導入しない。

- ADR-0012 が `--force` を CLI flag に倒した文脈は「 `--no-fingerprint` 文化を構造で抑止する」 = 「 env で常時 ON にすると毒性が高い」 という判断。 install drift 自動修復はその逆で、 「 ローカル開発で常時 ON が望ましい」 「 CI でも install を先に走らせるのが普通だから影響ゼロ」 という性質を持つ。 default-on にして scrutiny を `ALLOW_STALE_DEPS` の opt-out 経路に集約するほうが UX 一貫性が高い
- 新フラグを増やすと「 `--auto-install` と `SLOFF_ALLOW_STALE_DEPS` の優先順位は?」 という cognitive overhead が必ず生じる。 「 ReadOnly ( = ALLOW_STALE_DEPS) のときは Fix を呼ばない」 という 1 つのルールに集約することで仕様が単純化する

### D2. ADR-0008 D7 ( build/run は cmd 責務) との関係

D7 が「 sloff は build orchestration をしない」 と決めた根拠は (a) build には source rebuild の orchestration が必要で task の inputs/outputs cycle と絡む (b) `go run` / `pnpm build && exec` のような compile+execute の一体化は cmd 内で表現するほうが自然、 という 2 点。

install drift の自動修復はこの根拠のどちらにも当たらない:

- install は SSoT ( lockfile) と runtime state ( node_modules) の同期のみが問題で、 source rebuild と無関係。 task の inputs/outputs cycle に組み込まれない
- `pnpm install` は cmd の compile+execute の一部ではなく、 cmd 実行の前提条件 ( `bash` でいう `source ~/.bashrc` 的な環境準備)。 preflight の「 cmd 実行前の state 検証」 と概念的に同 layer

つまり **D7 の境界は変更しない**。 ADR-0008 §D7 末尾の "preflight の責務との分離" 段落に、 「 install drift は preflight Fixer 経由 ( 本 ADR) で remediate される。 build/run は依然 cmd 内責務」 という cross-reference を追加することで読者の混乱を避ける。

### Runner 仕様

`runner.runPreflight` の改造内容:

1. `r.opts.Preflight.Run(ctx, ".", checkers)` で Check 実行
2. `res.OK == true` → return ( 現状どおり)
3. `res.OK == false`:
   - **3a.** `r.opts.ReadOnly == true` ( SLOFF_ALLOW_STALE_DEPS=1):
     - `reportPreflightIssues` で warn 出力 → 続行 ( **Fix を呼ばない、 現状完全保存**)
   - **3b.** `r.opts.ReadOnly == false` ( default):
     1. `reportPreflightIssues` を warn variant で呼ぶ ( "preflight [...] -- attempting auto-fix")
     2. checkers のうち `Fixer` を実装しているものを抽出
     3. 抽出結果が空 → 既存どおり `"preflight failed (%d issues); set SLOFF_ALLOW_STALE_DEPS=1 to bypass"` で fail
     4. 抽出結果が非空 → 各 Fixer の `Fix(ctx, repoRoot)` を逐次実行。 失敗時は即 `fmt.Errorf("auto-install failed: %s: %w", checker.Name(), err)` で return
     5. すべて成功 → 同じ checkers で **再度** `Preflight.Run` を実行
     6. 再 Check で OK → `logger.Infof("auto-install resolved drift: %v", channels)` して続行
     7. 再 Check で依然 NOT OK → `fmt.Errorf("auto-install ran but drift persists: ...")` で fail

再 Check ( step 5) を入れる根拠: install が exit 0 を返しても何らかの理由で snapshot が更新されない異常ケース、 複数 channel のうち片方しか Fixer を持たないケースで stale なまま task 実行に進む invariant 違反を防ぐ。

### 観測性

`runner.runPreflight` の span に attribute を追加:

- `sloff.preflight.autofix_attempted` ( bool): Fixer を呼んだか
- `sloff.preflight.autofix_succeeded` ( bool): 再 Check で OK になったか
- `sloff.preflight.autofix_channels` ( string[]): Fix を呼んだ channel 名

### `pnpmlocal.Checker` 仕様

- `Checker` struct に `installer func(ctx context.Context, repoRoot string) error` フィールドを追加 ( nil の場合は `runPnpmInstall` を呼ぶ)
- `New(repoRoot string, opts ...Option)` に variadic option を追加し、 `WithInstaller(fn)` を export
- `Fix(ctx, repoRoot string) error` メソッドを実装 — installer を呼び、 stdout/stderr は Checker が持つ `io.Writer` に forward
- `runPnpmInstall` は `internal/sloff/preflight/pnpmlocal/install.go` に置く。 `exec.CommandContext(ctx, "pnpm", "install")`、 `Dir = repoRoot`、 non-zero exit は `auto-install failed: pnpm install exited with code N: <stderr>` で wrap

## Consequences

### 正の影響

- 「 `git pull` → `pnpm install` 忘れ → `sloff run` fail」 の日常的な摩擦が、 利用者の介入なしに解消する
- preflight subsystem に Fixer という拡張点が用意されることで、 将来の lockfile-based channel ( `bun` / `cargo` 等) が同じパターンで remediation を実装できる
- ADR-0008 D7 の境界 ( build/run は cmd 責務) は維持される ( install は別概念だと明文化された)
- `SLOFF_ALLOW_STALE_DEPS=1` 経路 / drift なし経路 / Fixer 未実装 channel の挙動は無変化

### 負の影響 / 注意点

- 実 `pnpm` バイナリへの依存が production code path に入る ( `exec.Command("pnpm", "install")`)。 ただし pnpm-local channel を使う時点で pnpm は前提なので、 sloff 起動環境に追加要求は実質ゼロ
- 「 sloff が勝手に install を打つ」 ことに違和感がある利用者は `SLOFF_ALLOW_STALE_DEPS=1` で旧挙動 ( warn 降格 + read-only) に戻せる。 ただしこの挙動切り替えは「 escape hatch を立てる」 という明示の操作が必要 → 既定の挙動が変わることを README / CHANGELOG で告知する必要あり
- pnpm 未インストール環境では `auto-install failed: pnpm: command not found` で fail する。 旧 `"preflight failed; set SLOFF_ALLOW_STALE_DEPS=1 to bypass"` メッセージと変わるため、 既存ログ監視を持つ CI ではアラート文言の更新が要る可能性

### 撤回時の影響

`Fixer` interface と `pnpmlocal.Checker.Fix` を削除して runner の type assert 分岐を外せば、 即座に旧挙動 ( drift で fail) に戻る。 `Issue` / `Checker` の data shape は変えないため、 撤回コストは小さい。

### 後続の更新

本 ADR の決定を受けて以下を更新する:

1. [ADR-0008](./0008-tool-as-first-class-spec-entity.md) §D7 末尾 "preflight の責務との分離" 段落: 「 install drift は preflight Fixer 経由 ( ADR-0013) で remediate される。 build/run は依然 cmd 内責務」 を追記
2. [Design Doc: resolver-pnpm-local.md](../design/resolver-pnpm-local.md) §"preflight 経由にする利点": 「 auto-install hook point を持てる ( ADR-0013) — escape hatch path との両立を維持」 を追加
3. [Design Doc: architecture.md](../design/architecture.md) preflight 章: Fixer interface の説明、 default-on の auto-fix 挙動、 `SLOFF_ALLOW_STALE_DEPS=1` での skip を追記
4. `internal/sloff/preflight/fixer.go` ( 新規): `Fixer` interface 定義
5. `internal/sloff/preflight/pnpmlocal/install.go` ( 新規): `runPnpmInstall` 実装
6. `internal/sloff/preflight/pnpmlocal/pnpmlocal.go`: `Checker` に installer フィールド + `WithInstaller` option + `Fix` メソッド
7. `internal/sloff/runner/runner.go`: `runPreflight` 改造 + span attribute
8. E2E test ( `testdata/e2e/runner/`): drift → auto-install 成功 / drift → auto-install 失敗 / drift + `SLOFF_ALLOW_STALE_DEPS=1` で skip の 3 ケース
