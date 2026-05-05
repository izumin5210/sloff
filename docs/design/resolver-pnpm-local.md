# Resolver: pnpm-local

`pnpmLocalResolver` は pnpm workspace 内に実装された **内製 js/ts ツール** ( `workspace:*` 参照) の論理 version を解決する。

関連:
- [Architecture](./architecture.md)
- [Resolver: pnpm-external](./resolver-pnpm-external.md) ( 外部公開パッケージ側の対応物)
- [Resolver: go-local](./resolver-go-local.md) ( Go 側の対応物 = 内製 Go CLI)

## Context

pnpm workspace では複数パッケージを 1 リポジトリに同居させ、 `workspace:*` 指定で相互依存できる。 ここで配布される内製 codegen ツール (`@org/my-codegen` 等の workspace local package) は npm registry に公開されておらず、 SemVer による論理 version を持たない。 そのため **ツールを構成するソースファイル全集合の SHA256** を論理 version 文字列として用いる。

「内製ソース ( = `local`)」という意味では [go-local](./resolver-go-local.md) の対応物。 「pnpm ecosystem の workspace 内 local package」と「Go ecosystem の repo local main package」が概念的に対をなす。

ソースファイル列挙には Resolver 内部 helper の `SourceLister` を使う。 標準実装は **esbuild の Go API を直接 import した `esbuildLister`** で、 entry point から transitive な import 解析で「実際に bundle されるファイル」のみを抽出する。

## Resolver の動作

### 取得元

1. `pnpm-lock.yaml` で当該パッケージが `workspace:*` 参照であるかを判定
2. 該当パッケージの `package.json` から `bin` / `main` を読み、 entry point を確定
3. 内部 `SourceLister` ( デフォルト `esbuildLister`) で entry point から transitive な依存ファイル全集合を取得
4. 取得したファイルを path 昇順で SHA256

### 論理 version 文字列の形式

```
"pnpm-local:<package-name>@sha256:<hex>"
```

例: `"pnpm-local:@org/my-codegen@sha256:abcd1234..."`

### CanResolve / dispatch

`cmd[0]` が `pnpm exec <name>` / `node ./packages/foo/dist/main.js` のいずれかで、 `<name>` が `pnpm-lock.yaml` の **`workspace:*` 参照パッケージ** として解決できれば true。

### Resolver 実装イメージ

```go
package pnpmlocal

import (
    "context"
    "encoding/hex"

    "github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
    "github.com/izumin5210/lazygen/internal/lazygen/toolresolver/lister"
)

type Resolver struct {
    lockfile *pnpmLockfile
    lister   lister.SourceLister // DI: 標準は esbuildLister
}

func New(repoRoot string, l lister.SourceLister) (toolresolver.Resolver, error) {
    lf, err := loadPnpmLockfile(repoRoot)
    if err != nil {
        return nil, err
    }
    return &Resolver{lockfile: lf, lister: l}, nil
}

func (r *Resolver) Name() string { return "pnpm-local" }

func (r *Resolver) CanResolve(specDir string, cmd []string) bool {
    bin, ok := extractBinaryName(cmd)
    if !ok {
        return false
    }
    pkg := r.lockfile.lookupBinary(specDir, bin)
    return pkg != nil && pkg.IsWorkspace
}

func (r *Resolver) Resolve(ctx context.Context, specDir string, cmd []string, declaredKey string) ([]toolresolver.ToolVersion, error) {
    var pkg *workspacePackage
    if declaredKey != "" {
        pkg = r.lockfile.lookupWorkspaceByName(declaredKey)
    } else {
        bin, _ := extractBinaryName(cmd)
        pkg = r.lockfile.lookupWorkspaceByBinary(specDir, bin)
    }
    entry := pkg.entryPoint() // package.json の bin / main
    files, err := r.lister.List(ctx, entry)
    if err != nil {
        return nil, err
    }
    sum := hashFiles(files) // path 昇順で SHA256
    return []toolresolver.ToolVersion{{
        Name:    pkg.Name,
        Version: "pnpm-local:" + pkg.Name + "@sha256:" + hex.EncodeToString(sum),
        Source:  "pnpm-local:" + pkg.Name,
    }}, nil
}
```

## SourceLister: `esbuildLister` ( esbuild を Go API で直接 import)

`pnpmLocalResolver` の標準実装は `esbuildLister`。 **esbuild ( Go 製) の Go API `github.com/evanw/esbuild/pkg/api` を直接 import** して in-process で呼び出す。 esbuild は Evan Wallace 作の Go 製 bundler で、 ライブラリとして lazygen バイナリに組み込み可能。

```go
import "github.com/evanw/esbuild/pkg/api"

result := api.Build(api.BuildOptions{
    EntryPoints: []string{"./packages/foo/src/main.ts"},
    Bundle:      true,
    Platform:    api.PlatformNode,
    Metafile:    true,
    Write:       false, // 実際の build artifact は書き出さない
})
// result.Metafile (JSON 文字列) を parse して inputs を取得 → path 昇順で SHA256
```

