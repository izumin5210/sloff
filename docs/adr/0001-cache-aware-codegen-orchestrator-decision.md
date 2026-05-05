# ADR-0001: キャッシュ可能コード生成オーケストレーターの選定

## Context

### 背景

中〜大規模の polyglot monorepo では、 コード生成 ( proto / SQL モデル / mock / GraphQL / 内製 protoc plugin / pnpm 系コードジェネレータ など 数十のツール) にかかる時間が開発生産性のボトルネックになりやすい。 さらに 多くのチームで **開発者間 / CI 間でキャッシュを共有できない** 構造になっており、 `git pull` 直後 / ブランチ切替直後には毎回フル再生成が走る。

この状況を解消するには「キャッシュ可能で、 かつ結果を共有できるコード生成オーケストレーター」を導入する必要がある。 大きな選択は **既製品を採用するか / 自作するか** に集約される。 本 ADR ではこの分岐を確定する。

### 評価軸

#### 一般的な機能要件

- Go ツールチェーン (`go.mod` 1.24 `tool` ディレクティブ、 `go run`、 内製 Go CLI) と整合すること
- Node ツールチェーン ( pnpm workspace、 `pnpm exec`、 workspace 内自作ツール) と整合すること
- aqua のようなパッケージマネージャで配布される OSS バイナリと整合すること
- 既存 monorepo 構造 ( Go module、 pnpm workspace 等) を大きく変えないこと
- 全エンジニアに課す日常メンテコストが許容範囲に収まること

#### キャッシュ健全性の 3 防御線 ( 本 ADR の核)

共有モデルでキャッシュを運用するうえで、 「**cache が `skip` を返したとき、 その出力が本当に正しいことが構造で保証される**」必要がある。 さもなければ「cache を信じきれず手動で `--no-cache` を打つ」習慣が現場に残り、 共有 cache の存在意義そのものが失われる。 そのために以下 3 つを構造で防ぐ仕組みが必要:

| 防御線 | 防ぐ嘘のシナリオ |
|---|---|
| **(1) OS 中立な論理 version 解決** | Mac で生成した record を Linux CI で再利用したいが、 ツールバイナリが OS 別で hash が分裂、 record が共有不能になる |
| **(2) lockfile vs install 状態の preflight 検証** | `pnpm-lock.yaml` を更新したが `pnpm install` 未実行で生成すると、 lockfile の新 version で計算した hash と古いバイナリの実行結果が乖離した「嘘 record」が共有される |
| **(3) output-comparison ヒット判定** | 別開発者が手元で output を drift させた / formatter が走った / cache record だけ pull した状態で input_hash が一致 → cache hit → 古い output のまま skip され誤動作 |

中〜大規模 monorepo ( 数百 task / 数百エンジニア / 日に数百回のコード生成実行) の規模では、 偽 cache が共有された場合の影響範囲が大きく、 これら 3 防御線を設計レベルで強制する価値は高い。

### References

- [ADR-0002: キャッシュヒット判定モデル](./0002-cache-hit-decision-model.md)
- [ADR-0003: キャッシュレコードのストレージ方式](./0003-record-storage-strategy.md)
- [Design Doc: lazygen Architecture](../design/architecture.md)

## Considered Options

### Comparison Table

| | A: Turborepo | B: Bazel | C: moonrepo | D: Pants | **E: 自作 lazygen (採用)** |
|---|---|---|---|---|---|
| 一般機能の成熟度 | ◎ | ◎ | ○ | ○ | × ( 自作する) |
| Go ツールチェーン対応 | × ( shell 起動) | ◎ ( rules_go) | ○ ( v2.1 で `go list --deps`) | ○ ( `go_mod`) | ◎ |
| 依存自動導出 ( ファイル粒度) | × ( 手動 `dependsOn`) | △ ( `gazelle` でパッケージ粒度) | △ ( `inferInputs` opt-in) | ◎ ( import 静的解析) | ○ ( inputs/outputs glob + import 解析) |
| **(1) OS 中立論理 version** | × ( machine 別 hash) | × ( ツールバイナリが action input) | △ ( proto 管理ランタイムのみ) | × ( REAPI 由来で OS 別) | ◎ ( channel 別 resolver) |
| **(2) preflight ( lockfile vs install)** | × | (内製化で回避) | △ ( lockfile を hash 化のみ) | △ ( Python のみ厳密) | ◎ |
| **(3) output-comparison 判定** | × | × ( `#14543`: empty output でも cache) | × | × | ◎ |
| 複数パッケージマネージャ ( aqua / pnpm / go) 並走 SSoT 直読み | × | × ( Bazel 内製化前提) | × ( aqua 統合なし) | × ( aqua 統合なし) | ◎ |
| 既存 monorepo 構造への影響 | △ ( package.json 増設) | × ( BUILD.bazel 全展開) | △ ( workspace.yml 追加) | △ ( BUILD ファイル) | ◎ ( 変更なし) |
| 初期実装コスト | 低 | 中 | 低 | 低 | 高 |
| 継続メンテコスト | △ | × | △ | △ | ○ ( 内製) |

### Option A: Turborepo

