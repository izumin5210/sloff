# Resolver: pnpm-local

`pnpmLocalResolver` は pnpm workspace 内に実装された **内製 js/ts ツール** ( `workspace:*` 参照) を扱う。 SemVer を持たないため、 ソース変更も transitive 外部 npm dep の bump も両方 invalidation シグナルに乗せる必要がある。

関連:
- [Architecture](./architecture.md)
- [ADR-0005: Resolver auto-dispatch を廃止し、 すべて declared 経由に統一](../adr/0005-eliminate-resolver-auto-dispatch.md)
- [ADR-0007: lazygen は外部依存専用 resolver を持たない](../adr/0007-no-external-dependency-resolver.md)
- [ADR-0008: tool を first-class spec entity とする](../adr/0008-tool-as-first-class-spec-entity.md) ( D7 で「 build / run は cmd 責務」 を規定)
- [Resolver: go-local](./resolver-go-local.md) ( Go 側の対応物)

## Context

pnpm workspace では複数パッケージを 1 リポジトリに同居させ、 `workspace:*` 指定で相互依存できる。 ここで配布される内製 codegen ツール (`@org/my-codegen` 等) は npm registry に公開されない。

設計上、 `pnpm-local` は go-local と **同じ責務分担** に揃えている ( ADR-0008 D7):

- **lazygen の役割**: workspace package のソース集合を hash 入力に取り、 transitive 外部 dep の resolved version を ToolVersion として contribute する。 これだけ
- **利用者の役割**: build が必要なら task の cmd 内に build を含める ( go-local の `go run ./cmd/foo` が compile + run を 1 cmd に閉じるのと同じ責務分担)

つまり lazygen は build orchestration をしない。 spec から「 build task と consumer task の path overlap で連携」 のような暗黙概念は消えている。

「 内製ソース ( = `local`)」 という意味では [go-local](./resolver-go-local.md) と並立で、 Result の shape も揃う:

| | go-local | pnpm-local |
|---|---|---|
| 内部ソース enumeration | `go/packages` で transitive 走査 → InternalFiles | git-tracked + transitive workspace dep の git-tracked walk → ExtraInputs |
| 外部 dep version | go.sum + module path@version | pnpm-lock.yaml snapshots BFS |
| build/run の責務 | `go run` が cmd 内で 1 発 | `pnpm build && exec` 等を cmd 内で 1 発 |

## Resolver の動作

### 取得元

宣言された workspace package について 2 経路で contribution を出す:

1. **workspace member 判定**: 当該 `@org/foo` が `pnpm-lock.yaml` の `importers` キーに対応する workspace package で、 `package.json` の `name` も一致するか確認。 そうでなければ `ErrNotWorkspacePackage` を返す ( ADR-0007 により外部公開 npm パッケージは script resolver の領分)
2. **入力 ( ExtraInputs) 経路**:
   - 当該 workspace package の dir、 および `pnpm-lock.yaml` の `link:` 参照を辿った transitive な workspace dep の dir、 を集める ( workspace dep walk)
   - 各 dir で **git-tracked + untracked-but-not-ignored ファイルを enumerate** ( `git ls-files --cached --others --exclude-standard`)
   - 全 dir のファイル集合を union → ExtraInputs として返す
3. **外部 dep version 経路**:
   - workspace dep walk と並行して、 各 importer の direct external deps を seed に `snapshots` graph を BFS
   - transitive な外部 npm dep を `<pkg>@<version-with-peer-suffix>` で列挙 → `pnpm-deps:` prefix を付けた ToolVersion として返す

`.gitignore` で除外されたファイル ( 典型的には `dist/`, `build/`, `node_modules/`) は ExtraInputs に含まれない ( 利用者がローカルで生成する build artefact が cache を汚さない)。

### Result の形式

