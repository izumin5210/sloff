# Resolver: buf

`bufResolver` は `buf generate` 経由で実行される protoc plugin 群の論理 version を解決する複合 resolver。

関連:
- [Architecture](./architecture.md)
- [Resolver: script](./resolver-script.md), [go-local](./resolver-go-local.md), [pnpm-external](./resolver-pnpm-external.md), [pnpm-local](./resolver-pnpm-local.md) ( buf 経由で再帰的にディスパッチされる)

## Context

`buf generate` task は通常複数の plugin を組み合わせて実行する複合 cmd で、 `buf.gen.yaml` に plugin が宣言される。 plugin は 3 形式 ( local / protoc_builtin / remote) があり、 それぞれ解決経路が異なる。 [ADR-0005](../adr/0005-eliminate-resolver-auto-dispatch.md) により lazygen には auto-dispatch が無く、 `buf` 経路は `tools: [{buf: <buf.gen.yaml の相対パス>}]` のような明示宣言で起動する想定。 `bufResolver` は宣言を受けて `buf.gen.yaml` を静的 parse し、 buf 本体 + 各 plugin の version を resolve する。

## Resolver の動作

### plugin type 別解決ルール

| plugin type ( buf.gen.yaml v2) | 例 | 解決経路 |
|---|---|---|
| `local: <name>` | `local: protoc-gen-go` | name を cmd[0] として通常の Resolver dispatch に再帰 ( script / go-local / pnpm-external / pnpm-local のいずれか) |
| `protoc_builtin: <name>` | `protoc_builtin: java` | `buf` 本体に組み込まれた plugin。 buf 本体の version で代用 ( script resolver で `buf --version` 取得) |
| `remote: <bsr-url>:<tag>` | `remote: buf.build/protocolbuffers/go:v1.35.2` | tag が pinned 必須 ( 後述)。 hash 入力 = `("buf-remote", host, owner, name, version, revision_or_empty)` |

### 論理 version 文字列の形式

`bufResolver` は単一 version ではなく、 buf 本体 + plugin 群の `[]ToolVersion` を返す ( Resolver interface は複数返却を許容している):

- buf 本体: `"script:buf@1.30.0"` 形式 ( scriptResolver が `buf --version` で取得)
- local plugin: 各 plugin に対応する resolver から ( 例: `"script:protoc-gen-go@v1.34.2"` を scriptResolver で取得)
- protoc_builtin: buf 本体の version と同じ ( 重複なら排除)
- remote plugin: `"buf-remote:<host>/<owner>/<name>@<version>+rev<revision>"`

### Resolver 実装イメージ

```go
package buf

import (
    "context"
    "errors"
    "fmt"

    "github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
)

const Name = "buf"

type Resolver struct {
    registry *toolresolver.Registry // local plugin / buf 本体の再帰 resolve に使う
}

func New(registry *toolresolver.Registry) *Resolver {
    return &Resolver{registry: registry}
}

func (r *Resolver) Name() string { return Name }

// Resolve は declared 経由でのみ呼ばれる ( ADR-0005)。 declared には buf.gen.yaml
// の spec-dir-relative path を持つフィールド ( 例: `BufGenPath`) を将来追加する想定。
func (r *Resolver) Resolve(ctx context.Context, specDir string, _ []string, declared *toolresolver.DeclaredTool) ([]toolresolver.ToolVersion, error) {
    if declared == nil {
        return nil, errors.New("buf: requires explicit tools[] declaration")
    }

    bufGen, err := loadBufGenYAML(specDir, declared) // 宣言で指定された buf.gen.yaml を読む
    if err != nil {
        return nil, err
    }

    var versions []toolresolver.ToolVersion

    // buf 本体は spec 側で `tools: [{exec: ["buf", "--version"]}]` も並べて宣言してもらうのが
    // 素直な運用。 buf resolver 内部で script declared を自前生成して registry 経由で
    // 呼び出す案もあるが、 declared-only の原則と整合させるなら spec に寄せる方が良い。

    // 各 plugin を type 別に解決
    for _, plugin := range bufGen.Plugins {
        switch {
        case plugin.Local != "":
            // local plugin は spec 側の tools[] 宣言で別 resolver に振ってもらう前提。
            // ( 例: `local: protoc-gen-go` なら `tools: [{exec: ["protoc-gen-go", "--version"]}]`)
            // buf resolver は plugin 名のみ記録し、 実 version は他宣言の結果と合流する。

        case plugin.ProtocBuiltin != "":
            // buf 本体 version で代用 ( buf 本体の宣言が tools[] に並んでいる前提)。

        case plugin.Remote != "":
            // pinned tag 検証 → そのまま hash 入力に
            host, owner, name, version, err := parseRemote(plugin.Remote)
            if err != nil {
                return nil, err // ":latest" / version 省略は parseRemote が fail
            }
            versions = append(versions, toolresolver.ToolVersion{
                Name:    fmt.Sprintf("%s/%s/%s", host, owner, name),
                Version: fmt.Sprintf("buf-remote:%s/%s/%s@%s+rev%d", host, owner, name, version, plugin.Revision),
                Source:  "buf.gen.yaml",
            })
        }
    }
    return versions, nil
}
```

