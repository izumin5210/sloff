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
# proto/lazygen.yml
commands:
  - name: gen-options
    tools:
      - exec: ["buf", "--version"]
      - go-local: ./cmd/protoc-gen-foo

# api/lazygen.yml
commands:
  - name: gen-api
    tools:
      - exec: ["buf", "--version"]      # 重複
      - go-local: ./cmd/protoc-gen-foo  # 重複
```

100 task で同じ block が 100 回出てくる。 「 buf を v1.31 に bump」 のような変更で 100 箇所編集が必要になり、 漏れた箇所が silent miss につながる。

### O3. 「 tool」 という抽象が spec に表れない

利用者は概念上 「 codegen pipeline で使う tool セット」 という単位で考えるが、 spec の中ではその単位が見えない。 `lazygen graph` / `--explain` の出力にも「 tool」 ノードが現れない。

### References

- [ADR-0004](./0004-spec-validation-and-output-conflict-policy.md) ( tools[] 必須化 / 出力重複検出)
- [ADR-0005](./0005-eliminate-resolver-auto-dispatch.md) ( declared-only)
- [ADR-0007](./0007-no-external-dependency-resolver.md) ( `<channel>-deps` 表記の根拠)

## Decision

### D1. tool を **named first-class entity** として spec に持たせる

各 `lazygen.yml` ファイルは top-level に `tools:` ( map[name]DeclaredTool) を持てる。 task は `commands[*].tools` で **tool 名のリスト** ( `[]string`) として参照する。 inline 形式は完全廃止。

```yaml
# proto/lazygen.yml
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

- 1 つの `lazygen.yml` は `tools:` だけ / `commands:` だけ / 両方を持つことが許される ( 少なくとも片方は要、 空ファイルは error)
- tool 定義は **どの `lazygen.yml`** に置いてもよい ( 共通 tool は root の `lazygen.yml` に集約してもよいし、 ある proto pipeline 専用 tool は `proto/lazygen.yml` 内で local に定義してもよい)
- 名前空間は **リポジトリ全体で 1 つの flat 空間**。 `lazygen run` 1 回で discover する全 `lazygen.yml` の `tools:` を merge して registry を組む
- **同名 tool の重複定義は load 時 error** ( error message は両定義箇所のパスを併記)
- **未定義 tool 名の参照は load 時 error** ( error message は参照元 task の場所を併記)

### D3. **path resolution は tool 定義側の dir 相対**

tool 定義に含まれる path 系フィールド ( `go-local: ./cmd/foo` 等) は、 **その tool が定義されている `lazygen.yml` の dir** を基準に解決する ( 参照元 task の dir ではない)。

これにより tool 定義が「 自己完結した単位」 になる ( 別 dir の task から参照されても解釈は変わらない)。

### D4. **slug-style な命名規約**: tool 名は `[a-z0-9_-]+` のみ許容

YAML 表記揺れや shell-like 解釈の事故を避けるため、 lower-case + 数字 + ハイフン / アンダースコアに限定。 violation は load 時 error。

### D5. **未使用 tool 定義は silent OK** ( warn なし)

漸進的な authoring を許容するため、 「 定義されているが誰も参照していない tool」 は許容する。 dead-code 警告などは将来 lint コマンドで提供する余地。

### D6. **resolver pre-pass で tool あたり 1 回 resolve**

Runner は `Run` の冒頭で discover 済み spec から `ToolRegistry` を構築 → 各 tool に対して **1 回だけ** `Resolver.Resolve` を呼んで `toolresolver.Result` を name 別キャッシュ。 task は cache から自分の参照する tool の Result を pick するだけ。

これで O1 の効率問題は構造的に消える ( N task が同じ tool を参照しても resolver 呼び出しは 1 回)。

## Rationale

### 案 A ( resolver-level memo cache のみ) を選ばなかった理由

resolver 内部で declared key の memo を持つだけでも O1 は解決できる。 ただしそれは「 効率の問題は解いたが、 表現の問題 ( O2 / O3) は残る」 状態。 spec に tool 抽象が現れていないと:

- 「 何 tool が repo にある?」 を全 task の inline を読み返さないと分からない
- 「 tool A の version を bump したい」 が 全 task の inline 編集 + 漏れ検証になる
- ADR-0004 が掲げる「 spec から完全に分かる」 原則と整合しない

このトレードオフを踏まえ、 named entity 化を選択。

### 配置を「 root-only」 にしなかった理由

「 root の `lazygen.tools.yml` 1 ファイルに集約」 案と比較:

- 1 spec dir だけで使う local tool に関しても root に置くことを強制すると、 tool 名が膨れる + ファイル間の往復が増える
- workspace package の close-knit な codegen pipeline は同じ dir で完結したい ( cognitive locality)
- 一方で「 buf / protoc-gen-go のような repo 横断 tool」 は root の `lazygen.yml` に置く運用が自然

flat 名前空間 + 分散定義の組み合わせで、 利用者が必要に応じて中央集約 / 分散を選べる。

### 配置を「 cascading scope」 にしなかった理由

「 spec dir A の tool は A 配下からのみ visible / unrelated B からは不可視」 のような scope rule もありうるが、 lazygen の規模 ( 200 task / 数十 tool オーダー) では:

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
- **`lazygen graph` / `--explain` で tool ノードが表現可能**になり、 「 task X が依存する tool 一覧」 の可視化ができる ( 将来実装)
- **tool の差し替え / override がやりやすい** ( 例: テスト時に `buf` の定義だけ別ファイルに切り替える、 等の運用)
- 各 `lazygen.yml` が「 自分の dir の責務」 + 「 他から使われる tool 提供」 の両方を担えるので、 monorepo の workspace 単位の自治が保てる

### 負の影響 / 注意点

- **既存 inline 形式は完全 break**。 pre-release ( v0.x、 まだリリース無し) なので問題は無いが、 移行作業は要る ( この ADR を含む PR で全 fixture / 例 を named 形式に統一)
- 名前付けの責任が増える ( 「 buf」 vs 「 buf-cli」 vs 「 buf-validate」 の命名規律は利用者次第)
- 抽象が 1 段増える ( tool 定義 → tool 名参照 の indirection)。 ただし inline 形式の重複コストとのトレードオフでは named 側が勝つと判断

### 将来再考の余地

- **cascading / qualified namespace**: 大規模化で flat 重複が頻発するなら、 spec dir prefix で qualify する形式 ( `proto:codegen`、 `services/api:gen` 等) を別 ADR で導入
- **import 構文**: 「 tool catalog 専用ファイル」 を opt-in で許す ( 例: `lazygen.tools.yml` を root 検出すれば自動 merge) のような UX 改善は将来検討
- **lint コマンド**: 未使用 tool / 命名違反 / 重複 spec を一括検出する `lazygen lint` の cli を別途検討
