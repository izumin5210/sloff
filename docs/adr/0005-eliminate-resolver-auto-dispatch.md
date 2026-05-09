# ADR-0005: Resolver auto-dispatch を廃止し、 すべて declared 経由に統一

## Context

設計初版 ([architecture.md](../design/architecture.md) の「Dispatch: 明示宣言を基本に少数の名前付き dispatch」節) では、 各 `Resolver` に `CanResolve(specDir, cmd) bool` を持たせ、 spec の `tools:` が空のとき cmd 形状から自動で resolver を選ぶ "auto-dispatch ループ" を備えていた。 例:

- `cmd[0]` が `go run ./...` 形式なら `goLocalResolver` が名乗り出る
- `cmd[0]` が `buf generate` 系なら `bufResolver` が名乗り出る

その後、 [ADR-0004](./0004-spec-validation-and-output-conflict-policy.md) で `tools:` を spec の必須フィールドにしたため、 declared が空になる経路は parse 時点で弾かれる。 `Registry.Resolve` の auto-dispatch ループは事実上 **死に経路** (関数の definition 上は残っているが、 通常運用では到達しない) になった。

加えて、 `goLocalResolver` を追加した PR #6 の Codex レビューで以下 2 点の問題が顕在化した:

1. **mixed 構成での silent stale cache**: `cmd: ["go", "run", "./cmd/gen"]` と `tools: [{exec: ["go", "version"]}]` を併記したとき、 declared が非空なので auto-dispatch loop は起動せず、 `./cmd/gen` の編集は `resolved_versions_hash` に入らない。 ユーザーが `go-local: ./cmd/gen` を併記し忘れただけで cache が嘘をつく。
2. **`go run` フラグ引数の暗黙パース**: `go run -exec ./bin/wrapper ./cmd/gen` のような cmd で「最初の `./` 始まり引数」を entry とみなすと、 entry を取り違える。 これは auto-dispatch のために cmd を内部でパースしているために発生する。

これらは「auto-dispatch を残しつつ正しく動かす」方向で個別対処もできるが、 本 ADR では仕組み自体を見直す。

### References

- [ADR-0002: キャッシュヒット判定モデル](./0002-cache-hit-decision-model.md)
- [ADR-0004: Spec 検証と output 重複検出のポリシー](./0004-spec-validation-and-output-conflict-policy.md)
- [docs/design/architecture.md](../design/architecture.md)
- [docs/design/resolver-go-local.md](../design/resolver-go-local.md)

## Decision

resolver auto-dispatch を **全廃** する。 すべての resolver は spec の `tools:` で明示宣言された経路でのみ呼ばれる。

具体的に:

1. `toolresolver.Resolver` interface から `CanResolve(specDir, cmd) bool` を **削除**
2. `toolresolver.Registry` から auto-dispatch ループ / 登録順 (`inOrder`) を **削除**。 lookup は `byName` のみ
3. 各 resolver 実装から `CanResolve` を削除し、 cmd を内部パースして entry を抽出する経路 (e.g. `golocal.extractGoRunEntry`) も **削除**。 必要な情報 (例: `go-local` resolver の entry) は `DeclaredTool` の専用フィールドで受け取る
4. `tools:` 必須化 ([ADR-0004 D1](./0004-spec-validation-and-output-conflict-policy.md#d1-tools-を-spec-の必須フィールドにする)) は維持

## Rationale

- **cache 健全性は「ユーザーが宣言した範囲で動く」ことを拠り所にしている**。 cmd の見た目を resolver が勝手に解釈して名乗り出る挙動は、 declared 漏れによる silent stale cache の原因になりやすい。 cache の正しさが暗黙挙動の正確性に依存するのは設計として弱い。
- **死に経路の削除**: ADR-0004 で `tools:` が必須になった以降、 auto-dispatch ループは到達しない。 残しておくと「死に経路の正しさ」を保守し続ける負担と、 上記 (2) のような暗黙パース由来のバグの温床になる。
- **省略の便益が小さい**: `tools: [{go-local: ./cmd/gen}]` と書く 1 行を省くために cmd 形状の暗黙ルール (`cmd[0] == "go" && cmd[1] == "run" && cmd[2..] の中の最初の "./" が entry`) を覚えるのは割に合わない。 さらに `go run -tags ... -overlay ...` のフラグパーサ問題のような追加複雑性を生む。
- **YAGNI**: buf resolver のような複合 cmd を扱う resolver で、 `cmd` を解析して plugin 一覧を抽出する機能はその resolver 自身の `Resolve` 内部で行えば足りる (declared 経由で起動した後で cmd を読めば良い)。 「Registry レベルの dispatch 多態性」は不要。 将来的にどうしても auto-dispatch が必要な場面が出てきたら、 別 ADR で再導入を検討する。

## Consequences

### 正の影響

- 暗黙挙動が消え、 「ある cmd でなぜこの resolver が呼ばれたか」が spec の `tools:` を読めば一意に決まる
- mixed 構成 (cmd の見た目と無関係な tool を `tools:` で宣言したいケース) で silent stale cache のリスクが構造的に消える。 必要な resolver は全て明示宣言される
- `Resolver` interface / `Registry` / 各 resolver 実装が単純化される。 死に経路を抱えなくて済む
- cmd の暗黙パース (フラグ前後の引数判定など) を実装する必要が無くなる

### 負の影響 / 注意点

- spec で `cmd: ["go", "run", "./cmd/gen"]` と書くだけでは go-local が動かない。 `tools: [{go-local: ./cmd/gen}]` を併記する必要がある (= 冗長だが明示的)
- 設計書 (`docs/design/architecture.md` / `docs/design/resolver-go-local.md`) の dispatch 関連記述を更新する必要がある
- 既存テストのうち、 auto-dispatch / `CanResolve` 系の振る舞いをアサートしていたものは削除または書き換えになる
- 将来 buf resolver (`cmd: buf generate ...` から `buf.gen.yaml` を読み出して plugin 一覧を返す) を実装する場合、 declared 経由で起動した buf resolver が `cmd` を見て plugin を resolve する形にする (auto-dispatch ではなく resolver 内部の cmd parsing として整理する)
