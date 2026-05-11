# Storage: DynamoDB

`dynamodb.Storage` は fingerprint record の永続化先として AWS DynamoDB を選べるようにする `fingerprint.Storage` 実装。 [ADR-0011](../adr/0011-dynamodb-remote-fingerprint-storage.md) で採択した、 大規模 monorepo ( 1000-10000 タスク) 向け remote backend。

関連:
- [Architecture](./architecture.md) — 全体像と Storage interface の責務分離
- [ADR-0011: 大規模 monorepo 向けの remote fingerprint backend は DynamoDB を採用](../adr/0011-dynamodb-remote-fingerprint-storage.md)
- [ADR-0003: fingerprint のストレージ方式](../adr/0003-fingerprint-storage-strategy.md) — local backend ( 既定) の決定経緯
- [ADR-0009: fingerprint binary serialization](../adr/0009-fingerprint-binary-serialization.md) — wire format は backend 非依存
- [ADR-0010: fingerprint filename への timestamp prefix](../adr/0010-fingerprint-filename-timestamp-prefix.md) — local backend の path uniqueness 担保 ( DynamoDB では不要)

## Decision

### 全体方針

- record の wire format は `local` と完全に同じ ( ADR-0009 の deterministic protobuf binary)。 `Marshal` / `Unmarshal` は backend 共通
- DynamoDB 上は **1 record = 1 item** で per-item KV モデル。 manifest 集約はしない
- `(spec_relpath, task_id, input_hash)` を一意キーとして per-item ライフサイクル
- 認証は `aws-sdk-go-v2` の **default credential chain** に任せる ( env / shared config / IRSA / IMDS)
- 接続先設定 ( table / region / endpoint) は **`.sloff/config.yml`** に記載
- リモート backend には常に **2 段ローカルキャッシュ** を decorator として被せる ( オフライン対応 + 再 fetch 削減)
- runtime のアクセスは **bulk API** で行う。 起動時 1 回 BatchGetItem、 終了時にまとめて BatchWriteItem

### テーブルスキーマ

| 属性 | 型 | 役割 |
|---|---|---|
| `pk` ( partition key) | S ( String) | `spec_relpath` ( 例: `path/to/spec`) |
| `sk` ( sort key) | S ( String) | `<task_id>#<input_hash>` ( 例: `protoc-gen-go#3f9a1c...`) |
| `record` | B ( Binary) | `fingerprintv1.Record` の deterministic protobuf bytes ( ADR-0009) |
| `created_at` | N ( Number) | Unix epoch seconds、 record の write 時刻。 `ListFilter.OlderThan` のフィルタ基準として使う ( local backend の mtime に相当)。 常に書き込む |
| `expires_at` | N ( Number、 任意) | Unix epoch seconds、 DynamoDB TTL の対象属性 ( opt-in)。 `ExpiresAfterDays > 0` のときだけ書く |

#### partition key と sort key の設計理由

`fingerprint.Storage.List(ListFilter)` の絞り込み軸 ( SpecRelpath / TaskID) と DynamoDB の Query パターンを一致させるため、 PK / SK を分離する:

| `List` 呼び出し | DynamoDB 実装 |
|---|---|
| `ListFilter{SpecRelpath: "x"}` | `Query(pk=x)` で 1 partition |
| `ListFilter{SpecRelpath: "x", TaskID: "y"}` | `Query(pk=x, sk begins_with "y#")` |
| `ListFilter{}` ( 全件) | `Scan` ( GC 用、 通常 path では使わない) |

PK = spec_relpath は **書き込み負荷を spec 単位で自動分散** する。 sloff の運用想定 ( 100+ PR の CI 並走 × 数 task/spec) でも単一 partition の WCU 上限 ( 1000/sec) には届かない。 詳細は ADR-0011 の比較表参照。

別案 ( PK に task / hash まで含める) を退ける理由:

- PK = `<spec>#<task>#<hash>` にすると 1 item = 1 partition となり、 `Query(pk=spec)` のような spec 単位の集約が **Scan を強要される**。 GC 経路や inspect ツールが壊滅的に重くなる
- sort key を活用しないため柔軟性が下がる