> NOTE: 実装着手時に `DeclaredTool` への buf 用フィールド ( `BufGenPath` 等) 追加と、 local plugin の version を spec 側で並列宣言させる運用 / buf resolver 内で script declared を自動生成する運用のどちらを採るかを ADR で確定させること。

## remote plugin の解決: pinned tag 強制 + 静的 parse

調査 ( 2026-05 時点の Buf 仕様) の結果、 codegen remote plugin の resolved version を取得する deterministic な静的経路は **`buf.gen.yaml` の pinned tag そのもの以外に存在しない**:

- **`buf.lock` には記録されない**。 buf の `buf.lock` v2 は `buf.yaml` の lint / breaking check plugin 専用で、 `buf.gen.yaml` の codegen remote plugin は対象外
- **`buf plugin` CLI には resolved version を返すサブコマンドがない**。 `buf plugin {push, update, prune}` の 3 つだけで、 すべて check plugin 対象。 `info` / `list` / `resolve` は存在しない
- **ローカルキャッシュにも plugin binary が残らない**。 BSR の codegen remote plugin はサーバ側で実行され生成物だけがクライアントに返るモデル ( "Remote Plugin Execution") で、 `~/.cache/buf/` 配下に plugin が存在しない
- `buf generate --debug` で出力される情報は安定 contract ではなく機械可読 parse には不向き

そのため lazygen は **`buf.gen.yaml` で remote plugin に pinned tag を強制** する。 spec lint で `:latest` 指定や version 省略を **検出時 fail** し、 必ず `:vX.Y.Z` 形式を要求する。 `revision:` フィールドが指定されていれば併せて hash 入力に含める ( buf 側が repackage 時に増やす整数で、 同じ upstream version でも revision が違えば再生成が必要)。

これは lazygen 全体の設計思想 (「lockfile / pinned 指定が現実を反映していないとキャッシュが嘘をつく」) と整合する選択。 aqua / pnpm / go.mod がすべて pinned 前提で動いているのと同じ規律を buf remote plugin にも適用する形。

### 将来拡張: BSR Connect API による resolution ( opt-in、 初版では実装しない)

`:latest` や revision 省略を許したい運用が将来出てきた場合、 BSR の Connect API ( `buf.build/buf.registry.plugin.v1beta1.PluginService`) を直接叩いて `PLUGIN_VERSION-MODULE_TIMESTAMP-COMMIT_ID.PLUGIN_REVISION` という composite identifier ( SemVer + 元 module commit + revision の合成) を fetch する経路は技術的には存在する。 認証は `BUF_TOKEN` 環境変数。

ただし以下の trade-off があるため初版では実装しない:

