# ADR-0006: buf resolver は remote plugin のみを解決し、 buf 本体 / local plugin / protoc_builtin は spec で並列宣言する

## Context

[docs/design/resolver-buf.md](../design/resolver-buf.md) の初版には、 実装着手時に確定すべき open question が NOTE で 1 点残されていた:

> `DeclaredTool` への buf 用フィールド ( `BufGenPath` 等) 追加と、 local plugin の version を spec 側で並列宣言させる運用 / buf resolver 内で script declared を自動生成する運用のどちらを採るかを ADR で確定させること。

`buf generate` 経由のコード生成タスクは、 通常 `buf.gen.yaml` に複数の plugin が並ぶ複合 cmd である。 plugin は v2 で 3 形式 ( `local` / `protoc_builtin` / `remote`) があり、 解決経路がそれぞれ異なる:

- `local` : `protoc-gen-go` のような実バイナリで、 通常の resolver dispatch ( script / go-local / pnpm-external / pnpm-local) で解決される
- `protoc_builtin` : `buf` 本体に組み込まれた plugin で、 `buf` 本体の version で代用する
- `remote` : BSR の codegen remote plugin で、 サーバ側で実行され生成物のみがクライアントに返る

この 3 種のうち、 `local` と `protoc_builtin` ( ＝ buf 本体) の version 取得は、 既に lazygen が持つ resolver ( script / go-local / pnpm-*) の責務と完全に重なる。 そのため「buf resolver が `buf.gen.yaml` を読んで他 resolver を内部呼び出しする」運用と、 「spec の `tools:` に並べて宣言してもらう」運用の 2 案が立てられた。

[ADR-0005](./0005-eliminate-resolver-auto-dispatch.md) で確定した「resolver は declared 経由でのみ呼ぶ」原則 (=auto-dispatch 全廃) との整合が、 本 ADR の判断軸となる。

### References

- [ADR-0004: Spec 検証と output 重複検出のポリシー](./0004-spec-validation-and-output-conflict-policy.md)
- [ADR-0005: Resolver auto-dispatch を廃止し、 すべて declared 経由に統一](./0005-eliminate-resolver-auto-dispatch.md)
- [docs/design/resolver-buf.md](../design/resolver-buf.md)

## Decision

### D1. buf resolver の責務は remote plugin の version 解決のみ

`bufResolver.Resolve` は、 spec で渡された `buf.gen.yaml` を静的 parse し、 **`remote: <bsr-url>:<tag>` 形式の plugin entry に対してのみ** OS 中立な version 文字列を生成する:

```
buf-remote:<host>/<owner>/<name>@<version>+rev<revision>
```

`local: <name>` / `protoc_builtin: <name>` / buf 本体は、 buf resolver の戻り値には **含めない**。 これらは spec の `tools:` で別 entry として並列宣言してもらう前提とする。

### D2. spec の `tools:` で buf-related tool を並列宣言する

ユーザーは `buf generate` を使う task の `tools:` に、 buf 本体 / local plugin / `buf.gen.yaml` ( remote plugin 解決用) を並べて書く:

```yaml
tools:
  - exec: ["buf", "--version"]                  # buf 本体 + protoc_builtin の代用 version
  - exec: ["protoc-gen-go", "--version"]        # local plugin の version
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
  - buf: buf.gen.yaml                           # remote plugin の version
```

各 entry は対応する resolver ( script / buf) で独立に解決され、 結果は `tools_hash` に sorted concat で混ざる。

### D3. `DeclaredTool` に `BufGenPath` フィールドを追加する

`spec.DeclaredTool` / `toolresolver.DeclaredTool` に `BufGenPath string` を追加し、 spec YAML の `buf: <path>` ( spec dir 相対の `buf.gen.yaml` パス) を受け取る。 `Resolver` 名は `"buf"`。 既存 ( `exec` / `go-local`) と排他とする。

## Rationale

### D1 / D2: spec で並列宣言する側を採る

