# Resolver: go-local

`goLocalResolver` はリポジトリ内に実装された **内製 Go CLI** (`go run ./cmd/...` 形式や repo local main package) を扱う。 SemVer を持たないため、 ツールを構成するソース集合と外部 module の resolved version を 2 経路で task の fingerprint key に流し込む。

関連:
- [Architecture](./architecture.md)
- [Resolver: script](./resolver-script.md) ( 外部配布 Go ツール側は script resolver で `<bin> --version` を取る)
- [Resolver: pnpm-local](./resolver-pnpm-local.md) ( pnpm 側の対応物 = workspace 内 内製パッケージ)
- [ADR-0007: sloff は外部依存専用 resolver を持たない](../adr/0007-no-external-dependency-resolver.md) ( go-deps / pnpm-deps 表記の根拠)

## Context

Go の generator は外部配布 module (`go.mod` の `tool` ディレクティブで宣言される SemVer pinned ツール) と、 リポジトリ内 main package として実装された内製ツール (内製 protoc plugin、 内製 codegen 等) の 2 種類に分かれる。 後者は SemVer を持たないため、 **ソースファイル + 依存 module 集合** から導出した識別子を fingerprint key に渡す。

「内製ソース ( = `local`)」 という意味では [pnpm-local](./resolver-pnpm-local.md) と対応物の関係。 両 resolver は同じ shape の Result を返す:

- **内製ソース** ( main module / repo-local replace の `.go` / 埋め込みアセット等) は **ExtraInputs** として task の inputs に union され、 files_hash で content invalidation される。 「 main module 内の Go ファイルを生成する upstream codegen task」 がある場合、 実行順序は consumer task の `depends` で明示する ( [ADR-0013](../adr/0013-explicit-task-dependencies.md))。 producer が **tool の import 閉包そのもの** を生成する場合は tool 定義の `depends` に一元宣言し、 runner の注入に任せる ( [ADR-0019](../adr/0019-tool-bootstrap-depends.md)、 後述「生成物を import する tool」)。 宣言漏れは ExtraInputs を含む union 後の inputs に対する overlap 検証が error で検出する
- **外部 Go module** は `go-deps:<path>@<version>+sum:<go.sum-hash>` 形式の **ResolvedVersion** として resolved_versions_hash に流れる。 module bump / go.sum drift で必ず invalidate される

外部配布 ( aqua / `go tool` 経由) の OSS ツールは [script resolver](./resolver-script.md) で `<bin> --version` を取るアプローチに統一されている ( ADR-0007)。

ソースファイル列挙には Resolver 内部 helper の `SourceLister` を使う。 標準実装は **`golang.org/x/tools/go/packages` の Go API を直接 import した `goPackagesLister`** で、 entry main package から transitive な依存解析で関係する `.go` ファイルのみを抽出する。

## Resolver の動作

### 取得元

1. spec の top-level `tools:` map で `go-local: <import-path>` 形式で named 定義された tool entry を取得 ( ADR-0008)。 entry は **その tool が定義された `sloff.yml` の dir** に相対
2. 内部 `SourceLister` ( デフォルト `goPackagesLister`) で transitive 依存を解析し `Listing{InternalFiles, ExternalModules}` を得る
3. **InternalFiles → Result.ExtraInputs** に詰めて返す ( runner が task.inputs に union)
4. **ExternalModules → 1 個ずつ Result.Versions の ResolvedVersion** に変換して返す

### Result の形式

```go
toolresolver.Result{
    ExtraInputs: []string{
        "cmd/foo/main.go",
        "internal/util/util.go",
        ...   // main module / repo-local replace の .go / 埋め込み / .s 等を含む
    },
    Versions: []toolresolver.ResolvedVersion{
        {Name: "example.com/dep",   Source: "go-local:./cmd/foo", Version: "go-deps:example.com/dep@v1.0.0+sum:<sha>"},
        {Name: "example.com/other", Source: "go-local:./cmd/foo", Version: "go-deps:example.com/other@v2.0.0+sum:<sha>"},
        ...
    },
}
```

`<sha>` は go.sum 行 ( `<path> <version> h1:...` 等) を SHA-256 した hex。 これにより同じ path@version でも go.sum の bytes が変われば ResolvedVersion が変わる ( 公式 Go 配布 + tampered mirror の食い違いを fingerprint が検知できる)。

