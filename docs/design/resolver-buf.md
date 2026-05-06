# Resolver: buf

`bufResolver` は `buf generate` 経由で実行されるコード生成タスクのうち、 **`buf.gen.yaml` の remote plugin と `buf.yaml` / `buf.lock` の BSR dependency の論理 version を解決する** resolver。

関連:
- [Architecture](./architecture.md)
- [ADR-0005: Resolver auto-dispatch を廃止し、 すべて declared 経由に統一](../adr/0005-eliminate-resolver-auto-dispatch.md)
- [ADR-0006: buf resolver は remote plugin のみを解決し、 buf 本体 / local plugin / protoc_builtin は spec で並列宣言する](../adr/0006-buf-resolver-declares-remote-plugins-only.md)
- [Resolver: script](./resolver-script.md), [go-local](./resolver-go-local.md), [pnpm-external](./resolver-pnpm-external.md), [pnpm-local](./resolver-pnpm-local.md) ( buf 経由でも plugin 種別に応じて並列宣言される)

## Context

`buf generate` task は通常複数の plugin を組み合わせて実行する複合 cmd で、 `buf.gen.yaml` ( v2) に plugin が宣言される。 plugin は 3 形式 ( `local` / `protoc_builtin` / `remote`) があり、 それぞれ解決経路が異なる。

[ADR-0005](../adr/0005-eliminate-resolver-auto-dispatch.md) により lazygen には auto-dispatch が無く、 [ADR-0006](../adr/0006-buf-resolver-declares-remote-plugins-only.md) により buf resolver の責務は **remote plugin と BSR module dependency の version 解決** に限定される。 buf 本体 / `protoc_builtin` / `local` plugin の version は spec の `tools:` で並列宣言する運用とする ( ADR-0006 D2)。 また `.proto` ファイルの入力範囲については spec の `inputs:` で宣言してもらう運用とする ( 後述「buf.gen.yaml の inputs を読まない理由」)。

## spec での宣言例

`buf generate` を使うタスクは、 buf 本体・各 local plugin・remote plugin 解決用の `buf.gen.yaml` パスを `tools:` に並べて宣言する:

```yaml
commands:
  - name: protoc-gen-go
    cmd: buf generate --template buf.gen.yaml
    inputs: ["**/*.proto", "buf.gen.yaml", "buf.yaml", "buf.lock"]
    outputs: ["**/*.pb.go", "**/*.connect.go"]
    tools:
      - exec: ["buf", "--version"]                     # buf 本体 ( + protoc_builtin の代用 version)
      - exec: ["protoc-gen-go", "--version"]           # local plugin の version
        extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
      - buf: buf.gen.yaml                              # remote plugin + BSR deps の version (spec dir 相対パス)
```

`buf:` フィールドの値は spec dir 相対の `buf.gen.yaml` パス。 `--template` flag で別ファイルを指す場合はそのパスを指定する。

`inputs:` には `buf.yaml` / `buf.lock` も含める運用を推奨する。 `files_hash` 経路でも `buf dep update` 後の commit 変更を拾えるため、 buf resolver の `buf-dep:` 経路と二重に押さえる defense-in-depth 構造になる ( buf-dep が落ちる/取りこぼしても files_hash で invalidate される)。

## Resolver の動作

### plugin type 別解決ルール

| plugin type ( buf.gen.yaml v2) | 例 | 解決経路 |
|---|---|---|
| `local: <name>` | `local: protoc-gen-go` | **spec 並列宣言**: spec の `tools:` で別 entry ( `exec` / `go-local` / `pnpm-*` のいずれか) として宣言する |
| `protoc_builtin: <name>` | `protoc_builtin: java` | **spec 並列宣言**: buf 本体に組み込まれた plugin。 buf 本体の `exec: ["buf", "--version"]` 宣言で代用する |
| `remote: <bsr-url>:<tag>` | `remote: buf.build/protocolbuffers/go:v1.35.2` | **buf resolver が解決**: tag が pinned 必須 ( 後述)。 hash 入力 = `buf-remote:<host>/<owner>/<name>@<version>+rev<revision>` |

