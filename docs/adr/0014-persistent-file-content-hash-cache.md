# ADR-0014: ファイル内容ダイジェストの run またぎ永続キャッシュ

## Status

Accepted

## Context

### 背景

[ADR-0002](./0002-fingerprint-hit-decision-model.md) で fingerprint hit 判定は
**input_hash による record 引き当て + output-comparison** と確定している。
input_hash の主要素は各タスクの input ファイルの **content digest** であり、
sloff は run 開始時の prefetch でほぼ全タスクの input ファイルをハッシュして
optimistic な input_hash を組み立てる。

setup phase (resolve → collect → prefetch) のプロファイルで、この prefetch の
ハッシュ計算が無視できないコストであることが分かった (実測: 約 300 タスク /
約 30,000 input パス / 約 545ms)。`hash.FileCache` (filecache.go) は同一 run 内では
(size, mtime) をキーに同じファイルを 2 度ハッシュしないよう memoise するが、
**run をまたいでは再利用しない**。そのため「proto を 1 つ変えただけ」のような
incremental な warm run でも、変化していない約 30,000 ファイルを毎回ゼロから
再ハッシュしている。

並列化・フェーズ統合 ([ADR でない範囲の最適化] resolve の単一パス化、glob の
base 単一 walk 化) はやり切っており、setup をさらに縮めるには
**「変化検出して warm run では再ハッシュをスキップする」永続キャッシュ**が必要、
という結論に至った。本 ADR はその方式を確定する。

### 制約 / 評価軸

- **R1 (warm 高速化)**: 内容が変わっていない input は再ハッシュしない
- **R2 (正確性・最重要)**: stale な digest を返してはならない。後述のとおり
  誤りは **非対称** で、「余計に RUN」(無害) ではなく「誤って SKIP」(stale 生成)
  に倒れる
- **R3 (cheap な変化検出)**: 変化検出自体がハッシュより十分安いこと (stat 程度)
- **R4 (ネットワーク / 外部 infra 不要)**: ADR-0003 と同じくローカル完結
- **R5 (run またぎの窓に耐える)**: 同一 run 内 (数秒) ではなく日単位で窓が開く
  ため、mtime を保存する操作 (rsync `--times` / `tar -x` / `cp -p` / バックアップ
  復元) や粗い mtime 解像度・clock 巻き戻しに耐える必要がある

### 正確性リスクの非対称性 (R2 の核心)

input ファイルの digest を **stale に返す**と次が起きる:

1. ファイル内容は変わったのに、永続キャッシュが古い digest を返す
2. その古い digest で組んだ input_hash が**過去の record に誤マッチ**する
3. その record の output_hash は「古い input から生成した古い output」のもの
4. タスクは未実行なので**現在の output = 古い output** であり、output-comparison
   (ADR-0002) は **古い output と古い record が一致して通る**
5. → タスクが **誤って SKIP** され、変わった input に対する出力が再生成されない
   = **サイレントな stale 生成**

つまり **output-comparison はこの種の誤りを救えない**。ゆえに input ファイルの
変化検出 (invalidation) は、ハッシュ計算を省く以上、極めて堅牢でなければならない。
逆向きの誤り (実際は不変なのに stale 判定 → 余計に再ハッシュ + 余計に RUN) は
無害なので、判定は「迷ったら dirty (再ハッシュ)」に倒すのが正しい。

### References

- [ADR-0002: fingerprint hit 判定モデル (output-comparison)](./0002-fingerprint-hit-decision-model.md)
- [ADR-0003: fingerprint のストレージ方式 (per-machine cache 層 / CacheRoot)](./0003-fingerprint-storage-strategy.md)
- `internal/sloff/hash/filecache.go` (within-run の FileCache)
- Git index の racy-clean 判定 (size + mtime + ctime + inode によるエントリ妥当性検査)

## Considered Options

### Comparison Table

| | A: (size, mtime) で永続化 | **B: (size, mtime, ctime, inode) + racy guard で永続化 (採用)** | C: 永続化しない (現状) | D: stat を使わず毎回フルハッシュ |
|---|---|---|---|---|
| warm 高速化 (R1) | ◎ | ◎ | × | × |
| 正確性 / 非対称リスク (R2) | △ (mtime 保存系で誤 SKIP の窓) | ◎ (ctime が保存系を捕捉) | ◎ (毎回実測) | ◎ |
| 変化検出コスト (R3) | ◎ (stat) | ◎ (stat) | — | × (常に read+SHA) |
| ネットワーク不要 (R4) | ◎ | ◎ | ◎ | ◎ |
| run またぎ堅牢性 (R5) | △ | ◎ | ◎ | ◎ |
| 実装複雑度 | 低 | 中 | ゼロ | ゼロ |

