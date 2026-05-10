# ADR-0008: tool を first-class spec entity とする

## Context

[ADR-0005](./0005-eliminate-resolver-auto-dispatch.md) で declared-only に統一して以降、 tool は task 単位で **inline 宣言** する形式を取っていた:

```yaml
commands:
  - name: gen
    cmd: ...
    tools:
      - exec: ["buf", "--version"]
      - go-local: ./cmd/protoc-gen-foo
```

この形式は task 1 つを書く分には素直だが、 monorepo で同じ tool を複数 task が共有する場合に 2 つの問題が顕在化する:

### O1. Resolver が tool あたり O(N task) 回走る

`@org/codegen` を 100 task が参照していると、 同じ pnpm-lock.yaml graph BFS や `go/packages.Load` を 100 回走らせることになる。 `lister.NewMemoized` は `SourceLister.List` の結果しかキャッシュしないので、 Resolver 自体の workspace lookup / `CollectExternals` BFS / 結果 assembly は毎回再実行される。 architecture.md は「 Resolver / SourceLister レベルでメモ化」 を documented intent として書いていたが実装が追いついていない状態。

### O2. inline 宣言は DRY を破る

```yaml
# proto/sloff.yml
commands:
  - name: gen-options
    tools:
      - exec: ["buf", "--version"]
      - go-local: ./cmd/protoc-gen-foo

# api/sloff.yml
commands:
  - name: gen-api
    tools:
      - exec: ["buf", "--version"]      # 重複
      - go-local: ./cmd/protoc-gen-foo  # 重複
```

100 task で同じ block が 100 回出てくる。 「 buf を v1.31 に bump」 のような変更で 100 箇所編集が必要になり、 漏れた箇所が silent miss につながる。

### O3. 「 tool」 という抽象が spec に表れない

利用者は概念上 「 codegen pipeline で使う tool セット」 という単位で考えるが、 spec の中ではその単位が見えない。 `sloff graph` / `--explain` の出力にも「 tool」 ノードが現れない。

### References

- [ADR-0004](./0004-spec-validation-and-output-conflict-policy.md) ( tools[] 必須化 / 出力重複検出)
- [ADR-0005](./0005-eliminate-resolver-auto-dispatch.md) ( declared-only)
- [ADR-0007](./0007-no-external-dependency-resolver.md) ( `<channel>-deps` 表記の根拠)

## Decision

### D1. tool を **named first-class entity** として spec に持たせる

各 `sloff.yml` ファイルは top-level に `tools:` ( map[name]DeclaredTool) を持てる。 task は `commands[*].tools` で **tool 名のリスト** ( `[]string`) として参照する。 inline 形式は完全廃止。

```yaml
# proto/sloff.yml
tools:
  buf:
    exec: ["buf", "--version"]
  protoc-gen-foo:
    go-local: ./cmd/protoc-gen-foo

commands:
  - name: gen-options
    cmd: buf generate
    inputs: ["**/*.proto", "buf.yaml", "buf.gen.yaml", "buf.lock"]
    outputs: ["**/*.options.pb.go"]
    tools: [buf, protoc-gen-foo]   # 名前参照のみ
```

### D2. **配置ルール**: 同一ファイル内共存可、 リポジトリ内分散定義可、 flat 名前空間

- 1 つの `sloff.yml` は `tools:` だけ / `commands:` だけ / 両方を持つことが許される ( 少なくとも片方は要、 空ファイルは error)
- tool 定義は **どの `sloff.yml`** に置いてもよい ( 共通 tool は root の `sloff.yml` に集約してもよいし、 ある proto pipeline 専用 tool は `proto/sloff.yml` 内で local に定義してもよい)
- 名前空間は **リポジトリ全体で 1 つの flat 空間**。 `sloff run` 1 回で discover する全 `sloff.yml` の `tools:` を merge して registry を組む
- **同名 tool の重複定義は load 時 error** ( error message は両定義箇所のパスを併記)
- **未定義 tool 名の参照は load 時 error** ( error message は参照元 task の場所を併記)

### D3. **path resolution は tool 定義側の dir 相対**

tool 定義に含まれる path 系フィールド ( `go-local: ./cmd/foo` 等) は、 **その tool が定義されている `sloff.yml` の dir** を基準に解決する ( 参照元 task の dir ではない)。

これにより tool 定義が「 自己完結した単位」 になる ( 別 dir の task から参照されても解釈は変わらない)。

#### Cross-spec 参照時の cmd 側責任 ( foot-gun 注意)

