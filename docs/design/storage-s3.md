# Storage: s3

`s3.Storage` は fingerprint record の永続化先として S3 互換 object storage を選べるようにする `fingerprint.Storage` 実装。 [ADR-0003](../adr/0003-fingerprint-storage-strategy.md) で Option C ( S3 等リモートストレージ) として将来追加候補に挙げられていた経路を、 既定 backend ( local) を残したまま **追加の選択肢** として実装する。

関連:
- [Architecture](./architecture.md) — 全体像と Storage interface の責務分離
- [ADR-0003: fingerprint のストレージ方式](../adr/0003-fingerprint-storage-strategy.md) — Option C の Pros / Cons は本 doc では再掲せず本 ADR を参照
- [ADR-0009: fingerprint binary serialization](../adr/0009-fingerprint-binary-serialization.md) — wire format は backend 非依存
- [ADR-0010: fingerprint filename への timestamp prefix](../adr/0010-fingerprint-filename-timestamp-prefix.md) — object key にも踏襲する

## Context

### 背景

ADR-0003 では「git per-task per-input ファイル」を主流に置きつつ、 S3 等のリモート object storage を Option C として将来候補に残した。 採用時点で典型的な monorepo 規模では git 方式が十分機能するが、 以下のいずれかが現実に発生する組織向けに **追加 backend** を提供する:

- record 容量が想定 ( 数 MB レンジ) を大きく超え、 git に置くと clone size / `git status` スループットを実害レベルで劣化させる
- record の git diff が PR ノイズの主因になっており、 `linguist-generated` + textconv の組み合わせでも運用負荷が下がらない
- AWS / GCP / S3 互換 object storage の認証・運用基盤が組織内で既に確立しており、 「外部 infra 運用責務」 の追加コストが ADR-0003 の試算より小さい

これらは ADR-0003 で「典型的」 とした monorepo の前提から外れる組織でのみ成立する条件で、 **既定 backend を `local` から差し替えるものではない**。 `s3` は組織が明示的に opt-in したときだけ有効になる。

### 制約 / 評価軸

ADR-0003 の R1〜R6 に加え、 S3 backend 固有の評価軸:

- **Sa**: 認証は標準 AWS 認証チェーン ( env / shared config / IRSA / IMDS) のみで成立し、 sloff 独自の credential 渡し経路を作らない
- **Sb**: kumo / MinIO / LocalStack 等の S3 互換 emulator に対して endpoint override + path-style addressing で接続できる ( テスト容易性)
- **Sc**: Storage interface の既存 contract ( Load / Save / Delete / List / CollapseDuplicates) を local と同じ semantics で満たし、 backend 切替で runner / gc subcommand 側の実装が分岐しない

## Decision

### 全体方針

- record の wire format は `local` と完全に同じ ( ADR-0009 の deterministic protobuf binary)。 **`Marshal` / `Unmarshal` は backend を跨いで共有**
- object key 層も ADR-0010 の filename layout を踏襲し、 `<prefix>/<spec_relpath>/<task_id>/<YYYYMMDDHHMMSSsss>-<input_hash>.pb` で配置する。 latest-timestamp wins と duplicate collapse の semantics を local と揃える
- 認証は `aws-sdk-go-v2` の **default credential chain** に任せる。 sloff 独自の credential env を増やさず、 AWS 標準の env / shared config / IRSA / IMDS のいずれでも動く
- 接続先設定 ( bucket / region / endpoint / path-style / prefix) は **リポジトリにコミットされる `.sloff/config.yml`** から読む。 backend 選択もここで行う ( CLI flag / env でのoverride は持たない、 後述)
- `sloff` は backend 切替を **設定ファイル driven** にする ( ADR-0003 の architecture.md 例示にあった `SLOFF_FINGERPRINT_BACKEND=...` env は採用せず、 単一 SSoT として config ファイルに集約)

### 設定ファイル `.sloff/config.yml`

リポジトリルート直下の `.sloff/config.yml` で fingerprint backend を宣言する。 ファイルが存在しない場合は **暗黙に `backend: local`** ( 既存挙動に下位互換) で動く。