```go
toolresolver.Result{
    ExtraInputs: []string{
        "packages/codegen/package.json",
        "packages/codegen/src/cli.ts",
        "packages/codegen/src/lib.ts",
        "packages/util/package.json",         // ← workspace transitive dep
        "packages/util/src/index.ts",
    },
    Versions: []toolresolver.ToolVersion{
        {Name: "lodash@4.17.21",      Source: "pnpm-local:@org/codegen", Version: "pnpm-deps:lodash@4.17.21"},
        {Name: "some-helper@1.2.3",   Source: "pnpm-local:@org/codegen", Version: "pnpm-deps:some-helper@1.2.3"},
    },
}
```

### Dispatch ( declared-only + named-tool)

[ADR-0005](../adr/0005-eliminate-resolver-auto-dispatch.md) + [ADR-0008](../adr/0008-tool-as-first-class-spec-entity.md) により lazygen は declared-only + named-tool。 `tools:` map で `pnpm-local: <package-name>` 形式 named 定義され、 task の `tools: [name]` で参照された場合にのみ起動する。

### Resolver の利用例

```yaml
# packages/codegen/lazygen.yml ( 共通配置だが、 root の lazygen.yml に集約しても良い)
tools:
  codegen:
    pnpm-local: "@org/codegen"

# proto/lazygen.yml ( 利用側 task)
commands:
  - name: gen
    cmd: ["sh", "-c", "pnpm --filter @org/codegen build && pnpm exec my-codegen"]
    inputs: ["**/*.proto"]
    outputs: ["**/*.pb.ts"]
    tools: [codegen]
```

ここで起きること:

- `gen` の inputs に pnpm-local 経由で `packages/codegen/**` の git-tracked ファイル ( + transitive workspace deps) が contribute される
- src 編集 → ExtraInputs ( files_hash 経由) が変化 → gen の cache miss → cmd 再実行 → cmd 内の `pnpm build` が走り、 続いて `pnpm exec my-codegen`
- 外部 npm dep ( lodash 等) の resolved version 変化 → tools_hash 変化 → gen 再実行
- ts-node / tsx で `bin: src/cli.ts` ( build 不要) なら cmd は `["pnpm", "exec", "my-ts-codegen"]` でよい ( ts-node が src を直接読む)

build が必要かどうかは **cmd の中身次第** で、 lazygen は知らない。 dist/src のような慣習名前を一切前提しない。

### Resolver 実装イメージ

```go
package pnpmlocal

const Name = "pnpm-local"
const DepsPrefix = "pnpm-deps:"

type Resolver struct {
    repoRoot   string
    enumerator FileEnumerator   // 標準は GitLsFiles
    // workspace は pnpm-lock.yaml + 各 importer の package.json を index 化したもの
    // ( 初回 Resolve で sync.Once 経由で lazy load)
}

func New(repoRoot string, enumerator FileEnumerator) (*Resolver, error) { /* ... */ }

func (r *Resolver) Resolve(ctx context.Context, _ string, _ []string, declared *toolresolver.DeclaredTool) (toolresolver.Result, error) {
    pkg, ok := r.workspace.Lookup(declared.PackageName)
    if !ok { return Result{}, fmt.Errorf("%w: %q", ErrNotWorkspacePackage, declared.PackageName) }

    // workspace 集合と外部 deps を 1 pass で walk
    walk, _ := WalkDeps(r.workspace.lockfile, filepath.ToSlash(pkg.Dir))

    // 各 workspace dir で git-tracked ファイルを enumerate
    extraInputs := r.collectFiles(ctx, walk.Workspaces)

    // 外部 deps を ToolVersion 化
    versions := []toolresolver.ToolVersion{}
    for _, e := range walk.Externals {
        versions = append(versions, toolresolver.ToolVersion{
            Name:    e,
            Source:  Name + ":" + pkg.Name,
            Version: DepsPrefix + e,
        })
    }
    return toolresolver.Result{Versions: versions, ExtraInputs: extraInputs}, nil
}
```

### `git ls-files` を enumerator に採用した理由

```
git ls-files --cached --others --exclude-standard -- <pkg-dir>
```

- `--cached`: 既に track 済みのファイル ( 利用者が commit した正規の集合)
- `--others --exclude-standard`: untracked かつ `.gitignore` で除外されていないファイル ( commit 予定だがまだ `git add` してないファイル)

