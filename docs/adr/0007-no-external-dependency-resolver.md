# ADR-0007: lazygen は外部依存専用 resolver ( go-external / pnpm-external) を持たない

## Context

lazygen の OS 横断 invalidate 戦略 ( architecture.md の対応節) では、 ツールを **どこから version 文字列を取るか** で channel を分類している。 当初の構想では「外部公開パッケージ」を独立 channel として扱う resolver を 2 種類想定していた:

- **go-external resolver**: `go.mod` / `go.sum` を SSoT に Go module の `path@version` を版として採用
- **pnpm-external resolver**: `pnpm-lock.yaml` を SSoT に npm registry 配布物の resolved version を採用

実装に踏み込む前に、 これらが本当に独立 resolver として lazygen に組み込まれる必要があるかを再検討した。 結論は「 go-external も pnpm-external も導入しない、 既存の汎用プリミティブ ( script resolver + 内製ソース resolver) で機能的に過不足なくカバーできる」。 [ADR-0006](./0006-no-buf-specific-resolver-or-preflight.md) で buf を special-case しないと決めた延長線上にある判断を、 外部依存にも適用する。

### References

- [ADR-0001: キャッシュ可能コード生成オーケストレーターの選定](./0001-cache-aware-codegen-orchestrator-decision.md)
- [ADR-0005: Resolver auto-dispatch を廃止し、 すべて declared 経由に統一](./0005-eliminate-resolver-auto-dispatch.md)
- [ADR-0006: lazygen は buf を special-case しない](./0006-no-buf-specific-resolver-or-preflight.md)
- [Resolver: script](../design/resolver-script.md)
- [Resolver: go-local](../design/resolver-go-local.md)
- [Resolver: pnpm-local](../design/resolver-pnpm-local.md)

### 観察

**O0. ただし内製ソース resolver は「自身が depend する transitive 外部 dep」 を hash 入力に取る必要がある**

ADR の主題から少し外れるが、 `go-local` / `pnpm-local` のような内製ソース resolver は **自身の hash 入力に「 そのツールが transitive に依存する外部公開パッケージの resolved version 集合」 を含める**。 これは「外部公開パッケージの専用 resolver を作らない」 という本 ADR の決定とは矛盾しない:

- 本 ADR が排除するのは「 spec で外部パッケージを直接 declare してその version を tools_hash に取り込むための専用 resolver ( go-external / pnpm-external)」
- 一方、 「 内製ツールが利用する外部 dep の version 変動が、 内製ツールの runtime 挙動を変える」 のは事実なので、 内製ソース resolver が surgical に walk して hash に取り込むのは **内製ソース resolver の責務として妥当**
- これは Turborepo が package 単位の hash に「その package が transitively 依存する外部 dep の resolved version」 を組み込むのと同じ哲学 ( 該当 package のスコープに閉じた surgical hashing)

具体的には [resolver-go-local.md](../design/resolver-go-local.md) は go.sum + module path@version を、 [resolver-pnpm-local.md](../design/resolver-pnpm-local.md) は pnpm-lock.yaml の `importers.<package-dir>` から `snapshots` を BFS で walk した transitive 集合を、 それぞれ自前で取り込む。 内部 hash 上は両 resolver とも `<channel>-deps:<pkg>@<version>` ( go-deps / pnpm-deps) という統一された ToolVersion 形式で contribution する ( = ADR で排除した「外部依存専用 resolver」 とは別物の、 内製ソース resolver 内部での `<channel>-deps` 表記)。 **本 ADR で排除されるのは「 spec から外部公開パッケージ単独を直接版付けする経路」 のみで、 内製ソース resolver 内部での外部 dep hash 取り込みは含まれない**。

**O1. Go の外部 module は go-local の external partition で既に hash 入力に入っている**

`go-local` resolver は `go/packages` で transitive 依存を解析し、 main module 内のソースは内容 hash、 外部 module は `path@version` + go.sum 行の hash 入力としている ( resolver-go-local.md 参照)。 したがって 内製 Go ツール **経由で利用される** 外部 module の version 変動は go-local の `tools_hash` 経路で既に invalidate される。 「外部 module 単体を独立に hash したい」要件は実用上発生しない ( 内製ツール経由で使うのが一般的)。