#### 単一 hash 衝突への対策

`(spec_relpath, task_id, input_hash)` を主キーにすることで、 万一 input_hash 単独が異なる task 間で偶然一致しても item は別キーとして共存する。 ADR-0010 のような timestamp prefix は不要 ( DynamoDB は単一 SSoT で git merge と同型の競合が無い)。

#### TTL ( Time-To-Live)

DynamoDB の組み込み TTL 機能を **opt-in** で利用する。 利用者が `expires_after_days` を config で指定したときだけ、 Save 時に `expires_at = now + N days` を書き込み、 テーブル側で TTL を有効化する。

設計判断: **デフォルトは TTL 無効**:

- record サイズが小さく ( <1 KB)、 5000 task × 数十 input_hash variant でも 100 MB レンジ。 DynamoDB 無料枠 ( 25 GB) 内で長期持続できる
- TTL を有効にすると、 sloff の write-skip ( hit 時に Save しない) と組み合わさって **「 hit し続けている record も TTL 期限で消える」** 問題が起きる。 hit のたびに `UpdateItem(expires_at=...)` を投げるとコストが跳ねるので、 やらない
- 結果として TTL は 「長期間誰も触っていない record を一括掃除する」 用途で十分

TTL を opt-in にする組織への注記: TTL 期限が切れた record は次回 `sloff run` で **その task 1 回分の generator が再実行される** だけ。 generator が deterministic である前提なら出力は同一で、 git tree への影響なし。

### 操作 semantics

`fingerprint.Storage` interface の各メソッドの DynamoDB 実装:

| Method | semantics |
|---|---|
| `Name()` | `"dynamodb"` |
| `Load(ctx, key)` | `GetItem(pk=spec, sk=task#hash)` を **結果整合性 read** で発行。 0 件なら `(nil, false, nil)` |
| `Save(ctx, key, rec)` | `PutItem` で record bytes を書く ( opt-in TTL 有効時は `expires_at` も書く) |
| `Delete(ctx, key)` | `DeleteItem(pk, sk)` |
| `List(ctx, filter)` | filter 内容に応じて `Query` ( PK 指定あり) / `Scan` ( PK 指定なし)。 `OlderThan` は server-side filter expression |
| `CollapseDuplicates(ctx)` | DynamoDB は構造的に重複が起きないので **`(0, nil)` を即返す** |
| **`LoadMany(ctx, []Key)`** | 100 keys/call の `BatchGetItem` を 並列発行。 `UnprocessedKeys` の自動再投入 |
| **`SaveMany(ctx, []KeyRecord)`** | 25 items/call の `BatchWriteItem` を並列発行。 `UnprocessedItems` の自動再投入 |

#### bulk API の意義

per-key の Load/Save をタスク数だけ繰り返すと RTT 律速になる ( 1000 task で起動 1 秒以上)。 bulk API は:

- runner が起動時 1 回 `LoadMany(allKeys)` を呼び、 結果を in-memory map で保持
- per-task Lookup はメモリ map 参照のみ ( 0 RTT)
- per-task Save は accumulator に push、 run 終了時に `SaveMany` で一括 write

これで **タスク数 × 並列度 × RTT が 数 RTT に圧縮** される。

#### 結果整合性 read

DynamoDB の strongly consistent read は eventually consistent の倍コスト ( RCU 単価 2 倍)。 fingerprint cache は self-healing ( 取りこぼしは generator 再実行で補正) なので **デフォルト結果整合性で十分**。 取りこぼし確率は 「 直前 1 秒以内に書かれた record を別 run が読みにいったとき」 のみ。

#### 並走 write 競合

同一 (spec, task, input_hash) に対する複数 sloff run の同時 Save は **wire-byte 同一** ( deterministic) なので、 `PutItem` の last-write-wins で問題なし。 ConditionExpression による楽観ロックは不要。

### 2 段ローカルキャッシュ

remote backend ( DynamoDB) の **前段に必ず disk cache を被せる**。 local backend には被せない ( local backend 自身が disk なので二重キャッシュは無意味)。

#### 配置

```
$XDG_CACHE_HOME/sloff/fingerprints/<host>/<owner>/<repo>/<spec_relpath>/<task_id>/<input_hash>.pb
```

