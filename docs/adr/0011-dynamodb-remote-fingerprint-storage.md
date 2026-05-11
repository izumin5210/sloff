# ADR-0011: 大規模 monorepo 向けの remote fingerprint backend は DynamoDB を採用

## Context

### 背景

[ADR-0003](./0003-fingerprint-storage-strategy.md) では fingerprint record を **git per-task per-input ファイル ( local backend)** で管理する方針を採用し、 S3 等のリモートストレージは Option C として将来候補に置いていた。 ADR-0003 採択時の前提は 「典型的 monorepo 規模 ( タスク数 数百程度)」 で、 local backend だけで運用が成立する。

その後、 sloff の利用が想定される実運用において以下が確認された:

- 既知の monorepo に **400 タスク級** が存在し、 タスク数は今後も成長する見込み ( 1000 超を前提に置く)
- 開発者 / CI 間で fingerprint を共有したい組織は実際に存在する。 git で共有する local backend の運用負荷 ( PR diff ノイズ / clone size / `git status` スループット低下) が、 規模が拡大するにつれて顕在化する

リモート backend の選定にあたって S3 を最初に検討したが ( PR #28、 closed)、 タスクごとの per-key API ( ListObjectsV2 + GetObject) は **タスク数に対して RTT が線形に増える** ため、 1000+ タスクで開発体験が壊れることが事前検証で判明した。 manifest 集約 ( single / per-spec) で改善する案も検討したが、 hot key 競合 / 接続数 / 将来 50,000+ タスクへの拡張余地で十分でなかった。

### 制約 / 評価軸

リモート backend に求める性質を改めて整理する:

- **Sa**: 1000-10000 タスク規模で **sub-second の lookup 完了** ( 起動 RTT が線形ではない)
- **Sb**: 同時に走る複数 sloff run ( 開発者 + 多 PR の CI) の write 競合に耐える
- **Sc**: 利用者の AWS 認証基盤 ( IAM / IRSA / SSO) にそのまま乗る
- **Sd**: AWS の標準サービスで、 利用者が運用習熟しているか習熟しやすい
- **Se**: コストが小規模組織で月数ドル、 中規模で月数十ドル程度に収まる
- **Sf**: 既存の `fingerprint.Storage` interface を抽象境界として保ち、 multi-cloud 展開時に backend 単位で実装追加できる

## Considered Options

### Comparison Table

| | A: S3 per-task object ( PR #28、 棄却済み) | B: S3 single manifest | C: S3 per-spec manifest | **D: DynamoDB per-item ( 採用)** | E: Redis / 自前 HTTP API |
|---|---|---|---|---|---|
| 1000 tasks の lookup RTT | List+Get/parallel = 線形 | 1 GET + 大きい body | List + 並列 Get | BatchGetItem 並列 | sub-ms |
| 5000 tasks 規模 | 数十秒 | manifest 10 MB で動く | 並列度依存で数秒 | サブ秒 | サブ秒 |
| 10000 tasks 規模 | 1 分超 | manifest 20 MB | 並列度限界 | サブ秒 | サブ秒 |
| 50000 tasks スケール余地 | × | manifest 100 MB ( 起動 1 秒超) | △ | ◎ | ◎ |
| 並走 write 競合 | 別 key で問題なし | 単一 hot key + ETag retry | spec 別に分散 | partition key で構造的に分散 | 単一サーバ |
| AWS 認証統合 | ◎ | ◎ | ◎ | ◎ | × |
| 利用者の運用習熟 | ◎ | ◎ | ◎ | ◎ | × ( server 立てる責任) |
| コスト ( 10k task × 100 run/day) | 約 $0.05/月 | 約 $0.05/月 | 約 $0.10/月 | 約 $11/月 ( on-demand) | inst $20+/月 |

### Option A: S3 per-task object ( PR #28 で実装後棄却)

`<prefix>/<spec>/<task>/<TS>-<input_hash>.pb` 1 オブジェクト 1 record。 ADR-0010 の path uniqueness をそのまま S3 に持ち込んだ。

👍 **Pros**
- 実装最小、 local backend と layout が揃う
- timestamp prefix で並走 write 衝突なし

👎 **Cons**
- **タスクごとに ListObjectsV2 + GetObject の 2 RTT**。 1000 タスクで RTT × 1000 / 並列度。 50ms RTT 同一リージョンで 200 タスク並列 = 1 タスクあたり 100ms 強。 数十タスク以上の規模で線形悪化が顕在化
- スケール ceiling が低い

### Option B: S3 single manifest

`<prefix>/manifest.pb` に全 record を `repeated Record` として格納。 起動時 1 GET、 終了時 1 PUT ( If-Match ETag で楽観的並行制御)。

👍 **Pros**
- 起動 RTT は 1 回
- PC 負荷最小 ( 1 socket)
- 実装簡素

👎 **Cons**
- 単一 hot key に全 write が集中。 多 PR の CI 並走で **ETag retry 連鎖**
- manifest size が record 数線形 ( 5000 task で 10 MB、 50000 task で 100 MB)。 起動転送遅延が伸びる
- 将来 sharding を入れると S3 の旨味 ( 単一 object simplicity) が消える

### Option C: S3 per-spec manifest

`<prefix>/<spec>/manifest.pb` に spec 単位でまとめる。 spec 数で write 競合を分散。

👍 **Pros**
- spec 別に競合分散
- B より scale 余地

👎 **Cons**
- 起動時に **spec 数分の GetObject** ( 1500 spec で並列度 50 → 30 ラウンド)。 PC 負荷は per-task より軽いが per-item KV ストアより重い
- 5000-10000 タスク規模では D に対する優位性が立たない

### Option D: DynamoDB per-item ( 採用)

各 (spec, task, input_hash) を 1 item として保存。 partition key = `spec_relpath`、 sort key = `<task_id>#<input_hash>`。 BatchGetItem ( 100 items/call) / BatchWriteItem ( 25 items/call) でまとめて I/O。

👍 **Pros**
- BatchGet/BatchWrite で 1000-10000 タスクが **sub-second** で完了
- partition key を spec にすることで write が **構造的に分散** ( hot partition の心配が事実上ない)
- sloff の `Storage.List(filter)` semantics と DynamoDB の Query パターンが一致
- DynamoDB **TTL** 機能で WCU 課金なしの自動 GC ( opt-in)
- sloff の record サイズ ( <1 KB) と RCU/WCU 単位が自然にマッチ
- Reserved Capacity / Provisioned で本番運用最適化の余地あり

👎 **Cons**
- AWS 専用 ( multi-cloud は別 backend 実装で対応する前提なので問題なし、 [ADR-0003](./0003-fingerprint-storage-strategy.md) §"Storage interface" の方針通り)
- テーブル作成 / IAM 設定の初期セットアップが利用者責務
- コストが S3 系より一桁高い ( 月 $10 オーダー、 規模に応じて伸びる)。 ただし絶対額は小さく、 2 段ローカルキャッシュ併用で更に下げられる

### Option E: Redis / 自前 HTTP API

ElastiCache / 自前 KV サーバ / Bazel-style HTTP cache をフロントに置く。

👍 **Pros**
- sub-ms レイテンシ
- 完全な multi-cloud 中立

👎 **Cons**
- 利用者が server を運用する責任 ( ADR-0003 が S3 の Cons として挙げた 「外部 infra 運用責務」 の最強版)
- sloff のように 「フットプリント小さく、 OSS 配布」 を狙うツールと運用思想が合わない

## Decision

**Option D: DynamoDB を remote fingerprint backend として採用する。**

採用根拠の論理連鎖:

1. **規模の前提が ADR-0003 採択時から変化した**。 「タスク数 数百」 を超えて 1000-10000 を視野に入れる必要が生じている
2. S3 系 ( Option A/B/C) は 「object storage を fingerprint cache の用途で使う」 こと自体が **本質的にミスマッチ** ( object storage は大きな blob を低頻度に出し入れする workload 向け、 fingerprint は逆)
3. DynamoDB の **per-item key-value + BatchGet/BatchWrite** モデルは fingerprint cache の access pattern と構造的に一致する。 partition key 設計 ( spec_relpath) で hot partition リスクがゼロに近い
4. AWS の標準サービスで、 ADR-0003 が 「S3 backend」 で要求した運用前提 ( AWS 認証 / 利用者の習熟) をそのまま満たす
5. 月 $10 オーダーのコストは 「OSS / 内部ツール」 として許容範囲。 2 段ローカルキャッシュ併用で更に下げられる

### local backend は引き続き既定

ADR-0003 が定めた local backend ( git per-task per-input ファイル) は **sloff の既定として変更しない**。 リモート共有が必要な組織だけが `.sloff/config.yml` で `backend: dynamodb` を opt-in する。

### Storage interface の bulk 化

DynamoDB を活かすには `Storage` interface を per-key だけでなく **bulk API** ( `LoadMany` / `SaveMany`) に拡張する必要がある。 これは backend を跨いで意味がある変更で、 local backend にも実装する ( per-key を errgroup で並列実行する trivial な実装)。

### 2 段ローカルキャッシュ

オフライン対応 ( 飛行機 / 出張中) と同一 record の再 fetch 削減のため、 リモート backend の前段に **`$XDG_CACHE_HOME/sloff/fingerprints/<host>/<owner>/<repo>/...` のディスクキャッシュ** を decorator として被せる。 local backend には被せない。

詳細は [Design Doc: Storage backend (DynamoDB)](../design/storage-dynamodb.md) を参照。

## Consequences

### 正の影響

- 1000-10000 タスク規模の monorepo で remote fingerprint 共有が実用に耐える
- partition key 設計で並走 write 競合を構造的に回避できる
- DynamoDB TTL で GC を運用コストゼロにできる
- `Storage` bulk interface への拡張は将来の backend 実装にとっても基盤になる
- 2 段ローカルキャッシュは backend 非依存で動くので、 将来 GCP / Azure backend を入れたときにも流用できる

### 負の影響

- AWS 専用 backend が増えることで、 multi-cloud 展開時には GCP / Azure 用に別 backend を実装する必要がある ( ADR-0003 が想定した抽象境界の通り、 想定内)
- 利用者は DynamoDB テーブルの作成 / IAM ポリシー設定 / ( opt-in 時) TTL 設定を担う
- 月 $10 オーダーのランニングコストが発生する ( S3 系より一桁高いが絶対額は小さい)
- 既存の `fingerprint.Storage` interface に `LoadMany` / `SaveMany` を追加する変更がある ( ただし既存 backend 実装を破壊しない後方互換的な追加)

### 後続の詳細設計

- [Design Doc: Storage backend (DynamoDB)](../design/storage-dynamodb.md): スキーマ / API 呼び出しパターン / 2 段ローカルキャッシュ / config / テスト方針
- [Architecture](../design/architecture.md): backend 一覧の更新 ( DynamoDB 追加、 S3 を含めない方針を反映)