旧版 ( PR の初期実装) は内部コード + 外部 module を 1 つの sha256 に詰めて `go-local:<entry>@sha256:<hex>` 形式の ResolvedVersion 1 本を返していたが、 現行は ExtraInputs / ResolvedVersion に分離している。 動機は [pnpm-local](./resolver-pnpm-local.md) と shape を揃えること、 および codegen → go-local の依存を depgraph に自動検知させること。

### Dispatch ( declared-only)

go-local resolver は `sloff.yml` の top-level `tools:` で `go-local: <import-path>` 形式 named 定義され、 task の `tools: [name]` で参照された場合にのみ起動する ( [ADR-0005](../adr/0005-eliminate-resolver-auto-dispatch.md) + [ADR-0008](../adr/0008-tool-as-first-class-spec-entity.md))。

```yaml
tools:
  protoc-gen-foo:
    go-local: ./cmd/protoc-gen-foo   # この tool 定義を含む sloff.yml の dir 相対
  go:
    exec: ["go", "version"]
    extract: 'go[0-9]+\.[0-9]+(?:\.[0-9]+)?'

commands:
  - name: gen
    cmd: ["go", "run", "./cmd/protoc-gen-foo"]
    inputs: ["**/*.proto"]
    outputs: ["**/*.pb.go"]
    tools: [protoc-gen-foo, go]   # Go toolchain bump も併せて取りたい場合は go も並べる
```

- entry は `./` / `../` 始まり、 または bare `.` / `..`。 nested 配置で parent dir 配下の generator を共有する場合は `../cmd/gen` の形を取れる ( ただし repoRoot を escape する path は OS-neutral fingerprint 保護のため fail)
- `cmd: ["go", "run", "./cmd/protoc-gen-foo"]` のように `go run` で起動する場合も、 上記の named 定義 + 参照を併記しない限り go-local は動かない ( cmd 形状からの auto-dispatch は持たない)
- build 済み binary を直接呼ぶケース ( `cmd: protoc-gen-foo`) も同様に named 参照を併記する

### Resolver 実装イメージ

```go
package golocal

import (
    "context"
    "crypto/sha256"
    "encoding/hex"
    "errors"

    "github.com/izumin5210/sloff/internal/sloff/toolresolver"
    "github.com/izumin5210/sloff/internal/sloff/toolresolver/lister"
)

const DepsPrefix = "go-deps:"

type Resolver struct {
    repoRoot string
    lister   lister.SourceLister // DI: 標準は goPackagesLister
}

func (r *Resolver) Resolve(ctx context.Context, specDir string, _ []string, declared *toolresolver.DeclaredTool) (toolresolver.Result, error) {
    if declared == nil || declared.Entry == "" {
        return toolresolver.Result{}, errors.New("go-local: declared entry is required")
    }
    listing, err := r.lister.List(ctx, specDir, declared.Entry)
    if err != nil {
        return toolresolver.Result{}, err
    }

    versions := make([]toolresolver.ResolvedVersion, 0, len(listing.ExternalModules))
    for _, m := range listing.ExternalModules {
        versions = append(versions, toolresolver.ResolvedVersion{
            Name:    m.Path,
            Source:  "go-local:" + declared.Entry,
            Version: encodeExternalVersion(m), // "go-deps:<path>@<version>+sum:<sha>"
        })
    }

    return toolresolver.Result{
        Versions:    versions,
        ExtraInputs: listing.InternalFiles, // files_hash 経由で content 反映
    }, nil
}

func encodeExternalVersion(m lister.ExternalModule) string {
    label := DepsPrefix + m.Path + "@" + m.Version
    if m.GoSumLine == "" {
        return label
    }
    sum := sha256.Sum256([]byte(m.GoSumLine))
    return label + "+sum:" + hex.EncodeToString(sum[:])
}
```

## SourceLister: `goPackagesLister` (`go/packages` を直接 import)

`goLocalResolver` の標準実装は `goPackagesLister`。 **`golang.org/x/tools/go/packages` パッケージを Go API で直接 import** して in-process で呼び出す。 sloff バイナリのメモリ空間内で完結し、 `go list` を subprocess で起動するオーバーヘッドがない。

