# Resolver: buf

`bufResolver` は `buf generate` 経由で実行されるコード生成タスクのうち、 **`buf.gen.yaml` に列挙される remote plugin の論理 version を解決する** resolver。

関連:
- [Architecture](./architecture.md)
- [ADR-0005: Resolver auto-dispatch を廃止し、 すべて declared 経由に統一](../adr/0005-eliminate-resolver-auto-dispatch.md)
- [ADR-0006: buf resolver は remote plugin のみを解決し、 buf 本体 / local plugin / protoc_builtin は spec で並列宣言する](../adr/0006-buf-resolver-declares-remote-plugins-only.md)
- [Resolver: script](./resolver-script.md), [go-local](./resolver-go-local.md), [pnpm-external](./resolver-pnpm-external.md), [pnpm-local](./resolver-pnpm-local.md) ( buf 経由でも plugin 種別に応じて並列宣言される)

## Context

`buf generate` task は通常複数の plugin を組み合わせて実行する複合 cmd で、 `buf.gen.yaml` ( v2) に plugin が宣言される。 plugin は 3 形式 ( `local` / `protoc_builtin` / `remote`) があり、 それぞれ解決経路が異なる。

[ADR-0005](../adr/0005-eliminate-resolver-auto-dispatch.md) により lazygen には auto-dispatch が無く、 [ADR-0006](../adr/0006-buf-resolver-declares-remote-plugins-only.md) により buf resolver の責務は **remote plugin の version 解決のみ** に限定される。 buf 本体 / `protoc_builtin` / `local` plugin の version は spec の `tools:` で並列宣言する運用とする ( ADR-0006 D2)。

## spec での宣言例

`buf generate` を使うタスクは、 buf 本体・各 local plugin・remote plugin 解決用の `buf.gen.yaml` パスを `tools:` に並べて宣言する:

```yaml
commands:
  - name: protoc-gen-go
    cmd: buf generate --template buf.gen.yaml
    inputs: ["**/*.proto", "buf.gen.yaml"]
    outputs: ["**/*.pb.go", "**/*.connect.go"]
    tools:
      - exec: ["buf", "--version"]                     # buf 本体 ( + protoc_builtin の代用 version)
      - exec: ["protoc-gen-go", "--version"]           # local plugin の version
        extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
      - buf: buf.gen.yaml                              # remote plugin の version (spec dir 相対パス)
```

`buf:` フィールドの値は spec dir 相対の `buf.gen.yaml` パス。 `--template` flag で別ファイルを指す場合はそのパスを指定する。

## Resolver の動作

### plugin type 別解決ルール

| plugin type ( buf.gen.yaml v2) | 例 | 解決経路 |
|---|---|---|
| `local: <name>` | `local: protoc-gen-go` | **spec 並列宣言**: spec の `tools:` で別 entry ( `exec` / `go-local` / `pnpm-*` のいずれか) として宣言する |
| `protoc_builtin: <name>` | `protoc_builtin: java` | **spec 並列宣言**: buf 本体に組み込まれた plugin。 buf 本体の `exec: ["buf", "--version"]` 宣言で代用する |
| `remote: <bsr-url>:<tag>` | `remote: buf.build/protocolbuffers/go:v1.35.2` | **buf resolver が解決**: tag が pinned 必須 ( 後述)。 hash 入力 = `buf-remote:<host>/<owner>/<name>@<version>+rev<revision>` |

`local` / `protoc_builtin` は buf resolver の戻り値には含めない ( ADR-0006 D1)。 spec 側で対応する resolver entry を並列宣言してもらう前提。

### 論理 version 文字列の形式

buf resolver は `buf.gen.yaml` の `plugins:` を走査し、 `remote:` 形式の entry に対してのみ以下の形式の `ToolVersion` を返す:

```
buf-remote:<host>/<owner>/<name>@<version>+rev<revision>
```

`revision:` フィールドが省略されている場合は `+rev0` とする。 buf resolver が返す `ToolVersion` の `Source` は `"buf-remote:<host>/<owner>/<name>"`、 `Name` は表示用に `<host>/<owner>/<name>` を使う。

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
    repoRoot string
}

func New(repoRoot string) *Resolver {
    return &Resolver{repoRoot: repoRoot}
}

func (r *Resolver) Name() string { return Name }