tool 定義の path は **resolver の hash 入力** ( files_hash / resolved_versions_hash 経路) に乗るだけで、 task の cmd が実行する binary 自体を sloff が解決するわけではない。 cmd は task 自身の `specRelpath` ( 参照元 task の dir) を cwd として実行されるので、 **「 cmd 側 path が tool 定義の指す target と同じものを参照しているか」 は cmd 作者の責任**:

```yaml
# packages/codegen/sloff.yml
tools:
  protoc-gen-foo:
    go-local: ./cmd/protoc-gen-foo   # → packages/codegen/cmd/protoc-gen-foo

# proto/sloff.yml
commands:
  - name: gen
    cmd: ["go", "run", "./cmd/protoc-gen-foo"]   # ⚠ proto/cmd/protoc-gen-foo に解決される (別物)
    tools: [protoc-gen-foo]
```

上記は **fingerprint key は packages/codegen 側の source を hash するが、 cmd は proto/cmd/protoc-gen-foo を実行する** という乖離が生じる ( 双方が存在すれば silent stale、 後者が不在なら早期 fail)。 別 dir の cwd-sensitive な tool ( `go run` / 相対 binary path 等) を参照するときは、 cmd 側で:

- full Go import path を書く: `cmd: ["go", "run", "github.com/org/repo/packages/codegen/cmd/protoc-gen-foo"]`
- task dir 基準の相対 path を書く: `cmd: ["go", "run", "../../packages/codegen/cmd/protoc-gen-foo"]`
- 事前 build した binary を PATH 経由で呼ぶ: `cmd: ["protoc-gen-foo"]`

など、 cwd 依存しない方法で同じ target を参照する。 sloff は cmd 文字列の中身を validate しない方針 ( cmd_hash に乗るだけ) のため、 ここはユーザ規律で担保する。 cwd-independent な resolver ( pnpm-local、 PATH 経由 binary を呼ぶ script tool 等) ではこの問題は起きない。

### D4. **slug-style な命名規約**: tool 名は `[a-z0-9_-]+` のみ許容

YAML 表記揺れや shell-like 解釈の事故を避けるため、 lower-case + 数字 + ハイフン / アンダースコアに限定。 violation は load 時 error。

### D5. **未使用 tool 定義は silent OK** ( warn なし)

漸進的な authoring を許容するため、 「 定義されているが誰も参照していない tool」 は許容する。 dead-code 警告などは将来 lint コマンドで提供する余地。

### D6. **resolver pre-pass で tool あたり 1 回 resolve**

Runner は `Run` の冒頭で discover 済み spec から `ToolRegistry` を構築 → 各 tool に対して **1 回だけ** `Resolver.Inputs` / `Resolver.Versions` を呼んで結果を name 別キャッシュ。 task は cache から自分の参照する tool の contribution を pick するだけ。

これで O1 の効率問題は構造的に消える ( N task が同じ tool を参照しても resolver 呼び出しは 1 回)。

#### IZU-16 補遺: Inputs / Versions 分割と内部メモ化

resolver の公開 API は `Inputs` ( task inputs に union される repo-relative path 集合) と `Versions` ( resolved_versions_hash に投入される ResolvedVersion 集合) の 2 メソッドに分割されている ( IZU-16)。 graph 構築 ( ExtraInputs のみ必要) と execution ( Versions も必要) の関心を別経路に分離するための切り分けで、 ここで言う「 1 回 resolve」 は **同一 tool に対する Inputs と Versions の連続呼び出しが内部の発見作業を共有する** ことを意味する:

- **`script`**: `Inputs` は常に `nil` を即返す ( source contribution なし)。 `Versions` のみが既存の `<bin> --version` cache を消費する。
- **`go-local`**: 両メソッドとも `lister.NewMemoized` 経由で `packages.Load` を呼ぶ。 memoize により 1 entry あたり 1 回の `packages.Load` で済み、 Inputs ( InternalFiles 抽出) / Versions ( ExternalModules 抽出) 双方が同じ Listing を slice する。
- **`pnpm-local`**: per-package の `pkgComputation` を `sync.Once` で初期化 ( WalkDeps + FileEnumerator)。 Inputs / Versions のどちらが先に呼ばれても 1 回の lockfile walk + 1 回の git ls-files で完結する。

