# Resolver: script

`scriptResolver` は **任意の prebuilt binary ツール** の論理 version を、 そのバイナリ自身に問い合わせて取得する汎用 resolver。 nix / mise / aqua 等の version manager で配布される OSS バイナリ や、 Go の `go.mod` `tool` ディレクティブ経由で得られるツール、 `pnpm exec` 経由で起動する npm bin 等を一律に扱う。

関連:
- [Architecture](./architecture.md)
- [ADR-0001: キャッシュ可能コード生成オーケストレーターの選定](../adr/0001-cache-aware-codegen-orchestrator-decision.md)
- [ADR-0007: sloff は外部依存専用 resolver を持たない](../adr/0007-no-external-dependency-resolver.md) — npm / Go OSS パッケージも script で吸収

## Context

prebuilt binary 配布物 (`darwin-arm64` / `linux-amd64` 等) は OS 別にバイナリ実体が異なるため、 ファイル本体 SHA256 を hash 入力にすると OS 横断キャッシュ共有 ( ADR-0001 の 防御線 (1)) を破壊する。 一方、 これらのバイナリは大抵 `--version` 等の version 取得コマンドを備えており、 出力される version 文字列は OS 横断で同一である。

「実際に install されている binary の `--version` 出力」を hash 入力にすれば、 以下が同時に成り立つ:

- **OS 中立**: 出力される version 文字列はバイナリ本体ではなく logical な識別子であり、 `darwin-arm64` / `linux-amd64` の差分が記録に乗らない
- **lockfile vs install のズレ自動解消**: 「lockfile を更新したが install 忘れた」状態でも、 `--version` は **実際に install されているバイナリ** の version を返すため、 hash は実 build 結果と必ず整合する。 嘘 record にはならない (旧 version の出力 → 旧 hash で記録され、 install しなおした瞬間に version 文字列が変わって自然 invalidate)

つまり script resolver は ADR-0001 の防御線 (1) と、 旧 (2) preflight を **構造的に同時達成する**。 preflight 追加検証は不要。

## Resolver の動作

### 取得元

spec の top-level `tools:` map で名前付き定義された `exec` ( cmd 配列) を実行し、 stdout を捕捉する。 必要なら `extract` で正規表現を当てて version 部分を切り出す ( ADR-0008 で named-tool 化)。

```yaml
# <spec_dir>/sloff.yml
tools:
  buf:
    exec: ["buf", "--version"]
    # 既定: stdout を trim してそのまま logical version とする
  protoc-gen-go:
    exec: ["protoc-gen-go", "--version"]
    # protoc-gen-go --version → "protoc-gen-go v1.34.2"
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'

commands:
  - name: protoc-gen-go
    cmd: buf generate --template buf.gen.yaml
    inputs: ["**/*.proto", "buf.gen.yaml"]
    outputs: ["**/*.pb.go", "**/*.connect.go"]
    tools: [buf, protoc-gen-go]
```

### 論理 version 文字列の形式

```
"script:<exec の base name>@<extracted or stdout>"
```

例:
- `"script:buf@1.30.0"` ( aqua 配布 buf の `buf --version` 出力)
- `"script:protoc-gen-go@v1.34.2"` ( `protoc-gen-go --version` 出力から regex 抽出)
- `"script:go@go1.26.2"` ( `go version` 出力から regex で `go[0-9.]+` を抽出)

### 既定挙動

`extract` が指定されていない場合、 stdout 全体を trim ( 前後空白 / 末尾改行除去) してそのまま version 文字列に採用する。

`extract` が指定されている場合、 Go の `regexp` で評価し、

- capturing group がある場合: group 1 のマッチ部分
- capturing group が無い場合: マッチ全体

を採用する。 `^/$` アンカーは必要なら自分で書く。

### 失敗時の挙動

以下のケースでは即時 fail:

- `exec` が non-zero exit code を返した
- stdout が空、 もしくは `extract` がマッチしなかった
- `exec[0]` が `$PATH` 上に見つからない

cmd 文字列だけで `resolved_versions_hash` を fallback 計算するような暗黙挙動は **入れない**。 「version が取れていない」事故を構造的に防ぐため、 失敗は明示する。

### Dispatch (declared-only)