この組合せで「 利用者が repo に置きたい / 置いている全ファイル ( ただし `.gitignore` で除外されたものは除く)」 を得る。 Turborepo の default も同じ哲学。

代替検討:
- **filepath.Walk + Go の gitignore library**: in-process だが `.gitignore` の semantics ( 否定 `!`、 nested rules、 `**` glob 等) を完全実装したライブラリの選定 / メンテコストがある
- **filepath.Walk + 慣習的な exclude list ( `dist/`, `build/`, `node_modules/`)**: 慣習依存 ( ADR-0008 D7 の精神に反する)
- **esbuild Go API で bin entry から transitive 解析** ( 旧設計): 精度高いが eval / 動的 require / 動的 import で死角、 esbuild バイナリサイズ増、 build 必要なツールでは bin が存在しない fresh checkout で fall-back 必要、 tool 概念と build task の path overlap link が必要 ( 暗黙性問題)

`git ls-files` 採用は subprocess 1 回の overhead を許容する代わりに、 git の事実だけを SSoT に取る一番素直な選択。 lazygen の cache record も git 管理前提なので、 git 依存はすでに implicit。

### 過剰 invalidate の許容

git-tracked enumeration は「 package dir 内の利用者が置いている全ファイル」 を input にするので、 bin から実際に transitive で import されてないファイル ( e.g., `README.md`、 別の bin の lib) も input に含まれる。 これらの編集で consumer task が rerun する **過剰 invalidate** が起きる:

- false hit ( 古い結果で cache hit) は起きない ( 健全)
- false miss ( 不要な rerun) が起きうる
- Turborepo の default も同じ精度で、 monorepo 規模での実用上の精度は問題にならないことが知られている

精度より「 死角ゼロ + シンプル」 を取った設計判断。 旧 esbuild walk 時代の Open Question ( 「 esbuild が解析できないパターンに対する fall-back」) は構造的に消えた。

## 外部 npm dep の surgical lockfile walk

宣言された workspace package について、 `pnpm-lock.yaml` の `importers.<package-dir>.dependencies` / `devDependencies` / `optionalDependencies` を起点に、 link: edge を辿って到達する全 importer の direct external deps を seed として `snapshots` graph を BFS で walk し、 transitive な外部 npm dep を `<pkg>@<version>` 文字列の sorted unique list で得る。 `link:` / `file:` ( workspace link) は外部扱いせず skip ( workspace 側は ExtraInputs 経路で別途 enumerate される)。

```go
walk := WalkDeps(lockfile, "packages/codegen")
// walk.Workspaces = ["packages/codegen", "packages/util", ...]
// walk.Externals  = ["lodash@4.17.21", "react-dom@18.0.0(react@18.0.0)", ...]
```

**workspace と external を 1 pass で walk するのが肝**: codegen が util ( link) を経由して lodash ( npm) に到達する場合、 codegen の importer entry 直下には lodash は無い。 link 経由で util の importer entry に降りて初めて lodash が seed に積まれる。 「 workspace-blind な external walk」 ではこのケースを取りこぼす。

詳細は [ADR-0007](../adr/0007-no-external-dependency-resolver.md) の「外部依存の surgical 取り扱い」 節も参照。

## Install drift check ( `pnpm install` 忘れ検出) — preflight 経由

pnpm-local の hash 入力は **lockfile から walk した resolved version** だが、 cmd 実行時は **node_modules に install された実体** を読み込む。 利用者が `git pull` で新 `pnpm-lock.yaml` を取り込んでから `pnpm install` を忘れると、 hash 経路は新 lockfile を、 runtime は古い install を見ることになる ( silent stale)。

これを構造的に防ぐため、 **`internal/lazygen/preflight/pnpmlocal`** に Checker を置いている。 runner が cmd 実行前に preflight を回す段階で:

```
hash(<root>/pnpm-lock.yaml) == hash(<root>/node_modules/.pnpm/lock.yaml) ?
  → yes: install in sync ( 続行)
  → no:  preflight.Issue を返し runner が fail-loudly ( "please run `pnpm install`")
```

