# ADR-0010: fingerprint filename への timestamp prefix

## Context

### 背景

[ADR-0003](./0003-fingerprint-storage-strategy.md) で record を git per-task per-input ファイル方式 (`.sloff/fingerprints/<spec_relpath>/<task_id>/<input_hash>.pb`) で配置することを確定し、 R5 「コンフリクト無し」 を採用根拠の中核に据えた。 [ADR-0009](./0009-fingerprint-binary-serialization.md) で proto binary 直列化に切替えた際、 R5 の達成手段は実質的に **「同一 input から同一 byte 列を生成する規約」 + 既存 record に対する書き戻しスキップルール** に依存する形となった。

しかしこの依存構造には別 branch で independently に initial write される ケースの穴が残っている:

- 開発者 A が branch A で `X.pb` を初回生成 (`generated_at = T1`)
- 開発者 B が branch B で同 input の `X.pb` を初回生成 (`generated_at = T2`)
- 双方マージ時、 同 filename `X.pb` の bytes が `generated_at` の差で異なる → git merge conflict

ADR-0009 §"byte stability の担保" の write-skip ルールは「既に local に record が存在する場合のみ」効くため、 上記 first-write 同士の競合は射程外。 R5 を「規約 + 局所的 write-skip」 で達成しているため、 schema に壁時計依存 / 環境依存 field が一つでも混入すると競合が復活する fragility がある。

本 ADR では R5 の達成手段を **規約レベルから layout 不変条件レベルに格上げする**:filename 自体に initial creation timestamp を prefix することで、 異なる writer が独立に初回書き込みしても **path uniqueness で構造的に conflict-free** にする。 副次的に schema 側から壁時計依存 field (`generated_at`) を排除でき、 byte stability への運用負荷が下がる。

### 制約 / 評価軸

- **R5 (コンフリクト無し)**: 別 branch で independently に initial write された record が衝突しない
- **ファイル数の有界性**: 同一 (spec, task, input) Key に対して record ファイルが累積しない
- **schema 進化への robust 性**: 将来 field 追加で R5 が再度規約依存に戻らない
- **外部 contract の維持**: `fingerprint.Key` (3-tuple) → record の lookup 契約は変えない
- **debug 容易性**: filename から initial creation 時刻が判別できる
- **test 容易性**: clock を test で固定可能

### References

- [ADR-0003: fingerprint のストレージ方式](./0003-fingerprint-storage-strategy.md)
- [ADR-0009: fingerprint の直列化形式 (protobuf binary)](./0009-fingerprint-binary-serialization.md)
- [Design Doc: sloff Architecture](../design/architecture.md)

## Considered Options

### Comparison Table

| | A: 現状 (`{hash}.pb` + 規約) | B: `generated_at` 削除 | **C: timestamp prefix (採用)** | D: append-only event log |
|---|---|---|---|---|
| R5 達成手段 | 規約 (byte equivalence) | 規約 (byte equivalence) | **構造 (path uniqueness)** | 構造 (path uniqueness) |
| 別 branch initial write 衝突 | × ( `generated_at` で破綻) | △ ( `source` field 等の他 drift 源で破綻しうる) | ◎ | ◎ |
| schema 進化への robust 性 | × ( 壁時計依存 field を入れた瞬間に破綻) | × ( 同上) | ◎ ( layout で守られる) | ◎ |
| Key あたりファイル数 | 1 | 1 | 1 ( 通常) / 一時的に N ( merge 直後) | 累積 |
| 外部 contract | 3-tuple → record | 3-tuple → record | 3-tuple → record (latest wins) | 3-tuple → 履歴 |

### Option A: 現状維持

`generated_at` 由来の bytes drift がある限り R5 が破綻する。 棄却。

### Option B: `generated_at` field 削除のみ

`generated_at` を proto wire から外し、 同一 input → 同一 wire bytes を取り戻す案。 **理論上は R5 を成立させ得る** が、 以下の構造的脆弱性を抱える:

- 現 schema には `ResolvedVersion.source` という別の drift 源が残る (e.g. aqua → mise 乗り換えで version 文字列は同じだが source 表記が変わる、 ADR-0009 §"byte stability の担保" §4 で述べた exception ケース)。 完全に conflict-free にするには `source` も削除が必要
- proto runtime の minor / patch 差で micro byte drift が発生しうる ( ADR-0009 §"byte stability の担保" §5 の write-skip ルールは local 既存 record でしか効かない)
- 将来の field 追加で「これは drift しないか」 の review 圧力が恒久的に発生する。 一度の見落としで fingerprint 全体のマージコンフリクトが復活する

R5 を 「規約」 で守り続ける構造は、 sloff の成長に伴って恒常コストとして積み上がる。

### Option C: timestamp prefix (採用)

filename を `{YYYYMMDDHHMMSSsss}-{hash}.pb` に変える。 timestamp は **そのファイルの initial creation 時刻** で、 in-place 上書きでも保持される (内容更新時の disambiguator ではなく path-level nonce)。

