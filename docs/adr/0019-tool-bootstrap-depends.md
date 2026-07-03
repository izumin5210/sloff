# ADR-0019: tool の bootstrap 依存を spec に明示宣言する (`tools.<name>.depends`)

## Status

Accepted

## Context

### 背景

[ADR-0008](./0008-tool-as-first-class-spec-entity.md) は tool を first-class spec entity とし、runner は Run 冒頭で **参照される全 tool の Inputs / Versions を task 実行前に一括解決**する ( `runner.go` の `resolveContribs`)。go-local resolver の Inputs は `packages.Load` で tool source の import 閉包を列挙するため、**閉包が compile 可能であることが解決の成立条件**になっている。

導入先 monorepo でこの前提が壊れるケースが顕在化した:

- 内製 protoc plugin ( go-local tool) が **自リポジトリの生成物 ( `*.pb.go`) を compile-time import** している
- `make clean && sloff run` 相当の運用で生成物を一括削除すると、cold state からの run は **tool 解決の時点で fatal** し、その pb.go を再生成するはずの task を 1 つも実行できない
- 同型の問題が pnpm-local にもある: ExtraInputs の列挙は `git ls-files` ベースのため **worktree から削除済みの tracked file** を含み、runner の fingerprint prefetch ( `optimisticKey`) が stat で fatal する

構造として、これは [ADR-0013](./0013-explicit-task-dependencies.md) が task ordering について解決した問題の再演である。ADR-0013 は「実行順序の導出がファイルツリー状態に依存する」欠陥を、**順序 = 明示宣言 / 健全性 = overlap 検証**という責務分離で解消した。しかし tool 解決には同じ分離が適用されておらず、**run の成立可能性そのものがツリー状態に依存する**箇所として残っている。

依存の実体は「**tool T の source 閉包に、task P の outputs が含まれる**」という **tool → task の依存**である。現状これを表現する語彙が spec に無く、導入先では「T を使う *全 consumer task* に P への depends を書く」ことで間接表現している ( ADR-0013 D3 の overlap 検証がこの edge を要求するため)。結果、同一の producer リストが consumer ごとにコメント付きで複製されている。宣言の持ち主が tool である以上、置き場所も tool であるべきである。

なお cold tree では tool の閉包自体が列挙不能なため、「閉包と task outputs の overlap から tool の依存を自動導出する」方向は原理的に成立しない ( 導出が必要になる状況でだけ導出できない)。これは ADR-0013 が Option A ( overlap 自動導出) を棄却したのと同じ理由であり、明示宣言が正しい答えになる。

### 評価軸

- **cold-state 成立性**: 生成物ゼロの状態から `sloff run` 一発で成功に到達できること
- **warm-path 無劣化**: 解決成功時の経路 ( upfront 一括解決 + optimistic prefetch) を一切変えないこと
- **fingerprint 健全性**: record は常に「完全な入力集合から exec 時に計算された hash」でのみ書かれること ( ADR-0002 / ADR-0009)
- **決定性**: 実行順序・エラーの帰属が、ツリー状態や scheduling の偶然に依存しないこと
- **明示宣言との整合**: 暗黙推論より明示宣言 + 検証を選ぶ既存路線 ( ADR-0005 / ADR-0013) と揃うこと
- **記述コスト**: consumer ごとの producer リスト複製を増やさない ( できれば減らす) こと

### References

- [ADR-0005: resolver の auto-dispatch を廃止する](./0005-eliminate-resolver-auto-dispatch.md)
- [ADR-0008: tool を first-class spec entity とする](./0008-tool-as-first-class-spec-entity.md) ( D3: path 系フィールドは tool 定義 spec dir 相対)
- [ADR-0013: タスク間依存を spec に明示宣言する (`depends`)](./0013-explicit-task-dependencies.md) ( D2: edge は宣言のみ / D3: overlap 検証の 2 段構え / D4: depends は input_hash に不参加)
- [ADR-0016: タスク依存をパターンで宣言する](./0016-pattern-task-dependencies.md)
- [ADR-0017: 集約専用の barrier task を first-class にする](./0017-barrier-tasks.md) ( D3: barrier はデータ読み取りの免罪符にならない)

