# ADR-0001: fingerprint ベースのコード生成オーケストレーターの選定

## Context

### 背景

中〜大規模の polyglot monorepo では、 コード生成 ( proto / SQL モデル / mock / GraphQL / 内製 protoc plugin / pnpm 系コードジェネレータ など 数十のツール) にかかる時間が開発生産性のボトルネックになりやすい。 さらに 多くのチームで **開発者間 / CI 間で fingerprint を共有できない** 構造になっており、 `git pull` 直後 / ブランチ切替直後には毎回フル再生成が走る。

この状況を解消するには「fingerprint ベースで、 かつ結果を共有できるコード生成オーケストレーター」を導入する必要がある。 大きな選択は **既製品を採用するか / 自作するか** に集約される。 本 ADR ではこの分岐を確定する。

### 評価軸

#### 一般的な機能要件

- Go ツールチェーン (`go.mod` 1.24 `tool` ディレクティブ、 `go run`、 内製 Go CLI) と整合すること
- Node ツールチェーン ( pnpm workspace、 `pnpm exec`、 workspace 内自作ツール) と整合すること
- 外部配布の prebuilt binary ( nix / mise / aqua 等の version manager で配布される CLI、 `pnpm exec` 経由の npm bin、 `go tool` 経由の Go ツール 等) と整合すること
- 既存 monorepo 構造 ( Go module、 pnpm workspace 等) を大きく変えないこと
- 全エンジニアに課す日常メンテコストが許容範囲に収まること

#### fingerprint の健全性の 2 防御線 ( 本 ADR の核)

共有モデルで fingerprint を運用するうえで、 「**fingerprint が `skip` を返したとき、 その出力が本当に正しいことが構造で保証される**」必要がある。 さもなければ「fingerprint を信じきれず手動で `--no-fingerprint` を打つ」習慣が現場に残り、 共有 fingerprint store の存在意義そのものが失われる。 そのために以下 2 つを構造で防ぐ仕組みが必要:

| 防御線 | 防ぐ嘘のシナリオ | 防御線が入ることで得られる嬉しさ |
|---|---|---|
| **(1) OS 中立な論理 version の取得元が runtime と必ず整合する** | (a) Mac で生成した record を Linux CI で再利用したいが、 ツールバイナリが OS 別で hash が分裂し、 record が共有不能になる。 (b) lockfile を更新したが install を忘れた状態で生成すると、 lockfile 上の新 version で計算した hash と古いバイナリの実行結果が乖離した「嘘 record」が共有される | (a) cross-OS で record を物理的に共有できる ( これが満たせないと共有 fingerprint store そのものが成立しない)。 (b) install 忘れ起因の嘘 record が record store に混入しない |
| **(2) output-comparison ヒット判定** | (a) fingerprint だけ pull したが、 別開発者が手元で formatter で output を書き換えた / rebase で output が drift / 部分 checkout で output が欠損 → input_hash 一致で skip → 古い / 壊れた output で動く。 (b) 過去 non-deterministic に振る舞った generator の record が残り、 input_hash 一致で誤 hit する。 (c) Bazel `bazelbuild/bazel#14543` 型「 empty output でも success として cache」 | input が一致していても output 実体の drift を検出して fail-fast / 再生成。 「 fingerprint が嘘をついていない」 を構造で保証する。 これがないと「 fingerprint を信じきれず手動で `--no-fingerprint` を打つ」 運用文化が共有 fingerprint store を死語にする |

防御線 (1) は channel 別に取得方法を変えて達成する:

- **外部配布の prebuilt binary** ( nix / mise / aqua 等の version manager で配布されるもの、 `pnpm exec` 経由の npm bin、 `go tool` 経由の Go ツール 等): 「実 install されているバイナリの `--version` 出力」を直接捕捉する。 lockfile vs install のズレが構造的に存在しないため preflight 不要 ( script resolver)
- **workspace 内 npm package**: lockfile を SSoT とし、 lockfile vs `node_modules` の整合は preflight で検証 ( pnpm-local resolver + checker)
- **内製ソース**: ソース hash を直接取るので runtime とのズレは起こらない ( go-local / pnpm-local)

中〜大規模 monorepo ( 数百 task / 数百エンジニア / 日に数百回のコード生成実行) の規模では、 偽 fingerprint が共有された場合の影響範囲が大きく、 これら 2 防御線を設計レベルで強制する価値は高い。

