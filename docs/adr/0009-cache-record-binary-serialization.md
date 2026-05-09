# ADR-0009: キャッシュレコードの直列化形式 (protobuf binary)

## Context

### 背景

[ADR-0003](./0003-record-storage-strategy.md) で record を git per-task per-input ファイル方式で配置することを確定し、 [Design Doc](../design/architecture.md) で具体的な YAML schema と deterministic ordering 規約を定めた。 初版は `goccy/go-yaml` による YAML 直列化で実装されている。

YAML 直列化は「中身を `cat` / エディタで読める」 という debug 体験の利点がある一方、 監視している (= 利用者を獲得する) 前段階で 2 つの構造的な摩擦が見えてきた:

1. **検索結果のノイズ**: record に含まれる `output.files` の path や hash は、 利用者の `rg` / `ag` / `git grep` / `grep`、 VSCode / IntelliJ の workspace search、 さらに各種 file indexer で軒並みヒットする。 緩和には全ツール / 全エディタで `.sloff/cache/` を ignore 設定する必要があり、 **チーム × ツール数 で配布コストが増える**。 PR diff の `linguist-generated` は git レイヤの解決で、 ローカル検索ツールには効かない
2. **schema 強制が手書き規律に依存**: deterministic ordering ( field 宣言順 = alphabetical、 path 昇順、 name 昇順) は `goccy/go-yaml` の挙動 + 手書きの `MarshalYAML` 実装 + `Record` struct の field 宣言順 で成立しており、 リファクタリング時に容易に壊れる脆弱な不変条件になっている

サイズ削減・ 後方互換性は本 ADR の主たる動機ではない。 サイズは [ADR-0003 §131](./0003-record-storage-strategy.md) の試算で問題化していないし、 schema 進化は現行 `schema_version` フィールドで原理的には足りる。

一方、 **利用者ゼロの今が format を切り替えるコストが恒久的に最も低い瞬間**であり、 後から切り替えると強制 breaking になる。 「opacity」 と 「schema 強制 + breaking change の自動検出」 を取りに行く判断を、 利用者がつく前に確定させる。

### 制約 / 評価軸

- **opacity (新)**: grep / 各種エディタ search index / file indexer に対して **format レベルで** opaque ( ツール側設定の徹底に依存しない)
- **byte stability (R2)**: 同一 in-memory Record → 同一 byte 列。 OS / proto runtime version が同じならば bit-identical
- **OS 非依存 (R3)**: `darwin/arm64` / `linux/amd64` / `linux/arm64` で同じ record を共有できる
- **schema 強制 (新)**: field 順 / 必須性 / 型を **コンパイル時** に強制する
- **breaking change の自動検出 (新)**: tag 番号変更 / 型変更 / required→optional 化を CI で検出
- **debuggability**: cache 不一致を疑った時に中身を確認する手段が確保されている
- **ビルド依存の最小化**: 新規 build tool を入れる場合も `go.mod` 経由で pin できる ( OS 中立、 セキュリティ可監査)

### References

- [ADR-0003: キャッシュレコードのストレージ方式](./0003-record-storage-strategy.md)
- [Design Doc: sloff Architecture](../design/architecture.md)

## Considered Options

### Comparison Table

| | A: 現状 YAML | B: gzipped YAML | **C: protobuf binary (採用)** | D: msgpack / CBOR |
|---|---|---|---|---|
| 検索ツール / index に opaque | × | ◎ | ◎ | ◎ |
| byte stability | ◎ ( 検証済) | ◎ ( 検証済) | ○ ( 後述の運用規約で担保) | ○ |
| compile-time schema 強制 | × | × | ◎ | × |
| breaking change 自動検出 | × | × | ◎ ( buf breaking) | × |
| debug ( inspect 容易性) | ◎ | ○ ( `gunzip \| less`) | △ ( decode サブコマンド要) | △ |
| ビルド依存追加 | なし | なし | protoc-gen-go + buf ( `go tool` で pin) | proto / cbor 系 lib のみ |
| 移行コスト ( 利用者ゼロの現在) | 0 | 0 | 0 ( record 再生成) | 0 |

### Option A: 現状 YAML を維持

検索ノイズは利用者各自に `.rgignore` / `search.exclude` 等を配布する運用で吸収する想定。

👍 **Pros**

- 移行コストゼロ
- debug 体験が最も良い

👎 **Cons**

