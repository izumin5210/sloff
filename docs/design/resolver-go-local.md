# Resolver: go-local

`goLocalResolver` はリポジトリ内に実装された **内製 Go CLI** (`go run ./cmd/...` 形式や repo local main package) の論理 version を解決する。

関連:
- [Architecture](./architecture.md)
- [Resolver: script](./resolver-script.md) ( 外部配布 Go ツール側は script resolver で `<bin> --version` を取る)
- [Resolver: pnpm-local](./resolver-pnpm-local.md) ( pnpm 側の対応物 = workspace 内 内製パッケージ)

## Context

Go の generator は外部配布 module (`go.mod` の `tool` ディレクティブで宣言される SemVer pinned ツール) と、 リポジトリ内 main package として実装された内製ツール (内製 protoc plugin、 内製 codegen 等) の 2 種類に分かれる。 後者は SemVer を持たないため、 **ツールを構成するソースファイル全集合の hash** を論理 version 文字列として用いる。

「内製ソース ( = `local`)」という意味では [pnpm-local](./resolver-pnpm-local.md) の対応物。 「Go ecosystem の repo local main package」と「pnpm ecosystem の workspace 内 local package」が概念的に対をなす。 外部配布 ( aqua / `go tool` 経由) のツールは [script resolver](./resolver-script.md) で `<bin> --version` を直接取るアプローチに統一されている。

ソースファイル列挙には Resolver 内部 helper の `SourceLister` を使う。 標準実装は **`golang.org/x/tools/go/packages` の Go API を直接 import した `goPackagesLister`** で、 entry main package から transitive な依存解析で関係する `.go` ファイルのみを抽出する。

## Resolver の動作

### 取得元

1. cmd から main package の import path を判定 (`go run ./cmd/foo/...` なら `./cmd/foo/...`、 build 済み binary なら spec で明示宣言)
2. 内部 `SourceLister` ( デフォルト `goPackagesLister`) で transitive 依存ファイル全集合を取得
3. 内部コード / 外部パッケージで戦略を分けて hash ( 後述)

### 論理 version 文字列の形式

```
"go-local:<main-package-path>@sha256:<hex>"
```

例: `"go-local:./cmd/protoc-gen-foo@sha256:abcd1234..."`

### Dispatch ( declared-only)

go-local resolver は spec の `tools: [{go-local: <import-path>}]` で明示宣言された場合にのみ起動する ([ADR-0005](../adr/0005-eliminate-resolver-auto-dispatch.md))。

- `tools: [{go-local: ./cmd/protoc-gen-foo}]` のように entry を明示する。 entry は必ず `./` で始まる spec dir 相対の import path
- `cmd: ["go", "run", "./cmd/protoc-gen-foo"]` のように `go run` で起動する場合も、 上記の宣言を併記しない限り go-local は動かない ( cmd 形状からの auto-dispatch は持たない)
- build 済み binary を直接呼ぶケース ( `cmd: protoc-gen-foo`) も同様に declared を併記する
- 同じ cmd で go-local 以外の resolver も使いたい場合 ( 例: Go toolchain 自体の bump も captureしたい) は `tools:` に複数 entry を書く: `tools: [{go-local: ./cmd/protoc-gen-foo}, {exec: ["go", "version"], extract: '...'}]`

### Resolver 実装イメージ

```go
package golocal

import (
    "context"
    "encoding/hex"
    "errors"

    "github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
    "github.com/izumin5210/lazygen/internal/lazygen/toolresolver/lister"
)

type Resolver struct {
    repoRoot string
    lister   lister.SourceLister // DI: 標準は goPackagesLister
}

func New(repoRoot string, l lister.SourceLister) toolresolver.Resolver {
    return &Resolver{repoRoot: repoRoot, lister: l}
}

func (r *Resolver) Name() string { return "go-local" }

func (r *Resolver) Resolve(ctx context.Context, specDir string, cmd []string, declared *toolresolver.DeclaredTool) ([]toolresolver.ToolVersion, error) {
    if declared == nil || declared.Entry == "" {
        return nil, errors.New("go-local: declared entry is required")
    }
    entry := declared.Entry // spec dir 相対 ( "./cmd/foo")
    // SourceLister は specDir を受け取り <repoRoot>/<specDir> を作業ディレクトリとして
    // entry を評価する。 こうすることで複数 Go module を含む monorepo でも、
    // spec の隣にある go.mod を参照して `go run ./cmd/foo` と同じ解決ができる。
    files, err := r.lister.List(ctx, specDir, entry)
    if err != nil {
        return nil, err
    }
    sum := hashFiles(files) // 内部/外部分離戦略で計算 ( 後述)
    return []toolresolver.ToolVersion{{
        Name:    entry,
        Version: "go-local:" + entry + "@sha256:" + hex.EncodeToString(sum),
        Source:  "go-local:" + entry,
    }}, nil
}
```

