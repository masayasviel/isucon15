# スロークエリログからのIssue作成

対象: `home/isucon/analysis/db/slow-query-dump.log`（`pt-query-digest` の出力）

## 1. ログの読み取り

1. 先頭の `# Profile` 表を読み、`Rank` 昇順（= `Response time` 降順）に処理する。`MISC` 行は対象外。
2. 各Rankについて、対応する `# Query N:` ブロックから以下を抜き出す。
   - Query ID、Response time（合計秒と割合）、Calls、R/Call
   - `Exec time` の avg / 95%、`Rows examine` の avg、`Rows sent` の avg
   - `# Tables` に出るテーブル名と、末尾のSQL
3. `Rows examine avg` と `Rows sent avg` の比を確認し、走査過多（インデックス不足・全件集計）か、呼び出し回数過多（N+1・ポーリング）かを判断する。

## 2. 発行元の特定

1. SQLの特徴的な断片（テーブル名、`WHERE` 句、`ORDER BY` 句）で `home/isucon/webapp/go/*.go` を検索し、発行している関数とエンドポイントを特定する。
2. `home/isucon/webapp/sql/1-schema.sql` で対象テーブルの主キー・インデックスを確認する。
3. 発行元を特定できない場合は実装Issueにせず、調査Issueにする。

## 3. Issue化の判断

- 原則として1 Rank = 1 Issue。Rankの番号順に作成し、Issue番号の順序とRankを一致させる。
- 同じ1回の変更（例: 同一インデックス追加）で解消する複数Rankは1 Issueにまとめ、タイトルには最上位Rankを書き、本文に全Rankを列挙する。
- `COMMIT` や `administrator command: Prepare` などSQL本体ではない項目は、原因（トランザクションの範囲、サーバーサイドプリペアドステートメント等）を実装・設定から確認できた場合だけ実装Issueにする。確認できなければ調査Issueにする。
- 作成前に `gh issue list --state all --search "<テーブル名 or 関数名>"` と `git log --oneline` で既存Issue・既存修正（revert含む）を確認し、重複は作らない。既存Issueがあれば番号を報告し、必要なら追記案を出す。
- 既定ではProfile表に載った全Rank（MISC除く）を対象にする。ユーザーが件数や範囲を指定した場合はそれに従う。

## 4. 本文の書き方

`references/issue-template.md` の構成に加え、背景に以下を入れる。

````md
## 背景

スロークエリログ（pt-query-digest）の Rank N。

| 項目 | 値 |
| --- | --- |
| Query ID | 0x... |
| Response time | 206.4s (38.3%) |
| Calls | 87 |
| R/Call | 2.37s |
| Rows examine (avg) | 527 |
| Rows sent (avg) | 5.5 |

```sql
-- リテラルは ? に置き換える
SELECT ... WHERE owner_id = ?
```

- 発行元: `home/isucon/webapp/go/owner_handlers.go` の `ownerGetChairs`（`GET /api/owner/chairs`）
- 推定原因: <事実と仮説を分けて書く>
````

- タイトル: `perf: <対象（エンドポイント/関数）>の<改善内容>（slow query Rank N）`
- 受け入れ条件: 対象SQLの `Rows examine` 削減やクエリ形の変更など、コードやEXPLAINで観測できるものにする。レスポンス仕様は維持する。
- 確認方法: ベンチマークと再計測はこの環境では行わないため、「デプロイ後に pt-query-digest を再実行し、対象Query IDの Response time / Rank を比較する」を後続作業として書く。
- SQL中のID・アクセストークン等のリテラル、`Hosts` のIPアドレス、ホスト名は本文に含めない。