prebuilt な OSS Go ツール ( 例: aqua / `go install` で配布されるもの、 `go tool` ディレクティブ経由で build されるもの) を使う場合は、 **script resolver で `<bin> --version`** を取れば同じ強度で版が取れる ( resolver-script.md 参照)。 つまり「Go の外部依存単独に専用 resolver」を成立させる枠は構造上存在しない。

**O2. npm 公開パッケージも script resolver で `--version` が取れる**

pnpm 経由で利用する npm 公開ツール ( 例: `pnpm exec my-codegen`) は、 ほぼ全ての CLI が `--version` を備える。

```yaml
tools:
  my-codegen:
    exec: ["pnpm", "exec", "my-codegen", "--version"]
```

このとき script resolver が SSoT とするのは「実 install されているバイナリの `--version` 出力」で、 `pnpm-lock.yaml` から resolved version を引いてくる場合と等価な強度の OS 中立 version 文字列が得られる。 lockfile drift ( lockfile が更新されたのに `pnpm install` を忘れた) の検出は本来 pnpm 自身の責務で、 利用者の CI / 開発フロー側で担保すべき ( ADR-0006 における buf の議論と同じ構造)。

**O3. 「lockfile を SSoT」 vs 「runtime --version を SSoT」 のトレードオフ**

外部公開パッケージで lockfile-based ( pnpm-external 想定) を選ぶと preflight ( lockfile vs install 整合) が必須になる。 一方 runtime `--version` を取れば preflight 不要 ( runtime バイナリそのものが SSoT なので構造的にズレない、 architecture.md の preflight 要否表を参照)。 lazygen の責務は **cache 健全性** であって **依存管理ツール ( pnpm) の運用 lint** ではない、 という ADR-0006 の責務境界に従えば、 後者を選ぶのが一貫する。

**O4. 内製ソース resolver は別の責務を持つので残す**

`go-local` / `pnpm-local` は SemVer を持たない repo 内ソースを「ソースファイル集合の hash」で表現するためのもの。 これは prebuilt binary の `--version` では代替できない ( binary が存在しない / build 必須 / 開発中で日々 source が動く)。 つまり内製ソース resolver は script で吸収できず、 独立 channel として残す必要がある。 本 ADR が削るのはあくまで「**外部公開パッケージ専用**」 resolver。

### esbuild Go API 採用 ( pnpm-local 用 SourceLister) の代替検討

pnpm-local の標準 SourceLister は Go binary 単体で完結する必要がある ( architecture.md の `SourceLister` 共通挙動)。 評価した代替:

- **esbuild Go API ( `github.com/evanw/esbuild/pkg/api`)** ( 採用): Go 製 bundler。 `import` で in-process 呼び出し、 subprocess 起動なし、 lazygen バイナリへの追加は数 MB。 `go.mod` で version pin できるため OS 横断キャッシュ共有を破らない
- **TypeScript Compiler API (tsc)**: Node.js runtime 必須。 lazygen バイナリ単体完結が崩れ、 開発者環境ごとに Node version が違うと別問題が出る
- **swc / oxc ( Rust 製)**: Go から呼ぶには別 binary 起動か wasm 経由が必要。 Subprocess 起動コスト + バイナリ配布の二重化
- **tree-sitter ( Go binding あり)**: import 抽出はできるが TypeScript の path mapping / `tsconfig.json` / workspace `workspace:*` 解決は自前で書く必要があり、 esbuild が無料で提供する範囲を全部再実装することになる
- **rollup / Babel / sucrase**: いずれも Node-only

esbuild 以外は subprocess または impl コストが許容できない。 esbuild が解析できないパターン ( runtime `require` / 動的 `import()` 等) のための retreat path は別途 `globLister` ( 既存) を `SourceLister` の差し替え先として用意してあるため、 死角も塞げる ( resolver-pnpm-local.md 参照)。

## Decision

### D1. lazygen は go-external / pnpm-external resolver を持たない

`internal/lazygen/toolresolver/goexternal` も `internal/lazygen/toolresolver/pnpmexternal` も導入しない。 spec の `tools:` にも `go-external:` / `pnpm-external:` 形式の declared 種別を追加しない。 対応する preflight checker も持たない。

