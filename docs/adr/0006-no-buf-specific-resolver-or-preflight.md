# ADR-0006: lazygen は buf を special-case しない

## Context

`buf generate` は lazygen が対象とする典型的な複合 generator の一つで、 buf module / codegen plugin / `.proto` 入力の組合せが cache 健全性に効く。 当初は他 channel ( script / go-local / pnpm-*) と同様に **buf 専用 resolver + preflight** を導入する方向で実装し、 [PR #8](https://github.com/izumin5210/lazygen/pull/8) で:

- `buf.gen.yaml` の `plugins[].remote` を parse して `buf-remote:host/owner/name@version+rev<n>` を `ToolVersion` に追加
- `buf.yaml` deps + `buf.lock` の commit を読んで `buf-dep:host/owner/name@<commit>` を `ToolVersion` に追加
- preflight で remote plugin の pinned tag lint と `buf.yaml` ↔ `buf.lock` 整合検証

を実装した。 しかし PR #8 の review プロセスで「これらは本当に lazygen 側の責務か」を問い直し、 以下の観察に至った。

### References

- [ADR-0001: キャッシュ可能コード生成オーケストレーターの選定](./0001-cache-aware-codegen-orchestrator-decision.md)
- [ADR-0002: キャッシュヒット判定モデル](./0002-cache-hit-decision-model.md)
- [ADR-0004: Spec 検証と output 重複検出のポリシー](./0004-spec-validation-and-output-conflict-policy.md)
- [ADR-0005: Resolver auto-dispatch を廃止し、 すべて declared 経由に統一](./0005-eliminate-resolver-auto-dispatch.md)
- PR #8: feat(buf): add buf resolver and preflight checker ( クローズ済)

### 観察

**O1. buf.lock の commit は immutable で、 spec.inputs に入れれば files_hash で完結する**

`buf.lock` は BSR module の resolved commit を記録する。 BSR の commit は git commit と同じく immutable ( 同 commit ⇒ 同内容) で、 `buf dep update` を走らせると `buf.lock` の content が書き変わる。 つまり「BSR module dep が変わった」事実は **`buf.lock` のファイル内容変化** として現れる。 spec の `inputs:` に `buf.lock` を含めれば、 lazygen の `files_hash` 経路で確実に invalidate される。 `buf-dep:` ToolVersion を別途 emit する積極的な理由は無い。

**O2. buf.gen.yaml も同様**

pinned tag (`:vX.Y.Z`) で記述された remote plugin / `protoc_builtin` / `local` plugin は、 全て `buf.gen.yaml` のファイル content として表現される。 spec.inputs に `buf.gen.yaml` を含めれば content 変更が files_hash で拾える。

**O3. 唯一の implicit drift は `:latest` だが、 これは buf 利用者の規律の問題**

「ファイル content は変えずに BSR 側で resolved version が動く」唯一の経路は codegen remote plugin の `:latest` 指定 ( buf.lock には codegen plugin が記録されないため)。 しかしこれは:

- セキュリティ上、 unpinned dependency は supply chain attack の温床
- 再現性の観点でもどの project でも避けるべき practice

であり、 buf 利用者が **buf レベルで規律として担保する** 性質のもの。 lazygen が cache 健全性の名のもとに lint を背負うのは越権で、 buf 自身の運用ルール ( CI で `buf format --diff` ならぬ pinned check を回す等) で担保すべき。

**O4. buf.yaml ↔ buf.lock 整合性は buf 自身が runtime で error を出す**

drift があると `buf generate` が error で落ちる。 lazygen が事前に検出するのは「より早く clean な error を出す」UX 改善でしかなく、 cache 健全性には影響しない。

**O5. buf の local cache (`~/.cache/buf/...`) は commit-keyed**

pnpm の `node_modules` のように「lockfile を更新したのに install を忘れた」状態が構造的に起きにくい。 commit hash でディレクトリが分かれており、 `buf generate` 時に missing なら自動 fetch される。 lazygen 側で local cache 整合性を check する必要はない。

## Decision

### D1. lazygen は buf 専用 resolver / preflight checker を持たない

`internal/lazygen/toolresolver/buf` も `internal/lazygen/preflight/buf` も導入しない。 spec の `tools:` にも `buf:` 形式の declared 種別を追加しない。 buf を lazygen から使うときは、 既存の汎用プリミティブ (`exec` resolver と spec.inputs glob) のみで完結させる。

### D2. buf 利用者は spec.inputs に buf 設定ファイルを含める

`buf.gen.yaml` / `buf.yaml` / `buf.lock` を spec.inputs に含めるのを推奨運用とする。 これにより:

- buf.gen.yaml の plugin pinned tag 変更 → invalidate
- buf.yaml の deps 編集 → invalidate ( + `buf dep update` 後の buf.lock 変更も同時に invalidate)
- `buf dep update` のみの変更 → buf.lock 変更で invalidate

の全経路が files_hash 一本で押さえられる。

### D3. buf 本体 / local plugin / protoc_builtin の version は spec の `tools:` で別 entry として宣言する

buf 本体や `protoc-gen-go` のような local plugin は、 通常の `tools: [{exec: [...]}]` ( script resolver) で各々 declare する。 これは ADR-0005 の declared-only 原則と整合する自然な扱いで、 buf 専用の dispatch 機構は不要。

```yaml
# 推奨される buf-driven task の宣言例
commands:
  - name: protoc-gen-go
    cmd: buf generate
    inputs:
      - "**/*.proto"
      - buf.gen.yaml
      - buf.yaml
      - buf.lock
    outputs: ["**/*.pb.go", "**/*.connect.go"]
    tools:
      - exec: ["buf", "--version"]
      - exec: ["protoc-gen-go", "--version"]
        extract: 'v[0-9]+\.[0-9]+\.[0-9]+'
```

### D4. pinned tag 強制 / buf.yaml ↔ buf.lock 整合検証は buf 利用者の責務

これらは buf レベルの規律として、 利用者の CI (`buf format` / `buf lint` / カスタム pinned-check ) や code review で担保する。 lazygen は cache orchestrator としての責務に集中する。

## Rationale

### responsability boundary

lazygen の core 責務は「OS 横断 / 共有可能な cache を、 generator inputs / outputs / tools の同一性に基づいて管理する」こと ( ADR-0001 / ADR-0002)。 各 channel の resolver は「lockfile / runtime バイナリ等から **OS 中立な version 文字列を取り出す**」のが本分で、 「その lockfile の書き方が正しいか」 を lint するのは隣接領域。

buf については O3 で示した通り「pinned 必須」は buf 利用者がそもそも持つべき規律で、 lazygen が機械的に強制するのは越権。 また buf-dep / buf-remote の hashing は spec.inputs に該当ファイルを入れる運用と機能的に重複しており、 多重防御として残す価値より「責務が重複している」 デメリットの方が大きい ( buf schema 変更時の追従コスト、 v1 互換性の議論コスト等)。

### Less is more

PR #8 の review で「v1 buf.lock の dep identity フィールド」「v1 buf.gen.yaml schema」 等の互換性論点が複数浮上した。 これらは「lazygen が buf schema の特定 version を理解する」という前提を持った瞬間に生じる API surface の負債。 buf を special-case しないことで、 これら全てを利用者側 ( buf 自身が schema migration を提供している) に委ねられる。

### `:latest` は本当に問題か

`:latest` を許す利用者は cache 健全性以前に security 問題を抱えている。 lazygen が silent に stale を起こすこと自体は問題だが、 「lazygen は pinned 前提で動く」と明示し ( このドキュメントで明示した)、 利用者が pinned 規律を守る限り問題は起きない。 守らない利用者は別の問題を先に踏むので、 lazygen の lint を待たずとも顕在化する。

## Consequences

### 正の影響

- lazygen 本体に buf 固有の知識が入らず、 buf schema 変更の追従コストがゼロ
- spec の宣言形式が「buf でも他 generator でも同じ ( exec + inputs)」に統一され、 学習コスト低減
- ADR-0005 の declared-only 原則が 100% 守られる ( buf 専用 dispatch を入れる誘惑が無い)
- v1 / v2 schema 互換性議論が lazygen から完全に外れる

### 負の影響 / 注意点

- `:latest` 等の unpinned 指定が silent stale を起こすケースを lazygen 側で検出できない。 利用者の規律 (CI / code review) で担保する必要があり、 これが破られた場合 cache が嘘をつく
- buf.yaml ↔ buf.lock drift は `buf generate` の runtime error として現れるため、 「lazygen 起動直後に検出」は得られない ( 数秒〜数十秒後の buf 起動時に error が出る)。 UX の劣化は許容
- 利用者は spec.inputs に `buf.gen.yaml` / `buf.yaml` / `buf.lock` を含める必要がある。 docs / 利用例 ( architecture.md / README) で明示的に案内する

### 将来再考の余地

`:latest` 利用者が想定外に多い / silent stale 事故が頻発する、 等の状況が生じた場合、 別 ADR で「opt-in の buf-pinned-lint preflight」 を再導入する余地はある。 ただし default で有効にはせず、 利用者が明示的に opt-in する形 ( `LAZYGEN_BUF_PINNED_LINT=1` 等) を推奨する。