`local` / `protoc_builtin` は buf resolver の戻り値には含めない ( ADR-0006 D1)。 spec 側で対応する resolver entry を並列宣言してもらう前提。

### BSR module dependency の解決

`buf.gen.yaml` を起点に祖先方向へ歩いて最初に見つかった `buf.yaml` を **buf module root** として扱い、 そこに記載された `deps:` 各 entry に対して `buf.lock` の resolved commit を引き、 以下形式の `ToolVersion` を追加で返す:

```
buf-dep:<host>/<owner>/<name>@<commit>
```

`buf.yaml` が見つからない / `deps:` が空のケースは何も emit しない ( BSR module を使わない repo は正常パス)。 `buf.yaml` に deps があるが `buf.lock` が無い / 不整合な場合は **resolver で error** を返す ( preflight でも検出するが、 preflight を bypass するパスでも cache に嘘が混ざらないようにする)。

### 論理 version 文字列の形式

buf resolver が返す `ToolVersion` のフォーマット:

| 種別 | Version 文字列 | Source | Name |
|---|---|---|---|
| remote plugin | `buf-remote:<host>/<owner>/<name>@<version>+rev<revision>` | `buf-remote:<host>/<owner>/<name>` | `<host>/<owner>/<name>` |
| BSR dep | `buf-dep:<host>/<owner>/<name>@<commit>` | `buf-dep:<host>/<owner>/<name>` | `<host>/<owner>/<name>` |

remote plugin の `revision:` 省略時は `+rev0`。 BSR dep の commit は `buf.lock` の `commit:` フィールド ( BSR が module 単位で発行する不変 ID) を使う。 digest を併用すると revision 相当の追加識別子が必要だが、 buf 自身が「commit が同じ＝同一 .proto セット」を保証するため commit のみで足りる。

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

## buf.gen.yaml の inputs を読まない理由

buf v2 の `inputs:` セクションには `directory:` / `paths:` / `excludes:` といった path-based なフィルタに加えて `types:` / `include_types:` / `exclude_types:` という **type-based なフィルタ** がある。 例えば `types: [foo.v1.UserService]` を指定すると、 protobuf type の transitive closure を計算した上で、 そこに含まれる `.proto` のみが実際の入力になる。

これを忠実に再現するには `.proto` の parse + symbol table 構築 + import 推移閉包の計算が必要で、 これは事実上 `protoc` / `buf build` 相当の仕事。 path-based フィルタだけ実装して type-based フィルタを無視すると、 「resolver は入力を正確に把握している」 という外形が崩れて over-invalidate / spurious depgraph edge を引き起こす ( cache が嘘をつくわけではないが pessimistic に振れる)。

部分対応で混乱を招くより、 **`.proto` の入力範囲は spec の `inputs:` で declarative に宣言してもらう** 方針を採る。 これは ADR-0006 で local plugin / `protoc_builtin` を spec 並列宣言に倒した思想と同じで、 「spec を読めば task の入出力が一意に分かる」 という説明性を優先する。

将来 type filter を使わない場面に限って auto-derive を opt-in する余地は残せるが ( 別 ADR で再検討)、 初版ではサポートしない。

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

- `buf.gen.yaml` の `inputs:` の auto-derive を将来 opt-in で復活させるか。 `types:` 系の type filter を使わない範囲なら path 列挙だけで安全に再現可能。 spec.inputs 宣言を省略したい強い要求が出てきたら別 ADR で検討
- `buf generate` の `--template` flag で渡される template path が `.gen.yaml` 以外 ( JSON 等) の場合の parse 対応。 初版は YAML のみ対応
- BSR private registry を使うケースで pinned tag 取得方法 / commit 取得方法に差分があるか ( 公式 BSR と self-hosted で挙動が異なる可能性)
- spec の `tools:` に並べた buf 本体 / local plugin の宣言と、 `buf.gen.yaml` の plugin 列挙の整合性検証 (「local plugin が宣言漏れ」検出) を preflight に追加するか
- buf.lock の `digest:` を hash material に含めるか。 commit が同じ＝同一内容を buf 自身が保証するため初版では `commit` のみだが、 buf 仕様変更時の defense-in-depth として digest 併用を検討する余地あり