## Considered Options

### Comparison Table

| | A: 外部 bootstrap | B: 失敗時に無条件遅延 | C: 解決を常に DAG 内へ | D: resolver の部分列挙 | **E: tool depends + 宣言 gated 遅延 (採用)** |
|---|---|---|---|---|---|
| cold-state 成立性 | △ ( sloff の外で担保) | ◎ | ◎ | ○ ( 解決は通るが…) | ◎ |
| warm-path 無劣化 | ◎ | ◎ | × ( prefetch が退化) | ◎ | ◎ |
| fingerprint 健全性 | ◎ | ◎ | ◎ | × ( 不完全な input set で record が書かれる) | ◎ |
| 決定性 | × ( 外部手順との暗黙契約) | △ ( エラーの帰属が scheduling 依存) | ◎ | ◎ | ◎ |
| 明示宣言との整合 | × | × ( 暗黙 fallback) | ○ | × | ◎ |
| 記述コスト | × ( 手順書 / Makefile) | ◎ ( ゼロ) | △ | ◎ | ○ ( tool に 1 箇所、consumer 複製は削減可能) |

### Option A: 外部 bootstrap ( 利用側の Makefile 等で先行生成)

sloff の実行前に「tool が import する生成物を素の generator で先行生成する」手順を利用側に置く案。導入先で実際に検証され ( cold 一発成功は達成)、「実行順序の契約が sloff の外に漏れ、根本解決でない」として利用側で棄却された。sloff の設計としても、orchestrator が自身の実行前提を自身の DAG で表現できないことを認めることになる。棄却。

### Option B: 失敗時に無条件で遅延解決 ( defer-on-any-error)

解決失敗した tool を無条件に「最初の consumer task の実行直前に再試行」へ降格する案。spec 無変更で cold が成立し、順序保証も「consumer が閉包 producer への depends を持つ」こと ( ADR-0013 D3 が検証で強制済み) から only-if-free で得られる。

- ○ 機構としては最小で、E の内部エンジンそのもの
- × エラーの帰属が「たまたま最初に scheduled された consumer task」になり、原因 ( tool) と主語 ( task) がズレる。エラー面がツリー状態と scheduling に依存する
- × entry の typo のような純粋な誤設定まで task 実行時へ遅延する
- × 「エラーなら何でも defer」は暗黙 fallback であり、明示宣言 + 検証を選んできた路線 ( ADR-0005 / ADR-0013) から半歩外れる

棄却 ( ただし遅延実行の機構は E がそのまま採用する)。

### Option C: tool 解決を常に DAG 内へ移す ( 常時 lazy)

解決を「tool の依存 task 完了後」に常に schedule する案。cold は成立するが、warm run で致命的な退化がある: consumer task の optimistic prefetch key は tool の ExtraInputs 込みで計算する必要があるため、解決が実行フェーズに入ると **warm no-op でも consumer 群を prefetch できず**、per-key の live Load ( remote backend では per-key RTT) に退化する。upfront 一括解決 + 一括 prefetch は warm-path の生命線であり、壊せない。棄却。

### Option D: resolver の部分列挙 ( partial enumeration)

go-local が `packages.Load` の error を無視し「列挙できた file だけ」を返す案。欠けているのはまさに生成予定の pb.go であり、それが input surface から漏れた状態で record が書かれると「pb.go が変わっても hit し続ける」stale cache を生む。fingerprint 健全性の直接毀損。exec 時再列挙で補正するなら結局 E に収束する。Prewarm の「結果を変えない純最適化」契約 ( `toolresolver.Prewarmer`) とも矛盾する。棄却。

### Option E: tool depends + eager 解決 + 宣言 gated 遅延 ( 採用)

