# Resolver: pnpm-local

`pnpmLocalResolver` は pnpm workspace 内に実装された **内製 js/ts ツール** ( `workspace:*` 参照) を扱う。 このツールは npm registry に公開されておらず、 SemVer による論理 version を持たない。 lazygen は cache 健全性を保つため、 そのソース変更も runtime で resolve される外部 npm 依存も両方 invalidation シグナルに乗せる必要がある。

関連:
- [Architecture](./architecture.md)
- [ADR-0005: Resolver auto-dispatch を廃止し、 すべて declared 経由に統一](../adr/0005-eliminate-resolver-auto-dispatch.md)
- [ADR-0007: lazygen は外部依存専用 resolver を持たない](../adr/0007-no-external-dependency-resolver.md)
- [Resolver: go-local](./resolver-go-local.md) ( Go 側の対応物 = 内製 Go CLI)

## Context

pnpm workspace では複数パッケージを 1 リポジトリに同居させ、 `workspace:*` 指定で相互依存できる。 ここで配布される内製 codegen ツール (`@org/my-codegen` 等) は npm registry に公開されない。 lazygen での扱いを考えるとき、 同じ workspace 内ツールでも build 必須かどうかで状況が変わる:

- **ts-node / tsx で直接 src を実行**: `bin` が src/ を指す。 src 編集が即 runtime に反映
- **build 必須 ( tsc / esbuild 等で dist/ を生成)**: `bin` が dist/ を指す。 src 編集後に build しないと runtime には反映されない

この 2 つを統一的に扱うため、 lazygen の pnpm-local は **「実際に runtime が読み込むファイル ( = bin/main の transitive import 集合) を input として task に contribute する」 input contributor 設計** を取る。 build orchestration 自体は別の通常 task ( codegen-build 等) として lazygen に書き、 depgraph が output overlap で勝手に依存を解決する ( = Turborepo の `dependsOn` を file-overlap でやる版)。

「内製ソース ( = `local`)」 という意味では [go-local](./resolver-go-local.md) の対応物。 ただし pnpm-local は **input contributor** として動く点 ( ExtraInputs を runner に返す)、 および **transitive な外部 npm dep の resolved version を `tools_hash` に注入する** 点で go-local と挙動が異なる。

ソースファイル列挙には Resolver 内部 helper の `SourceLister` を使う。 標準実装は **esbuild の Go API を直接 import した `esbuildLister`** で、 entry point から transitive な import 解析を行う。

## Resolver の動作

### 取得元

宣言された workspace package について 2 経路で contribution を出す:

1. **`pnpm-lock.yaml` で当該 package が workspace member であるかを判定** ( `importers` のキーに該当 dir があり、 そこの `package.json` の `name` が一致するか)。 そうでなければエラー ( ADR-0007 により外部公開 npm パッケージは script resolver の領分)
2. 該当 package の `package.json` から `bin` / `main` を読み、 entry point を確定
3. **入力 ( ExtraInputs) 経路**: `SourceLister` ( デフォルト `esbuildLister`) で entry point の transitive import 集合を repo-relative path で取得 → runner が task の inputs に union ( files_hash 経路に乗る)
4. **外部 dep version 経路**: `pnpm-lock.yaml` の graph を walk して当該 package の transitive 外部 npm dep を resolved version 付きで列挙 → ToolVersion として返す ( tools_hash 経路に乗る)

### 論理 version 文字列の形式

外部 dep ごとに 1 ToolVersion を返し、 文字列形式は:

```
"pnpm-external:<pkg>@<version-with-peer-suffix>"
```

例: `"pnpm-external:lodash@4.17.21"`、 `"pnpm-external:react-dom@18.0.0(react@18.0.0)"`

peer-context suffix ( pnpm の `(peer@x)` 形式) はそのまま保持し、 peer 変動も invalidate に乗せる。

ExtraInputs 側は ToolVersion ではないので version 文字列は持たない ( files_hash の構成要素として content hash 経路に流れる)。

### Dispatch ( declared-only)

[ADR-0005](../adr/0005-eliminate-resolver-auto-dispatch.md) により lazygen は declared-only。 `tools: [{pnpm-local: <package-name>}]` で宣言された場合に起動し、 cmd 形状からの auto-dispatch は持たない。 declared.PackageName が workspace に存在しなければ `ErrNotWorkspacePackage` を返す。

### Resolver の利用例