JS/TS 中心の monorepo タスクオーケストレータ。 Vercel が開発、 Rust 製。

👍 **Pros**

- 初期実装コスト低
- web 側で既に導入している場合、 資産が活かせる
- remote cache が標準で用意され、 公式 / OSS 互換実装がある

👎 **Cons**

- Go ツールチェーン対応はネイティブにはなく、 `go build` を shell コマンドとして起動するのみ。 `go.mod` 1.24 `tool` ディレクティブや `go list -deps` の解析は提供しない
- task 間依存は `turbo.json` の `dependsOn` で **手動宣言**、 inputs はデフォルト「非 gitignored 全ファイル」と粗い
- (1) OS 中立 version: 公式 discussion `vercel/turborepo#9004` で「machine 間で hash がズレる」事例多数報告。 globalHash に lockfile を含むだけで OS バイナリ hash の問題は未解決
- (2) preflight: 仕組みなし
- (3) output-comparison: input-only 判定、 output は検証しない
- aqua / go.mod は SSoT として認識されない

3 防御線すべてを満たさない。 「JS 単言語で素早く incremental」という設計目標が本 ADR の問題設定と合わない。

### Option B: Bazel

Google が開発する大規模 monorepo 向け業界標準ビルダー。 `rules_go` + `gazelle` + Bzlmod で Go 対応も成熟。

👍 **Pros**

- hermetic build による厳密な再現性
- artifact 共有も remote cache (CAS + Action Cache) で完結
- 大規模 monorepo の実績が豊富
- `gazelle` が import 文を静的解析して BUILD.bazel を自動生成 ( パッケージ粒度の依存自動導出)

👎 **Cons**

- `BUILD.bazel` を全リポジトリに撒く initial cost と継続メンテ cost が極大。 既存パッケージマネージャ ( aqua / pnpm / go.mod) を維持しつつ `BUILD.bazel` 同期責務を全エンジニアに載せるのは現実的でない
- (1) OS 中立 version: 公式 discussion `bazelbuild/bazel#18378` が "universal toolchain" を未解決課題として議論中。 ツールバイナリが action input に入るため `darwin-arm64` ⟷ `linux-amd64` の record 共有は構造的に困難
- (2) preflight: Bazel が install を内製化することで構造的に回避するが、 「Bazel に乗せ替える」コストと引き換え。 system 依存 (`/usr/bin/cc` 等) 由来の cache poisoning は `bazelbuild/bazel#4276` で長年の既知問題
- (3) output-comparison: input-only。 `bazelbuild/bazel#14543` で「Action successful and cached without outputs」が 2021 年から未解決
- aqua / pnpm / go.mod を SSoT として読みに行く設計は不向き ( Bazel が toolchain を所有する思想)

機能面では強力だが、 既存パッケージマネージャ群の維持と組み合わせると 3 防御線も満たせず ROI が見合わない。

### Option C: moonrepo

Rust 製の polyglot タスクランナー。 `v1.38` (2025-06) で Go toolchain を公式追加、 `v2.1` (2026-03) で `go list --deps` を呼ぶ project graph 拡張が入った。

👍 **Pros**

- 4 ツール中 lazygen 設計に最も近い思想 ( polyglot 一級対応、 proto による論理 version 管理)
- proto 経由でランタイムを管理しているため、 proto 管理下のツールに限り OS 中立 hash が成立 ( **(1) を部分的に満たす**)
- pnpm / npm / yarn / cargo / go.mod ネイティブ統合
- workspace.yml だけ追加すればよく、 BUILD.bazel ほど侵襲的ではない
- 既製品の成熟度 ( タスクランナー / remote cache / 並列実行)

👎 **Cons**

- (1) OS 中立 version: proto 管理外のツール ( aqua の xo / sqlc / tbls など) は対象外、 別途 lockfile / バイナリ hash で OS 別分裂
- (2) preflight: lockfile から resolved version を hash に組み込むだけで、 「lockfile 更新したが install してない」状態は検出しない
- (3) output-comparison: input-only + outputs を tarball 化して archive する hydration モデル。 output drift は検出しない
- aqua との直接統合は無し ( aqua install したバイナリを `~/.local/bin` 経由で叩く形になり、 論理 version は別途 spec で明示する必要)
- task 間依存は基本 `dependsOn` で手動宣言、 ファイル粒度の import 解析はない

3 防御線のうち (1) を部分的に満たすが (2) (3) は欠ける。 「moonrepo + 妥協」で実用上は回せる選択肢として真剣な対抗馬になる。

### Option D: Pants

Pantsbuild 製の polyglot ビルダー (2.31 が 2026-02 リリース)。 「**dependency inference is special sauce**」を明確に掲げる。

👍 **Pros**

- ファイル粒度の import 静的解析で BUILD ファイルの `dependencies` を完全自動推論 ( **依存自動導出は業界最強**)
- `./pants tailor` で BUILD 雛形も自動生成
- Python / Go / JS / JVM / Shell など複数言語の lockfile を SSoT として読みにいく設計
- REAPI 準拠の remote cache

👎 **Cons**

