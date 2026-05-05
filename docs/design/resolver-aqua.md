# Resolver: aqua

`aquaResolver` は [aqua](https://aquaproj.github.io/) パッケージマネージャで配布される OSS バイナリの論理 version を解決する。

関連:
- [Architecture](./architecture.md)
- [ADR-0001: キャッシュ可能コード生成オーケストレーターの選定](../adr/0001-cache-aware-codegen-orchestrator-decision.md)

## Context

aqua は YAML ベースの宣言的なパッケージマネージャで、 `aqua.yaml` に packages を pinned version で宣言し、 `aqua install` で `~/.aqua/` 配下にバイナリを配置する。 OSS の generator (`buf` / `xo` / `sqlc` / `tbls` 等) を aqua で配布しているチームが対象。

aqua バイナリは OS / arch 別 ( `darwin-arm64` / `linux-amd64` / `linux-arm64` 等) で実体が異なるため、 バイナリ本体の SHA256 を hash 入力にすると OS 横断で record が共有できない。 そこで **論理 version 文字列** を hash 入力にする。

## Resolver の動作

### 取得元

`aqua.yaml` の `packages[*]` 宣言を読む。 例:

```yaml
# aqua.yaml
packages:
  - name: bufbuild/buf@v1.30.0
  - name: kyleconroy/sqlc@v1.27.0
  - name: k1LoW/tbls@v1.94.5
```

### 論理 version 文字列の形式

aqua の package 識別子をそのまま使う:

```
"aqua:<owner>/<name>@<version>"
```

例: `"aqua:bufbuild/buf@v1.30.0"`

### CanResolve / dispatch

`cmd[0]` の base name から aqua の package binary を逆引きする:

- `aqua.yaml` の各 package を aqua registry で展開し、 配置される binary 名 ( 通常 `<name>` だが exception あり) を集める
- `cmd[0]` の base name が aqua 経由 binary に該当すれば `CanResolve` は true
- spec で `tools: [{aqua: <name>}]` と明示宣言されている場合は dispatch 順を skip して直接 lookup

### Resolver 実装イメージ

```go
package aqua

import (
    "context"
    "path/filepath"

    "github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
)

type Resolver struct {
    aquaConfig *aquaConfig // aqua.yaml の parse 結果
}

func New(repoRoot string) (toolresolver.Resolver, error) {
    cfg, err := loadAquaYAML(repoRoot)
    if err != nil {
        return nil, err
    }
    return &Resolver{aquaConfig: cfg}, nil
}

func (r *Resolver) Name() string { return "aqua" }

func (r *Resolver) CanResolve(specDir string, cmd []string) bool {
    bin := filepath.Base(cmd[0])
    _, ok := r.aquaConfig.lookupByBinary(bin)
    return ok
}

func (r *Resolver) Resolve(ctx context.Context, specDir string, cmd []string, declaredKey string) ([]toolresolver.ToolVersion, error) {
    var pkg *aquaPackage
    if declaredKey != "" {
        pkg = r.aquaConfig.lookupByName(declaredKey)
    } else {
        pkg = r.aquaConfig.lookupByBinary(filepath.Base(cmd[0]))
    }
    if pkg == nil {
        return nil, errPackageNotFound
    }
    return []toolresolver.ToolVersion{{
        Name:    pkg.Name,
        Version: "aqua:" + pkg.Name + "@" + pkg.Version,
        Source:  "aqua.yaml",
    }}, nil
}
```

## Preflight Checker

`aquaChecker` は `aqua.yaml` の宣言と installed 状態の整合性を検証する。

### 検証内容

`aqua.yaml` の `packages[*].{name, version}` 全件と、 `aqua install` 後に更新される **`aqua-checksums.json`** ( および `~/.aqua/global/...` の installed manifest) の version が完全一致するかを確認する。

不整合パターン:

- `aqua.yaml` に新 version が宣言されたが `aqua install` 未実行 → `aqua-checksums.json` が古いまま、 installed binary も古い
- `aqua.yaml` から package が削除されたが `~/.aqua/` に古い binary が残っている → これは fail にしない ( 不整合だが cache 汚染にはつながらない)

### 不整合検出時

lazygen を即時 fail させ、 stderr に以下を表示:

```
aqua: aqua.yaml に宣言された version と installed version が一致しません
  bufbuild/buf: aqua.yaml=v1.30.0, installed=v1.28.0
  please run: aqua install
```

`LAZYGEN_ALLOW_STALE_DEPS=1` で警告に降格できるが、 その場合 cache record は **書き込まない** ( read-only)。

### Preflight 実装イメージ

```go
package aqua

import (
    "context"

    "github.com/izumin5210/lazygen/internal/lazygen/preflight"
)

type Checker struct {
    aquaConfig *aquaConfig
}

func NewChecker(repoRoot string) (preflight.Checker, error) {
    cfg, err := loadAquaYAML(repoRoot)
    if err != nil {
        return nil, err
    }
    return &Checker{aquaConfig: cfg}, nil
}

func (c *Checker) Name() string { return "aqua" }

func (c *Checker) Check(ctx context.Context, specDir string) (preflight.Result, error) {
    checksums, err := loadAquaChecksums(c.aquaConfig.repoRoot)
    if err != nil {
        return preflight.Result{}, err
    }
    var issues []preflight.Issue
    for _, pkg := range c.aquaConfig.packages {
        installed := checksums.lookup(pkg.Name)
        if installed == "" || installed != pkg.Version {
            issues = append(issues, preflight.Issue{
                Channel:    "aqua",
                Detail:     fmt.Sprintf("%s: aqua.yaml=%s, installed=%s", pkg.Name, pkg.Version, installed),
                Suggestion: "aqua install",
            })
        }
    }
    return preflight.Result{OK: len(issues) == 0, Issues: issues}, nil
}
```

## SourceLister

外部配布物 ( SemVer pinned) を扱うため、 `SourceLister` は使わない ( ソース hash の対象外)。

## Open Questions

- aqua の `import` 機能 ( 別 yaml ファイル分割) や `registries` 機能を使った場合の解決経路。 初版は単一 `aqua.yaml` のみ対応とし、 必要が生じた段階で拡張
- aqua-installer のグローバルキャッシュ ( `~/.aqua/global/`) と repo local 設定の重ね合わせ時の優先順位