tool 定義に `depends` を追加し、(i) その tool を使う全 task へ edge として注入、(ii) 解決は従来通り run 冒頭に eager 実行、(iii) 失敗時は **depends を宣言した tool に限り** 遅延解決へ降格する。

- ◎ cold 一発成立。warm は解決成功時に現行と完全同一経路
- ◎ tool → task 依存の宣言が tool に一元化され、consumer 側の複製リストは注入で代替される ( 将来削除可能)
- ◎ depends 未宣言 tool の失敗は従来どおり run 冒頭 fatal — typo の早期検出が保存される
- ◎ エラーは tool に帰属し決定的 ( 「T は宣言 depends 完了後も解決不能」と一意に言える)
- △ spec 文法・注入 phase・遅延状態と、機構は 4 案中最大

## Decision

**Option E を採用する。tool 定義は `depends` で bootstrap 依存 ( 解決の前提となる task) を宣言でき、runner はそれを consumer task へ edge 注入し、解決失敗時は宣言 tool に限り「depends 完了後の遅延解決」へ降格する。**

### D1. spec 文法

```yaml
tools:
  protoc-gen-foo:
    go-local: ./cmd/protoc-gen-foo
    depends:
      - {task: gen-foo-proto}            # 同一 spec dir の task
      - {spec: ../gen, task: gen-bar}    # 解釈基準は tool 定義 spec の dir ( ADR-0008 D3)
```

- `depends` の entry は task depends と同形 ( `{spec, task}`)。`spec` は **tool を定義した sloff.yml の dir 相対** ( ADR-0008 D3 の path 解釈と同一)
- どの resolver 形式 ( script / go-local / pnpm-local) の tool でも宣言可 ( 遅延解決の適用条件は resolver 形式でなく宣言の有無)
- glob パターン ( ADR-0016) は **v1 では非対応** ( load 時 error)。tool の閉包 producer は具体的な task 集合であり、現時点で実例が無い。実例が出たら ADR-0016 の展開機構を注入前に適用する形で拡張する
- load 時 validation: `task` 必須 / `spec` は相対 path ( 絶対 path は error) / 同一 tool 内の重複 entry は error
- 参照先 task の存在検証は load 時ではなく **注入時** ( D2) に行う。command provider ( ADR-0015) が生成する task を参照できる必要があり、また参照されない catalog tool の depends を検証しないのは「参照されない tool は解決も検証もしない」既存方針 ( ADR-0008) と整合する

### D2. edge 注入 ( injection)

tool T を `tools[]` に列挙する全 task の `depends` に、T の `depends` を注入する。

- **時期**: command provider 展開 ( ADR-0015) → パターン depends 展開 ( ADR-0016) → tool registry 構築 → **注入** → depends 参照検証 → collectTasks。生成 task への参照と、生成 task への注入の両方が成立する
- **path 変換**: tool 定義 dir 基準の参照を、注入先 consumer の spec dir 基準へ変換して合流させる ( 以降の全 pass は無変更で literal edge として扱う)
- **dedup**: consumer が同じ edge を手書き宣言済みの場合は注入しない ( 既存 spec と共存する後方互換の要)。注入済み edge と手書き edge は depgraph / スケジューリング / graph 表示では区別しない。ただし「未観測 depends 警告」( D3 の inputs omission check) では**注入済み edge を除外**する: tool 由来の edge の無効化は `resolved_versions` ( ADR-0013 D4 / D7) を経由するため、ファイル overlap 不在の警告は原理的に誤りになる。同じ edge を手書きした場合は注入が skip されるため手書き edge には警告が適用される
- **自己 edge は error**: 注入先 task 自身が参照先になる場合 ( 「T の source を生成する task P が T を使っている」) は、bootstrap が構造的に不可能な spec なので tool 名を主語に error にする。silent skip にしない ( skip すると順序保証が静かに消える)。この error は「生成物を import する generator を、閉包 producer と同一 task で回している」構造矛盾の検出であり、修正は task 分割 ( 閉包 producer を tool 非依存の task に切り出す) になる
- **barrier は免罪符にならない**: `depends` に barrier ( ADR-0017) を書くことは可能だが、ADR-0017 D3 のとおり consumer が実 producer の生成物を読む ( = tool 閉包に入る) 場合の直接 edge 要求は免除されない。go-local tool の閉包 producer は実 task を直接列挙すること
- **`sloff graph` / Plan にも同様に注入する**。tool 由来の順序制約が graph に現れることは改善であり、Run と Plan で DAG が食い違わないことは決定性の要件

