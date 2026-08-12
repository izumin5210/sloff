# ADR-0021: CI 向けドリフト検証コマンド `sloff check`

## Status

Accepted

## Context

### 背景

生成物を git commit する運用 ( ADR-0002 前提) では、 「spec の inputs を変更したのに generator を回し忘れた」 「outputs を手で書き換えたまま commit した」 「生成物だけ commit して record を commit し忘れた」 といったミスが PR に混入しうる。 CI がこれを検出する素朴な手段は「全 task をフル再生成して `git diff --exit-code`」 だが、 generator 実行時間 (秒〜分オーダー × 数十 task) がそのまま PR のフィードバック遅延になる。

sloff の fingerprint 機構は、 この検証を **generator を一切実行せずに** 行える判定材料をすでに持っている:

- ヒット判定 ( [ADR-0002](./0002-fingerprint-hit-decision-model.md) output-comparison) は `input_hash` 一致 + record の `output.hash` と現ツリーの output ハッシュ一致で、 generator 実行を必要としない
- record は git commit される ( [ADR-0003](./0003-fingerprint-storage-strategy.md) local backend) ため、 CI checkout に判定材料が完結して揃う
- no-drift 状態の `sloff run` は全 task SKIP であり、 実質この検証と同じ処理をすでに行っている

つまり必要な機能は「**`sloff run` の SKIP 判定パスを read-only で全 task に評価し、 1 つでも RUN に落ちるなら fail する**」 という run の部分集合である。 本 ADR はこれを `sloff check` として導入する判断と、 その **検出保証の境界** を確定する。

### fingerprint チェックが構造的に検出できないもの

「fingerprint チェックだけで drift 検証が成立するか」 の批判的検討の結果、 以下の 4 クラスは **原理的に検出不能** であることを確認した。 これらは check の bug ではなく、 fingerprint の信頼モデル ( 「generator は宣言された inputs 以外を読まず outputs 以外を書かない」 「generator は deterministic」 「record は正直な run が書いた」) を check がそのまま継承する帰結である:

| # | 盲点 | 理由 | 補完手段 |
|---|---|---|---|
| B1 | **inputs 宣言漏れ** | generator が `inputs` glob 外のファイルを読んでいる場合、 そのファイルの変更は `input_hash` に乗らず、 古い record にヒットして PASS する | overlap 検証 ( ADR-0013) / spec lint 文化 / 定期フル再生成 job ( 後述) |
| B2 | **record の偽造・手編集** | record は開発者が commit する平文 protobuf であり、 手編集した outputs に整合する record を細工できる。 開発者を敵対者と見なす脅威モデルは本 ADR のスコープ外 | 定期フル再生成 job |
| B3 | **非決定性の隠蔽** | generator が non-deterministic でも、 committed outputs が「誰かの正規の run の結果」 と一致する限り PASS する ( これは CI の flaky 化を防ぐ利点でもある。 architecture.md Open Question Q1 と同根) | cross-OS double-run 検証 ( Q1) / 定期フル再生成 job |
| B4 | **stale な余剰生成物** | `.proto` を削除したのに残った古い `.pb.go` など、 record の `output.files` はどの task も生成しないファイルを関知しない。 なお **フル再生成 + `git diff` でも generator が古い出力を消さない限り検出できず**、 fingerprint チェック固有の弱点ではない | 将来拡張 ( 後述) |

### 評価軸

- **検出保証の境界が明文化されること**: check が緑 = 何が保証されるかを利用者が誤解しない
- **fingerprint の健全性 ( R4 invalidate 安全性) を毀損しないこと**: 検証の高速化のためにツール解決を省略するなどの「弱い判定」 を既定にしない
- **read-only であること**: CI 上の検証コマンドが record / 作業ツリーを書き換えない
- **機械判別可能であること**: CI workflow が「drift ( 利用者のミス)」 と「チェック実行不能 ( 環境問題)」 を分岐できる
- **escape hatch で保証を弱められないこと**: ADR-0012 が `--force` を CLI-only にしたのと同じ思想で、 env var 1 つで検証が骨抜きにならない