- ツール × チームで配布コストが増え続ける ( 構造的に解決しない)
- schema 強制が手書き規律に依存し続ける
- breaking change の自動検出が無い

### Option B: gzipped YAML

YAML 出力をそのまま gzip して `<input_hash>.yml.gz` として配置する。

👍 **Pros**

- 検索ノイズは format レベルで解決
- 既存 YAML schema / deterministic ordering の規約をそのまま流用できる
- debug は `gunzip -c | less` で完結

👎 **Cons**

- compile-time schema 強制が無い ( 規律依存のまま)
- breaking change の自動検出が無い
- gzip header の OS / mtime field 固定など別の byte stability 規律が必要

opacity 単独が目的ならば筋が良い案だが、 schema 強制を取りに行く本 ADR の動機を満たさない。

### Option C: protobuf binary (採用)

`.proto` schema を SSoT とし、 wire format で record を直列化する。 file 拡張子は `.pb`。

👍 **Pros**

- 検索ノイズは format レベルで構造的に解決
- compile-time に field 順 / 型 / 必須性が強制される
- `buf breaking` で wire-level breaking change を CI で自動検出
- record size が二次効果として小さくなる
- proto package が将来の S3 backend / 組織横断 cache 共有でそのまま流用できる

👎 **Cons**

- `proto.Marshal` のデフォルト挙動は決定論的ではない。 byte stability を保つために運用規約 ( 後述) が必要
- inspect 用のサブコマンド ( `sloff cache show`) を必須機能として追加する必要がある
- `protoc-gen-go` と `buf` を `go tool` 経由で pin する build pipeline 拡張が要る

### Option D: msgpack / CBOR

binary だが schema 強制 / 公式 breaking 検出ツールが proto と比べて弱い。 「opacity が欲しいだけ」 なら gzipped YAML の方が単純で、 「schema 強制も欲しい」 なら proto の方が成熟している。 中間案として明確な居場所を見出せず棄却。

## Decision

**Option C: protobuf binary を採用する。**

採用根拠の論理連鎖:

1. **opacity は format レベルでしか構造的に解決できない** ( ツール側設定徹底はチーム × ツール数で破綻する)。 A は除外
2. **opacity 単独が目的なら B (gzipped YAML) が最小コスト**だが、 本 ADR は schema 強制 + breaking 検出も同時に取りに行く動機がある。 B は除外
3. **schema 強制 + breaking 検出を両立する成熟した選択肢は proto**。 D ( msgpack / CBOR) は両軸とも弱いため除外
4. **proto 採用の主リスクは byte stability**。 これは下記の運用規約で実用的に解消可能と判断する
5. **利用者ゼロの今がコストゼロの切替タイミング**

### Schema 設計規則

- **proto3** を使用。 presence detection が必要な field のみ `optional` を明示
- **proto package を `sloff.cache.v1` に分離する**。 これにより top-level message を `Record` / `Spec` / `Input` / `Output` / `FileEntry` / `ResolvedVersion` の素直な名前で宣言できる ( package qualifier `cachev1.Spec` 等で曖昧さは解消される)。 親 `sloff.v1` 配下に置くと `sloff.v1.Spec` のように cache 文脈が消えて spec config 等と区別がつかない
- **Go 側で alias は使わず、 generated 型を直接引き回す**。 `cache` package は Storage interface / Marshal-Unmarshal helper / Sort / FilePaths / SchemaVersion 定数 / FileExt 定数のみを export し、 record 型は `cachev1.Record` を呼び出し側で直接使う
- **`map<,>` 禁止**。 順序が問題になる集合はすべて `repeated <Entry>` で表現し、 marshal 前に明示ソート
  - `output.files` → `repeated FileEntry { string path; string hash; }` を path 昇順
  - `input.resolved_versions` → `repeated ResolvedVersion { string name; string version; string source; }` を name 昇順