```yaml
# .sloff/config.yml — リポジトリ全体の sloff 設定。 spec ファイル ( <spec_dir>/sloff.yml) とは別物
fingerprint:
  # backend: local | s3
  # 省略時は local。 local は追加設定なし。
  backend: s3
  s3:
    bucket: my-org-sloff-fingerprints     # 必須 ( backend: s3 のとき)
    prefix: sloff/fingerprints            # 任意 ( default: "sloff/fingerprints")
    region: us-east-1                     # 任意 ( default: aws-sdk-go-v2 の resolver。 AWS_REGION 等から取得)
    endpoint: ""                          # 任意 ( emulator 接続用、 default: aws-sdk-go-v2 の resolver。 通常 AWS は空でよい)
    use_path_style: false                 # 任意 ( default: endpoint が非空なら true、 空なら false)
```

#### 認証情報を `.sloff/config.yml` に書かない理由

credential ( access key / secret / session token / role ARN) は **設定ファイルに一切登場させない**。 `.sloff/config.yml` は git にコミットされる前提で、 secret を流入させる経路を構造的に塞ぐ。 認証は次節の AWS 標準チェーンに委譲する。

#### `SLOFF_*` env を持たない理由

architecture.md の旧記述には `SLOFF_FINGERPRINT_BACKEND` / `SLOFF_S3_BUCKET` 等の env 例があったが、 本実装では採用しない:

- backend / bucket / prefix / region は **リポジトリ間で揃う設定** ( 同じ monorepo を扱う全開発者・全 CI で同じ S3 bucket を見る)。 SSoT は git にコミットされる config ファイルが妥当
- env で個別 override できると「 自分の手元だけ別 bucket / 別 prefix」 が発生し、 fingerprint 共有 ( R1) を構造的に破壊する経路ができてしまう
- 認証は別 SSoT ( AWS 標準 env / shared config) で扱うため、 sloff 側 env を増やす必然性がない

### AWS 認証の扱い

AWS 認証は **`aws-sdk-go-v2` の default credential chain に丸投げ** する。 sloff は credential を一切 parse しない / 保持しない / log に出さない。

aws-sdk-go-v2 の default chain は以下を順に解決する ( standard pattern; `terraform-provider-aws` / `eksctl` / `restic` / `aws-cli v2` 等が同様):

1. **環境変数**: `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_SESSION_TOKEN`
2. **共有 credential file**: `~/.aws/credentials` (`AWS_PROFILE` で profile 指定)
3. **共有 config file**: `~/.aws/config` (region / `sso_*` / `role_arn` / `source_profile` 等)
4. **IAM Roles for Service Accounts ( IRSA)**: `AWS_ROLE_ARN` + `AWS_WEB_IDENTITY_TOKEN_FILE`
5. **コンテナ credential provider**: `AWS_CONTAINER_CREDENTIALS_*`
6. **EC2 IMDS**: instance metadata service

Region の解決は ( `s3.region` を `.sloff/config.yml` で指定しない限り) AWS_REGION → AWS_DEFAULT_REGION → shared config で順に解決される。

Endpoint は ( `s3.endpoint` を `.sloff/config.yml` で指定しない限り) `AWS_ENDPOINT_URL_S3` → `AWS_ENDPOINT_URL` → SDK の標準 resolver で解決される。 後述の通り `s3.endpoint` を yaml で書かなくても env で kumo / MinIO 等に向けられる。

参照 ( 設計時に確認した sources):
- aws-sdk-go-v2 Configure the SDK
- AWS SDKs and Tools Service-specific endpoints (`AWS_ENDPOINT_URL_S3`)
- AWS SDKs and Tools Environment variables

### Object key layout

```
<bucket>/<prefix>/<spec_relpath>/<task_id>/<YYYYMMDDHHMMSSsss>-<input_hash>.pb
```

例: `bucket=my-org-sloff-fingerprints` / `prefix=sloff/fingerprints` / `spec_relpath=path/to/spec` / `task_id=protoc-gen-go` / `input_hash=3f9a1c...` / 初回作成 `2026-05-10 12:34:56.789 UTC`

```
my-org-sloff-fingerprints/sloff/fingerprints/path/to/spec/protoc-gen-go/20260510123456789-3f9a1c....pb
```

- prefix は `s3.prefix` ( default `sloff/fingerprints`)。 1 つの bucket を複数 monorepo / 複数 sloff 用途で共有するときの namespace 区切りとして使える
- timestamp prefix の意味は ADR-0010 と同じ ( initial creation 時刻、 in-place 上書きでも保持、 lexicographic = chronological)
- key delimiter は `/`。 spec dir 名にアンダースコアを含むケースでも List がロスレスに spec_relpath を復元できる ( architecture.md の方針を踏襲)

#### S3 では merge は無いが filename レイアウトを揃える理由