## SourceLister: `goPackagesLister` (`go/packages` を直接 import)

`goLocalResolver` の標準実装は `goPackagesLister`。 **`golang.org/x/tools/go/packages` パッケージを Go API で直接 import** して in-process で呼び出す。 lazygen バイナリのメモリ空間内で完結し、 `go list` を subprocess で起動するオーバーヘッドがない。

```go
import "golang.org/x/tools/go/packages"

cfg := &packages.Config{
    // NeedEmbedFiles で //go:embed 対象を、 IgnoredFiles ( NeedFiles で取得) で
    // 別 GOOS/GOARCH 用 build-tag ファイルを hash 入力に取り込む。 これらが無いと
    // embed アセット変更や OS 違いで cache 健全性 / OS 横断キャッシュが破れる。
    Mode: packages.NeedFiles | packages.NeedEmbedFiles |
        packages.NeedImports | packages.NeedDeps | packages.NeedModule,
    // spec の作業ディレクトリ ( <repoRoot>/<specDir>) を Dir にする。
    // 多 Go module の monorepo で spec の隣に go.mod がある構成でも正しく解決される。
    Dir:  filepath.Join(repoRoot, specDir),
}
pkgs, err := packages.Load(cfg, "./cmd/protoc-gen-foo/...")
```

これは `go list -deps -json` と同等の情報を返す Go 公式 API ( 内部的には `go list` と同じ機構を使うが、 同一プロセス内のライブラリ呼び出しとして動く)。

探索範囲を **per-task の対象 main package とその transitive 依存** に限定する (`./cmd/protoc-gen-foo/...` の形)。 monorepo 全体に対する解析 (`./...`) は CLI 計測で約 7.5 秒 (`3.10s user 5.38s system 112% cpu 7.526 total`) と重く、 task ごとに必要な範囲を遥かに超えるため使わない。

### hash 計算は内部コードと外部パッケージで戦略を分ける

transitive 依存には「リポジトリ内の `.go` ファイル」と「`$GOMODCACHE` 配下の外部 package のソース」の 2 種類が含まれる。 両者を一律にファイル本体 SHA256 すると、 外部 module の minor bump で何百ものファイルを再 hash することになり性能が悪い。 また go.mod 全体の transitive 変更 ( 例: 一般的な ライブラリ依存 module の patch bump) を「該当 module 内の全 .go ファイル」を読んで反映する必要もない ( go.sum で内容が暗号学的に保証されているため)。

そこで戦略を 2 つに分ける:

| 種別 | 判定 | hash 入力 |
|---|---|---|
| stdlib | `pkg.Module == nil` | hash 対象から除外 ( $GOROOT 絶対 path が OS 横断キャッシュを壊すため。 Go toolchain bump は別途 script resolver で `go version` を併記して捕捉する) |
| 内部コード | `pkg.Module.Main` ( 自リポジトリの module) | `pkg.GoFiles` + `pkg.EmbedFiles` + `pkg.IgnoredFiles` + `pkg.OtherFiles` のファイル本体を SHA256 ( `IgnoredFiles` で GOOS / GOARCH / build-tag に非依存、 `OtherFiles` で `.s` / `.c` / `.cc` / `.syso` 等の非 Go ソース変更も捕捉) |
| 外部パッケージ | `pkg.Module` が外部 module を指す | `module path@version` 文字列 + go.sum 該当行の hash ( replace ディレクティブも外部扱い)。 go.sum は **load された main module の `Module.GoMod` の隣** から読む ( nested-module monorepo で repo root の go.sum を引かないため) |

```go
for each pkg in transitive(pkgs):
    if pkg.Module == nil {
        // stdlib: ファイル本体は hash しない ( $GOROOT 配下の絶対 path が
        // OS 横断キャッシュを破壊するため)。 Go toolchain bump は
        // tools: [{exec: ["go", "version"], extract: ...}] を併記して捕捉する。
    } else if pkg.Module.Main {
        // 内部コード: ファイル本体を hash
        // GoFiles + EmbedFiles ( //go:embed 対象) + IgnoredFiles
        // ( 別 GOOS / build-tag のため現 build context で除外されたソース)
        // を全部含めて、 host 環境非依存に hash する。 _test.go は除外。
        for f in pkg.GoFiles + pkg.EmbedFiles + pkg.IgnoredFiles {
            hash.Write(readFile(f))
        }
    } else {
        // 外部パッケージ: version + go.sum 該当行で代用
        hash.Write([]byte(pkg.Module.Path + "@" + pkg.Module.Version))
        hash.Write([]byte(lookupGoSum(pkg.Module.Path, pkg.Module.Version)))
    }
```