#### declared-only 原則との整合

ADR-0005 は「ある cmd でなぜこの resolver が呼ばれたかが、 spec の `tools:` を読めば一意に決まる」ことを cache 健全性の核に据えた決定だった。 buf resolver が内部で script declared を自動生成して registry を再帰呼び出しする運用を採ると、 spec を読んだだけでは「buf resolver が裏で何を呼ぶか」が見えなくなり、 ADR-0005 が排除した「暗黙挙動 = silent stale cache の温床」が再導入される。

例えば、 ある repo で `buf` を aqua 経由で導入していて、 別の repo で `buf` を pnpm 経由で導入しているケースを考えると、 buf resolver が「buf 本体の version 取得は script で `buf --version` を叩く」と固定すれば、 後者のケースで `tools_hash` が pnpm-lock.yaml の更新に追従しない。 この問題の解決を buf resolver の内部で検出ロジックを増やす方向に倒すと、 結局 auto-dispatch を再現してしまう。

spec 並列宣言にすれば、 「buf 本体をどの channel で版管理しているか」をユーザーが明示するだけで上記の差分は吸収できる。 これは ADR-0005 の「spec を読めば挙動が一意」の延長線にある。

#### 責務分割の単純さ

buf resolver の責務を「remote plugin の解決のみ」に限定すれば、 実装は `buf.gen.yaml` パーサと pinned tag 強制の 2 つだけで済む。 内部で他 resolver を再帰呼び出しする経路を持たないため、 unit test も buf resolver 単体で完結する。 また将来 BSR の plugin spec が変わっても、 影響範囲が buf resolver に局所化される。

#### 冗長性のコストは小さい

「3 〜 4 行を spec に並べるのは冗長」という反論はあるが、 lazygen は元々 `tools:` 必須化 ([ADR-0004 D1](./0004-spec-validation-and-output-conflict-policy.md#d1-tools-を-spec-の必須フィールドにする)) を採用しており、 ユーザーは既に「使うツールを書き並べる」運用に慣れている前提。 buf 系も同じ規律で扱う方が、 spec の読み手にとって学習コストが下がる。

### D3: `BufGenPath` を独立フィールドにする

`exec` / `go-local` と同様に、 buf resolver が必要とする情報 ( `buf.gen.yaml` の spec 相対パス) を `DeclaredTool` の専用フィールドに乗せる。 `tools_hash` 計算に必要な情報は declared にあるべき / cmd には書かない、 という ADR-0005 の方針との整合。

## Consequences

### 正の影響

- spec を読めば「どの resolver がどの理由で呼ばれるか」が一意になる ( ADR-0005 の延長)
- buf resolver の実装が `buf.gen.yaml` parse + remote pinned tag 強制 + version 文字列組み立て、 という小さな範囲に閉じる
- `local` / `protoc_builtin` の version 解決は既存 resolver の再利用で済む。 buf resolver が他 resolver を import せずに済むため、 package の依存方向が一方通行 ( buf resolver → 既存 resolver は無し) で保たれる
- buf 本体の配布チャネル ( aqua / pnpm / 内製ビルド等) 切替が、 spec の `tools:` 編集だけで完結する

### 負の影響 / 注意点

- buf 経由の task は `tools:` に 3 〜 4 行書く運用となる。 「buf.gen.yaml に書いてあるのに spec にも書くのか」という冗長感はある (spec lint / docs で例示を充実させて緩和)
- buf 本体と plugin の間で `tools:` 宣言が漏れたとき、 silent stale cache の余地が残る。 これは ADR-0004/ADR-0005 と同じ規律の延長で、 spec lint / docs / リリースノートでの明示的な注意喚起が必要
- buf resolver は「buf.gen.yaml が指す local plugin が spec の tools[] に宣言されているか」までは検証しない。 検証したい場合は preflight 側 ( bufChecker) で別途チェックを追加する余地を残す ( 初版では実装しない)