- `XDG_CACHE_HOME` の解決は OS ごとに分岐:
    - Linux: `os.UserCacheDir()` ( `$XDG_CACHE_HOME` set ならそれ、 未設定なら `~/.cache`)
    - macOS: 明示的に `$XDG_CACHE_HOME` を優先し、 未設定時は `~/.cache` にフォールバック ( `os.UserCacheDir()` のデフォルト `~/Library/Caches` は使わない)。 dotfile を Linux / macOS 共通で管理するユーザがレイアウトを揃えられるようにするため
    - Windows: `os.UserCacheDir()` ( `%LocalAppData%`)
- `<host>/<owner>/<repo>` は **リポジトリの remote URL から導出** ( ghq の path ルールに準拠)。 同一リポジトリの別 worktree でも同じパスを共有できる
- ファイルレイアウトは local backend の `.sloff/fingerprints/<spec>/<task>/` と揃える ( 但し timestamp prefix は不要、 last-write-wins)

#### URL パース

`git config --get remote.origin.url` を読んで `<host>/<owner>/<repo>` に分解する:

| 入力 | host | path |
|---|---|---|
| `git@github.com:izumin5210/sloff.git` | `github.com` | `izumin5210/sloff` |
| `https://github.com/izumin5210/sloff.git` | `github.com` | `izumin5210/sloff` |
| `ssh://git@github.com/izumin5210/sloff` | `github.com` | `izumin5210/sloff` |

末尾の `.git` は剥がす。 `origin` を decisive に使い、 複数 remote ( upstream 等) は無視。

#### 振る舞い ( decorator パターン)

```go
type cachedStorage struct {
    inner    fingerprint.Storage  // remote backend ( dynamodb)
    cacheDir string
}

// LoadMany: cache 優先、 miss だけ inner に問い合わせて埋める
func (c *cachedStorage) LoadMany(ctx, keys []Key) (map[Key]*Record, error) {
    cached, missing := c.readCache(keys)
    if len(missing) > 0 {
        fetched, err := c.inner.LoadMany(ctx, missing)
        if err != nil { return nil, err }
        c.writeCache(fetched)
        for k, v := range fetched {
            cached[k] = v
        }
    }
    return cached, nil
}

// SaveMany: cache と inner の両方に書く ( write-through)
func (c *cachedStorage) SaveMany(ctx, items []KeyRecord) error {
    if err := c.inner.SaveMany(ctx, items); err != nil { return err }
    c.writeCache(items)
    return nil
}
```

#### 性質

- **自動 invalidate**: input_hash が変わると新しい cache ファイル名になるので、 古いキャッシュは hit しなくなるだけ ( 整合性問題は起きない)
- **破損時**: decode エラーは miss として扱い、 inner から再取得して上書き
- **同時書き込み**: tmp ファイル + rename で atomic write。 同 key への並走 write は内容同一で無害
- **CI 環境**: cache ディレクトリが揮発するなら毎回 inner から読む ( 期待通り)
- **GC**: 既存 record は未使用でもストレージ上に残る。 サイズが問題になったら `rm -rf $XDG_CACHE_HOME/sloff/` で OK ( 後で `sloff fingerprint gc-cache` 等を整備する余地)

#### キャッシュレイヤの ON/OFF

