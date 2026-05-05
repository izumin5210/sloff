# ADR-0003: キャッシュレコードのストレージ方式

## Context

### 背景

[ADR-0001](./0001-cache-aware-codegen-orchestrator-decision.md) で「自作」、 [ADR-0002](./0002-cache-hit-decision-model.md) で「output-comparison」が確定した。 自作する以上、 cache record (= input_hash → output_hash + output ファイル一覧の mapping) を **どこに置くか** を決める必要がある。

選択肢は git 内に置くか、 リポジトリ外 (S3 等) に置くかの大きく 2 軸、 さらに artifact 本体を含めるか含めないかで分岐する。 本 ADR ではこの選定を確定する。

### 制約 / 評価軸

- 開発者間 / CI 間で同じ record を読み書きできる ( R1: 共有可能)
- 別タスクを別ブランチで触っても record が衝突しない ( R5: コンフリクト無し)
- 累積する古い record を CI / ローカルから掃除できる ( R6: GC 可能)
- ネットワーク不要で動かせるか (オフライン時 / 新規 join 時)
- 外部 infra (bucket、 認証、 アクセス制御) の運用責務を増やさない
- 既存の monorepo 運用 (PR 中心のレビューフロー、 git pull で完結する習慣) と整合
- 一般的な monorepo 規模 ( タスク数 数百程度) で容量が現実的に収まる

### References

- [ADR-0001: キャッシュ可能コード生成オーケストレーターの選定](./0001-cache-aware-codegen-orchestrator-decision.md)
- [ADR-0002: キャッシュヒット判定モデル](./0002-cache-hit-decision-model.md)
- [Design Doc: lazygen Architecture](../design/architecture.md)

## Considered Options

### Comparison Table

| | **A: git per-task per-input ファイル (採用)** | B: 単一 hash ファイル | C: S3 等リモートストレージ | D: hash をファイル名に artifact 直コミット | E: Hybrid (git に index + S3 に artifact) |
|---|---|---|---|---|---|
| 配置 | git 内に分割ファイル | git 内に 1 ファイル | リポジトリ外 (S3 等) | git 内に artifact 本体含めて分割 | git 内に index、 artifact は S3 |
| 共有可能 (R1) | ◎ | ◎ | ◎ | ◎ | ◎ |
| コンフリクト無し (R5) | ◎ | × | ◎ | ◎ | ◎ |
| GC のしやすさ (R6) | ○ (CI sweep / コマンド) | ◎ (1 ファイル) | ◎ (TTL 自動) | × (容量肥大) | ○ + S3 TTL |
| ネットワーク不要 | ◎ | ◎ | × | ◎ | △ |
| 外部 infra 運用責務 | ◎ なし | ◎ なし | △ (bucket 運用) | ◎ なし | △ |
| 一般的 monorepo 規模での容量 | ◎ (数 MB) | ◎ (数 KB) | ◎ | × (数百 MB〜) | ◎ |

### Option A: git に per-task per-input ファイル (採用)

`.lazygen/cache/<spec_relpath>/<task_id>/<input_hash>.yml` の 1 ファイル / 1 record で git 管理する。 record は input_hash → output_hash + output ファイル一覧の mapping のみ ( artifact は含まない)。

👍 **Pros**

- 物理的に分割されているため構造的にコンフリクトしない (R5)
- git の同一コミットで完結するため認証 / 外部 infra / ネットワーク不要 (R1 を最も素直に満たす)
- artifact を含まないため record サイズが小さい (1 record 数 KB レンジ、 全体で数 MB 試算)
- `git pull` すれば自動的に最新の cache 状態が手元に揃うシンプルなメンタルモデル

👎 **Cons**

- record が累積する。 GC 機構が別途必要 (R6)
- record 自体は YAML の hash 値とパス羅列で **人間がレビューすべきフォーマットではない** ため、 PR diff にとってはノイズになる側面がある (PR template や `.gitattributes` の `linguist-generated` で diff 抑制する等の緩和は可能)
- record ファイル数の増加に伴う `git status` / `git add` のスループットは将来的に再評価の余地

### Option B: 単一 hash ファイル

単一 YAML / JSON ファイル ( 例: `.lazygen.hash.yml`) に全 task の hash を配列で記録する。

👍 **Pros**

- 実装が単純
- ファイル数が増えない、 GC が事実上不要

👎 **Cons**

- 構造的コンフリクト (R5) は本質的に解消できない。 ブランチ間で頻繁に衝突し、 マージ作業の運用負荷が常に発生する
- 共有モデルで採用すると複数開発者の更新が頻繁に競合する

### Option C: S3 等リモートストレージ

record を S3 / R2 等に put / get する。 turbo Remote Cache や bazel BuildBuddy と同じ思想を、 自作実装の中で同等の機構として構築する。

👍 **Pros**

- ゴミは TTL 自動削除でき、 リポジトリ肥大化なし
- 巨大化しても容量問題に当たらない
- record がリポジトリ外にあるため PR diff には現れない (PR ノイズの観点では git 方式より clean)

👎 **Cons**