- 別 branch initial write は **filename がそもそも違う** ため git は両方を別ファイルとして共存させる。 内容に関わらず構造的に conflict-free
- in-place 上書きで filename が変わらないため、 通常運用では Key あたり 1 ファイル
- merge 直後だけ N 件併存 → GC で 1 件に収斂 ( 詳細は後述)
- schema 側の byte equivalence への依存が解け、 `generated_at` を削除しても運用負荷増にならない (むしろ削除前提の設計)

### Option D: append-only event log

Save 毎に `{ts}-{hash}.pb` を新規作成する案。 R5 / schema robust 性は満たすが Key あたりファイル数が無制限に増える。 sloff scope (deterministic generator) では 同 input → 同 output で履歴を残す価値が薄く、 GC 負担だけ増える。 棄却。

## Decision

**Option C: filename への timestamp prefix を採用する。**

採用根拠:

1. **R5 の達成手段を規約 → layout 不変条件に格上げ**。 schema 進化に robust になる
2. **`generated_at` field を proto schema から削除可能**になる。 ADR-0009 で informational として残していた wall-clock field が filename に migration され、 wire bytes に壁時計が乗らない構造になる
3. **外部 contract (`fingerprint.Key` 3-tuple) は変えない**。 latest-wins ルールで「同 Key → 1 record」 という呼び出し側の契約を維持
4. **deterministic generator scope 下で latest-wins は correctness を損なわない**。 同 input から作られた複数 record は意味的に等価
5. **ファイル累積を回避**: in-place 上書きで通常時は Key あたり 1 件、 merge 直後の一時的併存は GC で収斂

### Filename format

```
.sloff/fingerprints/<spec_relpath>/<task_id>/{YYYYMMDDHHMMSSsss}-{hash}.pb
```

- `YYYYMMDDHHMMSSsss` は millisecond 精度の固定桁 (17 桁)。 lexicographic 比較で chronological 順になる
- prefix と hash 部の区切りは `-` 1 文字
- timestamp は **initial creation 時刻** で、 同一 file の in-place 上書きでは変わらない

### Save semantics

```
existing := list("*-{hash}.pb")
if len(existing) == 0:
    write("{now}-{hash}.pb", bytes)             // 0件: 新規作成
else:
    target := existing[earliest]                 // 1+件: 最古を残して collapse
    delete(existing[others])
    overwrite(target, bytes)
```

- **0 件 case** ( dominant): 完全初回書き込み、 もしくは input 変化で別 hash になった結果として新 Key の初回
- **1+ 件 case** ( rare): 同 input で output が変わった ( = non-deterministic generator = sloff scope 違反) か、 merge で一時的に複数併存した状態で再生成が発生した稀な経路

deterministic generator 前提下では 1+ 件 case で Save が fire する経路は実質起きない (write-skip ルールで吸収される) が、 defensive に collapse + 上書きで処理する。

### Load semantics

```
existing := list("*-{hash}.pb")
if len(existing) == 0:
    return not_found
return existing[latest]                          // 最新 timestamp の 1 件を返す
```

副作用なし。 検証なし。 deterministic generator 前提下で複数 record は意味的に等価のため latest を arbitrary に選ぶ。

### `generated_at` field の削除

filename prefix が initial creation timestamp を担うため、 `Record.generated_at` は redundant となり削除する。 ADR-0009 §"byte stability の担保" §4 で残した「最初に観測した時刻を保持する」 解釈は filename に migrate される。

debug 上の生成時刻情報が必要なら filename と git log から取得できるため、 informational field を schema に残す動機は無い。

### Schema version bump

`generated_at` 削除は wire-incompatible 変更のため、 `schema_version` を V2 → V3 に bump する。 ADR-0009 同様 「migration logic は実装しない」 方針を踏襲し、 V2 record は invalid として扱う ( 利用者ゼロ前提下では実質ゼロコスト)。

### `fingerprint.Key` および Storage interface

`fingerprint.Key` 構造体 (`SpecRelpath`, `TaskID`, `InputHash`) は **変更しない**。 timestamp は Storage 実装内部で生成・ 保持し、 外部からは 3-tuple → record の lookup として振る舞う。

```go
// fingerprint/storage.go (interface 不変)
type Storage interface {
    Save(ctx context.Context, key Key, record *fingerprintv1.Record) error  // timestamp は実装内で生成
    Load(ctx context.Context, key Key) (*fingerprintv1.Record, bool, error) // latest を返す
    Delete(ctx context.Context, key Key) error                        // 全 timestamp 分削除
    List(ctx context.Context, filter ListFilter) ([]Key, error)       // duplicate は dedupe して返す
    Name() string
}
```

Storage local 実装内部に **clock 抽象** を持たせ、 test で固定可能にする (`func() time.Time` の関数注入)。

### duplicate collapse の責務

merge 直後に複数 `*-{hash}.pb` が併存した状態は、 deterministic generator 前提下では Save 経路 (output 変化) が発火しないため Save 内 collapse では収斂しないことが多い。 そのため:

- **Save 経路の collapse は defensive**: たまたま Save が fire したらついでに collapse
- **`sloff fingerprint gc` が primary な収斂経路**: 同 Key の `*-{hash}.pb` を最古 1 件に collapse する処理を追加する

input が変わって別 hash に移行した場合、 旧 hash の duplicate は孤立した stale record として残るが、 これは既存 mtime-based GC ポリシーの責務 ( ADR-0003 §ゴミ ( 古い record) の扱い)。

### byte stability ルールの再定式化

ADR-0009 §"byte stability の担保" の各規約は本 ADR で次のように再整理される:

1. **`Deterministic: true` の単一呼び出し点維持**: 変更なし
2. **schema 設計で `map<,>` 禁止**: 変更なし
3. **proto wire の repeated 要素は marshal 前に sort**: 変更なし
4. **書き戻しスキップルール**: 規約としては残す ( proto runtime micro drift に対する safety net) が、 R5 の達成は本 ADR の path uniqueness が primary に担う。 既存 in-place 上書き経路で意味を持つ
5. **proto runtime の major upgrade で全 invalidate**: 変更なし

### test 容易性

E2E goldens は filename に timestamp が含まれるため、 比較時に正規化が必要:

- 推奨: E2E test 内で clock を固定値に注入する。 これにより golden filename も決定論的になり、 既存の byte 比較インフラがそのまま使える
- 代替: golden 比較時に filename の timestamp 部を `{TIMESTAMP}-` 等で mask する

clock 注入は test だけでなく将来の reproducibility 検証にも使えるため、 こちらを採用する。

## Consequences

### 正の影響

- R5 が schema 内容に依存しない layout 不変条件で守られるようになり、 schema 進化が robust になる
- `generated_at` 削除で proto wire bytes が input から完全 deterministic に派生 ( `source` field の drift も書き戻しスキップで吸収済み、 ADR-0009 §4)
- 別 branch initial write 衝突が構造的に消える ( `generated_at` 以外の drift 源も同時に解消)
- filename から initial creation 時刻が直接読み取れる ( debug 容易性 +)

### 負の影響

- `fingerprint.Storage` の Local 実装が directory listing ベースになり、 計算量が dir 内ファイル数に対して O(N) になる。 通常運用では Key あたり 1 ファイルなので影響は無視可能
- E2E goldens の filename に timestamp が含まれ、 clock 注入 (or mask) が必須に。 既存 testdata の再生成が必要
- `sloff fingerprint gc` に duplicate collapse 処理を追加する必要がある
- merge 直後の一時的な ファイル併存状態が PR diff に現れる (集合論的には R5 違反ではないが、 視覚的ノイズ)。 GC で収斂するため恒常的にはならない

### schema_version 移行

ADR-0009 同様、 V2 → V3 の migration logic は実装しない。 既存 V2 record は invalid として扱い、 利用者は再生成で V3 に移行する ( fingerprint miss → 通常 generator 実行)。

### 後続の更新

本 ADR の決定を受けて以下を更新する:

1. [ADR-0003](./0003-fingerprint-storage-strategy.md) §"Decision" §"PR ノイズ / grep ノイズの懸念": R5 の達成手段が path uniqueness に格上げされた旨を追記
2. [ADR-0009](./0009-fingerprint-binary-serialization.md): `generated_at` 削除、 schema_version V3 bump、 §"byte stability の担保" の write-skip ルールを「primary が本 ADR の path uniqueness、 micro drift 救済が write-skip」 に再整理
3. [Design Doc](../design/architecture.md):
    - §"ファイル配置規則" の filename を `{YYYYMMDDHHMMSSsss}-{hash}.pb` に更新
    - §"Schema (protobuf)" の JSON 例から `generated_at` を削除
    - §"Cache lookup アルゴリズム" の filename 表現を更新
    - §"Storage interface" のコメントに clock 注入 / list-based Save / Load を反映
4. proto schema (`proto/sloff/fingerprint/v1/fingerprint.proto`): `generated_at` field 削除、 `SCHEMA_VERSION_V3` 追加
5. Storage local 実装 (`internal/sloff/fingerprint/local/local.go`): clock 抽象、 list-based Save / Load、 collapse ロジック
6. runner (`internal/sloff/runner/runner.go`): `Record.GeneratedAt` assignment 削除、 write-skip rule のコメント更新
7. `sloff fingerprint gc` (`cmd/sloff/fingerprint.go`): duplicate collapse 処理追加
8. E2E ( `internal/sloff/runner/runner_test.go` ほか): clock 注入経路を E2E にも通す、 全 goldens を `-update` で再生成、 新規 case `concurrent-first-write-merge` を追加

### 撤回時の影響

採用後に timestamp prefix を撤回する場合、 record format が再度切り替わる ( 全 invalidate)。 利用者ゼロの段階での切替のため、 撤回コストは小さい。 採用判断は本 ADR で固める。