リモート backend を選んだら **常時 ON** がデフォルト。 「 一時的に cache を skip して remote を直接見る」 「 fingerprint そのものを bypass して全 task 強制実行」 の override は別途 [DEV-22](https://linear.app/izumin/issue/DEV-22/...) で扱う。 本 doc では実装しない。

### 設定ファイル `.sloff/config.yml`

```yaml
fingerprint:
  backend: dynamodb         # local | dynamodb ( 省略時 local)
  dynamodb:
    table: sloff-fingerprints       # 必須
    region: us-east-1               # 任意 ( default: AWS_REGION 等から解決)
    endpoint: ""                    # 任意 ( emulator 接続用、 default: SDK 標準解決)
    expires_after_days: 0           # 任意 ( default 0 = TTL 無効、 >0 で有効化)
```

認証情報は config に書かない ( `.sloff/config.yml` は git にコミットされる前提)。

### テーブル作成

sloff は **テーブルを自動作成しない**。 利用者は事前に以下のいずれかでテーブルを作る:

#### AWS CLI

```bash
aws dynamodb create-table \
  --table-name sloff-fingerprints \
  --attribute-definitions AttributeName=pk,AttributeType=S AttributeName=sk,AttributeType=S \
  --key-schema AttributeName=pk,KeyType=HASH AttributeName=sk,KeyType=RANGE \
  --billing-mode PAY_PER_REQUEST
```

#### Terraform

```hcl
resource "aws_dynamodb_table" "sloff_fingerprints" {
  name         = "sloff-fingerprints"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "pk"
  range_key    = "sk"

  attribute { name = "pk"; type = "S" }
  attribute { name = "sk"; type = "S" }

  ttl {
    attribute_name = "expires_at"
    enabled        = true   # config で expires_after_days を有効化する組織のみ
  }
}
```

理由: テーブル作成 / IAM / TTL 設定は組織のインフラ管理プロセスに乗せるべきで、 sloff CLI が CreateTable 権限を要求するのは権限拡張すぎる。

### 必要 IAM 権限

利用者の credential に必要な権限:

```
dynamodb:GetItem
dynamodb:PutItem
dynamodb:DeleteItem
dynamodb:Query
dynamodb:Scan        ( CollapseDuplicates / List 全件用、 通常 path では使わない)
dynamodb:BatchGetItem
dynamodb:BatchWriteItem
dynamodb:DescribeTable  ( startup 時の存在確認、 任意)
```

特定テーブル ARN に絞る IAM Policy 例:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:DeleteItem",
      "dynamodb:Query", "dynamodb:Scan",
      "dynamodb:BatchGetItem", "dynamodb:BatchWriteItem",
      "dynamodb:DescribeTable"
    ],
    "Resource": "arn:aws:dynamodb:*:*:table/sloff-fingerprints"
  }]
}
```

### ランタイムのアクセスパターン

#### Run 開始時 ( runner.Run の冒頭)

1. discovered specs と resolver Inputs から全 task の `(spec_relpath, task_id)` を確定
2. 各 task の `input_hash` を計算 ( files_hash + cmd_hash + resolved_versions_hash の合成)
3. `[]fingerprint.Key` を組み立て、 `Storage.LoadMany(ctx, keys)` を 1 回呼ぶ
4. cachedStorage が disk cache を見て、 miss だけ DynamoDB に BatchGetItem
5. 結果 `map[Key]*Record` をメモリ保持

#### per-task Lookup ( runTask 内)

- メモリ map 参照のみ。 0 RTT
- 既存の output-comparison ロジック ( ADR-0002) はそのまま動く

#### per-task Save

- generator 実行後、 新 record を runner ローカルの accumulator に push
- ( 同期 Save は実装しない。 中断時の損失は再 run で復元される性質を活かして、 end-of-run バッチに倒す)

#### Run 終了時

- accumulator から `[]KeyRecord` を組み、 `Storage.SaveMany(ctx, items)` で一括 write
- cachedStorage が disk + DynamoDB の両方に write-through

### コスト

ADR-0011 の Comparison Table 参照。 仮定: 10,000 task / 100 run/日 / record 平均 0.5 KB:

| 項目 | コスト |
|---|---|
| 読み込み ( BatchGetItem、 結果整合性、 100 万 reads/日) | 月 $3.75 |
| 書き込み ( BatchWriteItem、 miss 20% で 200,000 writes/日) | 月 $7.5 |
| ストレージ ( 50 MB) | 無料枠内 |
| **合計 ( on-demand)** | **約 $11/月** |

下げる手:

- 2 段ローカルキャッシュで remote read を 80% カット → 月 $3 程度
- Provisioned + Reserved Capacity で安定運用パターンが見えたら更に削減

### 操作 / 観測

- 起動時 / 終了時の bulk RTT は OpenTelemetry span で計測 ( 既存の `runner.fingerprint.load` / `save` span を流用)
- `cachedStorage` は cache hit / miss を span attribute として記録 ( debug / 性能評価用)
- `--explain` 経路で 「 cache hit / DynamoDB hit / 生成」 のどこに当たったかを表示 ( 既存 explain サブコマンド拡張、 別 PR)

## File Layout ( 実装側)

```
internal/sloff/fingerprint/
  storage.go                  # Storage interface ( bulk API 追加)
  record.go                   # 既存 ( Marshal / Unmarshal / Sort)
  config.go                   # .sloff/config.yml パース ( dynamodb section 追加)
  factory.go                  # backend dispatch
  cached/                     # ★ 新規: 2 段ローカルキャッシュ decorator
    cached.go                 # cachedStorage 実装
    repopath.go               # XDG + git remote URL から cache path 導出
    cached_test.go
  local/local.go              # 既存 ( bulk API は per-key を errgroup で並列)
  dynamodb/                   # ★ 新規 backend
    dynamodb.go               # Storage 実装
    keys.go                   # PK / SK 組み立て / 解析
    dynamodb_test.go          # kumo を使った integration test