### D3. 解決モデル: eager 維持 + 宣言 tool のみ遅延へ降格

run 冒頭の一括解決 ( `resolveContribs`) は維持する。per-tool の解決失敗時:

- **`depends` を宣言していない tool**: 従来どおり run 全体を fatal ( 挙動変化なし。typo・環境不備は即死のまま)
- **`depends` を宣言した tool**: WARN を出して **deferred** 状態に降格し、run を続行する。当該 tool の Inputs / Versions の contribution は暫定的に空として collectTasks を通す

ただし、`context.Canceled` / `context.DeadlineExceeded` による失敗は **宣言の有無によらず demote しない**。context キャンセルは「tool のソースがまだ生成されていない」ことと無関係であり、run は既にシャットダウン中である。demote して retry させても同じ context error で再失敗するだけなので、従来どおり即時失敗として伝播させる。

解決成功時は deferred への降格が発生せず、**現行コードパスと完全同一**である ( warm-path 無劣化)。

### D4. 遅延解決の実行点と失敗の帰属

deferred tool の再解決は **その tool を参照する task の `runTask` 冒頭**で行う ( tool 単位の singleflight。複数 consumer が並行到達しても解決は 1 回)。

- D2 の注入 edge により、consumer task の開始時点で tool の宣言 depends は完了済みであることが scheduler により保証されている
- 再解決成功 → その task の input set / versions に contribution を fold し、**exec 時の完全な入力集合から input hash を計算**する。この hash は warm run と同一 key に収束するため、cold で書いた record は warm で hit する ( cache の連続性)
- 再解決失敗 → その task を fail にする。error は tool を主語に、plan 時と exec 時双方の原因を併記する。SKIP 判定にも input hash ( = 解決結果) が必要なため、遅延解決が silent に握り潰される経路は存在しない — 同一 run 内で必ず顕在化する
- **DAG は run 中不変**。遅延解決の結果が edge を追加・並べ替えることはない。宣言不足 ( 閉包 producer への edge 欠落) は誤順序を「修復」せず、run-time overlap 検証 ( ADR-0013 D3) が遅延解決後の input surface で検出し、追加すべき depends を指して fail する。順序 = 宣言 / 健全性 = 検証、の分離は tool 解決でも維持される
- 遅延解決は fingerprint hit による SKIP を妨げない ( 解決さえ成功すれば、outputs が無傷の task は cold run 中でも hit → SKIP しうる)

### D5. prefetch は deferred consumer を除外する

deferred tool を参照する task は、tool contribution 抜きの optimistic key を計算しても保存済み record と一致し得ないため、prefetch の対象から除外する ( `prefetchedKeys` に登録しない)。当該 task の lookup は既存の live-Load fallback ( ADR-0011 で導入済みの経路) に落ちる。コストは cold run 限定で、cold は大半が RUN のため実害は小さい。

### D6. prefetch の missing-file 耐性

optimistic key 計算中の入力 file 不在 ( `fs.ErrNotExist`) は run の fatal ではなく、**当該 task の prefetch 除外**とする ( D5 と同じ fallback へ)。

- 動機: pnpm-local の ExtraInputs は git 基準で列挙されるため、「tracked だが worktree から削除済み」の file を含みうる。当該 file は consumer の宣言 depends が exec までに再生成する
- 「absent を hash に織り込む」案は採らない: record は常に exec 時 ( 全 file 存在) に書かれるため、absent 込みの key はいかなる record とも一致せず、無意味な lookup を増やすだけである
- `runTask` の exec 時 hash は従来どおり **strict** ( file 不在は error)。健全性の防衛線は動かさない