### References

- [ADR-0002: fingerprint hit 判定モデル](./0002-fingerprint-hit-decision-model.md)
- [ADR-0003: fingerprint のストレージ方式](./0003-fingerprint-storage-strategy.md)
- [ADR-0012: `--force` flag](./0012-force-rerun-flag.md)
- [ADR-0017: barrier tasks](./0017-barrier-tasks.md)
- [ADR-0019: tool bootstrap depends](./0019-tool-bootstrap-depends.md)
- [Design Doc: sloff Architecture](../design/architecture.md)

## Considered Options

### D1. 検出保証のレベル ( 脅威モデル)

| | A: ミス防止のみ (採用) | B: `--deep` ( フル再生成 + diff) も機能化 | C: record 偽造耐性まで担保 |
|---|---|---|---|
| B1〜B4 の扱い | docs に明記 + 定期フル再生成を運用レシピとして文書化 | B1 / B2 / B3 を CI 機能として捕捉 | B2 を署名等で構造的に排除 |
| 実装コスト | 小 | 中 ( run --force + ツリー diff の統合) | 大 ( 鍵管理 / 署名検証) |
| 追加価値 | — | `sloff run --force` + `git diff --exit-code` の組合せで既に実現可能 | monorepo 内部開発者を敵対者と見なすモデルは費用対効果が合わない |

**Option A を採用。** check の保証は「gen 忘れ / commit 忘れ / 手編集 / ツールバージョン乖離の検出」 に限定し、 盲点 4 クラスは本 ADR と architecture.md に明記する。 B1〜B3 の補完は **既存プリミティブの組合せ ( nightly 等の定期 job で `sloff run --force` + `git diff --exit-code`)** を推奨レシピとして文書化するに留め、 専用機能は追加しない。 これは ADR-0002 が Option 4 ( hybrid: input-only + 遅延 CI 検証) を「**ヒット判定モデルとしては**」 棄却した判断と矛盾しない — 本 ADR のレシピは fail-fast なヒット判定 ( output-comparison) の上に載る追加の防御線であり、 判定を遅延検証で代替するものではない。

### D2. 環境要件

| | A: フルツールチェーン必須 (採用) | B: hermetic モード併設 ( record の resolved_versions を信用) |
|---|---|---|
| ツール更新 drift ( R4) の検出 | ◎ ( CI のツールで再解決するため、 dev / CI のバージョン乖離も fail する) | × ( record の自己申告を信用した時点で invalidate 経路が死ぬ) |
| CI 要件 | run と同一 ( script tool のバイナリ / `pnpm install` 済み / Go toolchain / provider 実行) | 軽量 |

**Option A を採用。** `input_hash` の 3 構成要素のうち `resolved_versions_hash` は CI 上での実解決 ( script: `<bin> --version` 実行、 go-local: `go/packages` 解析、 pnpm-local: lockfile BFS) を要求する。 これを省略した判定は「そのバージョンのツールで生成された」 という保証を放棄することと等価であり、 弱い判定モードの併設は「check が緑」 の意味を context 依存にしてしまうため提供しない。 「gen は実行しないが、 gen 可能な環境が必要」 が check の環境要件である。 CI はどのみち lint / build 用に同等のツールチェーンを持つことが多く、 実運用上の追加コストは小さい。

### D3. CLI 表面

| | A: `sloff check` 新設 (採用) | B: `sloff run --check` | C: `sloff fingerprint verify` |
|---|---|---|---|
| 契約の独立性 | ◎ ( exit code 体系 / read-only 保証 / check 専用 flag の置き場) | △ ( 「run なのに実行しない」 + `--force` との排他処理) | △ |
| 抽象度の整合 | ◎ | ○ | × ( `show` / `gc` は record 単体操作。 check は provider 展開 / tool 解決 / preflight を伴う全 task オーケストレーション) |

**Option A を採用。**

## Decision