```go
import "golang.org/x/tools/go/packages"

cfg := &packages.Config{
    // NeedEmbedFiles で //go:embed 対象を、 IgnoredFiles ( NeedFiles で取得) で
    // 別 GOOS/GOARCH 用 build-tag ファイルを hash 入力に取り込む。 これらが無いと
    // embed アセット変更や OS 違いで fingerprint の健全性 / OS 横断 fingerprint が破れる。
    Mode: packages.NeedFiles | packages.NeedEmbedFiles |
        packages.NeedImports | packages.NeedDeps | packages.NeedModule,
    // spec の作業ディレクトリ ( <repoRoot>/<specDir>) を Dir にする。
    // 多 Go module の monorepo で spec の隣に go.mod がある構成でも正しく解決される。
    Dir:  filepath.Join(repoRoot, specDir),
}
pkgs, err := packages.Load(cfg, "./cmd/protoc-gen-foo/...")
```

これは `go list -deps -json` と同等の情報を返す Go 公式 API ( 内部的には `go list` と同じ機構を使うが、 同一プロセス内のライブラリ呼び出しとして動く)。

探索範囲を **per-task の対象 main package とその transitive 依存** に限定する (`./cmd/protoc-gen-foo/...` の形)。 monorepo 全体に対する解析 (`./...`) は CLI 計測で約 7.5 秒 (`3.10s user 5.38s system 112% cpu 7.526 total`) と重く、 task ごとに必要な範囲を遥かに超えるため使わない。

### hash 経路は内部コードと外部パッケージで分離する

transitive 依存には「リポジトリ内の `.go` ファイル」と「`$GOMODCACHE` 配下の外部 module のソース」の 2 種類が含まれる。 両者を一律にファイル本体 SHA256 すると、 外部 module の minor bump で何百ものファイルを再 hash することになり性能が悪い。 また go.mod 全体の transitive 変更 ( 例: 一般的な ライブラリ依存 module の patch bump) を「該当 module 内の全 .go ファイル」を読んで反映する必要もない ( go.sum で内容が暗号学的に保証されているため)。

そこで Listing から:

- **InternalFiles** ( 内部コード) → ExtraInputs として task の inputs に union → files_hash 経路 ( runner が content hash)
- **ExternalModules** ( 外部 module) → 個別 ResolvedVersion として resolved_versions_hash 経路 ( go.sum 行 sha256 + path@version で識別)

判定戦略は以下のとおり:

| 種別 | 判定 | hash 入力 |
|---|---|---|
| stdlib | `pkg.Module == nil` | hash 対象から除外 ( $GOROOT 絶対 path が OS 横断 fingerprint を壊すため。 Go toolchain bump は別途 script resolver で `go version` を併記して捕捉する) |
| 内部コード | `pkg.Module.Main` ( 自リポジトリの module) | `pkg.GoFiles` + `pkg.EmbedFiles` + `pkg.IgnoredFiles` + `pkg.OtherFiles` のファイル本体を SHA256 ( `IgnoredFiles` で GOOS / GOARCH / build-tag に非依存、 `OtherFiles` で `.s` / `.c` / `.cc` / `.syso` 等の非 Go ソース変更も捕捉) |
| local replace 依存 | `pkg.Module.Replace != nil && Replace.Version == ""` | 内部コードと同じ hash 戦略 ( replace 先 directory の `GoFiles + EmbedFiles + IgnoredFiles + OtherFiles` をファイル本体で SHA256)。 go.sum で守られないため content hash が必須。 ただし replace 先が repoRoot 外を指す場合は OS 横断 fingerprint を壊すため fail する |
| versioned replace 依存 | `pkg.Module.Replace != nil && Replace.Version != ""` | 外部パッケージ扱い。 label は元 import path + `replace=<replace path>@<replace version>` で置換先まで含めて識別。 go.sum lookup は **置換先** の `Replace.Path` / `Replace.Version` で引く ( go.sum はその key で記録されるため) |
| 外部パッケージ ( 通常依存) | `pkg.Module` が外部 module を指す ( replace なし) | `module path@version` 文字列 + go.sum 該当行の hash。 go.sum は **load された全 main module の `Module.GoMod` の隣** から読み、 連結する ( nested-module monorepo / `go.work` で複数の main module が visible なケースでも全 module の go.sum 行が引けるように) |