**除外判定の決定性**: task の入力 hash 計算が複数 file で失敗する場合、除外は「**全ての失敗が ErrNotExist のときのみ**」とする。非 ErrNotExist エラー ( 例: permission denied) が 1 つでも混在する場合は、そのエラーを入力順で最初のものを決定的に返し、prefetch を fatal にする。これにより、missing file と unreadable file を同時に入力に持つ task が scheduling 順によって「除外 (silent) → mid-run fatal」と「prefetch fatal」の間で揺れる非決定的挙動を排除する。

### D7. fingerprint 意味論は不変

- 注入 edge にも ADR-0013 D4 ( depends は input_hash に不参加) がそのまま適用される
- record schema・hit 判定 ( ADR-0002)・write-skip ( ADR-0009) に変更なし
- deferred → resolved で計算される hash は、warm run で eager に解決した場合と bit 単位で一致する ( 同じ resolver が同じ結果を返すため)。cold / warm で cache が連続することの根拠

### D8. 互換性

追加的変更であり、既存 spec は無変更で従来と同一の挙動をする ( `depends` 無し tool の解決失敗は fatal のまま)。command provider の出力 schema ( ADR-0015) は tool を emit しないため影響なし。

## Consequences

### 正の影響

- 生成物ゼロの cold state から `sloff run` 一発で成功に到達できる。orchestrator の実行前提が orchestrator 自身の DAG で完結する
- tool → task の bootstrap 依存が tool 定義に一元化される。consumer ごとに複製されていた producer リストは注入で代替され、段階的に削除できる
- `sloff graph` が tool 由来の順序制約を表示できるようになり、cold tree でも graph が壊れない

### 負の影響 / 注意点

- 解決が「eager / deferred」の 2 相になり、runner に遅延状態 ( deferred tool の管理・taskInfo の exec 時再構成) が増える
- deferred の間、plan-time の overlap 検証 ( ADR-0013 D3 前半) は tool contribution を見ない。これは劣化ではなく、clean state では plan-time 検証が元々無力で run-time 側が防衛線である、という ADR-0013 の既定の分担どおり
- `depends` を宣言した tool の解決エラーは task 実行時まで顕在化が遅れる ( 宣言による opt-in であり、未宣言 tool は従来どおり即死)
- tool の閉包が変わったとき ( 新しい生成 package を import し始めた等) の `depends` 追従は利用者の責任。追従漏れは cold run の遅延解決失敗 / run-time D3 error として検出される ( silent stale には到達しない)

### 撤回時の影響

`tools.<name>.depends` を各 consumer task の `depends` へ機械展開すれば従来構成に戻る ( 注入と逆向きの書き換え)。fingerprint record には何も書いていないため storage への影響は無い。

### 後続の更新

1. [Design Doc: architecture.md](../design/architecture.md): tool 定義 / 解決フェーズに `depends`・遅延解決を追記
2. [resolver-go-local.md](../design/resolver-go-local.md): 「生成物を import する tool」の bootstrap パターンを追記
3. `internal/sloff/spec`: `DeclaredTool.Depends` の parse と D1 の load 時 validation
4. `internal/sloff/runner`: D2 注入 phase / D3 deferred 降格 / D4 遅延解決 ( singleflight) / D5-D6 prefetch 除外
5. E2E test: cold bootstrap 一発成功 ( go-local) / 未宣言 tool の即死維持 / 宣言 tool の遅延解決失敗の帰属 / 注入 edge の dedup・自己 edge error / prefetch 除外と live fallback / graph の cold 成立
6. [ADR-0008](./0008-tool-as-first-class-spec-entity.md) の Status に Amended by 追記 ( tool 定義 shape への `depends` 追加)