> **Updated 2026-05-05**: 旧版では (1) OS 中立 logical version、 (2) lockfile vs install preflight、 (3) output-comparison の 3 防御線 と整理していた。 設計を進める中で、 prebuilt binary については「実 install バイナリの `--version` 出力」 を直接 hash 入力にする方式 ( script resolver) で (1) と旧 (2) が同時自動成立することが分かったため、 防御線を 2 つに統合し、 preflight は lockfile-based channel 固有の実装詳細に格下げした。

> **Updated 2026-05-08**: 比較対象に Nx を追加し、 各既製品の Pros を明記してフェアな評価に整理し直した。 また「 aqua」 等の固有ツール名は「 外部配布の prebuilt binary 」 という抽象に統一し、 sloff の Cons に scope ( codegen 専用 / artifact 非対応 / remote cache 初版未実装 / 実績ゼロ 等) を明示した。 Buck2 は Bazel と評価軸がほぼ同じため独立 Option としては立てず、 Bazel 節内で簡潔に言及する扱いに留めた。

### References

- [ADR-0002: fingerprint hit 判定モデル](./0002-fingerprint-hit-decision-model.md)
- [ADR-0003: fingerprint のストレージ方式](./0003-fingerprint-storage-strategy.md)
- [Design Doc: sloff Architecture](../design/architecture.md)

## Considered Options

### Comparison Table

| | A: Turborepo | B: Nx | C: Bazel ( Buck2 含む) | D: moonrepo | E: Pants | **F: 自作 sloff (採用)** |
|---|---|---|---|---|---|---|
| 一般機能の成熟度 | ◎ | ◎ | ◎ | ○ | ○ | × ( 自作する) |
| Go ツールチェーン対応 | × ( shell 起動) | △ ( community plugin `@nx-go/nx-go`) | ◎ ( rules_go) | ○ ( v2.1+ で `go list --deps`) | ○ ( `go_mod`) | ◎ |
| JS/TS ツールチェーン対応 | ◎ | ◎ | ○ ( rules_js) | ◎ | △ | ○ ( pnpm workspace) |
| 依存自動導出 ( Go) | × | △ ( plugin 依存) | △ ( `gazelle` パッケージ粒度) | ○ ( `go list --deps` / パッケージ粒度) | ◎ ( import 静的解析 / ファイル粒度) | ○ ( task 粒度: glob 交差 / 内製 CLI 内部: `go/packages`) |
| 依存自動導出 ( JS/TS) | × ( 手動 `dependsOn`) | ◎ ( import 解析 / ファイル粒度) | △ ( 手動 srcs) | △ ( 手動 `dependsOn` + workspace dep) | ○ ( import 解析) | ○ ( task 粒度: glob 交差 / 内製ツール内部: pnpm workspace の git-tracked enumeration) |
| **(1) OS 中立 logical version が runtime と整合** | × ( machine 別 hash) | × | × ( ツールバイナリが action input / REAPI 由来で OS 別) | △ ( proto 管理ランタイムのみ / install 検証なし) | × ( REAPI 由来で OS 別) | ◎ ( prebuilt = `--version` 直取り、 lockfile-based = lockfile + preflight、 内製 = ソース hash) |
| **(2) output-comparison 判定** | × | × | × ( `bazelbuild/bazel#14543` 未解決) | × | × | ◎ |
| 外部配布 prebuilt binary 群との SSoT 直読み | × | × | × ( Bazel が toolchain を所有) | × ( proto 管理外は対象外) | × | ◎ |
| 既存 monorepo 構造への影響 | △ ( `turbo.json` + `dependsOn` 整備) | △ ( workspace への寄せ替え推奨) | × ( `BUILD.bazel` / `BUCK` 全展開) | △ ( `workspace.yml` + `moon.yml`) | × ( `BUILD` ファイル) | ○ ( spec dir ごとに `sloff.yml` 必要だが既存設定は変更不要) |
| Remote cache ( 既製) | ◎ | ◎ ( Nx Cloud) | ◎ ( REAPI) | ◎ ( moonbase) | ◎ ( REAPI) | × ( 初版未実装) |
| Artifact cache | ◎ | ◎ | ◎ | ◎ | ◎ | × ( Non-Goal) |
| 初期実装コスト | 低 | 低 | 中〜高 | 低 | 中 | 高 |
| 継続メンテコスト | △ | △ | × | △ | △ | ○ ( 内製 / スコープを狭く絞ったぶん小さい) |