[ADR-0005](../adr/0005-eliminate-resolver-auto-dispatch.md) + [ADR-0008](../adr/0008-tool-as-first-class-spec-entity.md) により、 sloff には cmd 形状から resolver が自動的に名乗り出る auto-dispatch は無く、 script resolver も `tools:` map での named 定義を経由してのみ起動する。 「とりあえず `cmd[0] --version` を呼ぶ」自動推定は、 出力に build timestamp や OS-arch を含むツール ( e.g., `go version go1.26.2 darwin/arm64`) で OS 横断キャッシュを壊す可能性があるため、 利用者の明示宣言を必須とする。

```yaml
tools:
  buf:
    exec: ["buf", "--version"]

commands:
  - name: gen
    # ...
    tools: [buf]
```

宣言が欠けている spec は [ADR-0004 D1](../adr/0004-spec-validation-and-output-conflict-policy.md) の `tools:` 必須化により spec validation で fail する。 「宣言なしで script resolver に落ちる」 fallback 経路は存在しない。

### Resolver 実装イメージ

```go
package script

import (
    "bytes"
    "context"
    "errors"
    "fmt"
    "os/exec"
    "path/filepath"
    "regexp"
    "strings"
    "sync"

    "github.com/izumin5210/sloff/internal/sloff/toolresolver"
)

const Name = "script"

type Resolver struct {
    repoRoot string

    mu    sync.Mutex
    cache map[string]string // (exec joined + extract) → 解決済み version 文字列のメモ化
}

func New(repoRoot string) *Resolver {
    return &Resolver{repoRoot: repoRoot, cache: map[string]string{}}
}

func (r *Resolver) Name() string { return Name }

// Resolve は declared 経由でのみ呼ばれる ( ADR-0005)。 declared.Exec が
// script resolver の入力で、 declared.Extract が任意の正規表現。
func (r *Resolver) Resolve(ctx context.Context, specDir string, _ []string, declared *toolresolver.DeclaredTool) ([]toolresolver.ResolvedVersion, error) {
    if declared == nil {
        return nil, errors.New("script: requires explicit tools[] declaration; auto-dispatch is not supported")
    }
    if len(declared.Exec) == 0 {
        return nil, errors.New("script: exec is required")
    }

    cacheKey := strings.Join(declared.Exec, "\x00") + "\x01" + declared.Extract

    r.mu.Lock()
    if cached, ok := r.cache[cacheKey]; ok {
        r.mu.Unlock()
        return []toolresolver.ResolvedVersion{makeVersion(declared.Exec[0], cached)}, nil
    }
    r.mu.Unlock()

    stdout, err := r.runVersion(ctx, specDir, declared.Exec)
    if err != nil {
        return nil, err
    }
    captured, err := applyExtract(stdout, declared.Extract)
    if err != nil {
        return nil, err
    }

    r.mu.Lock()
    r.cache[cacheKey] = captured
    r.mu.Unlock()

    return []toolresolver.ResolvedVersion{makeVersion(declared.Exec[0], captured)}, nil
}

func (r *Resolver) runVersion(ctx context.Context, specDir string, argv []string) (string, error) {
    c := exec.CommandContext(ctx, argv[0], argv[1:]...)
    c.Dir = filepath.Join(r.repoRoot, specDir)
    var out bytes.Buffer
    c.Stdout = &out
    if err := c.Run(); err != nil {
        return "", fmt.Errorf("script: %s failed: %w", strings.Join(argv, " "), err)
    }
    return strings.TrimSpace(out.String()), nil
}

func applyExtract(stdout, pattern string) (string, error) {
    if pattern == "" {
        if stdout == "" {
            return "", errors.New("script: stdout is empty (no extract pattern configured)")
        }
        return stdout, nil
    }
    re, err := regexp.Compile(pattern)
    if err != nil {
        return "", fmt.Errorf("script: invalid extract pattern %q: %w", pattern, err)
    }
    m := re.FindStringSubmatch(stdout)
    switch {
    case m == nil:
        return "", fmt.Errorf("script: extract pattern %q did not match stdout %q", pattern, stdout)
    case len(m) >= 2:
        return m[1], nil
    default:
        return m[0], nil
    }
}

func makeVersion(execHead, captured string) toolresolver.ResolvedVersion {
    bin := filepath.Base(execHead)
    return toolresolver.ResolvedVersion{
        Name:    bin,
        Source:  "script:" + bin,
        Version: "script:" + bin + "@" + captured,
    }
}
```

`Source` には `exec[0]` の base name のみを使う ( e.g. `"script:buf"`)。 `extract` regex やフルコマンドラインを混ぜないのは、 record の `tools[].source` を読み解く際に「どの binary の version か」が即座に分かるようにするため。