**`sloff check` サブコマンドを新設する。 run と同一の計画フェーズを通したうえで、 全 task の fingerprint ヒット判定を read-only で評価し、 RUN に落ちる task があれば drift として fail する。 generator は一切実行しない。**

### 判定セマンティクス

check は run と同じ順序で計画フェーズを実行する: spec discover → command provider 展開 ( ADR-0015、 provider は実行される) → depends パターン展開 ( ADR-0016) → tool registry 構築 + bootstrap depends 注入 ( ADR-0019) → preflight → tool 解決 → collectTasks → depgraph 構築 → plan 時 overlap 検証 ( ADR-0013)。 これらの失敗はすべて「チェック実行不能」 ( 後述 exit 2) である。

そのうえで barrier でない全 task を評価する ( 実行順序制約がないため並列評価できる):

| 判定 | 条件 | 分類 |
|---|---|---|
| **ok** | `input_hash` の record が存在し、 record の `output.files` を現ツリーでハッシュした結果が `output.hash` に一致 | clean |
| **record miss** | 現在の `input_hash` に対応する record がない | **drift**。 「inputs 変更後の gen 忘れ」 と「record の commit 忘れ」 はこの時点で原理的に区別できないため、 メッセージは両方の可能性と修復手順 ( `sloff run` → 生成物と `.sloff/fingerprints/` を commit) を案内する |
| **output mismatch** | record はあるが、 `output.files` のいずれかが欠損 / 改変されている | **drift**。 欠損 / 改変ファイルを個別に列挙する |
| **input missing** | task の inputs ( tool contribution 含む) に現ツリーに存在しないファイルがある | **drift**。 典型は「上流 task の生成物 ( = この task の input) が commit されていない」 で、 上流 task 自体も別途 drift 判定される |
| **unverifiable** | 参照する tool の解決に失敗し `input_hash` を計算できない ( 後述) | tool の depends 先 producer の判定結果で drift / 実行不能に切り分け |

- **barrier task** ( ADR-0017) は fingerprint を持たないため判定対象外
- **run-time 側の検証は対象外**: producedBy 集計 ( ADR-0004 D3) / run 時 depends 漏れ検証 ( ADR-0013 D3 run-time 半分) / inputs 漏れ warning は task 実行を前提とするため check には存在しない。 plan 時 overlap 検証はそのまま効く

### 副作用ゼロ ( read-only 契約)

- record の書き込み / 上書き / collapse を一切行わない。 `--force` 相当の概念もない
- 作業ツリーを変更しない
- 例外は per-file content digest cache ( [ADR-0014](./0014-persistent-file-content-hash-cache.md)、 `$XDG_CACHE_HOME` 配下のホストローカル純粋性能キャッシュ) のみ。 run と同様に読み書きし、 `SLOFF_NO_FILE_HASH_CACHE` もそのまま効く。 repo 内容には影響しない

### exit code 契約

| code | 意味 | CI 側の典型対応 |
|---|---|---|
| 0 | clean ( 全 task ヒット) | pass |
| 1 | drift 検出 ( record miss / output mismatch / input missing / producer drift 由来の unverifiable) | 「`sloff run` を実行し、 生成物と `.sloff/fingerprints/` を commit してください」 を案内 |
| 2 | チェック実行不能 ( spec エラー / tool 解決失敗 / preflight 失敗 / provider 失敗 / storage エラー) | 環境 / spec の修正が必要。 drift とは別対応 |

### `SLOFF_ALLOW_STALE_DEPS` は check では無効

run では preflight 失敗を warn に降格 + read-only 化する escape hatch だが、 check は元々 read-only であり、 残る効果は「preflight 失敗でも判定を続行する」 だけになる。 preflight が失敗している状態 ( install drift) では `resolved_versions` の計算前提が崩れており、 その状態で緑を返すのは検証コマンドとして自己矛盾する。 よって check では **本 env var を無視し、 常に preflight 失敗 = exit 2** とする ( set されていたら「check では無視される」 旨を warn 表示する。 boolean として不正な値は run と同じく即エラー)。 検証コマンドの保証が env var で弱まらないことを構造的に担保する。 これは `--force` の env var mirror を提供しない ADR-0012 と同じ思想である。