### Option A: (size, mtime) で永続化

within-run FileCache の (size, mtime) キーをそのままディスク永続化する。

👍 **Pros**

- 実装が最も単純 (既存キーをそのまま保存するだけ)
- 普通の編集 / `git checkout` (差分ファイルの mtime を now に更新) では正しく invalidate される

👎 **Cons**

- **run またぎで窓が日単位に開くと、mtime を保存する操作で誤 SKIP が起きうる**:
  `rsync --times` / `tar -x` (mtime 保持) / `cp -p` / バックアップ復元 / Docker の
  mtime 持ち込みで「内容が違うのに (size, mtime) が過去のキャッシュと一致」しうる
- 粗い mtime 解像度の FS (一部 NFS / 古い FS / 一部 Docker volume) や clock 巻き戻しで
  同様の誤一致が起きる
- 上記はいずれも R2 の非対称リスク (誤 SKIP → stale 生成) に直結する

### Option B: (size, mtime, ctime, inode) + racy guard で永続化 (採用)

キーに **ctime と inode** を加える。Git の index が racy-clean 判定で使うのと同じ軸。

👍 **Pros**

- **ctime (inode 変更時刻) は `rsync --times` / `tar -x` / `cp -p` でも「コピー時刻 = now」に
  必ず更新される** (mtime は保存できても ctime は userspace から保存できない)。
  Option A が取りこぼす保存系操作をこの 1 軸で捕捉できる
- inode を加えると削除 → 再作成の取り違えも防げる
- racy guard (**観測 (= run 開始) 時刻**から見て mtime **または** ctime が
  racyMargin 以内 = まだ settle していないファイルは保存しない = dirty 扱い) で
  「同一解像度内の書込みレース」も保守的に再ハッシュへ倒す。基準は run 開始時刻に
  固定し、**保存時刻 (Save は run 終了時に遅延実行) は使わない**。長い run では
  「ハッシュ直後に同 tick で書き換わったファイル」も save 時には racyMargin を
  超えて settled に見えてしまい、stale を保存しうるため。また ctime も対象にするのが
  要点で、mtime 保存系操作では ctime だけが新鮮なため、mtime のみ見ると粗い ctime
  解像度の FS で「ハッシュ直後の mtime 保存書き換え」を取りこぼし stale を返しうる
- 変化検出は stat 1 回 (~µs) で、read+SHA (~十µs〜) より十分安い (R3)

👎 **Cons**

- ctime / inode の取得は platform 依存 (`syscall.Stat_t` の `Ctim`/`Ctimespec`, `Ino`)。
  Unix 系 (darwin / linux) は取得可能。それ以外の platform は (size, mtime) に degrade
  するか、キャッシュを無効化する fallback が必要
- 残リスクは「ctime も巻き戻る」極端ケース (clock 改ざん / FS レベルの細工) のみで、
  実用上無視できる
- 永続ストアの load/save・format・サイズ管理が新規に必要

### Option C: 永続化しない (現状)

within-run のみ。warm run でも全 input を毎回ハッシュ。

👍 **Pros**: 追加実装ゼロ・正確性は実測なので最も堅い
👎 **Cons**: setup 高速化という本 ADR の主目的を満たさない (R1 ×)

### Option D: stat を使わず毎回フルハッシュ

変化検出を諦め、常に read+SHA。

👍 **Pros**: stat heuristic の残リスクすら無い
👎 **Cons**: 最も遅い。現状 (C, within-run cache あり) より遅くなるため論外

## Decision

**Option B: (size, mtime, ctime, inode) + racy guard による per-machine 永続キャッシュを採用する。**

採用根拠:

1. **C / D は R1 (warm 高速化) を満たさない** ため除外。本 ADR の動機そのものを満たさない
2. **残った A vs B は invalidation の堅牢性の差**。R2 (誤りの非対称性 = 誤 SKIP は
   output-comparison でも救えない stale 生成) を踏まえると、run またぎ (日単位) の窓では
   mtime 保存系操作・粗い解像度・clock 巻き戻しに耐える必要がある (R5)
