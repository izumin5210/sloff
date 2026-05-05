# Resolver: go-external

`goExternalResolver` は Go の `go.mod` `tool` ディレクティブ ( Go 1.24 以降) で宣言された **外部配布 Go ツール** の論理 version を解決する。

関連:
- [Architecture](./architecture.md)
- [Resolver: go-local](./resolver-go-local.md) ( 内製 Go CLI 側の対応物)
- [Resolver: pnpm-external](./resolver-pnpm-external.md) ( pnpm 側の対応物 = 外部配布パッケージ)

## Context

Go 1.24 で導入された `tool` ディレクティブにより、 外部配布 Go ツール (公式 / サードパーティ製の codegen / mock / wire 等) を `go.mod` に宣言して `go tool <name>` で実行できる。 これらのツールは GOOS / GOARCH 別にビルドされるためバイナリ hash は OS 依存だが、 `go.mod` の `require` 行に書かれた **module path + SemVer** は OS 非依存な論理 version 文字列として使える。

「外部配布 ( = `external`)」という意味では [pnpm-external](./resolver-pnpm-external.md) の対応物。 「Go ecosystem の go.mod 経由」と「pnpm ecosystem の registry 経由」が概念的に対をなす。

## Resolver の動作

### 取得元

`go.mod` の `tool` ブロックと `require` 行を読む。 例:

```go
// go.mod
module github.com/example/myapp

go 1.26.0

tool (
    google.golang.org/protobuf/cmd/protoc-gen-go
    github.com/matryer/moq
)

require (
    google.golang.org/protobuf v1.34.2
    github.com/matryer/moq v0.5.0
)
```

`tool` ブロックの各 import path から、 対応する `require` 行を引いて module path + version を取得する。

### 論理 version 文字列の形式

```
"go-external:<module-path>@<version>"
```

例: `"go-external:google.golang.org/protobuf@v1.34.2"`

`<module-path>` は import path のうち module 単位の prefix ( `cmd/protoc-gen-go` を除いた部分)。 同じ module 配下に複数 tool が居る場合 ( 例: `cmd/protoc-gen-go` と `cmd/protoc-gen-go-grpc`) は version が同じになる ( module 単位で固定されているため)。

### CanResolve / dispatch

`cmd[0]` が `go tool <name>` 形式、 もしくは `tool` ディレクティブで宣言された binary 名と一致すれば `CanResolve` は true。 `go run <import-path>` 形式は `goLocalResolver` ( 内製 Go CLI) の領域で、 こちらは module 配下の tool ディレクティブのみ扱う。

### Resolver 実装イメージ

```go
package goexternal

import (
    "context"
    "path/filepath"

    "github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
    "golang.org/x/mod/modfile"
)

type Resolver struct {
    modFile *modfile.File
    tools   map[string]string // tool binary name → module path
}

func New(repoRoot string) (toolresolver.Resolver, error) {
    mf, err := loadGoMod(repoRoot)
    if err != nil {
        return nil, err
    }
    return &Resolver{
        modFile: mf,
        tools:   buildToolMap(mf),
    }, nil
}

func (r *Resolver) Name() string { return "go-external" }

func (r *Resolver) CanResolve(specDir string, cmd []string) bool {
    bin := filepath.Base(cmd[0])
    _, ok := r.tools[bin]
    return ok
}

func (r *Resolver) Resolve(ctx context.Context, specDir string, cmd []string, declaredKey string) ([]toolresolver.ToolVersion, error) {
    var importPath string
    if declaredKey != "" {
        importPath = declaredKey
    } else {
        importPath = r.tools[filepath.Base(cmd[0])]
    }
    modPath, version, err := r.lookupModuleVersion(importPath)
    if err != nil {
        return nil, err
    }
    return []toolresolver.ToolVersion{{
        Name:    importPath,
        Version: "go-external:" + modPath + "@" + version,
        Source:  "go.mod",
    }}, nil
}
```

## Preflight Checker

`goExternalChecker` は `go.mod` の宣言と `$GOMODCACHE` の actual install 状態の整合性を検証する。

### 検証内容

`go.mod` の `require` 行と、 `go list -m -json <module>` ( in-process で `golang.org/x/tools/go/packages` 経由) で取得できる actual resolved version が一致するかを確認する。

不整合パターン:

- `go.mod` に新 version が宣言されたが `go mod download` 未実行 → `$GOMODCACHE` に該当 module が未取得 → `go list` が missing を返す
- `go.sum` が `go.mod` と同期していない ( `go mod tidy` 漏れ) → ハッシュ不一致

### 不整合検出時

lazygen を即時 fail させ、 stderr に以下を表示:

```
go-external: go.mod の require と installed module が一致しません
  google.golang.org/protobuf: go.mod=v1.34.2, installed=missing
  please run: go mod download
```

### Preflight 実装イメージ

```go
package goexternal

import (
    "context"

    "github.com/izumin5210/lazygen/internal/lazygen/preflight"
    "golang.org/x/tools/go/packages"
)

type Checker struct {
    modFile *modfile.File
    tools   map[string]string
}

func (c *Checker) Check(ctx context.Context, specDir string) (preflight.Result, error) {
    cfg := &packages.Config{Mode: packages.NeedModule, Dir: c.modFile.repoRoot}
    var issues []preflight.Issue
    for _, modPath := range c.uniqueToolModules() {
        pkgs, err := packages.Load(cfg, modPath)
        if err != nil || len(pkgs) == 0 || pkgs[0].Module == nil {
            issues = append(issues, preflight.Issue{
                Channel:    "go-external",
                Detail:     fmt.Sprintf("%s: missing in $GOMODCACHE", modPath),
                Suggestion: "go mod download",
            })
            continue
        }
        declared := c.modFile.requireVersion(modPath)
        if pkgs[0].Module.Version != declared {
            issues = append(issues, preflight.Issue{
                Channel:    "go-external",
                Detail:     fmt.Sprintf("%s: go.mod=%s, installed=%s", modPath, declared, pkgs[0].Module.Version),
                Suggestion: "go mod download",
            })
        }
    }
    return preflight.Result{OK: len(issues) == 0, Issues: issues}, nil
}
```

## SourceLister

外部配布 module ( SemVer pinned) を扱うため、 `SourceLister` は使わない ( ソース hash の対象外)。

内製 Go CLI ( `go run ./cmd/...` 形式 / リポジトリ内 main package) は [goLocalResolver](./resolver-go-local.md) の領域。

## Open Questions

- `tool` ディレクティブを使わず `go run <import-path>` で外部ツールを呼ぶケースの扱い ( `tool` ディレクティブ強制とするか、 `require` 行から推論するか)
- `replace` ディレクティブで local 置換されている module の扱い ( 純粋な外部 module ではないため、 `goLocalResolver` 領域に委譲すべきか)