```yaml
commands:
  # workspace package の build を「ただのコード生成 task」として宣言
  - name: codegen-build
    cmd: ["pnpm", "--filter", "@org/codegen", "build"]
    inputs:
      - "packages/codegen/src/**"
      - "packages/codegen/package.json"
      - "packages/codegen/tsconfig.json"
    outputs:
      - "packages/codegen/dist/**"
    tools:
      - exec: ["pnpm", "--version"]

  # 利用側 task ( pnpm-local が dist/cli.js とその transitive を inputs に contribute)
  - name: gen
    cmd: ["pnpm", "exec", "my-codegen"]
    inputs: ["**/*.proto"]
    outputs: ["**/*.pb.ts"]
    tools:
      - pnpm-local: "@org/codegen"
```

ここで起きること:

- `gen` の inputs が pnpm-local 経由で `packages/codegen/dist/cli.js` ( と transitive) を含む
- depgraph: `codegen-build.outputs = packages/codegen/dist/**` と overlap → **`gen → codegen-build` の依存が自動で貼られる**
- src 編集 → codegen-build の files_hash 変化 → codegen-build 再実行 → dist 更新 → gen の files_hash 変化 → gen 再実行
- 外部 npm dep ( lodash 等) の resolved version 変化 → gen の tools_hash 変化 → gen 再実行 ( ts-node / 直接 src 実行のケースで build task を介さなくても流れる)
- ts-node / tsx で `bin: src/cli.ts` の場合: ExtraInputs が src ファイルを直接拾うので、 build task は不要 ( src 編集が即 invalidate)

ts-node なら build task 不要、 build 必須なら codegen-build を併設、 と利用者が宣言する。 lazygen 側は `dist/` `src/` という慣習名を一切前提しない。

### Resolver 実装イメージ

```go
package pnpmlocal

import (
    "context"
    "errors"

    "github.com/izumin5210/lazygen/internal/lazygen/toolresolver"
    "github.com/izumin5210/lazygen/internal/lazygen/toolresolver/lister"
)

const Name = "pnpm-local"

type Resolver struct {
    repoRoot string
    lister   lister.SourceLister // DI: 標準は esbuildLister
    // workspace は pnpm-lock.yaml + 各 importer の package.json を index 化したもの
    // ( 初回 Resolve で sync.Once 経由で lazy load)
}

func New(repoRoot string, l lister.SourceLister) (*Resolver, error) { /* ... */ }

func (r *Resolver) Resolve(ctx context.Context, _ string, _ []string, declared *toolresolver.DeclaredTool) (toolresolver.Result, error) {
    pkg, ok := r.workspace.Lookup(declared.PackageName)
    if !ok {
        return toolresolver.Result{}, fmt.Errorf("%w: %q", ErrNotWorkspacePackage, declared.PackageName)
    }

    // ExtraInputs: bin/main から transitive で import されるファイル群
    extraInputs := r.collectInputs(ctx, pkg)

    // Versions: workspace package の transitive 外部 npm dep ( surgical lockfile walk)
    versions := r.collectExternalVersions(pkg)

    return toolresolver.Result{Versions: versions, ExtraInputs: extraInputs}, nil
}
```

### 初期 build 前 ( fresh checkout) の振る舞い

dist/ が gitignore された典型的な構成では、 fresh clone 直後に `bin` ファイル ( e.g. `packages/codegen/dist/cli.js`) はまだ存在しない。 esbuild は missing entry でエラーになるので、 Resolver は **bin path 単独を ExtraInputs に含めて lister 呼び出しを skip** する fall-back を持つ:

- depgraph は bin path と build task の `dist/**` outputs glob で overlap 検出 → 依存 edge を貼る
- 1 回目の lazygen run: build → dist 生成 → gen 実行
- 2 回目以降: bin が存在するので esbuild が transitive を walk、 通常通り

1 回目は inputs 集合が「 bin path のみ」 なので transitive 部分の invalidation 精度が一時的に下がるが、 2 回目以降で完全な集合に切り替わるだけで cache 健全性は壊れない ( 「 false hit」 ではなく 「 1 回目だけ過剰 invalidate」 側に倒れる)。

## SourceLister: `esbuildLister` ( esbuild を Go API で直接 import)

`pnpmLocalResolver` の標準 SourceLister は esbuild の Go API `github.com/evanw/esbuild/pkg/api` を直接 import した `esbuildLister`。 `Bundle: true` + `Metafile: true` で entry の transitive import 集合を取り、 metafile の `inputs` から repo-relative path を抽出する。 `node_modules` 配下のパスは ADR-0007 に従って除外し、 ToolVersion 側 ( surgical lockfile walk) で扱う。