- **`Input` を tools 関連の SSoT にする**。 旧 `tools_hash` ( `input.components` の sub-hash) と informational だった `generator_version_snapshot` を 1 箇所に統合し、 `Input.resolved_versions_hash` ( hash 入力) と `Input.resolved_versions` ( per-entry detail) として並べる。 「 何が hash に効いて、 何が informational か」 が同じ階層で読める
- **`ToolVersion` → `ResolvedVersion` に rename**。 `tools_hash` も同様に `resolved_versions_hash` へ。 旧名は 「 user-declared tool の version」 を示唆するが、 実体は `Resolver.Versions()` が返す **OS 中立な version pin** で、 script resolver の declared tool 自身に加えて go-local の外部 Go module / pnpm-local の外部 npm package pin も含む ( `Tool` という呼称は narrow すぎ)。 Go 側 (`internal/sloff/toolresolver/resolver.go` の `ToolVersion` 型 ほか 30 箇所程度) も同 rename を後続項目として追従する
- **`google.protobuf.Timestamp`** で `generated_at` を表現
- **proto package 名に version segment を含める** (`sloff.cache.v1`)。 wire-incompatible な変更が必要になった時は新 package (`sloff.cache.v2`) を切る運用とし、 既存 v1 reader を壊さない
- **`schema_version` は enum で表現** し、 valid version のみ wire レベルで表現可能にする ( 不明値は `SCHEMA_VERSION_UNSPECIFIED` に集約)。 enum 値は version 番号と一致させ、 YAML 時代の `schema_version: 1` は proto enum には含めない

### byte stability の担保

proto wire format はデフォルトで決定論的でない (`proto.Marshal` は map 順序や unknown field 順序が実装依存)。 git にコミットする record で byte stability を保つために、 以下を **規約として固める**:

1. **`protoc-gen-go` / `google.golang.org/protobuf` を `go.mod` で pin**。 既存 `tool ( ... )` ブロック ( gofumpt / lefthook と同じ機構) に追加し、 `go tool buf generate` で再現可能に codegen する
2. **`proto.MarshalOptions{Deterministic: true}` を 1 関数経由でのみ呼び出す**。 直接 `proto.Marshal` を呼ぶことを禁止し、 lint / vet レベルでガードする
3. **schema 設計で `map<,>` を使わない** ( 上記 schema 規則の再掲)
4. **書き戻しスキップルール**: runner が record を書き出す前に既存 record を load し、 「semantic に同一」 ならば **書き込みをスキップする**。 これにより proto runtime の bit-level drift が起きても git diff には現れない
   - 「semantic に同一」 = `output.hash` が一致 かつ `output.files` の (path, hash) 集合が一致
   - `input.hash` は file 名 = key の構成要素なので必ず一致する
   - `generated_at` は drift しても書き戻しを誘発しない ( 「最初に観測した時刻」 を保持する解釈になる)
   - `input.resolved_versions` は通常 `resolved_versions_hash` 経由で `input.hash` に伝搬するため、 ここが drift する状況は基本的に input.hash も drift して別 key になる。 例外は informational `source` field のみが変わるケース (e.g. aqua から mise への乗り換えで version 文字列は同じだが source 表記が変わる) で、 この場合は **書き戻さない** ( informational drift のみで git に touch しない)
5. **proto runtime の major upgrade は `schema_version` bump = キャッシュ全 invalidate のイベント** として扱う。 minor / patch upgrade で起きうる micro byte drift は、 (4) の write-skip ルールで git に届かない (semantic 同一なら overwrite しない) 前提で運用する

### breaking change の自動検出

- **`buf` を `go.mod` の `tool ( ... )` ブロックで pin** ( gofumpt / lefthook と同じ機構)
- `buf.yaml` に `BREAKING_FILE` (= wire-incompatible 変更を禁ずる ruleset) を設定
- CI に `go tool buf breaking --against '.git#branch=main'` を追加し、 PR で wire-breaking な変更を弾く
- 「 wire-breaking な変更が必要になった場合」 は package を `sloff.cache.v2` に切る (= 新 reader/writer を実装し、 v1 を deprecated として一定期間並走させる) 運用ルールを併記する

### debug 経路

binary 化で失う inspect 容易性を、 同 PR で以下のサブコマンドで置換する ( 本 ADR の **必須 follow-up**):

- `sloff cache show <path>`: proto record を YAML / JSON にデコード出力 (人間可読)
- `sloff cache diff <path-a> <path-b>`: 2 record の semantic diff ( どの field が違うか)

加えて、 利用者向けの設定例として `git config diff.sloff-cache.textconv "sloff cache show"` の運用を docs に併記する ( ローカル `git diff` で decode 結果が見える)。

### `schema_version` 移行戦略

- 本 ADR 適用時に `schema_version` を **1 → 2 に bump** する ( 直列化 format が wire レベルで非互換のため)
- 利用者ゼロの段階での切替のため、 1 → 2 の **migration logic は実装しない**。 既存 YAML record は読まず、 全 record を再生成 ( miss → 通常の generator 実行) させる
- [Design Doc の Non-Goals](../design/architecture.md) にある 「record の `schema_version` 移行戦略 ( 初版は schema_version 1 固定、 将来 schema を変える必要が生じた段階で別途検討)」 は本 ADR で更新する