```go
for each pkg in transitive(pkgs):
    if pkg.Module == nil {
        // stdlib: ファイル本体は hash しない ( $GOROOT 配下の絶対 path が
        // OS 横断 fingerprint を破壊するため)。 Go toolchain bump は
        // tools: [go] ( go: { exec: ["go", "version"], extract: ... }) を併記して捕捉する。
    } else if pkg.Module.Main {
        // 内部コード: ファイル本体を hash。 GoFiles + EmbedFiles ( //go:embed 対象)
        // + IgnoredFiles ( 別 GOOS / build-tag のため現 build context で除外
        // されたソース) + OtherFiles ( .s / .c / .cc / .syso 等の非 Go ソース)
        // を全部含めて、 host 環境非依存に hash する。 _test.go は除外。
        for f in pkg.GoFiles + pkg.EmbedFiles + pkg.IgnoredFiles + pkg.OtherFiles {
            hash.Write(readFile(f))
        }
    } else if pkg.Module.Replace != nil && pkg.Module.Replace.Version == "" {
        // local replace ( e.g. `replace foo => ../local`):
        // go.sum で保護されないため、 内部コードと同じく置換先 directory の
        // ファイル本体を hash する。 ただし replace 先が repoRoot 外を指す場合
        // ( 絶対 path / `../sibling-repo` 等) は dev machine ごとに path が
        // 異なって OS 横断 fingerprint が破れるため fail する ( future work)。
        for f in pkg.GoFiles + pkg.EmbedFiles + pkg.IgnoredFiles + pkg.OtherFiles {
            hash.Write(readFile(f))
        }
    } else {
        // 外部パッケージ ( 通常依存 or versioned replace): version + go.sum
        // 該当行で代用。 versioned replace は label に置換先 path/version を
        // 含めて識別、 go.sum lookup も置換先で引く。
        hash.Write([]byte(label(pkg.Module)))
        hash.Write([]byte(lookupGoSum(combinedGoSum, sumPath, sumVersion)))
    }
```

### 利点

- 外部 module の patch bump → version 文字列が変わる → 自動 invalidate ( ファイル本体を読まなくても version 比較で済む)
- 同 version なら中身を読まないので高速 ( `$GOMODCACHE` は数 GB 規模になりうる、 全 .go を SHA256 すると重い)
- go.sum で「version → 中身」が暗号学的に保証されているため、 「version + go.sum hash」が「中身全 hash」と等価な強度を持つ
- 内部コードは細かい変更にも敏感 ( `_test.go` 除外以外は普通の hash 戦略)
- 内部コードに `IgnoredFiles` を含めることで、 host の GOOS / GOARCH / build-tag 状態に依存せず同一 hash になる ( 別 OS の CI で計算しても同じ digest)
- stdlib は hash から除外。 Go toolchain bump を捕捉したい場合は spec の `tools:` map に `go: { exec: ["go", "version"], extract: 'go[0-9]+\.[0-9]+(?:\.[0-9]+)?' }` のような script tool を併設して、 task 側で `tools: [..., go]` と並べる

### `globLister` への retreat

`go/packages` で正しく取れない構造の Go プロジェクトに対応する場合は、 該当 `goLocalResolver` の `SourceLister` を `globLister` に切り替える:

```go
import "github.com/izumin5210/sloff/internal/sloff/toolresolver/lister"

resolver := golocal.New(lister.NewGlob([]string{"**/*.go"}, []string{"**/*_test.go"}))
```

これで該当パッケージのみ「精度は下がるが死角ゼロ」運用に切り替わる。 影響範囲は Resolver 単位なので局所化される。

### `SourceLister` 共通の挙動