local backend で timestamp prefix を導入した直接の動機は「 別 branch 独立 first-write 同士の path-level 衝突回避」 ( ADR-0010 の R5)。 S3 は **単一 SSoT** で git merge と同型の競合は発生しないため、 prefix を省略しても technically は機能する。 それでもレイアウトを揃える理由:

1. **Storage interface の semantics 統一** — Load が「同 (spec, task, input_hash) で複数 object が存在したら最新 timestamp を返す」 contract を local と共有する。 GC / inspect ロジックを local 専用に bifurcate しない
2. **複数 writer の並走耐性** — 同 input_hash に対し 2 つの runner が同時に first-write した場合、 同 object key への並列 PUT は last-write-wins で 1 件に潰れる。 timestamp prefix を載せておくと **両者の object が並存** し、 後段の collapse / GC で 1 件に収斂する。 wire-byte 同一 record が前提下では結果は同等だが、 「 並走 PUT が片方の record をログ上から消す」 状態を避けられる
3. **ADR-0003 Option E ( Hybrid) への移行余地** — git ( local) と S3 を併用するとき、 record key が同じレイアウトであるほど移行 / 同期が単純になる

### 操作 semantics

backend interface ( `fingerprint.Storage`) の 5 メソッド + `CollapseDuplicates` を `local` と同じ contract で実装する。 `record.Marshal` / `Unmarshal` は再利用するため byte-for-byte で local と互換 ( 同じ record を local で書いた後 backend を s3 に切り替え、 既存 record を `aws s3 cp` で同期しても整合する)。

| Method | semantics |
|---|---|
| `Name()` | `"s3"` |
| `Load(ctx, key)` | `ListObjectsV2(prefix=<key prefix>)` で同 input_hash の suffix を持つ object を列挙し、 lexicographic 最大 ( = 最新 timestamp) を `GetObject` してから `Unmarshal`。 0 件なら `(nil, false, nil)` |
| `Save(ctx, key, rec)` | `Marshal` 後、 既存 object を List。 0 件なら新規 `<TS>-<hash>.pb` を `PutObject`。 1+ 件なら **最古 ( lex 最小) prefix を維持して PutObject + 残りを DeleteObject** ( duplicate collapse) |
| `Delete(ctx, key)` | 同 input_hash の全 timestamp variant を List + Delete。 0 件は noop |
| `List(ctx, filter)` | `ListObjectsV2` を `<prefix>/<filter.SpecRelpath>/<filter.TaskID>/` 単位で paginate。 同 Key の duplicate variant は 1 件に dedupe ( latest LastModified を採用)。 `OlderThan` は object の LastModified ( S3 が返す server-side time) で判定 |
| `CollapseDuplicates(ctx)` | `List` 結果を回し、 各 Key で最古以外の variant を `DeleteObject`。 ADR-0010 §"duplicate collapse の責務" の S3 版 |

#### `Storage` interface への `CollapseDuplicates` 追加

現状 `CollapseDuplicates` は `local.Storage` の concrete method で、 `cmd/sloff/fingerprint.go` の `gc` subcommand が直接呼んでいる。 S3 backend でも GC は必要なので、 これを `fingerprint.Storage` interface に昇格する:

```go
type Storage interface {
    Name() string
    Load(ctx context.Context, key Key) (*fingerprintv1.Record, bool, error)
    Save(ctx context.Context, key Key, rec *fingerprintv1.Record) error
    Delete(ctx context.Context, key Key) error
    List(ctx context.Context, filter ListFilter) ([]Key, error)
    CollapseDuplicates(ctx context.Context) (int, error) // ★ 追加
}
```

これにより `cmd/sloff/fingerprint.go` の `gc` 経路と `cmd/sloff/run.go` / `graph.go` の Storage 構築経路が統一され、 backend 切替が単一の構築関数 ( 後述の `loadStorage`) に閉じる。

#### 並行性 / 整合性

S3 は read-after-write strong consistency ( 2020 以降全 region) なので Save 直後の Load は確実に新 object を見る。 並列 Save の race については上記「 timestamp prefix を S3 でも載せる理由」 の 2 で議論した通り、 wire-byte 同一前提下で問題は表面化しない。

#### Save の duplicate collapse 動作

`local.Save` は merge 直後の状態 ( 同 hash で複数 timestamp variant が並存) を **fingerprint miss + Save 時に collapse する**。 S3 でも同じ動作を再現する:

1. `ListObjectsV2(prefix=<key prefix>)` で `<TS>-<hash>.pb` を全列挙
2. 0 件 → 新規 `<now>-<hash>.pb` を PutObject
3. 1+ 件 → lex 最小の object key に PutObject ( 上書き) + 残りを DeleteObject