3. **ctime はそれらの保存系操作で必ず更新され、userspace から保存できない**。
   inode と合わせることで、Git index の racy-clean 判定と同等の枯れた堅牢性が得られる。
   B は A の残リスクを実用上ゼロまで縮める
4. 変化検出は stat 1 回で済み、ハッシュ計算より十分安い (R3) ため、warm run では
   「変わったファイルだけ再ハッシュ」になり目的の高速化を達成する
5. **per-machine に閉じる** (mtime / ctime / inode はマシン依存): 配置は ADR-0003 の
   `CacheRoot` (XDG_CACHE_HOME 配下) を流用し、git にもコミットしない。共有しないことで
   マシン間の stat 不一致 (= 大量 miss or 誤一致) を構造的に避ける

判定シーケンス (digest 取得時):

- stat して (size, mtime, ctime, inode) を得る
- 永続キャッシュにキー一致エントリがあり、かつ racy guard に抵触しなければ → 保存 digest を返す
- それ以外は read+SHA で再計算し、(read 後の re-stat で identity が安定していれば) キャッシュ更新
- 迷ったら必ず dirty (再ハッシュ) に倒す = R2 を満たす安全側

format / 無効化:

- **on-disk フォーマットは protobuf** (`proto/sloff/filecache/v1/filecache.proto` の
  `Cache` メッセージ) とする。fingerprint record ([ADR-0009](./0009-fingerprint-binary-serialization.md))
  と同じ方式に揃え、`gob` のような Go 専用・スキーマレスな表現を避ける
  (スキーマの明示・後方互換の管理・将来の言語/ツール跨ぎの読み取りがしやすい)
- バージョンは **closed enum** (`SchemaVersion`) で持たせ、sloff 側の on-disk スキーマ
  または digest 規約変更時はキャッシュ全体を invalidate (cold 扱い) する。
  fingerprint と同様、互換性のないキャッシュは読み飛ばして再ハッシュに倒せる
- エントリは marshal 前に path でソートし `Deterministic: true` で書き出す。
  content-addressed ではない (削除しても安全な per-machine キャッシュ) ため byte 一致は
  正確性要件ではないが、再現性とデバッグ時の差分の取りやすさのために決定的に書く
- 不安時の **escape hatch** として `SLOFF_NO_FILE_HASH_CACHE` env を用意し、疑わしければ
  常に「実測 (C 相当)」に戻せるようにする。boolean として解釈し
  (`1`/`true` で永続キャッシュ無効、`0`/`false`/未設定で有効、解釈できない値は即エラー)、
  `SLOFF_ALLOW_STALE_DEPS` と同じ厳格パースに揃える。有効時は永続ストアを読み書きせず
  within-run の memoise のみで走る。`--force` ([ADR-0012](./0012-force-rerun-flag.md)) は
  fingerprint hit の bypass であって digest 自体はこのキャッシュ経由で計算されるため、
  キャッシュ汚染からの復旧手段にはならない (責務が異なる) 点に注意

## Consequences

### 正の影響

- warm run の prefetch ハッシュが「全 input 再ハッシュ」から「変わった input だけ再ハッシュ +
  残りは stat のみ」になり、setup phase が短縮される (実測想定: prefetch の
  optimisticKeys ~545ms → ~数十 ms)
- within-run FileCache が既に (size, mtime) heuristic を採用しており、その自然な
  「run またぎ延伸 + ctime/inode 強化」であって、新たな正確性モデルを持ち込まない

### 負の影響

- 永続ストアの load / save・format version・サイズ増大の管理 (GC / 上限) が新規に必要。
  ADR-0003 の fingerprint cache と同じ per-machine ディレクトリ配下に置き、肥大時は
  同様の sweep 戦略で対応する
- ctime / inode 取得が platform 依存。Unix 系を一級サポートとし、非対応 platform は
  (size, mtime) degrade または無効化で fallback する
- cold run (初回 / 変更後) は従来どおりフルハッシュ。高速化はあくまで warm run の効果
- 残リスク (ctime 巻き戻しの極端ケース) は受容する。受容できない利用者向けに env で
  無効化できる escape hatch を残す

### 後続の詳細設計

- `internal/sloff/hash/filecache.go` の永続化 (キー拡張 + load/save) と、
  `cmd/sloff` での起動時 load / 終了時 save の配線で実装する
- 出力バイト一致テスト (cold / warm 両方) と、「内容変更で digest 更新」
  「(size, mtime) 据え置きでも ctime 変化で再ハッシュ」「racy-clean ガード」の
  ユニットテストで R2 を担保する
