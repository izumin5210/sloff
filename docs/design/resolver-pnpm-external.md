# Resolver: pnpm-external

`pnpmExternalResolver` は pnpm で配布される **外部公開 npm パッケージ** ( npm registry / GitHub Packages 等) の論理 version を解決する。

関連:
- [Architecture](./architecture.md)
- [Resolver: pnpm-local](./resolver-pnpm-local.md) ( workspace 内 内製パッケージ側の対応物)
- [Resolver: script](./resolver-script.md) ( prebuilt binary 全般 — 同じ「外部配布 / runtime と整合する logical version」のもう一つの解として、 lockfile 不要バージョン)

## Context

pnpm は `pnpm-lock.yaml` に依存パッケージの resolved version を完全 pinned で記録する。 npm registry や GitHub Packages 等から配布される外部公開パッケージは、 同じ lockfile から install すれば全環境で同じ実体が手に入る ( pnpm が SHA512 / integrity hash で検証する)。 そのため lockfile の resolved version を **論理 version 文字列** として使える。

「外部配布 ( = `external`)」という意味では [script resolver](./resolver-script.md) と並び立つ位置だが、 pnpm 配布物は実行時に Node が間に挟まり、 binary の `--version` を当てにできるとは限らない ( workspace 内 entry script が直接 import する形が多い)。 そのため pnpm ecosystem では lockfile を SSoT にする方が素直で、 lockfile vs `node_modules` の整合は preflight で検証する。

## Resolver の動作

### 取得元

`pnpm-lock.yaml` の `packages` セクションを読む。 例:

```yaml
# pnpm-lock.yaml ( 抜粋)
packages:
  /@graphql-codegen/cli@5.0.2:
    resolution: { integrity: sha512-... }
    ...
  /@bufbuild/protoc-gen-es@1.10.0:
    resolution: { integrity: sha512-... }
    ...
```

importer ( workspace package) の `dependencies` / `devDependencies` から package 名と resolved version を辿る。

### 論理 version 文字列の形式

```
"pnpm-external:<package-name>@<version>"
```

例: `"pnpm-external:@graphql-codegen/cli@5.0.2"`

scoped package (`@scope/name`) も非 scoped package (`some-cli`) も同じ形式で扱う。

### CanResolve / dispatch

`cmd[0]` が `pnpm exec <name>` / `npx <name>` / `node ./node_modules/.bin/<name>` のいずれかで、 `<name>` が `pnpm-lock.yaml` の **外部公開パッケージ** ( `workspace:*` 参照ではない) として解決できれば true。 workspace 内パッケージは [pnpmLocalResolver](./resolver-pnpm-local.md) の領域。

### Resolver 実装イメージ

```go
package pnpmexternal

import (
    "context"

    "github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
)

type Resolver struct {
    lockfile *pnpmLockfile
}

func New(repoRoot string) (toolresolver.Resolver, error) {
    lf, err := loadPnpmLockfile(repoRoot)
    if err != nil {
        return nil, err
    }
    return &Resolver{lockfile: lf}, nil
}

func (r *Resolver) Name() string { return "pnpm-external" }

func (r *Resolver) CanResolve(specDir string, cmd []string) bool {
    bin, ok := extractBinaryName(cmd) // pnpm exec / npx / node_modules/.bin/ から抽出
    if !ok {
        return false
    }
    pkg := r.lockfile.lookupBinary(specDir, bin)
    return pkg != nil && !pkg.IsWorkspace
}

func (r *Resolver) Resolve(ctx context.Context, specDir string, cmd []string, declaredKey string) ([]toolresolver.ToolVersion, error) {
    var pkg *pnpmPackage
    if declaredKey != "" {
        pkg = r.lockfile.lookupByName(specDir, declaredKey)
    } else {
        bin, _ := extractBinaryName(cmd)
        pkg = r.lockfile.lookupBinary(specDir, bin)
    }
    if pkg == nil || pkg.IsWorkspace {
        return nil, errPackageNotFound
    }
    return []toolresolver.ToolVersion{{
        Name:    pkg.Name,
        Version: "pnpm-external:" + pkg.Name + "@" + pkg.Version,
        Source:  "pnpm-lock.yaml",
    }}, nil
}
```

## Preflight Checker

`pnpmExternalChecker` は `pnpm-lock.yaml` の宣言と `node_modules/` の actual install 状態の整合性を検証する。

### 検証内容

`pnpm-lock.yaml` の `packages` 全 entry と `node_modules/.modules.yaml` の `packages` ( および各 `node_modules/<pkg>/package.json` の version) が一致するかを確認する。

不整合パターン:

- `pnpm-lock.yaml` を更新したが `pnpm install` 未実行 → `node_modules/.modules.yaml` が古いまま
- 別の lockfile から install された痕跡が残っている → `node_modules/.modules.yaml` の `lockfileChecksum` が現在の `pnpm-lock.yaml` と一致しない

### 不整合検出時

lazygen を即時 fail させ、 stderr に以下を表示:

```
pnpm-external: pnpm-lock.yaml と node_modules が一致しません
  please run: pnpm install --frozen-lockfile
```

### Preflight 実装イメージ

```go
package pnpmexternal

import "github.com/izumin5210/lazygen/internal/lazygen/preflight"

type Checker struct {
    lockfile *pnpmLockfile
    nodeMods *nodeModulesManifest // node_modules/.modules.yaml
}

func (c *Checker) Check(ctx context.Context, specDir string) (preflight.Result, error) {
    if c.nodeMods.LockfileChecksum != c.lockfile.checksum() {
        return preflight.Result{
            OK: false,
            Issues: []preflight.Issue{{
                Channel:    "pnpm-external",
                Detail:     "node_modules/.modules.yaml の lockfileChecksum が pnpm-lock.yaml と一致しません",
                Suggestion: "pnpm install --frozen-lockfile",
            }},
        }, nil
    }
    // 各 package の version 個別検証は省略 ( lockfileChecksum で十分)
    return preflight.Result{OK: true}, nil
}
```

## SourceLister

外部公開パッケージ ( SemVer pinned + integrity hash 検証) を扱うため、 `SourceLister` は使わない ( ソース hash の対象外)。

workspace 内 内製パッケージは [pnpmLocalResolver](./resolver-pnpm-local.md) で別途 SourceLister を持つ。

## Open Questions

- `pnpm install --frozen-lockfile` ではなく `pnpm install` で lockfile が更新された場合の検出 ( CI では `--frozen-lockfile` 強制が前提だがローカルは緩い)
- monorepo の hoisting 設定 ( `node-linker: hoisted` 等) で binary が `<root>/node_modules/.bin/` に置かれる場合の解決経路