### Option A: Turborepo

JS/TS 中心の monorepo タスクオーケストレータ。 Vercel が開発、 Rust 製。

👍 **Pros**

- 初期実装コスト極小。 学習コストも小さく、 数日で workspace 全体を載せ替え可能
- web 側で既に導入している場合、 資産をそのまま活かせる
- remote cache ( Vercel 公式 / OSS 互換実装) が標準で用意され、 配線が簡単
- Vercel / Next.js / Remix 等のフレームワーク連携が手厚く、 周辺エコシステムが大きい

👎 **Cons**

- Go ツールチェーン対応はネイティブにはなく、 `go build` を shell コマンドとして起動するのみ。 `go.mod` 1.24 `tool` ディレクティブや `go list -deps` の解析は提供しない
- task 間依存は `turbo.json` の `dependsOn` で **手動宣言**、 inputs はデフォルト「非 gitignored 全ファイル」と粗い
- (1) OS 中立 version: 公式 discussion `vercel/turborepo#9004` で「machine 間で hash がズレる」事例多数報告。 globalHash に lockfile を含むだけで OS バイナリ hash の問題は未解決
- (2) output-comparison: input-only 判定、 output は検証しない
- 外部配布の prebuilt binary ( nix / mise / aqua / `go tool` 等) は SSoT として認識されない

2 防御線すべてを満たさない。 「JS 単言語で素早く incremental」という設計目標が本 ADR の問題設定 ( polyglot codegen / 共有 fingerprint store の健全性) と合わない。

### Option B: Nx

Nrwl ( 現 Nx) が開発する polyglot monorepo プラットフォーム。 Turborepo より plugin エコシステムが厚く、 開発プラットフォームとしての周辺機能 ( generators / migration / graph 可視化) が豊富。

👍 **Pros**

- ファイル粒度の import 解析でプロジェクトグラフを構築 ( JS/TS は標準で ◎)、 `nx affected` で差分実行
- plugin エコシステムが厚く ( 公式 + community、 Go / Rust / Python / .NET 等)、 generators ( scaffolding) や migration ツールが充実
- `nx graph` の可視化 UX が現代的に最も洗練されている
- Nx Cloud で remote cache + 分散実行 ( DTE) が標準提供
- `nx init` で既存 repo に段階的導入可

👎 **Cons**

- Go ツールチェーン対応は community plugin 経由で一級ではない。 `go.mod` 1.24 `tool` ディレクティブの認識や `go list -deps` の解析は plugin 実装に依存
- (1) OS 中立 version: fingerprint key 構成は plugin 依存で、 host 由来の入力 ( 環境変数 / 絶対パス / OS 別バイナリ) が混入しやすい。 構造的な保証はない
- (2) output-comparison: input-only
- 外部配布の prebuilt binary との SSoT 直読みは標準では持たない
- Nx workspace 構造への寄せ替え ( `nx.json` / `project.json` / `apps` `libs` 配置) を効果最大化のために行うと侵襲的になりがち。 段階導入は可能だが、 効果を最大化するには寄せ替えが必要

JS/TS 中心 monorepo の現代的標準として Turborepo の対抗馬。 polyglot codegen の fingerprint の健全性は本ツールの主問題設定ではない。

### Option C: Bazel

Google が開発する大規模 monorepo 向け業界標準ビルダー。 `rules_go` + `gazelle` + Bzlmod で Go 対応も成熟。

👍 **Pros**

- hermetic build による厳密な再現性
- artifact 共有も remote cache ( CAS + Action Cache) で完結。 コンパイル結果まで含めた fingerprint 共有はどのツールよりも強い
- 大規模 monorepo の実績が豊富 ( Google / 多数のテック企業)
- `gazelle` が import 文を静的解析して `BUILD.bazel` を自動生成 ( パッケージ粒度の依存自動導出)
- 分散実行 ( Remote Build Execution) で巨大 build を線形にスケール
- 商用サポート / 周辺ツール ( BuildBuddy / EngFlow 等) が充実

👎 **Cons**