[Architecture > SourceLister 共通の挙動 / 利点](./architecture.md#sourcelister-共通の挙動--利点) を参照。 メモ化 / OS 非依存 / sloff バイナリ単体完結等の共通機能。

## Batch prewarm ( spec dir 単位の `packages.Load` 集約)

多数の内製 generator を持つ monorepo では、 同一 Go module 内に住む go-local tool が
数十個 referenced されることがある。 resolver は tool ごとに `packages.Load` を呼ぶため、
**module の共有依存グラフ ( `go list` コストの大半) を tool 数だけ重複ロード**してしまう。
大きめの monorepo では go-local の解決フェーズが setup の最大項の一つになっていた。

これを `toolresolver.Prewarmer` フックで解消する:

1. runner は resolve のファンアウト前に 1 度だけ `Registry.Prewarm` を呼ぶ。 reqs は
   referenced な全 tool の `(specDir, declared)`。 `Run` ( Inputs + Versions) と
   `Plan` ( Inputs のみ) の両方で温める。
2. go-local resolver の `Prewarm` は reqs を **spec dir ごとに entry 集約**し、 各 spec dir で
   `BatchSourceLister.ListBatch` を 1 回呼ぶ。 spec dir 間は独立な `packages.Load` なので
   並列実行し、 warm phase のコストを「最大の spec dir 1 回ぶん」に均す。 ただし各 batch は
   `go list` ( 内部で GOMAXPROCS 並列) を起動しうるため、 並列度は NumCPU で上限を設ける
   ( runner の per-tool resolver fan-out と同方針。 spec dir が多い monorepo で file system と
   toolchain の並列を stampede させない)。
3. `goPackagesLister.ListBatch` は **1 回の `packages.Load(cfg, entry1, …, entryN)`** で全 entry を
   ロードし、 各 root package を **package directory で entry に対応付け**て個別 Listing を返す。
   結果は `Memoized` lister に prime されるので、 後続の per-tool `Inputs`/`Versions` は cache hit。

### soundness

batch の Listing は per-entry `List` と **byte-identical** でなければならない ( 異なると
`resolved_versions_hash` が変わり、 全 task が miss して再生成される)。 次の性質で担保する:

- `packages.Load` は同一 `PkgPath` の package を 1 インスタンスに dedup するが、 各 entry の
  Listing は **その entry の root から独立に walk** して作る ( visited セットは walk ごとにリセット)。
  → ある entry の到達可能集合は単一ロードと同じで、 他 entry の依存が混ざらない。
- go.sum 母集団は **各 entry の root から到達可能な main module だけ**にスコープして読む
  ( per-entry `List` と同一の母集団)。 単一 module なら全 entry が同じ母集団を共有するので
  読み込みは 1 回で済むが、 `go.work` で相互 import しない複数 main module が同一 spec dir に
  混在する場合でも、 ある entry の Listing に別 module の go.sum 行が混入しない
  ( batch 全体の母集団を共有すると `resolved_versions_hash` が prewarm の有無で変わってしまう)。
- entry が 1 root に 1:1 対応しないもの ( `./...` wildcard、 不在 package) は batch から除外し、
  呼び出し側が per-entry `List` に fallback する。

`Prewarm` は純粋な cache 温め最適化であり、 失敗は致命ではない ( runner は warn して per-tool
パスに委ねる)。 per-tool パスが同じ discovery を再計算し、 本物のエラーはそこで再浮上する。

## Preflight Checker は持たない ( Go の install model に由来する構造的理由)

go-local には install drift / build artefact freshness いずれの Checker も置いていない。 これは「 必要なのに省略している」 のではなく、 **Go の install model が drift を構造的に作らない** ため。

### Go は別途 install ステップを持たない ( on-demand download)

`go run ./cmd/foo` / `go build` / `go test` は default の `-mod=mod` で「 必要な module を実行時に `$GOMODCACHE` へ on-demand download」 する。 「 利用者が `git pull` で新 `go.mod` / `go.sum` を取り込んだだけで、 `go install deps` を別途走らせていない」 という pnpm 的な状態が **構造上発生しない**:

- 利用者の cmd ( `go run ./cmd/foo`) が実行されると、 必要 module は download or 既存 cache から read される
- sloff の `packages.Load` ( `go/packages` 経由) も同じ auto-download 経路を共有
- → cmd 実行時と sloff の hash 計算時で必ず同じ install state を見る

つまり pnpm-local が抱える「 lockfile を SSoT に取った hash」 vs 「 古い install で動く cmd」 という乖離が **Go では起こりえない**。

### エッジケース ( vendor / readonly / proxy off) も silent stale にはならない

- `GOFLAGS=-mod=readonly`: download 禁止モード。 必要 module 不在なら `go run` が **早期 fail**
- `GOFLAGS=-mod=vendor` / `vendor/` ディレクトリあり: `vendor/` を SSoT に。 ただし `go mod vendor` で `go.mod` と sync する運用を破ると `go build` が「 vendor の状態が go.mod と合わない」 で **早期 fail**
- `GOPROXY=off`: fingerprint miss なら **早期 fail**

いずれも「 fail-loudly か正しく動く」 のいずれかで、 silent stale に倒れない。 この性質は ADR-0007 で script resolver について書いた「 runtime バイナリの `--version` を取れば lockfile vs install drift が SSoT を runtime に置いた時点で構造的に発生しない」 と同じ構造で、 Go の場合は cmd ( `go run` 等) 自体が runtime SSoT として振る舞っている。

### 副次的: `packages.Load` 自体が install 検証を兼ねる

`packages.Load` を Resolve の中で呼ぶことそのものが実 build 経路 ( `$GOMODCACHE` 整備 + module graph 解決) の存在確認になっている。 transitive 依存が download されていなければ Resolve 段階でエラー → sloff 全体が止まる。 別途 preflight Checker を立てる意味は無い。

### pnpm-local との対比

pnpm は対照的に **`pnpm install` を別途明示実行** する model なので、 lockfile を SSoT に取りつつ install state がそれと乖離する余地がある。 そのため pnpm-local は preflight に install drift Checker を持つ ( [resolver-pnpm-local.md の Install drift check 節](./resolver-pnpm-local.md#install-drift-check-pnpm-install-忘れ検出--preflight-経由))。 「 preflight Checker の有無」 は資質の差ではなく、 **language ecosystem の install model の差** に由来する。

## 生成物を import する tool ( bootstrap パターン)

内製 tool の import 閉包に **自 repo の生成物** ( 例: 生成済み `*.pb.go`) が含まれる構成では、 生成物を一括削除した cold tree で `packages.Load` が閉包を compile できず、 Inputs / Versions の解決自体が失敗する。 解決の前提となる producer task を tool 定義の `depends` に宣言することで、 cold state からの `sloff run` 一発成功が成立する ( [ADR-0019](../adr/0019-tool-bootstrap-depends.md)):

```yaml
tools:
  protoc-gen-foo:
    go-local: ./cmd/protoc-gen-foo    # ./cmd/protoc-gen-foo が ../gen/options/*.pb.go を import している
    depends:
      - {spec: ../gen, task: gen-options}    # その pb.go を生成する task ( tool 定義 dir 相対)
```

- runner は宣言を **当該 tool を使う全 task** へ depends エッジとして注入する。 consumer ごとに producer リストを複製する従来の回避策は不要になる
- cold tree では解決が deferred に降格し、 宣言 depends の完了後 ( = 閉包が compile 可能になった後) に再解決される。 warm run は従来どおり run 冒頭の eager 解決で、 経路の変化はない
- `depends` の追従は利用者の責務: tool が新しい生成 package を import し始めたら宣言を追加する。 追従漏れは cold run の遅延解決失敗 / run-time overlap 検証 error として顕在化する ( silent stale には到達しない)
- 閉包 producer 自身がその tool を使う構成 ( 生成と利用が同一 task) は bootstrap が構造的に不可能なので注入時 error になる。 閉包 producer を tool 非依存の task に分割する

## Open Questions

- ~~`go run` 形式 ( CLI から呼ぶたびに `go build` する) と build 済み binary 形式の使い分け。 spec で明示宣言する形を採るか、 CLI 形式を auto-detect するか~~ → [ADR-0005](../adr/0005-eliminate-resolver-auto-dispatch.md) で declared-only に統一済み ( 両形式とも spec での明示宣言を必須とする)
- ~~transitive 依存に `replace` ディレクティブで local 置換された module が混じった場合の扱い ( 内部コード扱いにする / 外部扱いにする)~~ → versioned replace (`=> path version`) は外部扱い ( 置換先 path/version で go.sum lookup)、 local replace (`=> ../foo`) は内部扱い ( 置換先 directory のファイルを内部コードと同じく content hash)
- ~~`go.work` で複数 repo-local module を束ねる構成~~ → サポート済み: `packages.Visit` で全 main module を見つけ、 各 module dir の go.sum を連結して lookup の母集団とする
- 内製 protoc plugin が `go.mod` の `internal/...` パッケージに依存する場合の subset hash 戦略
- repoRoot 外を指す local replace ( 絶対 path / `../sibling-repo` 等) は dev machine ごとに path が変わるため、 現状は fail させる。 必要になった段階で「外部 directory を sandbox に正規化して hash する」戦略を ADR で起こす