### 利点

- 外部 module の patch bump → version 文字列が変わる → 自動 invalidate ( ファイル本体を読まなくても version 比較で済む)
- 同 version なら中身を読まないので高速 ( `$GOMODCACHE` は数 GB 規模になりうる、 全 .go を SHA256 すると重い)
- go.sum で「version → 中身」が暗号学的に保証されているため、 「version + go.sum hash」が「中身全 hash」と等価な強度を持つ
- 内部コードは細かい変更にも敏感 ( `_test.go` 除外以外は普通の hash 戦略)
- 内部コードに `IgnoredFiles` を含めることで、 host の GOOS / GOARCH / build-tag 状態に依存せず同一 hash になる ( 別 OS の CI で計算しても同じ digest)
- stdlib は hash から除外。 Go toolchain bump を捕捉したい場合は spec 側で `tools: [{exec: ["go", "version"], extract: 'go[0-9]+\.[0-9]+(?:\.[0-9]+)?'}]` のような script resolver 宣言を併記する

### `globLister` への retreat

`go/packages` で正しく取れない構造の Go プロジェクトに対応する場合は、 該当 `goLocalResolver` の `SourceLister` を `globLister` に切り替える:

```go
import "github.com/izumin5210/lazygen/internal/lazygen/toolresolver/lister"

resolver := golocal.New(lister.NewGlob([]string{"**/*.go"}, []string{"**/*_test.go"}))
```

これで該当パッケージのみ「精度は下がるが死角ゼロ」運用に切り替わる。 影響範囲は Resolver 単位なので局所化される。

### `SourceLister` 共通の挙動

[Architecture > SourceLister 共通の挙動 / 利点](./architecture.md#sourcelister-共通の挙動--利点) を参照。 メモ化 / OS 非依存 / lazygen バイナリ単体完結等の共通機能。

## Preflight Checker

`goLocalChecker` は `go/packages` での解析が完走するかで install 状態を間接的に検証する。

### 検証内容

`go list -deps -json ./<main_package>/...` ( in-process で `packages.Load`) がエラー無く完走するかを確認する。 transitive 依存が `$GOMODCACHE` に存在しないと `packages.Load` がエラーを返すため、 これを代理シグナルにする。 script resolver は preflight を持たないため、 Go toolchain 自体の install 状態 ( e.g., `go` が `$PATH` 上に居るか) は本 Checker と script resolver 実行時のエラーで間接検出する。

### 不整合検出時

lazygen を即時 fail させ、 stderr に以下を表示:

```
go-local: ./cmd/protoc-gen-foo の transitive 依存解析に失敗
  no required module provides package ...
  please run: go mod download
```

`packages.Load` を呼ぶことそのものが実 build 経路 ( `$GOMODCACHE` 整備 + module graph 解決) の存在確認になっており、 別途 preflight Checker を立てる必要は無い。

## Open Questions

- ~~`go run` 形式 ( CLI から呼ぶたびに `go build` する) と build 済み binary 形式の使い分け。 spec で明示宣言する形を採るか、 CLI 形式を auto-detect するか~~ → [ADR-0005](../adr/0005-eliminate-resolver-auto-dispatch.md) で declared-only に統一済み ( 両形式とも spec での明示宣言を必須とする)
- ~~transitive 依存に `replace` ディレクティブで local 置換された module が混じった場合の扱い ( 内部コード扱いにする / 外部扱いにする)~~ → 外部扱いに確定 ( replace 先のファイル本体は再読しない、 `Replace.Version` または `replace=<path>` ラベルで version diversity を表現)
- 内製 protoc plugin が `go.mod` の `internal/...` パッケージに依存する場合の subset hash 戦略
- `go.work` で複数 repo-local module を束ねる構成は **現状サポート外**。 transitive 依存が複数 main module にまたがると、 sibling module の go.sum lookup を取りこぼし得るため、 lister は複数の `Module.Main` を検出した時点で fail する。 必要になった段階で「全 main module の go.sum を結合する」拡張を ADR で起こす