- `BUILD.bazel` を全リポジトリに撒く initial cost と継続メンテ cost が極大。 既存パッケージマネージャ ( aqua / pnpm / go.mod 等) を維持しつつ `BUILD.bazel` 同期責務を全エンジニアに載せるのは現実的でない
- (1) OS 中立 version: 公式 discussion `bazelbuild/bazel#18378` が "universal toolchain" を未解決課題として議論中。 ツールバイナリが action input に入るため `darwin-arm64` ⟷ `linux-amd64` の record 共有は構造的に困難。 system 依存 (`/usr/bin/cc` 等) 由来の cache poisoning は `bazelbuild/bazel#4276` で長年の既知問題
- (2) output-comparison: input-only。 `bazelbuild/bazel#14543` で「Action successful and cached without outputs」が 2021 年から未解決
- 外部配布の prebuilt binary を SSoT として読みに行く設計は不向き ( Bazel が toolchain を所有する思想と衝突)

機能面ではどのツールよりも強力で、 「 artifact / コンパイル結果まで fingerprint 共有したい」 「 巨大 monorepo を分散実行で回したい」 ニーズには第一選択。 ただし本 ADR の問題設定 ( 既存パッケージマネージャ群を温存しつつ codegen の fingerprint の健全性を上げる) では、 移行コストと 2 防御線を欠く構造的な問題で ROI が見合わない。

> Meta の **Buck2** ( Bazel 系の Rust 実装、 dynamic deps / incremental 性能改善 / REAPI 互換) も hermetic build 系の現代的選択肢として存在するが、 本 ADR の評価軸 ( 2 防御線 / 既存 package manager 温存 / 外部配布 prebuilt binary との連携) では Bazel とほぼ同じ評価になるため、 独立 Option としては立てず本節に内包する。 新規導入で hermetic を選ぶ場合は Bazel と Buck2 を別途比較する価値があるが、 sloff との対比文脈では区別不要と判断した。

### Option D: moonrepo

Rust 製の polyglot タスクランナー。 `v1.38` (2025-06) で Go toolchain を公式追加、 `v2.1` (2026-03) で `go list --deps` を呼ぶ project graph 拡張が入った。

👍 **Pros**

- 既製品の中で sloff の問題設定に最も近い ( polyglot 一級対応、 proto による論理 version 管理)
- proto 経由でランタイムを管理しているため、 proto 管理下のツールに限り OS 中立 hash が成立 ( **(1) を部分的に満たす**)
- pnpm / npm / yarn / cargo / go.mod ネイティブ統合
- `workspace.yml` + `moon.yml` だけ追加すればよく、 `BUILD.bazel` ほど侵襲的ではない
- 既製品の成熟度 ( タスクランナー / remote cache "moonbase" / 並列実行 / CI 連携 / dashboard 等)

👎 **Cons**

- (1) OS 中立 version: proto 管理外のツール ( nix / aqua 等で配布される prebuilt binary 全般) は対象外、 別途 lockfile / バイナリ hash で OS 別分裂
- install drift 検出は持たない: lockfile から resolved version を hash に組み込むだけで、 「lockfile 更新したが install してない」状態は検出しない
- (2) output-comparison: input-only + outputs を tarball 化して archive する hydration モデル。 output drift は検出しない
- 外部配布の prebuilt binary ( proto 管理外) との直接統合は無し ( install したバイナリを `~/.local/bin` 経由で叩く形になり、 論理 version は別途 spec で明示する必要)
- task 間依存は基本 `dependsOn` で手動宣言、 ファイル粒度の import 解析はない

2 防御線のうち (1) を部分的に満たす ( proto 管理ランタイム以外 / install 検証なし) が (2) output-comparison は欠ける。 「moonrepo + 妥協」で実用上は回せる選択肢として真剣な対抗馬になる。

### Option E: Pants

Pantsbuild 製の polyglot ビルダー (2.31 が 2026-02 リリース)。 「**dependency inference is special sauce**」を明確に掲げる。

👍 **Pros**

- ファイル粒度の import 静的解析で `BUILD` ファイルの `dependencies` を完全自動推論 ( **依存自動導出は業界最強**)
- `./pants tailor` で `BUILD` 雛形も自動生成 ( メンテ負担は Bazel より軽い)
- Python / Go / JS / JVM / Shell など複数言語の lockfile を SSoT として読みにいく設計の徹底度は高い
- REAPI 準拠の remote cache
- Python monorepo での実績が厚い