- ネットワーク必須 ( オフライン環境で破綻)
- BSR への認証 token 配布 / 管理が必要
- レート制限の考慮
- そもそも「pinned 強制で済む運用」の方が cache 健全性の観点で望ましい

将来必要が生じたら `LAZYGEN_BUF_RESOLVE_REMOTE=1` 等の opt-in env で有効化する設計余地を残す。

## Preflight Checker

`bufChecker` は buf 関連の整合性を多面的に検証する。

### 検証内容

| 対象 | 検証内容 |
|---|---|
| `buf.yaml` BSR 依存 | `buf.yaml` の `deps:` 行と `buf.lock` の resolved version が一致するか (`buf mod update` 未実行で `buf.yaml` だけ進んだ状態を検出)。 さらに `buf.lock` で参照されるモジュールが BSR cache (`~/.cache/buf/` 等) に取得済みか |
| `buf.gen.yaml` remote plugin の pinned 強制 | 各 entry が pinned tag (`:vX.Y.Z`) を持っているかを spec lint で検証 ( `:latest` / version 省略を検出したら fail) |
| `buf.gen.yaml` local plugin | 対応する resolver の preflight に再帰的に委譲 ( pnpm-external / pnpm-local ; script / go-local は preflight 不要なので素通り) |

### 不整合検出時

lazygen を即時 fail させ、 stderr に以下を表示:

```
buf: buf.gen.yaml の remote plugin に pinned 指定が必要です
  remote: buf.build/protocolbuffers/go (version 省略)
  please specify pinned version: buf.build/protocolbuffers/go:vX.Y.Z

buf: buf.yaml deps と buf.lock が一致しません
  please run: buf mod update
```

### Preflight 実装イメージ

```go
package buf

import (
    "context"

    "github.com/izumin5210/lazygen/internal/lazygen/preflight"
)

type Checker struct {
    registry *preflight.Registry
}

func (c *Checker) Check(ctx context.Context, specDir string) (preflight.Result, error) {
    var issues []preflight.Issue

    // buf.yaml deps ↔ buf.lock 整合性
    if !c.bufLockMatchesBufYAML(specDir) {
        issues = append(issues, preflight.Issue{
            Channel:    "buf",
            Detail:     "buf.yaml の deps と buf.lock が一致しません",
            Suggestion: "buf mod update",
        })
    }

    // buf.gen.yaml remote plugin の pinned 強制 lint
    bufGen, err := loadBufGenYAML(specDir, nil)
    if err == nil {
        for _, plugin := range bufGen.Plugins {
            if plugin.Remote != "" && !hasPinnedTag(plugin.Remote) {
                issues = append(issues, preflight.Issue{
                    Channel:    "buf",
                    Detail:     fmt.Sprintf("remote plugin に pinned 指定が必要: %s", plugin.Remote),
                    Suggestion: "version を :vX.Y.Z 形式で指定してください",
                })
            }
        }
    }

    // local plugin は対応する resolver の preflight に再帰委譲 ( 省略)

    return preflight.Result{OK: len(issues) == 0, Issues: issues}, nil
}
```

## SourceLister

`bufResolver` 自体は SourceLister を持たない ( ソース hash の対象ではない)。 ただし local plugin として呼び出される内製 generator は `goLocalResolver` / `pnpmLocalResolver` 経由で SourceLister を使う。

## Open Questions

- `buf.gen.yaml` の `inputs:` ( v2 で追加された input 制御機能) を hash 入力に含めるか。 inputs に変化があれば task の `inputs` glob にも反映されている前提なら不要だが、 buf 側固有のフィルタ ( `paths` / `excludes`) があれば別途考慮が必要
- `buf generate` の `--template` flag で渡される template path が `.gen.yaml` 以外 ( JSON 等) の場合の parse 対応
- BSR private registry を使うケースで pinned tag 取得方法に差分があるか ( 公式 BSR と self-hosted で挙動が異なる可能性)