この分割により、 graph 系 caller ( 例: `sloff graph`) は Versions の取得コスト ( script なら subprocess spawn) を払わずに depgraph を構築できる。 「 graph 構築 → execution」 のフェーズ分離はパフォーマンスを犠牲にしないのが肝で、 各 resolver が同じ declared tool への 2 メソッド呼び出しを「 1 回分の発見コスト + 結果 slice」 に縮める責務を負う。

### D7. **internal-source tool の build / run は cmd 内責務**

`pnpm-local` / `go-local` のような internal-source resolver が指す workspace package の **「 source 変更時の rebuild」 と「 実行」 は task の cmd 内に書く** 利用者責任とする。 sloff 自身は build orchestration をしない:

```yaml
tools:
  codegen:
    pnpm-local: "@org/codegen"

commands:
  - name: gen
    # cmd が build + run を 1 行で行う ( go run の `compile + execute` と同じ責務分担)
    cmd: ["sh", "-c", "pnpm --filter @org/codegen build && pnpm exec my-codegen"]
    inputs: ["**/*.proto"]
    outputs: ["**/*.pb.ts"]
    tools: [codegen]
```

cmd を組み立てる責務は利用者にある。 sloff の関与は:

- pnpm-local resolver が当該 workspace package の **git-tracked + transitive workspace dep の git-tracked ファイル** を ExtraInputs に contribute → files_hash 経路で source 変更を検知
- 当該 workspace の **transitive 外部 npm dep の resolved version** を `pnpm-deps:<pkg>@<ver>` ResolvedVersion として contribute → resolved_versions_hash 経路で外部 dep bump を検知

source 変更は files_hash で invalidate → cmd 再実行 → cmd 内の build + run が走る → 新しい binary で task が走る。

**理由**:

- go-local は `go run ./cmd/foo` が compile + execute を内包しており sloff は build を意識しない。 pnpm-local も同じ責務分担に揃えるのが consistent
- spec から「 build task と consumer task の関連付け」 という暗黙概念が消え、 spec が単純化する
- 利用者は通常、 cmd を `pnpm run gen` 等の package.json script に逃がせるので spec の verbose さは増えない

#### 検討した代替案 ( 採用しなかった理由)