```go
import "github.com/evanw/esbuild/pkg/api"

result := api.Build(api.BuildOptions{
    EntryPoints: []string{absPath},
    AbsWorkingDir: repoRoot,
    Bundle:      true,
    Platform:    api.PlatformNode,
    Metafile:    true,
    Write:       false, // 実際の build artifact は書き出さない
    LogLevel:    api.LogLevelSilent,
})
// result.Metafile (JSON) を parse → inputs.keys のうち node_modules を除外 → 配列 sort
```

利点 / 制約 / 代替検討は [ADR-0007](../adr/0007-no-external-dependency-resolver.md) に詳述。 要点:

- esbuild バイナリの別途 install 不要 ( go.mod で固定、 OS 横断キャッシュを破らない)
- subprocess なし ( in-process)
- esbuild が静的解析できないパターン ( eval / runtime `require` / 動的 `import()`) は非サポート → 該当 package のみ `lister.NewGlob` に切替えて「精度低下を受け入れて死角ゼロ」 運用に retreat 可能 ( DI 経由)

### `globLister` への retreat

```go
import "github.com/izumin5210/lazygen/internal/lazygen/toolresolver/lister"

resolver, _ := pnpmlocal.New(repoRoot,
    lister.NewGlob(repoRoot,
        []string{"**/*.{ts,tsx,js,json}"},
        []string{"**/*.test.ts", "**/*.spec.ts", "dist/**", "node_modules/**"}))
```

影響範囲は Resolver 単位なので、 1 つの「 esbuild 解析不能 package」 のためにリポジトリ全体の精度を落とす必要はない。

## 外部 npm dep の surgical lockfile walk

宣言された workspace package について、 `pnpm-lock.yaml` の `importers.<package-dir>.dependencies` / `devDependencies` / `optionalDependencies` を起点に `snapshots` graph を BFS で walk し、 transitive な外部 npm dep を `<pkg>@<version>` 文字列の sorted unique list で得る。 `link:...` / `file:...` ( workspace link) は skip。

```go
externals := CollectExternals(lockfile, "packages/codegen")
// → ["lodash@4.17.21", "react-dom@18.0.0(react@18.0.0)", "react@18.0.0", ...]
```

これで「ある workspace package の transitive npm dep」 だけを surgical に hash 入力に取る。 別 workspace package の dep が動いても、 当該 package に関係なければ tools_hash は変わらない ( = Turborepo 同等の精度)。

詳細は [ADR-0007](../adr/0007-no-external-dependency-resolver.md) の「esbuild Go API 採用」 節と「外部依存の surgical 取り扱い」 節を参照。

## Preflight Checker は持たない

旧設計では「`bin` が `dist/` を指していれば build 必須、 `dist/` が存在 / src より新しいかで判定」 という preflight checker を持っていた。 **これは廃止された**:

- `dist/` `src/` は pnpm / npm 標準ではなくコミュニティ慣習にすぎず、 lazygen がそれを前提にすると別 layout のリポジトリで破綻する
- 「rebuild 忘れ」 は build を **通常の lazygen task として宣言する** ことで構造的に消える ( depgraph が build → consumer の連鎖を勝手に貼り、 src 編集 → build 再実行 → consumer 再実行 が自動で流れる)
- pnpm-local 自身は「実際に runtime が読み込むファイルを inputs に乗せる」 input contributor だけを担う

つまり Preflight が必要だった問題を、 input contribution + depgraph の組み合わせで構造的に解消している。

## Open Questions

- 複数 entry point ( `bin` が複数定義) を持つパッケージの hash 計算は現状「全 entry の union」 でよいか ( esbuild を entry 数だけ呼ぶコスト vs 漏れリスク)
- esbuild の `tsconfig.json` パス解決 ( 親階層を遡る挙動) が大きな monorepo で正しく動くか
- workspace transitive dep ( `@org/util` を `@org/codegen` から import) を esbuild は symlink 経由で real path に resolve するか実環境検証が必要 ( 設計上は `PreserveSymlinks: false` で resolve され、 metafile の `inputs` キーは real path となるはず)
- 「 surgical lockfile walk」 の cache key (= `<pkg>@<version-with-peer>`) が pnpm-lock.yaml schema 変動 ( v6 → v9 等) で互換性を破る場合のための schema version pin / 検出 ( 現状 v9 専用)