// Resolve は declared 経由でのみ呼ばれる ( ADR-0005)。 declared.BufGenPath は spec dir
// 相対の buf.gen.yaml パスを指す。 buf 本体 / local plugin / protoc_builtin は別 declared
// として並列宣言される前提なので、 ここでは扱わない ( ADR-0006 D1)。
func (r *Resolver) Resolve(ctx context.Context, specDir string, _ []string, declared *toolresolver.DeclaredTool) ([]toolresolver.ToolVersion, error) {
    if declared == nil {
        return nil, errors.New("buf: requires explicit tools[] declaration")
    }
    if declared.BufGenPath == "" {
        return nil, errors.New("buf: declared buf-gen-path is required")
    }
    bufGen, err := loadBufGenYAML(r.repoRoot, specDir, declared.BufGenPath)
    if err != nil {
        return nil, err
    }

    var versions []toolresolver.ToolVersion
    for _, plugin := range bufGen.Plugins {
        if plugin.Remote == "" {
            continue // local / protoc_builtin は spec で並列宣言される (ADR-0006)
        }
        host, owner, name, version, err := parseRemote(plugin.Remote)
        if err != nil {
            return nil, err // ":latest" / version 省略は parseRemote が fail
        }
        identity := fmt.Sprintf("%s/%s/%s", host, owner, name)
        versions = append(versions, toolresolver.ToolVersion{
            Name:    identity,
            Source:  "buf-remote:" + identity,
            Version: fmt.Sprintf("buf-remote:%s@%s+rev%d", identity, version, plugin.Revision),
        })
    }
    // buf.gen.yaml に remote plugin が無い場合でも、 declared を書いた事実は spec の意図
    // ( = remote が無いことを buf resolver で確認したい) として扱い、 version を返さない
    // ことは正常パス。 上位で空配列を弾く必要は無い。
    return versions, nil
}
```

### remote plugin の解決: pinned tag 強制 + 静的 parse

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

`bufChecker` は buf 関連の整合性を検証する。

### 検証内容

| 対象 | 検証内容 |
|---|---|
| `buf.gen.yaml` remote plugin の pinned 強制 | 各 entry が pinned tag (`:vX.Y.Z`) を持っているかを spec lint で検証 ( `:latest` / version 省略を検出したら fail) |
| `buf.yaml` BSR 依存 | `buf.yaml` の `deps:` 行と `buf.lock` の resolved version が一致するか ( `buf dep update` 未実行で `buf.yaml` だけ進んだ状態を検出) |

`buf.gen.yaml` の `local:` plugin について「spec の `tools:` に対応宣言があるか」までは初版では検証しない ( ADR-0006 で local plugin は spec 並列宣言とした帰結。 必要が出たら spec lint 側に拡張予定)。

### 不整合検出時

lazygen を即時 fail させ、 stderr に以下のような issue を表示する:

```
buf: buf.gen.yaml の remote plugin に pinned 指定が必要です
  remote: buf.build/protocolbuffers/go (version 省略)
  please specify pinned version: buf.build/protocolbuffers/go:vX.Y.Z

buf: buf.yaml deps と buf.lock が一致しません
  please run: buf dep update
```

### Preflight 実装イメージ

```go
package buf

import (
    "context"

    "github.com/izumin5210/lazygen/internal/lazygen/preflight"
)

type Checker struct {
    repoRoot string
}

func (c *Checker) Check(ctx context.Context, specDir string) (preflight.Result, error) {
    var issues []preflight.Issue

    // buf.gen.yaml remote plugin の pinned 強制 lint
    if bufGen, ok, err := tryLoadBufGenYAML(c.repoRoot, specDir); err != nil {
        return preflight.Result{}, err
    } else if ok {
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

    // buf.yaml deps ↔ buf.lock 整合性
    if mismatch, ok, err := compareBufLockWithYAML(c.repoRoot, specDir); err != nil {
        return preflight.Result{}, err
    } else if ok && mismatch {
        issues = append(issues, preflight.Issue{
            Channel:    "buf",
            Detail:     "buf.yaml の deps と buf.lock が一致しません",
            Suggestion: "buf dep update",
        })
    }

    return preflight.Result{OK: len(issues) == 0, Issues: issues}, nil
}
```

## SourceLister

buf resolver 自体は SourceLister を持たない ( ソース hash の対象ではない)。 ただし local plugin として並列宣言される内製 generator は `goLocalResolver` / `pnpmLocalResolver` 経由で SourceLister を使う ( これは ADR-0006 で spec 並列宣言に倒した自然な帰結)。

## Open Questions

- `buf.gen.yaml` の `inputs:` ( v2 で追加された input 制御機能) を hash 入力に含めるか。 inputs に変化があれば task の `inputs` glob にも反映されている前提なら不要だが、 buf 側固有のフィルタ ( `paths` / `excludes`) があれば別途考慮が必要
- `buf generate` の `--template` flag で渡される template path が `.gen.yaml` 以外 ( JSON 等) の場合の parse 対応。 初版は YAML のみ対応
- BSR private registry を使うケースで pinned tag 取得方法に差分があるか ( 公式 BSR と self-hosted で挙動が異なる可能性)
- spec の `tools:` に並べた buf 本体 / local plugin の宣言と、 `buf.gen.yaml` の plugin 列挙の整合性検証 (「local plugin が宣言漏れ」検出) を preflight に追加するか