👎 **Cons**

- `BUILD` ファイルは必要 ( 自動生成されるとはいえ管理対象は増える)
- (1) OS 中立 version: REAPI 由来でツールバイナリが action input → OS / CPU 別 hash 分岐 ( Bazel と同様の問題)
- install drift 検出: Python の lockfile (`*.lock`) では実行前拒否の仕組みがあるが、 Go では `go.sum` を読むだけで install 状態は検証しない
- (2) output-comparison: input-only ( REAPI モデル)
- 外部配布の prebuilt binary ( nix / mise / aqua 等) との直接統合は無し、 pnpm 対応は薄い

依存自動導出は sloff の参考にすべき優れた設計だが、 2 防御線のうち (1) ( OS 別 hash) と (2) output-comparison を欠き、 外部配布 prebuilt binary 群との直接統合も持たないため、 本 ADR の問題設定では ROI が見合わない。

### Option F: 自作 sloff ( 採用)

monorepo の実情に合わせた専用オーケストレーターを実装する。

👍 **Pros**

- **2 防御線すべてを設計レベルで強制できる**:
  - (1) channel 別の取得経路で OS 中立な logical version を必ず runtime と整合させる ( prebuilt = `--version` 直取り、 workspace 内 npm = lockfile + preflight、 内製 = ソース hash)
  - (2) output-comparison 二段判定で drift を fail-fast に検出
- 外部配布の prebuilt binary 群 ( nix / mise / aqua 等で配布されるもの、 `pnpm exec` / `go tool` 経由のもの 等) を均等に SSoT として読みにいく resolver を組める
- 既存 monorepo 構造 ( Go module、 pnpm workspace、 lockfile 群) を変更不要
- 内製ゆえ全エンジニアに新しい既製ツールの学習コストを課さない
- Pants 流の import 解析 ( ファイル粒度の依存導出) を内製ソース hash に取り込める ( Design Doc 参照)

👎 **Cons**

- 初期実装コストが大きい
- 共有 fingerprint store ( record schema、 ストレージ、 invalidate 戦略、 GC) を自作する必要がある
- メンテ責務を内製で持ち続ける必要がある
- 既製品の plugin エコシステム / コミュニティサポート ( Nx Cloud / moonbase / Bazel rules / Pants plugin / Turborepo の Vercel 連携 等) を享受できない
- **スコープが codegen 専用** で一般のタスクランナーではない。 build / test / lint / dev server をまとめて orchestrate したいニーズには別ツール ( moonrepo / Nx / Turborepo / Make 等) を併用する想定
- **artifact cache 非対応** ( Non-Goal)。 generator output が git 管理されている前提。 大きい binary / 画像 / 動画を生成する monorepo には不向き
- **remote cache 初版未実装** ( interface は切るが LocalStorage のみ)。 record の git 管理で「実質的な remote cache」になる設計だが、 git に乗らない大きい record や organization 横断の fingerprint 共有は対象外
- 設定ファイル数は Bazel / Pants ほどではないが少なくない: spec dir ごとに `sloff.yml` が必要 ( 数百タスクなら数十ファイル)
- watch モード / Windows 非対応 ( Non-Goal)
- 実績ゼロ。 既製品のような巨大 monorepo での battle-tested さは持たない
- 依存自動導出の精度は Pants ほど厳密ではない ( task 間は inputs / outputs glob の交差で導出するため、 glob を雑に書くと精度が落ちる)

## Decision

**Option F: 自作 ( sloff) を採用する。**

採用根拠:

1. **既製品 5 ツール ( Bazel に Buck2 を含めて 6 製品) のいずれも「fingerprint の健全性の 2 防御線」を満たさない**
   - (1) OS 中立な logical version が runtime と整合: moonrepo が proto 管理ランタイムのみ部分対応 ( かつ install 検証なし)、 他は OS 別バイナリ hash で分裂。 外部配布の prebuilt binary 群との直接統合はどのツールにも無し。 lockfile と install 状態の整合検証 ( workspace 内 npm package で必要) も既製品では仕組みが無い
   - (2) output-comparison: 全ツール input-only、 Bazel `bazelbuild/bazel#14543` のように「empty output でも cache」が業界共通の既知問題