### D2. 外部公開ツールは script resolver で個別に declare する

```yaml
# Go OSS ツール (aqua / go install / go tool ディレクティブ等で配布) と
# npm OSS ツール (pnpm install で取り込むもの) を named tool として登録 (ADR-0008)
tools:
  buf:
    exec: ["buf", "--version"]
  protoc-gen-go:
    exec: ["protoc-gen-go", "--version"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
  my-codegen:
    exec: ["pnpm", "exec", "my-codegen", "--version"]
```

各 task は `tools: [buf, protoc-gen-go]` のように **名前で参照** する。

これにより:

- 「runtime のバイナリが SSoT」原則 ( ADR-0005 / resolver-script.md) が保たれる
- preflight は構造的に不要になる ( 実 install されたバイナリの `--version` が取れるなら、 lockfile vs install drift は SSoT を runtime に置いた時点で発生しない)

### D3. 内製ソース resolver ( go-local / pnpm-local) は維持する

外部依存とは責務が異なる ( O4)。 これらは本 ADR の対象外。

## Rationale

### Responsibility boundary ( ADR-0006 と同じ論)

lazygen の core 責務は「OS 横断 / 共有可能な cache を、 generator inputs / outputs / tools の同一性に基づいて管理する」 こと ( ADR-0001 / ADR-0002)。 lockfile drift の検出は依存管理ツール ( go / pnpm) 自体や利用者 CI の責務で、 lazygen が機械的に強制するのは越権。

### Less is more ( ADR-0006 と同じ論)

go-external / pnpm-external 専用 resolver を導入すると、

- pnpm-lock.yaml schema ( v6 / v7 / v9 で構造が違う) の追従コスト
- go.mod 系の `replace` / `exclude` / private module の取扱い
- preflight 失敗時の UX 設計 ( 同一不整合に対する 2 種類のエラーメッセージ整合)

といった API surface 負債が発生する。 これらは本 ADR の決定で全て不要になる。

### 一貫性

ADR-0006 で buf を special-case しないと決めた延長線上にあり、 「lazygen は **lockfile-based 専用 resolver を持たない**」 一貫した立場が取れる ( 内製ソース resolver の `go.sum` 取り込みは内製 Go ツール解析の副産物に過ぎず、 外部依存単独を hash する目的ではない)。

## Consequences

### 正の影響

- lazygen 本体に Go module / pnpm-lock の schema 知識が入らず、 schema 変更の追従コストがゼロ
- preflight の対象が **内製ソース resolver のみ** ( pnpm-local の dist build 整合) に絞られ、 全体構成がシンプル
- spec 宣言が「外部か内製かに関係なく `exec` + 必要なら内製ソース resolver」 で統一される
- ADR-0005 の declared-only 原則と整合 ( 暗黙の lockfile parse がゼロ)

### 負の影響 / 注意点

- `pnpm install` を忘れて lockfile drift した状態でも、 lazygen は runtime `--version` ベースで版を確定するため、 「stale な install の方が SSoT になる」現象は理論上起き得る。 ただし `pnpm exec` は install されていない bin を実行できないので、 実用上は最初の `pnpm exec ... --version` 起動時に失敗する ( = 早期 fail と等価)
- 外部 OSS ツールが `--version` を持たない / `--version` 出力に build timestamp / OS-arch を含む edge case では script resolver の `extract` regex で正規化が必要。 これは resolver-script.md の既存スコープ
- 「外部依存を 1 行で全部 hash したい」要件 ( 大量の依存の version 一覧を一括取得) は満たせない。 必要なら利用者側で `tools:` map を機械生成する逃げ道が残る ( ADR-0008 で tool が named entity となったので、 codegen-of-spec 系の自動化と相性が良い)

### 将来再考の余地

実際の運用で「script `--version` 経路の煩雑さ」「外部依存数が多すぎて declared が爆発する」 等の問題が顕在化した場合、 opt-in の `bulk-from-lockfile:` 形式 (= 利用者が明示宣言したパッケージ群を lockfile から一括 resolve する補助 resolver) を別 ADR で再検討する余地はある。 ただし default では導入せず、 declared-only 原則と responsibility boundary を守る形に留める。