- ネットワーク必須。 オフライン環境 (出張中、 飛行機内、 ネット不安定な状況) で生成を走らせると cache fetch に失敗する。 ただし「過去にダウンロード済みの record を ローカルに 2 段キャッシュする」ことで頻度は緩和できる
- bucket / lifecycle policy / アクセス制御 / metrics の運用責務が新たに発生する
- record の状態がリポジトリ外に置かれるため、 cache 起因の挙動を git history から追跡できない ( bisect で再現性を取りづらい)

> **クラウド認証** ( AWS / GCP / Azure 等) が組織内で既に確立されているチームでは、 セットアップコストはほぼ追加発生しない。 一方で OSS として配布する場合、 利用組織ごとの認証セットアップが必要なため初期導入のハードルになりうる。

### Option D: hash をファイル名に artifact 直コミット

input_hash をファイル名にして、 生成物本体を `.lazygen/cache/` 配下にそのまま git commit する。

👍 **Pros**

- artifact 共有が自動的に実現する
- 認証 / 外部 infra 不要
- ネットワーク不要

👎 **Cons**

- ファイル数が `タスク数 × 並走世代数 × 生成ファイル数` で増え、 数万〜数十万オーダーに膨張する見込み
- git clone size / git operations (`status` / `add` / `log`) の劣化が無視できない
- 本シリーズの ADR が解消したい "ゴミ累積" の最も悪い形

### Option E: Hybrid (git に index + S3 に artifact)

git に小さな mapping (record) を持ち、 artifact を S3 に置く。

👍 **Pros**

- record の deterministic 性 / コンフリクト無しは Option A 同様に確保
- artifact 共有も実現できる

👎 **Cons**

- S3 への依存が部分的に発生 (Option C のネットワーク必須デメリットを部分的に背負う)
- 実装複雑度が Option A + Option C を足したものになる
- artifact 共有自体は本シリーズの ADR でメリットが小さいと判断されている (ADR-0002 で Option 3 artifact restore が棄却された理由を参照: deterministic generator では再生成と復元の結果が同じで、 generator 実行時間が秒オーダーなら復元の時間短縮メリットが薄い)

## Decision

**Option A: git per-task per-input ファイル方式を採用する。**

採用根拠は以下の論理連鎖で論証される:

1. **R1 (共有可能) と R5 (コンフリクト無し) を両立する候補は A / C / D / E の 4 つ。** B (単一ファイル) は構造的にコンフリクト不可避で除外
2. **D は artifact 本体を全て git に含めるためファイル数が爆発し、 git operations を破壊するため除外**
3. **E は artifact を S3 に置くが、 そもそも artifact 共有自体のメリットが ADR-0002 の Option 3 棄却ロジックで小さいと判断されており、 record の git 管理に S3 依存を上乗せする実装複雑度に見合わない**
4. **残った A vs C は「git で完結する素朴さ」 vs 「外部ストレージで GC を自動化する clean さ」のトレードオフ**
   - 一般的 monorepo 規模試算 (record 1 つ 約 2 KB × タスク数 200 × 並走世代 10 ≒ 4 MB) では、 git に置いて困る容量ではない
   - GC は CI nightly sweep / `cache gc` サブコマンド / pre-commit hook で実装可能
   - C の主たるデメリットは「ネットワーク必須」と「外部 infra 運用責務」。 ローカル 2 段キャッシュで前者は緩和できるが、 後者 (bucket 運用、 lifecycle policy、 metrics) は恒常的なコスト
   - C の主たるメリットは「PR diff にノイズが現れない」「容量無制限」だが、 一般的 monorepo 規模ではどちらも決定的ではない
5. **結果、 A の素朴さ ( git pull で完結、 ネットワーク不要、 外部 infra ゼロ) が、 一般的 monorepo の規模と運用フローに最も適合する**

PR ノイズの懸念 (A の Cons) については、

- `.lazygen/cache/**` を `.gitattributes` で `linguist-generated` 指定し、 GitHub PR diff の default collapsed にする
- PR template に「`.lazygen/cache/` 配下の差分は人間レビュー対象外」と明記する

で運用上緩和する。

## Consequences

### 正の影響

- 構造的コンフリクトを解消し、 ブランチ間で record が衝突しなくなる
- 認証 / 外部 infra / ネットワークなしで開発者間 / CI 間で cache を共有できる
- `git pull` で cache 状態も同期される単純なメンタルモデル
- 既存パッケージマネージャ群 (`go.mod` / `aqua.yaml` / `pnpm-lock.yaml`) を変更せずに済む

### 負の影響

- record が累積するため GC 機構の実装が必要:
  - CI nightly sweep で古い record の削除 PR を bot 投稿
  - `lazygen cache gc` サブコマンドで同一 task 配下の record を mtime 古い順に削除
  - lefthook / pre-commit hook で task rename / 削除コミット時に対応 record も削除
- record 差分が PR diff に現れるため、 `linguist-generated` 等のノイズ抑制設定が必要
- record ファイル数の増加に伴う git operations のスループットは将来的に再評価
- 容量が想定を超えた場合、 もしくは将来 artifact 共有が必要になった場合は Hybrid (Option E) への拡張余地を残す

### 後続の詳細設計

- 詳細は [Design Doc](../design/architecture.md) を参照