`node_modules/.pnpm/lock.yaml` は pnpm が `pnpm install` 時に **`pnpm-lock.yaml` を byte-for-byte コピー** して書き出す install state snapshot。 byte 比較で:

- snapshot 不在 → `pnpm install` 未実行
- byte mismatch → lockfile 編集後 install 忘れ ( whitespace のみの edit でも検知、 これは feature)

実装の本体 ( byte 比較ロジック) は `AssertInstallInSync(repoRoot)` ( `drift.go`)、 sentinel は `ErrInstallStale`。 preflight Checker (`preflight/pnpmlocal`) はこれを呼んで結果を `preflight.Issue` に詰めるだけの薄いラッパ。

### preflight 経由にする利点 ( resolver 内に置かないこと)

- **`LAZYGEN_ALLOW_STALE_DEPS=1` の escape hatch を継承**: 利用者が一時的に通したいケース ( experimental edit を試したい等) で warn 降格 + read-only モードで run できる。 resolver 内で fail させると、 この escape hatch 経路が効かない
- **概念整理**: 「 preflight = state 検証」 という general subsystem として一貫させ、 「 build 用 preflight は廃止 / install drift 用 preflight は別経路」 のような暗黙の分類を作らない ( ADR-0008 D7 末尾参照)
- **scope-by-referenced-resolver**: runner は「 spec で実際に referenced されている resolver name」 集合を作って、 一致する Checker だけ起動する。 pnpm-local 未使用の repo では Checker そのものが起動しないので、 catalog-style な repo でも余計な validation が走らない

### 設計上の補足

- pnpm の hash アルゴリズム ( `getLockfileHash`) を replicate せず、 「 pnpm が書いた snapshot をそのまま比較」 で済ませている。 アルゴリズム drift / pnpm version 互換の risk なし
- subprocess なし ( `pnpm install --frozen-lockfile` のような外部 invocation を使わない)
- pnpm-lock.yaml が SSoT であることは変わらない ( ADR-0007 の責務境界そのまま)。 install state は **drift detection のみ** に使い、 hash 入力には混ぜない ( drift 通過時は両者一致しているので冗長)
- 検討した代替案: `.modules.yaml.rootProject.lockfile.checksum` field を読む案、 アルゴリズムを Go で replicate する案、 `pnpm install --frozen-lockfile --offline` を subprocess する案。 いずれも .pnpm/lock.yaml の byte 比較より複雑 / 脆い ( 実検証で `.modules.yaml` には期待していた checksum field が無いことも判明)

## Preflight Checker は持たない

旧設計では「`bin` が `dist/` を指していれば build 必須、 `dist/` が存在 / src より新しいかで判定」 という preflight checker を持っていたが、 ADR-0008 D7 で「 build / run は cmd 責務」 と決めた時点で構造的に不要になった:

- `dist/` `src/` は pnpm / npm 標準ではなくコミュニティ慣習にすぎず、 lazygen が前提にすると別 layout の repo で破綻する
- 「 rebuild 忘れ」 は cmd 内に `pnpm build && exec` を書いてもらうことで構造的に消える ( source 編集 → files_hash 変化 → cmd 再実行 → cmd 内 build → 最新 binary で実行)
- pnpm-local 自身は「 利用者が repo に置いているファイルを inputs に乗せる」 input contributor だけを担う

`pnpm install` 忘れの「 install drift」 は別で検出する ( 上記 「 Install drift check」)。

## Open Questions

- pnpm-lock.yaml schema が v6 / v9 等で変動した場合の互換性 ( 現状 v9 専用、 v6 で動かすと importers / snapshots の shape が違って fail する。 schema version で fail-fast するか、 マルチ schema 対応するかは将来検討)
- non-git repository 環境 ( CI で git history なしで checkout 等) では `git ls-files` が fail する。 現状は明示 error にしているが、 fall-back path ( filepath.Walk + 簡易 ignore) を提供する必要があるかは将来判断
- workspace transitive dep walk で `link:` 以外の workspace 表現 ( pnpm の `workspace:^` `workspace:~` `workspace:*` で resolved 形式が変わる ) のカバレッジ確認 ( 現状は `link:` のみ判定)