これにより local backend と「 Save によって自然に dedupe される」 性質が揃う。 単独 SSoT である S3 では merge 由来の duplicate は通常発生しないが、 **並列 first-write race** ( 同 hash を独立に PUT した 2 runner) で自然に作られた variant は同じ機構で 1 件に収斂する。

### Backend 切替の起動経路

`cmd/sloff/run.go` / `graph.go` / `fingerprint.go` の `local.New(root)` 直接呼び出しを **`fingerprint.LoadStorage(ctx, root)`** に集約する ( 関数の置き場所は `internal/sloff/fingerprint/config.go` 等):

```go
// 擬似コード
func LoadStorage(ctx context.Context, repoRoot string) (fingerprint.Storage, error) {
    cfg, err := loadConfig(repoRoot) // .sloff/config.yml をパース ( 無ければ既定値)
    if err != nil { return nil, err }
    switch cfg.Fingerprint.Backend {
    case "", "local":
        return local.New(repoRoot), nil
    case "s3":
        return s3.New(ctx, repoRoot, cfg.Fingerprint.S3)
    default:
        return nil, fmt.Errorf("unknown fingerprint backend %q", cfg.Fingerprint.Backend)
    }
}
```

config の読み込み点は単一にし、 backend 別の実装パッケージ ( `local` / `s3`) は config 型を import しない ( DI コンテナ的に外側で組み立てる)。

`s3.New` は `aws-sdk-go-v2` の `config.LoadDefaultConfig` で credential / region を解決し、 `s3.NewFromConfig` で client を構築する。 `endpoint` / `use_path_style` が config に明示されていればそれを優先、 無ければ SDK の env / shared config 解決に委ねる。

### File Layout ( 実装側)

```
internal/sloff/fingerprint/
  storage.go                 # Storage interface ( CollapseDuplicates を追加)
  record.go                  # 既存 ( Marshal / Unmarshal / Sort)
  config.go                  # ★ 新規: .sloff/config.yml の load / 既定値
  config_test.go
  local/local.go             # 既存 ( CollapseDuplicates は既に存在、 interface に昇格しても何もしない)
  s3/                        # ★ 新規 backend
    s3.go                    # S3 Storage 実装
    s3_test.go               # kumo を起動する integration test
    keys.go                  # key 組み立て / 解析 helper ( unit-testable)
    keys_test.go
```

`cmd/sloff` 側は `local.New(root)` 呼び出しを `fingerprint.LoadStorage(ctx, root)` ( または等価な builder) に置換する。 Runner / Plan / gc subcommand は backend 非依存になる。

### kumo を使ったテスト

S3 backend の test は kumo (`github.com/sivchari/kumo`) を S3 互換 emulator として起動して回す:

- kumo は `go.mod` の `tool` ディレクティブに `github.com/sivchari/kumo/cmd/kumo` を追加し、 `go tool kumo` で起動する ( ADR-0007 と整合: kumo は外部 OSS パッケージで `--version` 持ちの CLI として扱える。 開発者 / CI は別途 install 不要)
- `internal/sloff/fingerprint/s3` パッケージの `TestMain` で kumo を spawn し、 全テストで 1 プロセスを共有する ( spawn コストはバイナリ起動で数 s 〜 7 s オーダー — 償却したい)
- 起動後は TCP dial → S3 `ListBuckets` の 2 段で readiness を確認 ( kumo に health endpoint は未提供のため)
- 認証は kumo の固定 `test/test` ( static credential)、 region は `us-east-1`、 path-style は強制 ON にして接続する。 これらは test setup helper 内に閉じ、 production code に kumo 固有のコードは入らない
- 各 test は **異なる bucket 名** で動かし、 並列実行時の干渉を避ける ( S3 名前空間内で衝突しない)

#### kumo 固有の制約 ( v0.18.2 時点で観測)

ユーザー要望は「ランダムポート割り当て」 だったが、 kumo の現状実装に以下のバグがあり test 設計上の妥協が必要になる。 これらは kumo 側に PR を出して直すのが本筋で、 直り次第 test を修正する:

1. **`KUMO_PORT` / `KUMO_HOST` env と `--port` / `--host` flag が無視される** — `cmd/kumo/main.go` が `server.DefaultConfig` ( host `0.0.0.0` / port `4566` ハードコード) しか読まない。 README / `--help` には env で設定できると書いてあるが実装が追従していない。 結果として **ポート 4566 を固定で使うしかない**。 4566 が他プロセスに占有されている場合は test 起動時に明確なエラーで落とす ( pre-bind チェック)
2. **`ListObjectsV2` の `LastModified` が timezone を取り違える** — `HeadObject` は real UTC を返すが、 `ListObjectsV2` は **system local time を UTC タグで返す**。 同じ object に対して 2 endpoint で異なる時刻が返るため、 `OlderThan` filter の test は wall-clock ではなく `ListObjectsV2` 自身が返す `LastModified` を anchor として cutoff を組み立てる必要がある

#### テスト範囲

`local` backend の `local_test.go` で網羅されている観点を S3 で再現する:

- Save / Load round-trip ( 単一 record)
- Load miss は `(nil, false, nil)`
- Save の prefix 保存 ( 同 hash 再 Save で timestamp prefix が変わらない)
- duplicate collapse ( 既存複数 variant がある状態で Save → 最古に折り畳まれる)
- Load が duplicate のうち latest を返す
- Delete / Delete on missing は noop
- List / spec / task で filter / OlderThan
- List の duplicate dedupe
- CollapseDuplicates が最古以外を全削除する
- 不正 byte の Object → Load が decode error を surface する
- ListFilter.OlderThan: kumo が返す LastModified に対する境界判定

#### 共通 conformance test

local と s3 で挙動が揃うことを担保するため、 `internal/sloff/fingerprint/storagetest/` 等の package を切り、 「Storage 実装に対して走らせる test 集」 を提供する。 local の test と s3 の test 双方からこの set を呼ぶことで、 backend 追加時の retest コストが減り、 semantics drift も検出しやすい ( YAGNI とのトレードオフ; 初版で提供するか、 s3 単独 test だけで進めるかは実装時に判断する。 後者から始めて 2 backend で重複が増えたら抽出する選択も合理的)。

## Consequences

### 正の影響

- ADR-0003 Option C を完全な選択肢として実装に降ろせる ( 容量・PR ノイズ問題に当たった組織が backend 切替だけで対応可能)
- `Storage` interface の semantics が 2 backend で揃い、 「 interface だけ切ってあって 1 実装しかない」 状態を解消
- AWS 認証を SDK default chain に丸投げするため、 IRSA / SSO / IMDS / `~/.aws/credentials` のいずれの組織標準にも自動追従

### 負の影響

- 利用者リポジトリの configuration surface が増える ( 新規ファイル `.sloff/config.yml`)。 ただし backend を切り替えない組織は ファイル不要なので零コスト
- aws-sdk-go-v2 が新規 direct dependency として go.mod に入る ( s3 backend を使わない組織でも binary size が増える)。 build flag / build tag による分離は YAGNI として初版では取らない
- ADR-0003 で議論された S3 backend 固有の Cons ( ネットワーク必須 / 外部 infra 運用責務 / git history からの追跡性低下) はそのまま継承。 これらは backend を opt-in で選択する組織が引き受ける trade-off

## Open Questions

- **Q1**: 並列 first-write race ( 同一 input_hash を複数 runner が同時に first-write) で生まれた duplicate を Save の collapse がきれいに 1 件に潰すかは S3 上での実測が要る。 kumo 上の test では決定論的に再現するが、 production S3 ( eventual consistency 解消後の挙動 + 高頻度 ListObjectsV2) で観測する余地。 現状の Save 実装は wire-byte 同一前提が崩れない限り問題ない設計
- **Q2**: prefix 内 object 数が monorepo 規模 × 並走世代数 ( 例 200 task × 10 世代 = 2000 オーダー) を超えて伸びた場合に `ListObjectsV2` の paginate コストが Load / Save の per-call latency に乗る。 1 hash あたりの List 範囲は `<spec>/<task>/` 配下なので通常 1 page ( 最大 1000 keys) に収まるが、 実運用で観測したい。 Hybrid ( ADR-0003 Option E) 検討時の閾値判断材料
- **Q3**: `s3.endpoint` を yaml 設定と env (`AWS_ENDPOINT_URL_S3`) のどちらに優先順位を持たせるかは aws-sdk-go-v2 の標準 ( yaml = code = `BaseEndpoint`, env = SDK resolver) に従い yaml > env としている。 これが「 開発者の手元だけ kumo に向ける」 の運用と整合するかは実運用知見待ち。 必要なら `s3.endpoint: "@env"` のような sentinel で env に明示委譲する経路を後追いする