### 1 run 内のメモ化

同一の `(exec, extract)` 組は sloff 1 run 内で **1 度だけ実行** し、 結果を memoize する。 数十 task が同じ `buf --version` を要求しても subprocess は 1 回しか起動しない。

## Preflight Checker

**不要**。 script resolver は「実際に install されているバイナリ」を直接呼ぶため、 lockfile と install 状態のズレが構造的に生じない。 旧 aqua / go-external 系で必要だった `aqua-checksums.json` 検証 / `go list -m` 検証は廃止。

利用者が「aqua.yaml と install 状態の整合性を CI で強制したい」場合は、 sloff の責務外として `aqua install --update-checksum` 等の事前 check を CI step で別途回す運用に分離する。

## SourceLister

外部配布物 ( バイナリ自身が `--version` を返せる pinned 配布物) を扱うため、 `SourceLister` は使わない ( ソース hash の対象外)。

内製ツール ( SemVer を持たないリポジトリ内ソース) は [resolver-go-local](./resolver-go-local.md) / [resolver-pnpm-local](./resolver-pnpm-local.md) で別経路。

## 適用例

### aqua 配布の OSS バイナリ

```yaml
tools:
  buf:
    exec: ["buf", "--version"]

commands:
  - name: protoc
    cmd: buf generate
    inputs: ["**/*.proto", "buf.gen.yaml"]
    outputs: ["**/*.pb.go"]
    tools: [buf]
```

aqua install 後の `buf` バイナリは `buf --version` で `1.30.0` を出力する ( v 接頭辞無しのバージョン)。 stdout 全体をそのまま採用。

### Go `go.mod` `tool` 経由のツール

```yaml
tools:
  mockgen:
    exec: ["go", "tool", "mockgen", "-version"]
    extract: 'v[0-9]+\.[0-9]+\.[0-9]+'

commands:
  - name: gen-mock
    cmd: ["go", "tool", "mockgen", "..."]
    inputs: ["**/*.go"]
    outputs: ["**/*_mock.go"]
    tools: [mockgen]
```

`go tool` 経由でも `--version` (or `-version`) は通る。 出力例 `mockgen v0.5.0` から regex で取り出す。

### Go の `go version` (build メタ含む)

```yaml
tools:
  go:
    exec: ["go", "version"]
    extract: 'go[0-9]+\.[0-9]+(?:\.[0-9]+)?'
```

`go version go1.26.2 darwin/arm64` から `go1.26.2` だけを切り出し、 OS-arch suffix は除外する。 `tools: [go]` で参照する task が、 Go toolchain bump で自動 invalidate する。

### `--version` を持たないツール

このケースは script resolver では扱えない。 利用者は次のいずれかを選ぶ:

- 該当 generator が repo 内ソースから build される場合は [go-local](./resolver-go-local.md) / [pnpm-local](./resolver-pnpm-local.md) ( workspace package なら) に振る ( 外部公開パッケージ専用 resolver は持たない、 [ADR-0007](../adr/0007-no-external-dependency-resolver.md))
- shim を書く ( `sloff.yml` の `tools: my-tool: { exec: ["bash", "-c", "cat .my-tool-version"] }` のような lockfile 風文字列を返すスクリプト)。 ただし shim ファイル自体の更新が反映されるかを利用者が責任を持つ

shim を許容するかは Open Question ( 後述)。

## Open Questions

- **`extract` regex の caputure group 位置**: group 1 を採用するか、 名前付き group ( `(?P<v>...)`) を強制するか。 初版は group 1 採用 / 無ければマッチ全体、 で良さそう
- **stderr 取得**: 一部ツール ( 古い Python など) は `--version` を stderr に出す。 spec で `capture: stderr` のような切替えを追加するか、 ユーザーが `bash -c "tool --version 2>&1"` でラップするか。 初版は後者で十分
- **メモ化のスコープ**: 現案は sloff 1 run 単位。 daemon 化した場合の TTL ( 5 分?) は後で検討
- **shim 経由でツール非サポート tool に対応する範囲**: 「lockfile 風文字列を出すスクリプトが書ける = 任意の channel に拡張できる」 が、 shim 自体の更新が反映されないリスクがある。 推奨パターンを doc で示すか、 むしろ非推奨にして専用 resolver を増やす方を選ぶか