- BUILD ファイルは必要 ( 自動生成されるとはいえ管理対象は増える)
- (1) OS 中立 version: REAPI 由来でツールバイナリが action input → OS / CPU 別 hash 分岐 ( Bazel と同様の問題)
- (2) preflight: Python の lockfile (`*.lock`) では実行前拒否の仕組みがあるが、 Go では `go.sum` を読むだけで install 状態は検証しない
- (3) output-comparison: input-only ( REAPI モデル)
- aqua 統合は無し、 pnpm 対応は薄い

依存自動導出は lazygen の参考にすべき優れた設計だが、 3 防御線のうち (1) (3) を欠き、 複数パッケージマネージャ環境にも適合しない。

### Option E: 自作 lazygen ( 採用)

monorepo の実情に合わせた専用オーケストレーターを実装する。

👍 **Pros**

- **3 防御線すべてを設計レベルで強制できる**:
  - (1) channel 別 resolver で aqua / pnpm / go.mod 横断の OS 中立論理 version
  - (2) lockfile vs install 状態の preflight 検証を強制実行
  - (3) output-comparison 二段判定で drift を fail-fast に検出
- aqua / pnpm / go.mod の複数パッケージマネージャ SSoT を直接読みに行く resolver を組める
- 既存 monorepo 構造 ( Go module、 pnpm workspace) を変更不要
- 内製ゆえ全エンジニアに新しい既製ツールの学習コストを課さない
- Pants 流の import 解析 ( ファイル粒度の依存自動導出) を取り込める ( Design Doc 参照)

👎 **Cons**

- 初期実装コストが大きい
- 共有 cache ( record schema、 ストレージ、 invalidate 戦略、 GC) を自作する必要がある
- メンテ責務を内製で持ち続ける必要がある
- 既製品の plugin エコシステム / コミュニティサポートを享受できない

## Decision

**Option E: 自作 ( lazygen) を採用する。**

採用根拠:

1. **既製品 4 ツールのいずれも「キャッシュ健全性の 3 防御線」を満たさない**
   - (1) OS 中立論理 version: moonrepo が proto 管理ランタイムのみ部分対応、 他は OS 別バイナリ hash で分裂。 aqua との直接統合はどのツールにも無し
   - (2) preflight: 仕組みを持つツールは無い ( Bazel は install 内製化で回避するのみ)
   - (3) output-comparison: 全ツール input-only、 Bazel `#14543` のように「empty output でも cache」が業界共通の既知問題
2. **中〜大規模 monorepo の規模では、 偽 cache が共有された場合の影響範囲が大きい**。 「cache を信じきれず `--no-cache` を打つ習慣」が現場に残ると共有 cache の存在意義そのものが失われる。 3 防御線を構造で強制する価値は高い
3. **既製品を採用すると 3 防御線を欠いたまま運用することになる**。 偶発的に cache が嘘をついたとき「うちの cache は信じきれない」という不信感が継続する。 これは技術的負債というより組織的負債で、 後から取り戻すのが難しい
4. **依存自動導出 / Go ツールチェーン対応 / lockfile hash 化など、 既製品に追いついている既存技術は積極的に取り込む**。 Pants の import 解析手法は lazygen の依存自動導出 / pnpm workspace 内 内製ツール hash に取り込む ([Design Doc 参照](../design/architecture.md))

### 反論への応答 ( moonrepo 採用案について)

moonrepo は 4 ツール中最も lazygen 設計に近く、 「moonrepo + 妥協」での運用は技術的には可能。 ただし以下のトレードオフがある:

- (1) は proto 管理ランタイム以外で OS 別 hash 分裂が残る
- (2) (3) は欠けたまま
- aqua との連携は spec 側で論理 version を明示する形になり、 lazygen の自動 resolver と比べて運用負荷が増す

「3 防御線への投資は過剰、 偶発的事故は手動で回す」と判断するなら moonrepo は現実解。 中〜大規模 monorepo で「事故が起きたとき発覚するまでの調査コスト」「不信感の組織的影響」を考慮すると、 自作のコストが上回ると判断した。

## Consequences

### 正の影響

- Go / Node / aqua の複数パッケージマネージャ環境を変更せずに共有キャッシュを導入できる
- キャッシュ健全性の 3 防御線が設計レベルで強制され、 「cache は信じきれる」運用文化を構築できる
- 既製品の学習コストを全エンジニアに広く課さずに済む
- Pants の import 解析、 moonrepo の resolved version 思想など、 既製品の優れた設計は積極的に取り込める

### 負の影響

- 共有キャッシュの実装とメンテ責務が内製で残る
- 既製品の remote cache インフラを使えないため、 ストレージ方式は別途設計する必要がある
- 自作する以上、 詳細設計を別途確定する必要がある:
  - キャッシュヒット判定モデル → [ADR-0002](./0002-cache-hit-decision-model.md)
  - record のストレージ方式 → [ADR-0003](./0003-record-storage-strategy.md)
  - 具体実装 ( spec 文法、 record schema、 OS 横断 invalidate、 preflight、 import 解析、 タスク間依存自動導出、 GC) → [Design Doc](../design/architecture.md)