```

`cmd/sloff` の builder は config から `backend = dynamodb` のとき:

```go
inner, err := dynamodb.New(ctx, cfg.DynamoDB)
if err != nil { return nil, err }
return cached.New(repoRoot, inner)
```

local backend は cached でラップしない:

```go
return local.New(repoRoot)
```

## テスト

### kumo 利用方針

S3 PR で確立した kumo + `go tool` 起動の TestMain 構造を流用。 kumo は DynamoDB API ( CreateTable / Query / BatchGetItem / BatchWriteItem / TTL) を網羅していることを事前確認済み。

`internal/sloff/fingerprint/dynamodb` に integration test:

- DynamoDB Local ( kumo) を `TestMain` で起動
- 各 test で per-test テーブルを作成 / 破棄
- `LoadMany` / `SaveMany` / `Load` / `Save` / `Delete` / `List` / `CollapseDuplicates` の挙動確認
- BatchGet/BatchWrite の `UnprocessedKeys` / `UnprocessedItems` 再投入経路は **mock** で確認 ( kumo がそこまでスロットルしないため)

### キャッシュ層のテスト

`internal/sloff/fingerprint/cached` 単体テスト:

- in-memory mock backend を inner に差し込み、 cache hit/miss / write-through / corruption recovery を確認
- repo path 導出 ( git remote URL → `<host>/<owner>/<repo>`) の単体テスト
- 実際のキャッシュディレクトリは `t.TempDir()` で隔離

### local backend の bulk API

既存 `local_test.go` に `LoadMany` / `SaveMany` のテストを追加。 動作は per-key の繰り返しを並列化したのみで、 既存挙動を破壊しないことを確認する。

## Migration / 互換性

- `fingerprint.Storage` interface に `LoadMany` / `SaveMany` を追加 ( 既存メソッドは維持)。 既存 backend ( local) には trivial 実装を生やすので **後方互換的な追加**
- `.sloff/config.yml` は新規ファイル ( S3 PR で導入する予定だった構造を引き継ぐが、 `s3` ブロックは作らず `dynamodb` のみ)
- record 自体の wire format は不変 ( ADR-0009)

## Open Questions

- **Q1**: 起動時の `LoadMany` 並列度の最適値。 BatchGetItem の 100 keys/call を 50-100 並列で投げる前提だが、 sloff の他 I/O ( files hash 計算等) との競合と合わせた実測が要る
- **Q2**: TTL 機能を「いずれの利用者でも無効、 オプトイン」 にしたが、 「組織標準で TTL 365 日にしたい」 ようなニーズが出てきたとき、 config だけで運用できるか / テーブル作成側 ( terraform 等) と二重設定になるかの整理
- **Q3**: cached backend の disk 上限 / 自動 GC ポリシー。 単純な mtime 古い順で trim する `sloff fingerprint gc-cache` を将来追加するか、 必要になるまで放置で良いか
- **Q4**: `Storage.List(ListFilter{})` の Scan は GC 用途のみだが、 大規模テーブルで Scan のコストが顕在化するシナリオがあるか。 必要なら GSI の追加 ( e.g. `created_at` を per-spec partition で並べる) を後追いで検討