| 案 | 内容 | 棄却理由 |
|---|---|---|
| **build を別 sloff task として宣言、 path overlap で link** | `codegen-build` task の outputs ( dist/**) と pnpm-local の bin path が path overlap → depgraph が依存 edge を貼る | link が暗黙 ( 文字列一致による偶然) で読み手の認知負荷が高い。 path 不一致は silent fail |
| **build task に `builds: [tool-name]` フィールドを宣言** | 「 この task が tool を build する」 を named cross-ref で明示化 | spec field 増。 overlap 機構との二重管理 |
| **tool が `build:` block を内包 ( tool = build pipeline)** | tool 定義に cmd / inputs / outputs を持たせ、 task との境界を統合 | tool と task の概念境界が崩れる。 ADR-0008 の D1 ( tool は first-class entity) の意図と外れる |
| **co-location 制約 ( pnpm-local は package dir の sloff.yml に置く)** | 配置位置で「 同じ package について話している」 ことを明示 | 暗黙性は緩和されるが mechanism は path overlap のままで根本解決ではない |
| **cmd 内 build ( 採用)** | go-local の go run と同じ責務分担、 sloff は source hash だけ担当 | source 集合の精度を esbuild walk から git-tracked enumeration に下げる必要があるが、 過剰 invalidate にしか倒れず fingerprint の健全性は壊れない |

**精度トレードオフ**: 旧 esbuild walk は「 bin から transitive に import される実ファイルだけ」 を hash 入力にしていた。 git-tracked enumeration は「 package dir の全ファイル ( gitignore で除外されたものを除く)」 を入れる。 後者は **過剰 invalidate** ( 関係ない src/utils.ts 編集で gen が rerun) するが false hit にはならない。 Turborepo の default も同じアプローチで、 monorepo 規模での実用上の精度は問題にならないことが知られている。

**preflight の責務との分離**: D7 で削除したのは **「 build 必須かを spec から推測 + dist/src を慣習で扱う」 旧 preflight checker** であって、 preflight subsystem 自体ではない。 preflight は依然として「 cmd 実行前の state 検証」 という general な役割を持ち、 channel 別に必要なら Checker を持つ。 例えば pnpm-local は **install drift 検出** ( `pnpm-lock.yaml` vs `node_modules/.pnpm/lock.yaml` の byte 一致確認) のための Checker を `preflight/pnpmlocal/` に持っている。 これは ADR-0008 D7 の「 build / run は cmd 責務」 とは独立の話で、 「 lockfile を SSoT に取る resolver は install state が lockfile と一致していることを前提にする」 ための前段検証。 「 preflight = build 専用」 でも「 preflight = drift 専用」 でもなく、 channel ごとに validation したい invariant があるなら持つ、 という general subsystem。

## Rationale

### 案 A ( resolver-level memo cache のみ) を選ばなかった理由

resolver 内部で declared key の memo を持つだけでも O1 は解決できる。 ただしそれは「 効率の問題は解いたが、 表現の問題 ( O2 / O3) は残る」 状態。 spec に tool 抽象が現れていないと:

- 「 何 tool が repo にある?」 を全 task の inline を読み返さないと分からない
- 「 tool A の version を bump したい」 が 全 task の inline 編集 + 漏れ検証になる
- ADR-0004 が掲げる「 spec から完全に分かる」 原則と整合しない

このトレードオフを踏まえ、 named entity 化を選択。

### 配置を「 root-only」 にしなかった理由

「 root の `sloff.tools.yml` 1 ファイルに集約」 案と比較:

- 1 spec dir だけで使う local tool に関しても root に置くことを強制すると、 tool 名が膨れる + ファイル間の往復が増える
- workspace package の close-knit な codegen pipeline は同じ dir で完結したい ( cognitive locality)
- 一方で「 buf / protoc-gen-go のような repo 横断 tool」 は root の `sloff.yml` に置く運用が自然

flat 名前空間 + 分散定義の組み合わせで、 利用者が必要に応じて中央集約 / 分散を選べる。

### 配置を「 cascading scope」 にしなかった理由

「 spec dir A の tool は A 配下からのみ visible / unrelated B からは不可視」 のような scope rule もありうるが、 sloff の規模 ( 200 task / 数十 tool オーダー) では:

- name collision は実用上 滅多に起きない ( 起きたら名前を改名すればよい)
- visibility rule を導入すると import 構文 / forward-ref の議論が発生し、 spec 形式が複雑化

YAGNI で flat にする。 必要が生じたら別 ADR で qualified namespace ( `proto:codegen` 等) を再検討。

### inline 宣言を残さない理由

「 inline と named を共存」 は authoring の自由度を増すが:

- 同じ tool が inline でも named でも書ける状態は cache キーの一意性を曖昧にする
- spec を読む側が「 inline ? named ? どっち?」 を毎回判断する負担を負う
- pre-release のうちに 1 形式に絞るほうが教育コストが低い

named のみに統一する。 「 1 task 限定の使い捨て tool」 も name を付けて宣言する ( `oneoff-foo:` 等)。

## Consequences

### 正の影響

- **tool あたり 1 回 resolve** 構造が組み込まれ、 N tasks × M tools の monorepo でも O(M) の resolver 呼び出しに収まる
- **DRY**: 共通 tool は 1 箇所定義 → 全 task が短い名前で参照
- **`sloff graph` / `--explain` で tool ノードが表現可能**になり、 「 task X が依存する tool 一覧」 の可視化ができる ( 将来実装)
- **tool の差し替え / override がやりやすい** ( 例: テスト時に `buf` の定義だけ別ファイルに切り替える、 等の運用)
- 各 `sloff.yml` が「 自分の dir の責務」 + 「 他から使われる tool 提供」 の両方を担えるので、 monorepo の workspace 単位の自治が保てる

### 負の影響 / 注意点

- **既存 inline 形式は完全 break**。 pre-release ( v0.x、 まだリリース無し) なので問題は無いが、 移行作業は要る ( この ADR を含む PR で全 fixture / 例 を named 形式に統一)
- 名前付けの責任が増える ( 「 buf」 vs 「 buf-cli」 vs 「 buf-validate」 の命名規律は利用者次第)
- 抽象が 1 段増える ( tool 定義 → tool 名参照 の indirection)。 ただし inline 形式の重複コストとのトレードオフでは named 側が勝つと判断

### 将来再考の余地

- **cascading / qualified namespace**: 大規模化で flat 重複が頻発するなら、 spec dir prefix で qualify する形式 ( `proto:codegen`、 `services/api:gen` 等) を別 ADR で導入
- **import 構文**: 「 tool catalog 専用ファイル」 を opt-in で許す ( 例: `sloff.tools.yml` を root 検出すれば自動 merge) のような UX 改善は将来検討
- **lint コマンド**: 未使用 tool / 命名違反 / 重複 spec を一括検出する `sloff lint` の cli を別途検討