### File 拡張子

`.yml` → `.pb` に変更する。 `linguist-generated` 設定は `.sloff/cache/**` 配下の `*.pb` 全体を対象にできるよう `.gitattributes` を更新する。

## Consequences

### 正の影響

- record が **format レベルで** grep / 各種 search index / file indexer に opaque
- field 順 / 型 / 必須性が compile time で強制され、 手書き sort 規律への依存が解消
- wire-incompatible breaking change が CI で自動検出される ( 利用者を獲得する前に compat baseline が確立する)
- record サイズが二次効果として縮小
- proto package は S3 backend / 組織横断 cache 共有 / 将来の RPC 化で再利用できる資産になる

### 負の影響

- inspect 用サブコマンド (`sloff cache show` / `sloff cache diff`) を必須機能として追加する必要がある ( 本 ADR と同 PR スコープ)
- byte stability の担保が「Marshal 出力自体の決定論」 から 「Marshal 関数の単一化 + 書き戻しスキップ」 の合成に変わる ( 規律が単一箇所に集約され、 形は変わるが保証強度は下がらない)
- `protoc-gen-go` / `buf` を `go tool` 経由で pin する build pipeline 拡張が必要 ( 既存 gofumpt / lefthook と同じ機構)
- E2E goldens ( `internal/sloff/runner/testdata/`) は binary 比較になる。 `-update` フローと PR レビュー時の差分視認性は decode 経路で吸収する ( 後続実装で具体化)
- `goccy/go-yaml` への依存は spec パース側で残るが、 record 側からは外れる

### 後続の詳細設計

本 ADR の決定を受けて以下を実装する。 各項目は別途 PR / ADR で具体化する:

1. **proto schema 配置**: `proto/sloff/cache/v1/cache.proto` に SSoT、 `internal/proto/sloff/cache/v1/cache.pb.go` ( package `cachev1`) に generated code。 buf の `PACKAGE_DIRECTORY_MATCH` lint を満たすため out path も proto package 階層をミラーする
2. **Go 側 `ToolVersion` → `ResolvedVersion` rename**: `internal/sloff/toolresolver/resolver.go` の型定義および registry / golocal / pnpmlocal / 各 test の参照箇所 (約 30 箇所) を proto rename と同期。 `tools_hash` を参照する命名 (resolver doc / architecture.md) も `resolved_versions_hash` に揃える。 本 ADR と同 PR で実施する
3. **buf 設定**: `buf.yaml` ( BREAKING_FILE ruleset) / `buf.gen.yaml` を repo root に配置
4. **`go.mod` の `tool ( ... )` 追加**: `google.golang.org/protobuf/cmd/protoc-gen-go`、 `github.com/bufbuild/buf/cmd/buf`
5. **`internal/sloff/cache/record.go` の置換**: `Marshal` / `Unmarshal` を proto 経由に切替、 `proto.MarshalOptions{Deterministic: true}` を単一の helper に集約
6. **`internal/sloff/runner` の write-skip ルール**: 既存 record load → semantic 同一性チェック → 必要時のみ write の経路を実装
7. **`sloff cache show` / `sloff cache diff` サブコマンド**: cobra コマンドとして `cmd/sloff/` に追加
8. **CI 拡張**: `go tool buf generate` で `*.pb.go` の uncommitted check、 `go tool buf breaking --against '.git#branch=main'` の実行ステップ
9. **`docs/design/architecture.md` の更新**:
    - 「キャッシュレコード schema」 節を proto schema にリライト
    - Non-Goals の `schema_version` 1 固定文言を削除
    - File Layout の `<input_hash>.yml` を `<input_hash>.pb` に更新
    - Storage interface のコメントから 「deterministic YAML エンコード」 言及を削除
    - `tools_hash` の表記を `resolved_versions_hash` に統一
10. **ADR-0003 の更新**: 「PR ノイズの懸念」 節を、 grep ノイズも含めた format-level 解決に書き換える

### 撤回時の影響

採用後に proto への移行を撤回する場合、 record format が再度切り替わる ( 全 invalidate)。 利用者がつく前 ( 本 ADR 適用直後) は撤回コストが小さいが、 利用者を獲得した後の撤回は破壊的変更になるため、 採用判断は本 ADR 確定の段階で固める。