### 利点

- **esbuild バイナリの別途 install が不要**。 `go.mod` の依存として静的リンクされるため、 aqua / pnpm 経由で esbuild を配布 / preflight 検証する必要がない
- **subprocess 起動コストがゼロ** ( in-process 呼び出し)
- **esbuild バージョンは lazygen の `go.mod` で固定** されるため、 環境依存で挙動が変わるリスクがない ( 同じ lazygen バイナリなら全開発者で同じ esbuild バージョンが使われる)
- esbuild が「実際に bundle されるファイル」を解決するため、 `src/` 全ファイルを hash する粗い手法と異なり **実際に import されているファイルのみ** が対象
- workspace 内 transitive 依存 ( `workspace:*` 参照先) も import 経由で自動的に解決される ( esbuild が module 解決を担う)
- TypeScript の type-only import は esbuild がデフォルトで除外 ( 実行時に必要なものだけ)
- JSX / CSS modules / dynamic import など edge case も esbuild 側がハンドリング

### trade-off

- esbuild が静的解析できないパターン ( eval / runtime `require` / 動的 `import()` / 動的 module 解決) は **`esbuildLister` では明確に非サポート**。 部分的な解析結果を hash 入力にすると、 解析の死角に入った依存変更が invalidate されず cache の嘘につながるため。 こうした内製ツールを扱う必要が生じた場合は、 該当 `pnpmLocalResolver` の `SourceLister` を `globLister` に切り替えて運用する ( DI で構成可能)。 「精度低下を受け入れる代わりに死角ゼロ」というトレードオフを開発者が選べる
- per-task で esbuild build 処理コストが発生 ( 100 ms 〜 数百 ms)

### 将来の代替

SWC ( Rust 製、 Go から呼ぶには別 process / wasm 経由)、 TypeScript Compiler API ( `tsc` の Programmatic API、 Node.js が必要)、 rollup plugin など。 これらは Go から in-process 呼び出すには別 runtime が必要なため、 初版は esbuild 一択。 すべて `SourceLister` interface に従って差し替え可能 ( Resolver 内部の DI で切替)。

### `globLister` への retreat

esbuild 解析できないパターンを含む内製ツールに対応する場合は、 該当 `pnpmLocalResolver` の `SourceLister` を `globLister` に切り替える:

```go
import "github.com/izumin5210/lazygen/internal/lazygen/toolresolver/lister"

resolver, _ := pnpmlocal.New(repoRoot, lister.NewGlob([]string{"**/*.{ts,tsx,js,json}"}, []string{"**/*.test.ts", "**/*.spec.ts", "dist/**", "node_modules/**"}))
```

これで該当パッケージのみ「精度は下がるが死角ゼロ」運用に切り替わる。 影響範囲は Resolver 単位なので局所化される。

## Preflight Checker

`pnpmLocalChecker` は workspace package が build 済みかを検証する。

### 検証内容

build 必須なツールは `dist/` の存在 + `src/` の最新更新時刻との整合性で「build 結果が src より新しいか」を確認する。 build 必須でないツール ( ts-node / tsx で直接 source 実行) は検証スキップ。

不整合パターン:

- `dist/` が存在しない → build 未実行
- `src/` のファイル mtime が `dist/` より新しい → src 変更後に build していない

### 不整合検出時

lazygen を即時 fail させ、 stderr に以下を表示:

```
pnpm-local: @org/my-codegen の dist/ が src/ より古いか存在しません
  please run: pnpm --filter @org/my-codegen build
```

### Preflight 実装イメージ

```go
package pnpmlocal

import "github.com/izumin5210/lazygen/internal/lazygen/preflight"

type Checker struct {
    lockfile *pnpmLockfile
}

func (c *Checker) Check(ctx context.Context, specDir string) (preflight.Result, error) {
    var issues []preflight.Issue
    for _, pkg := range c.lockfile.workspacePackages() {
        if !pkg.RequiresBuild {
            continue
        }
        if !pkg.distExists() || pkg.srcNewerThanDist() {
            issues = append(issues, preflight.Issue{
                Channel:    "pnpm-local",
                Detail:     fmt.Sprintf("%s: dist/ が src/ より古い or 存在しない", pkg.Name),
                Suggestion: fmt.Sprintf("pnpm --filter %s build", pkg.Name),
            })
        }
    }
    return preflight.Result{OK: len(issues) == 0, Issues: issues}, nil
}
```

build 必須かどうかは `package.json` の `bin` が `dist/...` を指していれば必要、 `src/...` を直接指していれば不要、 と判定する ( ts-node / tsx 想定)。

## Open Questions

- 内製 ts ツールが build 不要 ( ts-node / tsx で直接 src を実行) な場合の判定精度
- 複数 entry point ( `bin` が複数定義) を持つパッケージの hash 計算 ( 全 entry の union を hash)
- esbuild の `tsconfig.json` パス解決 ( 親階層を遡る挙動) が大きな monorepo で正しく動くか