### deferred tool ( ADR-0019) との相互作用

`depends` を宣言した tool の解決失敗は、 run では「producer task 実行後に再解決」 で救済されるが、 check は producer を実行しないため再解決しても状況は変わらない。 clean な committed ツリーなら tool のソース閉包は揃っていて解決は成功するはずであり、 失敗するのは (a) producer の生成物が commit されていない = それ自体が drift、 または (b) 純粋な環境問題、 のいずれかである。 tool 解決エラー単体からは区別できないが、 **producer task 自体の判定は check が行う** ので機械的に切り分ける:

- 解決失敗した tool の consumer task は **unverifiable** として報告する
- その tool の depends 先 producer ( 推移的閉包) のいずれかが drift 判定 → 根本原因は gen / commit 忘れ。 **drift として exit 1** ( エラーは tool 主語で producer の drift を併記)
- producer がすべて clean → 環境問題。 **exit 2**

depends 未宣言 tool の解決失敗は run と同じく即 fatal ( exit 2。 typo の早期検出は check でも不変)。

### 検出保証の境界 ( 明文化)

check の緑が保証するのは「**committed outputs が、 現在の inputs ( CI 上で解決したツールバージョン込み) に対して、 いずれかの正直な `sloff run` が記録した結果と一致している**」 ことである。 Context 節の B1〜B4 は検出できない。 利用者向けドキュメントにも保証範囲と補完レシピ ( 定期 `sloff run --force` + `git diff --exit-code` job) を明記する。

### 将来拡張 ( 本 ADR では実装しない)

- **stale 余剰ファイル検出 ( B4)**: 全 task ヒット時、 ヒットした record 群の `output.files` の和集合は「現 spec が生成するはずの全ファイル」 になるため、 現ツリーで outputs glob にマッチするがどの record にも含まれないファイルを追加コストほぼゼロで列挙できる。 outputs glob 配下に手書きファイルが同居する運用での false positive を考慮し、 導入時は warning / `--strict` 昇格の形を想定。 需要が確認できた段階で別 ADR で扱う
- **機械可読出力 ( `--format json`)**: PR コメント自動生成等の需要が出た段階で追加する

## Consequences

### 正の影響

- PR ごとのドリフト検証が generator 実行時間から解放される ( 判定コストはファイルハッシュ + ツール解決のみ。 no-drift 時の `sloff run` と同等)
- 「record の commit 忘れ」 が CI で構造的に検出される ( architecture.md Open Question Q2 の一部を解消)
- dev / CI のツールバージョン乖離が drift として顕在化する ( R4 の検証が CI に載る)
- 検証コマンドの保証が env var で弱まらない

### 負の影響 / 注意点

- CI に run と同一のフルツールチェーンが必要 ( D2)。 環境を持たない軽量 CI では使えない
- B1〜B4 は検出できない。 「check が緑 = フル再生成しても diff ゼロ」 **ではない** ことを利用者が理解する必要がある ( docs で明記 + 定期フル再生成レシピで補完)
- record miss のメッセージは「gen 忘れ」 と「record commit 忘れ」 を区別できず、 利用者は両方を確認する必要がある
- CLI 表面が 1 つ増える

### 後続の更新

1. [Design Doc: architecture.md](../design/architecture.md): `sloff check` 節を追加 ( 判定セマンティクス / CI レシピ / 保証境界)、 Open Question Q2 に本 ADR への参照を追記
2. `internal/sloff/runner`: `Check(ctx)` を追加 ( 計画フェーズは Run と共有、 評価は read-only)
3. `cmd/sloff/check.go`: サブコマンド追加 ( exit code マッピング / drift レポート出力)
4. E2E test: clean pass / record miss / output mismatch ( 改変・欠損) / input missing / barrier 除外 / escape hatch 無効 / deferred tool の drift ↔ 環境問題切り分け / read-only 性 ( 全 case で expected == initial) の fixture 群を追加