2. **中〜大規模 monorepo の規模では、 偽 fingerprint が共有された場合の影響範囲が大きい**。 「fingerprint を信じきれず `--no-fingerprint` を打つ習慣」が現場に残ると共有 fingerprint store の存在意義そのものが失われる。 2 防御線を構造で強制する価値は高い
3. **既製品を採用すると 2 防御線を欠いたまま運用することになる**。 偶発的に fingerprint が嘘をついたとき「うちの fingerprint は信じきれない」という不信感が継続する。 これは技術的負債というより組織的負債で、 後から取り戻すのが難しい
4. **依存自動導出 / Go ツールチェーン対応 / lockfile hash 化など、 既製品に追いついている既存技術は積極的に取り込む**。 Pants の import 解析手法は sloff の依存自動導出 / pnpm workspace 内 内製ツール hash に取り込む ([Design Doc 参照](../design/architecture.md))

ただし、 採用には以下のスコープ制約が伴う ( Cons の再強調):

- sloff は **codegen 専用** ( 一般タスクランナーではない)、 **artifact cache 非対応**、 **remote cache 初版未実装**、 **実績ゼロ**
- 上記が許容できない問題設定 ( artifact / コンパイル結果まで fingerprint 共有したい / build / test まで全部一括で orchestrate したい / 巨大 binary を生成する monorepo / battle-tested さを最優先したい) では、 既製品 ( Bazel / Buck2 / Pants / moonrepo / Nx) のいずれかが現実解になる

### 反論への応答

#### moonrepo 採用案について

moonrepo は既製品中、 最も sloff 設計に近く、 「moonrepo + 妥協」での運用は技術的には可能。 ただし以下のトレードオフがある:

- (1) は proto 管理ランタイム以外で OS 別 hash 分裂が残り、 install 検証も無いため「lockfile 更新したが install してない」の嘘 record が混入しうる
- (2) output-comparison は欠けたまま
- 外部配布の prebuilt binary 群との連携は spec 側で論理 version を明示する形になり、 sloff の script resolver と比べて記述粒度の自由度が低い

「2 防御線への投資は過剰、 偶発的事故は手動で回す」と判断するなら moonrepo は現実解。 中〜大規模 monorepo で「事故が起きたとき発覚するまでの調査コスト」「不信感の組織的影響」を考慮すると、 自作のコストが上回ると判断した。

#### Nx 採用案について

JS/TS 比率が高い monorepo なら Nx は強力な対抗馬。 ただし sloff の問題設定 ( polyglot codegen の fingerprint の健全性) では:

- (1) fingerprint key が plugin 依存で host 由来入力が混入しやすく、 cross-OS record 共有を構造で保証できない
- (2) output-comparison は持たない
- Go の一級対応は community plugin 依存
- Nx workspace 構造への寄せ替えを効果最大化のために行うと侵襲的になる

JS/TS 中心ならフル機能を享受できる ( computation cache + Nx Cloud + nx affected + plugin エコシステム) が、 polyglot codegen の fingerprint の健全性は別問題として残る。

## Consequences

### 正の影響

- Go / Node / 外部配布 prebuilt binary 群を含む複数のパッケージマネージャ環境を変更せずに共有 fingerprint storeを導入できる
- fingerprint の健全性の 2 防御線が設計レベルで強制され、 「fingerprint は信じきれる」運用文化を構築できる
- 既製品の学習コストを全エンジニアに広く課さずに済む
- Pants の import 解析、 moonrepo の resolved version 思想、 Nx のプロジェクトグラフ可視化など、 既製品の優れた設計は積極的に取り込める

### 負の影響

- 共有 fingerprint storeの実装とメンテ責務が内製で残る
- 既製品の remote cache インフラ ( Nx Cloud / moonbase / REAPI 互換) を使えないため、 ストレージ方式は別途設計する必要がある
- スコープ ( codegen 専用 / artifact 非対応 / remote cache 初版未実装) を超える要求が出た場合は別ツール併用が必要
- 自作する以上、 詳細設計を別途確定する必要がある:
  - fingerprint hit判定モデル → [ADR-0002](./0002-fingerprint-hit-decision-model.md)
  - record のストレージ方式 → [ADR-0003](./0003-fingerprint-storage-strategy.md)
  - 具体実装 ( spec 文法、 record schema、 OS 横断 invalidate、 preflight、 import 解析、 タスク間依存自動導出、 GC) → [Design Doc](../design/architecture.md)
