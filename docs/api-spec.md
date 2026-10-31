# ISUREAL API 仕様書

`home/isucon/webapp/go` の Go 実装から読み取った、全 **42 エンドポイント**の仕様。
エンドポイントごとに「処理内容 / リクエスト / 処理の流れ / レスポンス / エラー」を記載する。

- 実装のソース: `home/isucon/webapp/go/*.go`
- ルーティング定義の正: `server.go`
- スキーマの正: `home/isucon/webapp/sql/001-schema.sql`

---

## 目次

- [第1部 共通仕様](#第1部-共通仕様)
  - [1.1 配置とベースURL](#11-配置とベースurl)
  - [1.2 ミドルウェアチェーン](#12-ミドルウェアチェーン)
  - [1.3 認証](#13-認証)
  - [1.4 リージョン](#14-リージョン)
  - [1.5 エラーレスポンス](#15-エラーレスポンス)
  - [1.6 ページネーション規約](#16-ページネーション規約)
  - [1.7 画像と署名付きURL](#17-画像と署名付きurl)
  - [1.8 共通レスポンス型](#18-共通レスポンス型)
  - [1.9 共通判定ロジック](#19-共通判定ロジック)
  - [1.10 入力バリデーション一覧](#110-入力バリデーション一覧)
- [第2部 エンドポイント詳細](#第2部-エンドポイント詳細)
  - [2.1 システム](#21-システム)
  - [2.2 認証](#22-認証)
  - [2.3 自分のアカウント](#23-自分のアカウント)
  - [2.4 ユーザー参照](#24-ユーザー参照)
  - [2.5 投稿枠](#25-投稿枠)
  - [2.6 投稿](#26-投稿)
  - [2.7 フィード](#27-フィード)
  - [2.8 メモリー](#28-メモリー)
  - [2.9 コメント](#29-コメント)
  - [2.10 RealMoji](#210-realmoji)
  - [2.11 画像](#211-画像)
  - [2.12 通知](#212-通知)
  - [2.13 関係性](#213-関係性)
  - [2.14 管理者](#214-管理者)
- [第3部 付録](#第3部-付録)
  - [3.1 エンドポイント一覧](#31-エンドポイント一覧)
  - [3.2 エラーコード索引](#32-エラーコード索引)
  - [3.3 ファイル対応表](#33-ファイル対応表)
  - [3.4 実装上の注意点・引っかかりどころ](#34-実装上の注意点引っかかりどころ)

---

# 第1部 共通仕様

## 1.1 配置とベースURL

```
クライアント
   │ :80
   ▼
nginx  (etc/nginx/sites-available/isureal.conf)
   ├─ location /api/  → proxy_pass http://127.0.0.1:8080   （アプリへ）
   └─ location /      → try_files $uri /index.html         （SPA 静的配信）
   client_max_body_size 32m
   │ :8080
   ▼
Go アプリ  echo v5 + sqlx + pgx    (cmd/isureal/main.go → server.go)
   │ :5432
   ▼
PostgreSQL 16  @ ISUREAL_DB_HOST（env.sh では別ホスト）
```

全 API のパスは `/api/` 配下。アプリは `:8080` を直接 listen する。

## 1.2 ミドルウェアチェーン

`server.go` の `NewServer` で以下の順に適用される。

```
① IPExtractor       … X-Client-IP ヘッダがあればそれを、無ければ接続元IPを c.RealIP() とする
                       (ip_ban.go: extractClientIP)
② ActionLogMiddleware… ハンドラ実行後に JSON 1行ログを stdout へ出力
③ middleware.Recover … panic を 500 に変換
④ IPBanMiddleware   … ip_bans を照合（200ms キャッシュ）→ 該当すれば 403 ip_banned
⑤ SessionMiddleware  … ルートごとに付与。Bearer トークンを検証
⑥ RegionMiddleware   … ルートごとに付与。セッションのリージョンとサーバのリージョンを照合
⑦ ハンドラ
```

`HTTPErrorHandler` は `APIErrorHandler`（`api_error.go`）。

### ActionLogMiddleware のログ形式

```json
{"level":"info","action":"create_post","status":201,"ip":"10.0.0.1","user_id":"…"}
```

| フィールド | 内容 |
|---|---|
| `level` | `status >= 500` → `error` / GET かつ「403+`abuse=invalid_signature`」でない → `debug` / それ以外 → `info` |
| `action` | `action_log.go` の `actionNames` によるルート名（未登録なら `null`） |
| `status` | HTTP ステータス |
| `ip` | `c.RealIP()` |
| `user_id` | セッションがあればユーザーID、無ければ `null` |
| `username` | `login` / `register` のときのみ追加（パターン不一致なら `null`） |
| `abuse` | ハンドラが `c.Set("abuse", …)` した場合のみ追加 |

`ISUREAL_LOG_LEVEL`（既定 `debug`）より低いレベルのログは出力されない。

> **運用上の注意**: `isureal-guard.service` が journald 上のこのログを監視し、**10秒間に10回エラーを出した IP を 2分間 BAN**（`ip_bans` へ INSERT）する。500 を出し続けるとクライアント側が疎通不能になる。

## 1.3 認証

### ユーザー認証（`session.go`）

```
Authorization: Bearer <token>
        │
        ▼
 SHA256(token) = token_hash
        │
        ▼
 SELECT s.user_id, s.token_hash, s.created_at, u.region
 FROM login_sessions s JOIN users u ON u.id = s.user_id
 WHERE s.token_hash = $1 AND s.active = true AND u.status = 'active'
```

- ヘッダが無い → セッション無し扱い（`authRequired` なら 401 `invalid_session`）
- `Bearer ` 以外のプレフィックス → **401 `invalid_session`**
- 該当行なし → セッション無し扱い（401 `invalid_session`）
- トークンは平文では保存されず、`login_sessions.token_hash` のみ保持
- **ログイン時に同一ユーザーの既存アクティブセッションは全て無効化される**（`reason = 'replaced'`）。つまり**同時ログインは1セッションのみ**

### 管理者認証（`admin_session.go`）

同形式（`Authorization: Bearer <token>`）だが `administrator_sessions` を参照。失敗時のコードは **`invalid_administrator_session`**（401）。ユーザーセッションとは完全に別系統。

- 管理者ログイン時は既存セッションを無効化**しない**（複数セッション可）

## 1.4 リージョン

ユーザーは `america` / `europe` / `asia` のいずれかに属する（`users.region`）。
サーバ側も `ISUREAL_REGION` 環境変数で1リージョンに固定される（`config.go`、既定 `asia`）。不正値ならプロセス起動時に panic（`server.go`）。

```
Config.Region（サーバ）     session.Region（= users.region）
        │                            │
        └────── RegionMiddleware ─────┘
             不一致なら 403 region_mismatch
```

### リージョン判定が「無い」エンドポイント

以下は `RegionMiddleware` が付いておらず、**どのリージョンのサーバでも処理できる**。

| エンドポイント | 備考 |
|---|---|
| `POST /api/initialize` | 認証もなし |
| `POST /api/auth/register` | 登録時にリージョンを選ぶ |
| `POST /api/auth/login` | |
| `POST /api/auth/logout` | 認証のみ |
| `PATCH /api/me` | **リージョン変更が可能**（下記） |
| `PUT /api/me/profile-image` | |
| `DELETE /api/me/profile-image` | |
| `GET /api/images/:imageId` | 認証すらなし。署名で保護 |
| `POST /api/admin/*`, `PUT/DELETE /api/admin/*` | 管理者認証のみ |

### リージョンが効いてくる箇所

| 箇所 | 内容 |
|---|---|
| `RegionMiddleware` | 上記以外の全エンドポイントで一致必須 |
| `posting_windows` | `UNIQUE(region, local_date)` ＋ `ended_at IS NULL` の行はリージョンごとに**最大1件**（部分ユニークインデックス `posting_windows_one_active_region`） |
| 投稿枠の取得 | `WHERE region = <ユーザーの region> AND ended_at IS NULL`（`post_api.go`, `posting_window_api.go`） |
| 枠オープン | 管理者が `region` を指定して開く。`notifications` はそのリージョンの active ユーザー全員に INSERT |
| `HasPostedToday` 判定 | 「自分の region で現在開いている枠」と同じ `local_date` の枠に投稿があるか（`post.go`） |
| SSE 通知 | 接続中に `users.region` がセッション時のリージョンと変わったら接続を閉じる（`notification_api.go`） |
| `PATCH /api/me` | `region` を変更できる。変更後はそのユーザーは別リージョンのサーバでしか API を使えなくなる |

> **構造的な意味**: リージョンごとに `posting_windows` が独立しており、全エンドポイントがリージョン一致を要求するため、**ユーザー単位でのデータ分割（シャーディング）が仕様として成立している**。ただし `users` / `posts` / `friendships` などのテーブル自体はリージョンで分かれていないため、「友だち」や「フィード」はリージョンを跨いだ関係を持ちうる。

## 1.5 エラーレスポンス

全エラーは `api_error.go` の `APIErrorHandler` が以下の形で返す。

```json
{ "error": "<エラーコード>" }
```

- `*APIError` → その `Status` と `Code`
- `echo.HTTPStatusCoder`（Bind エラー等）→ そのステータス、メッセージは `err.Error()` の文字列
- それ以外 → **500** ＋ `err.Error()` の文字列（かつ `c.Logger().Error` で記録）
- レスポンス送信済み（`Committed`）なら何もしない

全コードの索引は [3.2 エラーコード索引](#32-エラーコード索引)。

## 1.6 ページネーション規約

一覧系はすべて **「50件 + `hasMore` フラグ」** のカーソル方式。内部では 51 件取得し、51件目が存在すれば `hasMore: true` にして 50 件だけ返す。

```json
{ "items": [ … ], "hasMore": true }
```

カーソルパラメータは**ペアで指定しないと 400 `invalid_pagination`**（片方だけ指定は不可）。

| エンドポイント | カーソル | 並び順 | SQL側 LIMIT |
|---|---|---|---|
| `GET /api/feeds/:feed`（通常） | `beforePostedAt` + `beforeId` | `posted_at DESC, id DESC` | **なし**（Go 側で打ち切り） |
| `GET /api/feeds/friends-of-friends` | `beforeScore` + `beforePostedAt` + `beforeId` | `score DESC, posted_at DESC, id DESC` | **なし**（SQLカーソルも無効化） |
| `GET /api/memories`, `/api/memories/:date` | `beforePostedAt` + `beforeId` | `posted_at DESC, id DESC` | 51 |
| `GET /api/relationships/:kind` | `beforeUpdatedAt` + `beforeId` | `updated_at DESC, id DESC` | 51 |
| `GET /api/posts/:postId/realmojis` | `beforeCreatedAt` + `beforeId` | `created_at DESC, id DESC` | 51 |
| `GET /api/posts/:postId/comments` | `afterCreatedAt` + `afterId` | **`created_at ASC, id ASC`** | 51 |
| `GET /api/users` | `beforeUsername` + `beforeId` | **`username ASC, id ASC`** | **なし** |

> **注意**: `GET /api/users` のパラメータ名は `beforeUsername` だが、SQL は `(username, id) > ($2, $3)` の**昇順・次ページ送り**。名前と意味が逆。
> `GET /api/posts/:postId/comments` だけ `after*` で昇順（古い順）。

## 1.7 画像と署名付きURL

画像バイナリは DB の `image_objects.data`（`bytea`）に格納され、`created_at` で月次パーティションされている。

> `001-schema.sql` に「画像をDBにおかないと、別のサーバーからデータを返却したときに404になってしまう。ファイル配置に変更して障害になってしまったので注意。**ここは決して変更しないこと**」という注記がある。

### URL 生成（`image_signature.go`）

```
/api/images/<imageId>?exp=<unix秒>&sig=<base64url>

鍵  key = users.secret_key_hash を hex デコードした 32 バイト（= 画像の所有者の鍵）
exp = now + 24時間
sig = HMAC-SHA256 を 1000 回反復した結果を base64url（パディングなし）

  value = "<exp>:<imageId>"
  1000回:  value = HMAC-SHA256(key, value)
  sig = base64url(value)
```

レスポンスに画像URLを含める都度、この 1000 回反復 HMAC が実行される。

### 検証（`image_api.go`）

`GET /api/images/:imageId` は認証不要だが、以下を全て満たさないと 403 `image_forbidden`。

1. クエリに `exp` と `sig` がそれぞれ**ちょうど1個**
2. `exp` が正準10進表記で、かつ `exp > now`
3. `sig` が strict base64url かつ正準表記、デコード長が 32 バイト
4. `image_objects` に該当 ID があり、所有者が `status = 'active'`
5. HMAC が一致（不一致時は `abuse = "invalid_signature"` をログに残す）
6. その画像が**今も現役**であること（下記いずれか）
   - `users.profile_image_id = i.id`（プロフィール画像）
   - 生存している `posts.image_id = i.id`（`deleted_at IS NULL`）
   - 現役 `realmojis.image_id = i.id`（`replaced_at IS NULL AND deleted_at IS NULL`、かつ対象投稿が生存・投稿者が active）
7. 再度 `exp > now`（検証処理中の経過を考慮）

成功時は `Content-Type: <media_type>` で生バイナリ。**常に `Cache-Control: no-store`** が付く。

## 1.8 共通レスポンス型

### `sessionUserResponse`（自分の情報）

```json
{
  "id": "uuid",
  "username": "alice",
  "displayName": "Alice",
  "bio": "...",
  "region": "asia",
  "accountType": "regular",
  "status": "active"
}
```

- `accountType`: `regular` | `certified_public`
- `status`: `active` | `deleted`

### `relationshipUserResponse`（他人の情報）

```json
{
  "id": "uuid",
  "username": "bob",
  "displayName": "Bob",
  "profileImageUrl": "/api/images/…?exp=…&sig=…",
  "bio": "",
  "accountType": "regular"
}
```

- `profileImageUrl` はプロフィール画像が無ければ `null`
- `region` / `status` は含まれない
- **`bio` のマスキング規則**:

| エンドポイント | `bio` を返す条件 |
|---|---|
| `GET /api/me` | 常に返す（自分） |
| `GET /api/users`（検索） | `accountType == "certified_public"` のときのみ。それ以外は `""` |
| `GET /api/users/:userId` | `certified_public` **または** 自分の友だち |
| `GET /api/relationships/:kind` | `certified_public` **または** `kind == "friends"` |
| 投稿・コメント・RealMoji 内の著者情報 | マスキングなし（DB の値をそのまま） |

### `createPostResponse`（投稿の共通表現）

```json
{
  "id": "uuid",
  "author": {
    "id": "uuid", "username": "alice", "displayName": "Alice",
    "profileImageUrl": "/api/images/…", "bio": "...", "accountType": "regular"
  },
  "postingWindowId": "uuid",
  "imageUrl": "/api/images/…?exp=…&sig=…",
  "caption": "lunch",
  "visibility": "friends",
  "location": { "kind": "exact", "latitude": 35.6, "longitude": 139.7 },
  "postedAt": "2026-10-31T03:00:00Z",
  "onTime": true,
  "lateSeconds": 0
}
```

- `location.kind`: `none` | `exact` | `approximate`
  - `none` → `latitude` / `longitude` / `locality` すべて省略
  - `exact` → `latitude` + `longitude`（`locality` 省略）
  - `approximate` → `locality` のみ
- `onTime` = `postedAt <= posting_windows.on_time_deadline`
- `lateSeconds` = 遅れた秒数。`onTime` が true のときは `0`（フィールド自体は常に存在）

この型は以下に埋め込まれて再利用される。

| 派生型 | 追加フィールド | 使用箇所 |
|---|---|---|
| `feedItemResponse` | `score`（int、ランキングフィードのみ。それ以外は省略） | `GET /api/feeds/:feed` |
| `memoryPostResponse` | `realmojis[]`, `comments[]`（各最大50件） | `GET /api/memories*` |
| `exportPostResponse` | `imagePath`（ZIP内パス） | `GET /api/me/export` |

## 1.9 共通判定ロジック

### `viewerContext`（`post.go`: `loadViewerContext`）

多くのエンドポイントが、リクエストごとに閲覧者の関係性スナップショットをメモリに構築する。

```
loadViewerContext(viewerID)
  ① getUser(viewerID)                    … users 1行（status='active' 限定）
  ② HasPostedToday                       … 「自分の region で開催中の枠と同じ local_date の枠に
                                            生存投稿があるか」を EXISTS で判定
  ③ Blocked                              … block_relations から双方向のIDをmapに
  ④ Friends                              … friendships から自分の友だちIDリスト
  ⑤ FriendsOfFriends / MutualFriends     … ④で得たID群を IN 句に入れて friendships を再検索
                                            「友だちの友だち」と「共通友だち数」を集計
```

友だちが0人なら ⑤ はスキップされる。

### `canView`（投稿を閲覧できるか）

```
post.AuthorID == viewer.ID ?                     ─ yes ─▶ true（自分の投稿は常に見える）
         │ no
         ▼
post.WindowEndedAt != NULL ?                     ─ yes ─▶ false（枠が閉じた投稿は見えない）
viewer.HasPostedToday == false ?                 ─ yes ─▶ false（★自分が未投稿なら他人の投稿は見えない）
viewer.Blocked[post.AuthorID] ?                  ─ yes ─▶ false（どちらがブロックしていても）
         │
         ▼ post.Visibility で分岐
  "public"             ─▶ true
  "friends"            ─▶ viewer.Friends[author]
  "friends_of_friends" ─▶ viewer.Friends[author] || viewer.FriendsOfFriends[author]
  その他                ─▶ false
```

### `canComment`（コメントを読み書きできるか / `comment.go`）

`canView` より厳しい。

```
canView(post) == false            ─▶ false
post.WindowEndedAt != NULL        ─▶ false
viewer == post.Author             ─▶ viewer.HasPostedToday を返す
post.Visibility == "public"       ─▶ true
それ以外                           ─▶ viewer.Friends[author]（直接の友だちのみ。
                                     friends_of_friends では不可）
```

**コメントの GET にもこの判定が使われる**ため、「投稿は見えるがコメントは読めない」状態が存在する（例: 友だちの友だちの `friends_of_friends` 投稿）。

### 投稿枠（posting window）のライフサイクル

```
 管理者: POST /api/admin/posting-window {region, localDate, onTimeSeconds}
    │
    ├─ LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE
    ├─ 同 region に local_date >= 指定日 の枠があれば 409 posting_window_conflict
    ├─ startsAt = clock_timestamp()
    ├─ UPDATE posting_windows SET ended_at = startsAt WHERE region=? AND ended_at IS NULL
    │     → 旧枠を閉じる（この瞬間、旧枠の投稿は canView で見えなくなる）
    ├─ INSERT posting_windows (starts_at = startsAt,
    │                          on_time_deadline = startsAt + onTimeSeconds秒)
    └─ INSERT notifications … そのリージョンの status='active' ユーザー全員分
           │
           ▼
      GET /api/notifications (SSE) が拾って配信
```

`ended_at IS NULL` の行はリージョンごとに1件だけ（部分ユニークインデックスで DB が保証）。

### 投稿可否（`canPost`）

```
canPost = (同 local_date の生存投稿が 0 件)
        && (同 local_date の削除済み投稿が 2 件未満)
        && (now >= window.starts_at)
```

つまり **1日1枚**、ただし**削除してやり直せるのは2回まで**（3回目の削除以降は枠を使い切る）。

---

# 第2部 エンドポイント詳細

凡例:

- **認証**: `ユーザー` = `SessionMiddleware(required)` / `管理者` = `AdminSessionMiddleware(required)` / `なし`
- **リージョン**: `要` = `RegionMiddleware` あり（不一致で 403 `region_mismatch`）
- 全エンドポイントに `IPBanMiddleware` が効く（403 `ip_banned`）ので個別には記載しない
- 本文 Bind の失敗は echo のステータス（通常 400）でそのまま返る

---

## 2.1 システム

### `POST /api/initialize`

| 項目 | 内容 |
|---|---|
| 処理内容 | ベンチマーク前の初期化。DB をベンチ開始時の状態に巻き戻す |
| 認証 | なし |
| リージョン | 不要 |
| 実装 | `initialize.go` |

**リクエスト**: なし

**処理の流れ**

```
① exec.Command("../sql/init.sh") を実行（カレントは /home/isucon/webapp/go）
     init.sh → env.sh を読んで psql で sql/reset.sql を実行
       ├─ TRUNCATE ip_bans, login_sessions, administrator_sessions,
       │           friendships, friend_requests, follows, block_relations
       ├─ initial_data_boundary.created_at より後に作られた
       │  notifications / comments / realmojis / posts / posting_windows /
       │  image_objects / users を DELETE
       │  （image_objects は境界以降のパーティションを TRUNCATE してから DELETE）
       ├─ users を initial_users の内容へ復元（差分があるものだけ UPDATE）
       └─ friend_requests / friendships / follows / block_relations を
          initial_* テーブルから再INSERT
② 失敗したらそのエラーを返す（→ 500）
③ 固定の JSON を返す
```

**レスポンス** `200 OK`

```json
{ "language": "go", "agent": "human", "harness": "manual", "model": "brain" }
```

> ソースのコメント: 「`language` は実装言語に設定する。このアプリを最適化するエージェントは `agent` / `harness` / `model` に使用した名前を設定する。複数モデルならカンマ区切り、AI を使わない場合は `brain`」

**エラー**: `init.sh` の失敗 → 500

> **重要**: `reset.sql` は `initial_data_boundary` / `initial_users` / `initial_friend_requests` / `initial_friendships` / `initial_follows` / `initial_block_relations` に依存する。これらは `003-initial-snapshot.sql` が作る退避テーブルで、**壊すと初期化が復旧不能になる**。

---

## 2.2 認証

### `POST /api/auth/register`

| 項目 | 内容 |
|---|---|
| 処理内容 | 新規ユーザー登録。シークレットキーを発行して返す |
| 認証 | なし |
| リージョン | 不要 |
| 実装 | `user_api.go` |

**リクエスト** `application/json`

```json
{ "username": "alice", "displayName": "Alice", "region": "asia" }
```

| フィールド | 制約 |
|---|---|
| `username` | `^[a-z0-9_.-]{1,64}$` |
| `displayName` | 有効な UTF-8、1〜100 文字（ルーン数） |
| `region` | `america` / `europe` / `asia` |

**処理の流れ**

```
① Bind → c.Set("username", …)（ログ用）
② バリデーション失敗 → 400 invalid_registration
③ secretKey = uuid、hash = hex(SHA256(secretKey))
④ INSERT users (id, username, secret_key_hash, display_name, region)
     ON CONFLICT (username) DO NOTHING RETURNING …   ← 1 文で完結（明示的なトランザクションなし）
⑤ 0 行なら 409 username_taken
```

**レスポンス** `201 Created`

```json
{
  "secretKey": "uuid",
  "user": { "id":"…","username":"alice","displayName":"Alice","bio":"",
            "region":"asia","accountType":"regular","status":"active" }
}
```

`secretKey` は**このレスポンスでしか返らない**（DB にはハッシュのみ）。

**エラー**: 400 `invalid_registration` / 409 `username_taken`

> `users.username` の一意性は UNIQUE インデックス `users_username`（`900-extra-index.sql`）で担保している。部分インデックスではないため、退会済みユーザーのユーザー名も再利用できない。`ON CONFLICT (username)` はこのインデックスが無いとエラーになるので、`make index` をコードのデプロイより先に実行する。

---

### `POST /api/auth/login`

| 項目 | 内容 |
|---|---|
| 処理内容 | ログインしてアクセストークンを得る |
| 認証 | なし |
| リージョン | 不要 |
| 実装 | `user_api.go`, `session.go` |

**リクエスト** `application/json`

```json
{ "username": "alice", "secretKey": "uuid" }
```

**処理の流れ**

```
① username がパターン不一致 → 401 invalid_credentials
② トランザクション開始
③ SELECT … FROM users WHERE username = ? FOR NO KEY UPDATE
     行なし → 401 invalid_credentials
④ status != 'active' または hex(SHA256(secretKey)) != secret_key_hash
     → 401 invalid_credentials
⑤ createLoginSessionTx:
     ├─ users 行を FOR NO KEY UPDATE でロック（status='active' 限定）
     ├─ UPDATE login_sessions SET active=false, reason='replaced'
     │    WHERE user_id=? AND active=true        ← ★既存セッションを全部無効化
     ├─ INSERT login_sessions (id, user_id, token_hash=hex(SHA256(uuid)))
     └─ 生成した平文トークンを返す
⑥ COMMIT
```

**レスポンス** `200 OK`

```json
{ "token": "uuid", "user": { …sessionUserResponse… } }
```

以降は `Authorization: Bearer <token>` を付与する。

**エラー**: 401 `invalid_credentials`

---

### `POST /api/auth/logout`

| 項目 | 内容 |
|---|---|
| 処理内容 | 現在のトークンを無効化 |
| 認証 | ユーザー |
| リージョン | 不要 |
| 実装 | `user_api.go`, `session.go` |

**リクエスト**: なし

**処理の流れ**

```
① セッション取得（無ければ 401 invalid_session）
② InvalidateLoginSession:
     ├─ トランザクション開始
     ├─ users 行を FOR NO KEY UPDATE でロック
     ├─ UPDATE login_sessions SET active=false, invalidated_at=now, reason='logout'
     │    WHERE user_id=? AND active=true AND token_hash=?
     └─ COMMIT
```

**レスポンス** `204 No Content`

**エラー**: 401 `invalid_session`

---

## 2.3 自分のアカウント

### `GET /api/me`

| 項目 | 内容 |
|---|---|
| 処理内容 | 自分のプロフィールを取得 |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `user_api.go` |

**リクエスト**: なし

**処理の流れ**: `getUser(session.UserID)` → `newGetMeResponse`（プロフィール画像があれば署名付きURLを生成）

**レスポンス** `200 OK`

```json
{
  "id":"…","username":"alice","displayName":"Alice","bio":"hello",
  "region":"asia","accountType":"regular","status":"active",
  "profileImageUrl":"/api/images/…?exp=…&sig=…"
}
```

`profileImageUrl` は未設定なら `null`。`bio` はマスキングされない。

**エラー**: 401 `invalid_session` / 403 `region_mismatch` / 404 `account_not_found`

---

### `PATCH /api/me`

| 項目 | 内容 |
|---|---|
| 処理内容 | 表示名 / 自己紹介 / リージョンを更新 |
| 認証 | ユーザー |
| リージョン | **不要**（＝どのサーバからでも更新でき、リージョン移動ができる） |
| 実装 | `user_api.go` |

**リクエスト** `application/json`（部分更新。更新したい項目だけ指定）

```json
{ "displayName": "Alice2", "bio": "new bio", "region": "europe" }
```

| フィールド | 制約 |
|---|---|
| `displayName` | 1〜100 文字 |
| `bio` | 0〜500 文字 |
| `region` | `america` / `europe` / `asia` |

**処理の流れ**

```
① 3つすべて未指定 → 400 invalid_profile
② 指定されたものが制約違反 → 400 invalid_profile
③ トランザクション内で、指定された項目ごとに個別の UPDATE 文を実行
     （display_name / bio / region を別々の UPDATE で、updated_at も更新）
④ getUser で読み戻し（行なし → 404 account_not_found）
⑤ COMMIT
```

**レスポンス** `200 OK` — `GET /api/me` と同じ形

**エラー**: 400 `invalid_profile` / 401 `invalid_session` / 404 `account_not_found`

> `region` を変更すると、以降そのユーザーは**変更後のリージョンのサーバ**でしか大半の API を使えなくなる。`HasPostedToday` の判定基準となる投稿枠も変わる。

---

### `PUT /api/me/profile-image`

| 項目 | 内容 |
|---|---|
| 処理内容 | プロフィール画像を差し替え（古い画像は DB から削除） |
| 認証 | ユーザー |
| リージョン | 不要 |
| 実装 | `profile_image_api.go` |

**リクエスト**

- ヘッダ: `Content-Type: image/jpeg`（**完全一致が必要**。それ以外は 415）
- ボディ: **JPEG の生バイナリ**（multipart ではない）

**処理の流れ**

```
① Content-Type != "image/jpeg" → 415 unsupported_media_type
② ボディを全読み
③ jpeg.Decode 失敗 → c.Set("abuse","invalid_image") して 400 invalid_profile_image
④ 幅 > 512 または 高さ > 512 → 400 invalid_profile_image
⑤ トランザクション開始
⑥ SELECT profile_image_id FROM users WHERE id=? FOR NO KEY UPDATE
     行なし → 404 account_not_found
⑦ INSERT image_objects (uuid, owner_user_id=自分, data, 'image/jpeg')
     ※ created_at は DEFAULT clock_timestamp()
⑧ UPDATE users SET profile_image_id = 新ID, updated_at = now
⑨ 旧 profile_image_id があれば DELETE FROM image_objects WHERE id = 旧ID
⑩ COMMIT
```

**レスポンス** `204 No Content`

**エラー**: 400 `invalid_profile_image` / 401 `invalid_session` / 404 `account_not_found` / 415 `unsupported_media_type`

---

### `DELETE /api/me/profile-image`

| 項目 | 内容 |
|---|---|
| 処理内容 | プロフィール画像を削除 |
| 認証 | ユーザー |
| リージョン | 不要 |
| 実装 | `profile_image_api.go` |

**処理の流れ**

```
① トランザクション開始
② SELECT profile_image_id FROM users WHERE id=? FOR NO KEY UPDATE
     行なし → 404 account_not_found
     NULL（画像未設定）→ 404 account_not_found
③ UPDATE users SET profile_image_id = NULL, updated_at = now
④ DELETE FROM image_objects WHERE id = 旧ID
⑤ COMMIT
```

**レスポンス** `204 No Content`

**エラー**: 401 `invalid_session` / 404 `account_not_found`（画像が無い場合もこれ）

---

### `POST /api/me/account-deletion`

| 項目 | 内容 |
|---|---|
| 処理内容 | 自分のアカウントを論理削除 |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `account_deletion_api.go` |

**処理の流れ**

```
① トランザクション開始
② SELECT id FROM users WHERE id=? AND status='active' FOR NO KEY UPDATE
     行なし → 404 account_not_found
③ UPDATE users SET status='deleted', deleted_at=now, updated_at=now
④ UPDATE login_sessions SET active=false, invalidated_at=now,
       reason='account_deletion' WHERE user_id=? AND active=true
⑤ COMMIT
```

**レスポンス** `204 No Content`

**エラー**: 401 `invalid_session` / 403 `region_mismatch` / 404 `account_not_found`

**削除の波及**（物理削除はされず、各クエリの `status='active'` 条件で除外される）

| 対象 | 挙動 |
|---|---|
| 投稿 | `selectPostsWithAuthor` が `JOIN users … status='active'` なのでフィードから消える |
| コメント / RealMoji | 同様に一覧から消える |
| 画像 | `image_api.go` が所有者 `status='active'` を要求するので 403 になる |
| セッション | 全て無効化。`getSessionFromHeader` も `u.status='active'` を要求 |
| 友だち関係 | `friendships` の行は残る（`getFriendIDs` はフィルタしないが、表示側で JOIN により除外） |

---

### `GET /api/me/export`

| 項目 | 内容 |
|---|---|
| 処理内容 | 自分の全投稿を画像ごと ZIP でダウンロード |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `export_api.go` |

**リクエスト**: なし（**ページネーションなし。全件**）

**処理の流れ**

```
① getUser(自分)
② SELECT postColumns + i.data AS image_data
     FROM posts p JOIN posting_windows w JOIN image_objects i
     WHERE p.author_id = 自分 AND p.deleted_at IS NULL
     ORDER BY p.posted_at, p.id
     ※ LIMIT なし。全投稿の画像バイナリをメモリに載せる
③ ZIP を bytes.Buffer 上に構築
     ├─ images/000001.jpg, images/000002.jpg, … （zip.Store = 無圧縮）
     └─ posts.json … exportPostResponse の配列
④ Content-Disposition / Cache-Control: no-store を設定して返す
```

**レスポンス** `200 OK`、`Content-Type: application/zip`

- ヘッダ: `Content-Disposition: attachment; filename="isureal-posts.zip"`、`Cache-Control: no-store`
- ZIP 内 `posts.json`:

```json
[
  { …createPostResponse…, "imagePath": "images/000001.jpg" }
]
```

**エラー**: 401 `invalid_session` / 403 `region_mismatch`

---

## 2.4 ユーザー参照

### `GET /api/users`

| 項目 | 内容 |
|---|---|
| 処理内容 | ユーザー検索（username / displayName の部分一致） |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `user_read_api.go` |

**リクエスト** クエリパラメータ

| 名前 | 必須 | 内容 |
|---|---|---|
| `q` | **必須** | 検索語。1〜100 文字 |
| `beforeUsername` | — | カーソル（実際は「この username より後」の意味） |
| `beforeId` | — | カーソル |

**処理の流れ**

```
① q が 1〜100 文字でない → 400 invalid_relationship
② beforeUsername / beforeId の片方だけ指定 → 400 invalid_pagination
③ SELECT userColumns FROM users
     WHERE status='active' AND id <> 自分
       AND ($2='' OR (username,id) > ($2,$3))
     ORDER BY username, id            ← ★LIMIT なし。全件取得
④ getBlockedUserIDs(自分)
⑤ Go 側でループ:
     ├─ 正規化 normalizeSearchText = NFC → caseFold → NFC
     ├─ username にも displayName にも含まれない → スキップ
     ├─ ブロック関係（どちらの向きでも）→ スキップ
     ├─ accountType != 'certified_public' なら bio = ""
     └─ 51件たまったら break
⑥ hasMore = (件数 > 50) → 先頭50件を返す
```

**レスポンス** `200 OK`

```json
{ "items": [ { …relationshipUserResponse… } ], "hasMore": false }
```

**エラー**: 400 `invalid_relationship` / 400 `invalid_pagination` / 401 `invalid_session` / 403 `region_mismatch`

---

### `GET /api/users/:userId`

| 項目 | 内容 |
|---|---|
| 処理内容 | 特定ユーザーのプロフィールを取得 |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `user_read_api.go` |

**リクエスト**: パスパラメータ `userId`

**処理の流れ**

```
① getUser(userId)（status='active' 限定）
     行なし → 404 relationship_not_found
② isBlocked(自分, 相手)（双方向）→ true なら 404 relationship_not_found
③ getFriendIDs(自分)
④ accountType != 'certified_public' かつ 友だちでない → bio = ""
```

**レスポンス** `200 OK` — `relationshipUserResponse`

**エラー**: 401 `invalid_session` / 403 `region_mismatch` / 404 `relationship_not_found`（存在しない・削除済み・ブロック関係のいずれも同じコード）

---

## 2.5 投稿枠

### `GET /api/posting-window/current`

| 項目 | 内容 |
|---|---|
| 処理内容 | 自分のリージョンで開催中の投稿枠と、自分の投稿状況を返す |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `posting_window_api.go` |

**リクエスト**: なし

**処理の流れ**

```
① 単一クエリで枠と自分の投稿数を集計
     SELECT w.*,
            count(p.id) FILTER (WHERE p.deleted_at IS NULL)     AS active,
            count(p.id) FILTER (WHERE p.deleted_at IS NOT NULL) AS deleted
     FROM users u
     JOIN posting_windows w ON w.region = u.region AND w.ended_at IS NULL
     LEFT JOIN posting_windows posted_window ON posted_window.local_date = w.local_date
     LEFT JOIN posts p ON p.author_id = u.id AND p.posting_window_id = posted_window.id
     WHERE u.id = ?
     GROUP BY w.id
② 行なし（開催中の枠が無い）→ 200 で空レスポンス
③ hasPosted      = active > 0
   canPost        = active == 0 && deleted < 2 && now >= starts_at
   deletionCount  = deleted
```

**レスポンス** `200 OK`

```json
{
  "window": {
    "id": "uuid",
    "region": "asia",
    "localDate": "2026-10-31",
    "startsAt": "2026-10-31T03:00:00Z",
    "onTimeDeadline": "2026-11-01T03:00:00Z"
  },
  "hasPosted": false,
  "canPost": true,
  "deletionCount": 0
}
```

開催中の枠が無い場合:

```json
{ "window": null, "hasPosted": false, "canPost": false, "deletionCount": 0 }
```

**エラー**: 401 `invalid_session` / 403 `region_mismatch`

> **注意**: `LEFT JOIN posting_windows posted_window ON posted_window.local_date = w.local_date` に**リージョン条件が無い**。同じ `local_date` の枠が他リージョンにも存在する場合、それらの枠に紐づく自分の投稿も集計対象になる（`PATCH /api/me` でリージョン移動した場合に影響しうる）。

---

## 2.6 投稿

### `POST /api/posts`

| 項目 | 内容 |
|---|---|
| 処理内容 | 開催中の投稿枠に写真を1枚投稿する |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `post_api.go` |

**リクエスト** `multipart/form-data`

| フィールド | 必須 | 制約 |
|---|---|---|
| `image` | **必須** | JPEG のみ。4096×4096 以内 |
| `caption` | — | 0〜1000 文字 |
| `visibility` | **必須** | `friends` / `friends_of_friends` / `public` |
| `location_kind` | **必須** | `none` / `exact` / `approximate` |
| `latitude` | 条件付き | `exact` のとき必須。-90〜90 |
| `longitude` | 条件付き | `exact` のとき必須。-180〜180 |
| `locality` | 条件付き | `approximate` のとき必須。1〜100 文字 |

位置情報の組み合わせ規則（`post.go`: `PostLocation.valid`）

| `location_kind` | `latitude` | `longitude` | `locality` |
|---|---|---|---|
| `none` | なし | なし | なし |
| `exact` | **必須** | **必須** | なし |
| `approximate` | なし | なし | **必須** |

**処理の流れ**

```
① Bind
② image 無し、または caption が 0〜1000 文字でない → 400 invalid_post
③ visibility が3種以外 → 400 invalid_post
④ location.valid() が false → 400 invalid_post
⑤ 画像を全読み → jpeg.Decode 失敗 → 400 invalid_post
⑥ 幅 > 4096 または 高さ > 4096 → 400 invalid_post
⑦ トランザクション開始
⑧ SELECT id FROM users WHERE id=? FOR NO KEY UPDATE   ← 自分の行をロック
⑨ getUser(自分)
⑩ SELECT id, local_date, starts_at FROM posting_windows
     WHERE region = <自分の region> AND ended_at IS NULL FOR UPDATE
     行なし → 409 post_slot_unavailable
⑪ 同 local_date の自分の投稿数を集計
     SELECT count FILTER(deleted_at IS NULL) AS active,
            count FILTER(deleted_at IS NOT NULL) AS deleted
     FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
     WHERE p.author_id = ? AND w.local_date = ?
⑫ postedAt = time.Now().UTC()
   active != 0 || deleted >= 2 || postedAt < window.starts_at
     → 409 post_slot_unavailable
⑬ INSERT image_objects (uuid, 自分, data, 'image/jpeg', created_at = postedAt)
⑭ INSERT posts (uuid, author, window, image, caption, visibility,
                 location_*, posted_at = postedAt)
⑮ getPost で読み戻し → COMMIT
```

**レスポンス** `201 Created` — `createPostResponse`

**エラー**: 400 `invalid_post` / 401 `invalid_session` / 403 `region_mismatch` / 409 `post_slot_unavailable`

> ⑩ は `sql.ErrNoRows` のとき**まったく同じクエリをもう一度実行**している（実装のまま。リトライとして意味を持たない）。
> ⑬ で `image_objects.created_at` に `postedAt` を明示している点に注意。`created_at` は月次パーティションのキーなので、対応するパーティションが無い値だと `image_objects_default` に入る。

---

### `GET /api/posts/me/current`

| 項目 | 内容 |
|---|---|
| 処理内容 | 開催中の枠に対する自分の投稿を取得 |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `post_api.go` |

**処理の流れ**

```
① SELECT p.id FROM posts p
     JOIN posting_windows w ON w.id = p.posting_window_id
     JOIN users u ON u.id = p.author_id
     JOIN posting_windows current
          ON current.region = u.region AND current.ended_at IS NULL
         AND current.local_date = w.local_date
     WHERE p.author_id = ? AND p.deleted_at IS NULL
     行なし → 404 post_not_found
② getPost(postID) → 行なし → 404 post_not_found
③ getUser(自分)
```

**レスポンス** `200 OK` — `createPostResponse`

**エラー**: 401 `invalid_session` / 403 `region_mismatch` / 404 `post_not_found`

---

### `GET /api/posts/:postId`

| 項目 | 内容 |
|---|---|
| 処理内容 | 投稿を1件取得（可視性判定あり） |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `post_api.go` |

**処理の流れ**

```
① getPostWithAuthor(postId)（deleted_at IS NULL、著者が active）
     行なし → 404 post_not_found
② loadViewerContext(自分)
③ viewer.canView(post) が false → 404 post_not_found
```

**レスポンス** `200 OK` — `createPostResponse`

**エラー**: 401 `invalid_session` / 403 `region_mismatch` / 404 `post_not_found`

> 見る権限が無い場合も **403 ではなく 404** を返す（投稿の存在を隠す設計）。

---

### `PATCH /api/posts/:postId`

| 項目 | 内容 |
|---|---|
| 処理内容 | 自分の投稿のキャプション / 公開範囲を変更 |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `post_api.go` |

**リクエスト** `application/json`

```json
{ "caption": "updated", "visibility": "public" }
```

| フィールド | 制約 |
|---|---|
| `caption` | 0〜1000 文字 |
| `visibility` | `friends` / `friends_of_friends` / `public` |

**処理の流れ**

```
① 両方未指定 → 400 invalid_post
② caption が制約違反 → 400 invalid_post
③ visibility が3種以外 → 400 invalid_post
④ トランザクション開始
⑤ SELECT id FROM users WHERE id=? FOR NO KEY UPDATE
⑥ getPost(postId) → 行なし → 404 post_not_found
⑦ post.AuthorID != 自分 → 403 post_not_editable
⑧ getPostingWindow(post.PostingWindowID)
⑨ window.EndedAt != NULL → 403 post_not_editable   ← 枠が閉じたら編集不可
⑩ 指定された項目ごとに個別 UPDATE
⑪ 読み戻して COMMIT
```

**レスポンス** `200 OK` — `createPostResponse`

**エラー**: 400 `invalid_post` / 401 `invalid_session` / 403 `region_mismatch` / 403 `post_not_editable` / 404 `post_not_found`

---

### `DELETE /api/posts/:postId`

| 項目 | 内容 |
|---|---|
| 処理内容 | 自分の投稿を論理削除（やり直し用。1日2回まで） |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `post_api.go` |

**処理の流れ**

```
① トランザクション開始
② SELECT id FROM users WHERE id=? FOR NO KEY UPDATE
③ getPost(postId) → 行なし → 404 post_not_found
④ post.AuthorID != 自分 → 403 post_not_editable
⑤ getPostingWindow(post.PostingWindowID)
⑥ 同 local_date における自分の削除済み投稿数を数える
     SELECT count(*) FROM posts p JOIN posting_windows w …
     WHERE p.author_id=? AND w.local_date=? AND p.deleted_at IS NOT NULL
   >= 2 → 409 post_slot_unavailable
⑦ UPDATE posts SET deleted_at = clock_timestamp() WHERE id = ?
⑧ COMMIT
```

**レスポンス** `204 No Content`

**エラー**: 401 `invalid_session` / 403 `region_mismatch` / 403 `post_not_editable` / 404 `post_not_found` / 409 `post_slot_unavailable`

> `PATCH` と違い、**枠が終了済みかどうかをチェックしていない**。削除回数が2未満なら過去の枠の投稿も削除できる。

---

## 2.7 フィード

### `GET /api/feeds/:feed`

| 項目 | 内容 |
|---|---|
| 処理内容 | 4種類のフィードを返す。`friends-of-friends` のみスコア順 |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `feed_api.go` |

**リクエスト**

パスパラメータ `feed`:

| 値 | 対象 |
|---|---|
| `friends` | 自分の友だちの投稿 |
| `friends-of-friends` | 友だちではない「友だちの友だち」の投稿（**スコア順**） |
| `public` | `visibility = "public"` の投稿 |
| `following` | 自分がフォローしているユーザーの投稿 |

クエリパラメータ:

| 名前 | 型 | 必要な組み合わせ |
|---|---|---|
| `beforePostedAt` | RFC3339 | `beforeId` とセット |
| `beforeId` | string | 同上 |
| `beforeScore` | int64 | `friends-of-friends` のときのみ必須（他は無視される） |

**処理の流れ**

```
① feed が4種以外 → 400 invalid_post   ★コードは invalid_post
② (beforePostedAt == nil) != (beforeId == "") → 400 invalid_pagination
③ ranked = (feed == "friends-of-friends")
   ranked かつ (beforeScore == nil) != (beforeId == "") → 400 invalid_pagination
④ loadViewerContext(自分)
     ErrNoRows → 200 で {"items":[], "hasMore":false}
⑤ feed == "following" なら SELECT followee_id FROM follows WHERE follower_id=? を map に
⑥ ranked ならカーソルを無効化（beforePostedAt=nil, beforeId=""）
⑦ 候補を SQL で取得（★LIMIT なし）
     selectPostsWithAuthor(
       "AND p.author_id <> $1 AND w.ended_at IS NULL
        AND ($2::timestamptz IS NULL OR (p.posted_at, p.id) < ($2,$3))
        ORDER BY p.posted_at DESC, p.id DESC")
     → 開催中の全リージョンの枠に属する、自分以外の生存投稿すべて
⑧ Go 側で絞り込み: viewer.canView(post) && matchesFeed(viewer, following, post, feed)
⑨ ranked なら recommendationScores を計算し、スコア降順に安定ソート
⑩ ranked ならカーソル（score, postedAt, id）より後ろのものだけ採用
⑪ 51件たまったら break → hasMore = (件数 > 50) → 先頭50件
```

### `matchesFeed` の判定

```
"public"             → post.Visibility == "public"
"following"          → following[post.AuthorID]
"friends"            → viewer.Friends[post.AuthorID]
"friends-of-friends" → !viewer.Friends[author] && viewer.FriendsOfFriends[author]
```

`canView` と AND されるので、例えば `public` フィードでも「自分が今日未投稿」なら空になる。

### レコメンドスコア（`recommendationScores`）

`friends-of-friends` のみ計算。

```
score = 10 × (その投稿に反応した自分の友だちの数)
      +  3 × (投稿者との共通友だち数)
      +      min(投稿者の友だちの、自分との共通友だち数の合計, 30)
```

具体的には:

```
① 候補投稿の著者ID / 投稿IDを収集
② friendships を著者ID群で検索し、connections[著者] に
     「その著者の友だち」の MutualFriends 値を加算
③ realmojis と comments を UNION で引き、投稿ごとの「反応したユーザー」を取得
     （どちらも active ユーザー・生存レコードのみ）
④ そのうち自分の友だちの数を friendEngagements[postID] に集計
⑤ score = 10*friendEngagements + 3*MutualFriends[著者] + min(connections[著者], 30)
```

ソートは `sort.SliceStable` なので、同スコアでは SQL の順（`posted_at DESC, id DESC`）が保たれる。

### カーソル比較（`rankedBefore`）

```
score != beforeScore          → score < beforeScore
postedAt != beforePostedAt    → postedAt < beforePostedAt
それ以外                        → id < beforeId
```

**レスポンス** `200 OK`

```json
{
  "items": [
    { …createPostResponse…, "score": 43 }
  ],
  "hasMore": true
}
```

`score` は `friends-of-friends` のときのみ含まれる。

**エラー**: 400 `invalid_post`（feed 名不正）/ 400 `invalid_pagination` / 401 `invalid_session` / 403 `region_mismatch`

---

## 2.8 メモリー

### `GET /api/memories`

| 項目 | 内容 |
|---|---|
| 処理内容 | 指定月の自分の過去投稿を、RealMoji とコメント付きで返す |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `memory_api.go` |

**リクエスト** クエリパラメータ

| 名前 | 必須 | 形式 |
|---|---|---|
| `month` | **必須** | `2006-01`（例 `2026-10`） |
| `beforePostedAt` | — | RFC3339。`beforeId` とセット |
| `beforeId` | — | 同上 |

パース失敗 → 400 `invalid_memory_period`。期間は `[月初, 翌月初)`。

### `GET /api/memories/:date`

同じ処理で、パスパラメータ `date`（`2006-01-02` 形式）の1日分。期間は `[その日, 翌日)`。

**処理の流れ**（共通 `listMemories`）

```
① 期間をパース（失敗 → 400 invalid_memory_period）
② (beforePostedAt == nil) != (beforeId == "") → 400 invalid_pagination
③ selectPostsWithAuthor(
     "AND p.author_id = $1 AND w.local_date >= $2 AND w.local_date < $3
      AND ($4::timestamptz IS NULL OR (p.posted_at,p.id) < ($4,$5))
      ORDER BY p.posted_at DESC, p.id DESC LIMIT 51")
     ※ 自分の投稿のみ。可視性判定なし。枠が終了済みでも取得できる
④ hasMore = (件数 > 50) → 先頭50件
⑤ loadMemoryInteractions: 50件分の post_id をまとめて2クエリ
     ├─ selectCurrentRealMojis(post_id IN (…) ORDER BY created_at DESC, id DESC)
     │    → 各投稿につき先頭 50 件まで realmojis に詰める
     └─ selectCommentsWithAuthor(post_id IN (…) ORDER BY created_at, id)
          → 各投稿につき先頭 50 件まで comments に詰める
```

**レスポンス** `200 OK`

```json
{
  "items": [
    {
      …createPostResponse…,
      "realmojis": [
        { "id":"…", "reactor": { …relationshipUserResponse… },
          "reactionType":"heart", "imageUrl":"/api/images/…", "createdAt":"…" }
      ],
      "comments": [
        { "id":"…", "author": { …relationshipUserResponse… },
          "body":"nice", "createdAt":"…" }
      ]
    }
  ],
  "hasMore": false
}
```

`realmojis` / `comments` は投稿ごとに最大50件。投稿が0件でも `items: []` と空配列になる。

**エラー**: 400 `invalid_memory_period` / 400 `invalid_pagination` / 401 `invalid_session` / 403 `region_mismatch`

> メモリーは**自分の投稿だけ**なので `canView` を通らない。「枠が終了した投稿は見えない」という制約の例外であり、過去の投稿を振り返る唯一の経路。
> RealMoji / コメントは `created_at` でソートされるが、`memory_api.go` の並び順は RealMoji が降順、コメントが昇順。

---

## 2.9 コメント

### `POST /api/posts/:postId/comments`

| 項目 | 内容 |
|---|---|
| 処理内容 | 投稿にコメントする |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `comment_api.go` |

**リクエスト** `application/json`

```json
{ "body": "nice photo" }
```

`body` は 1〜1000 文字（必須）。

**処理の流れ**

```
① body が 1〜1000 文字でない → 400 invalid_interaction
② getPost(postId) → 行なし → 403 interaction_forbidden   ★404 ではない
③ トランザクション開始
④ 自分と投稿者の users 行を ID 昇順でロック（FOR NO KEY UPDATE）
     ※ slices.Compact で自分＝投稿者のときは1回だけ
⑤ canComment(自分, postId) が false → 403 interaction_forbidden
⑥ INSERT comments (uuid, post_id, author_id, body, created_at=clock_timestamp())
⑦ 読み戻し + getUser(自分) → COMMIT
```

**レスポンス** `201 Created`

```json
{
  "id": "uuid",
  "author": { …relationshipUserResponse… },
  "body": "nice photo",
  "createdAt": "2026-10-31T03:05:00Z"
}
```

**エラー**: 400 `invalid_interaction` / 401 `invalid_session` / 403 `region_mismatch` / 403 `interaction_forbidden`

---

### `GET /api/posts/:postId/comments`

| 項目 | 内容 |
|---|---|
| 処理内容 | 投稿のコメント一覧（古い順） |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `comment_api.go` |

**リクエスト** クエリパラメータ

| 名前 | 内容 |
|---|---|
| `afterCreatedAt` | RFC3339。`afterId` とセット |
| `afterId` | 同上 |

**処理の流れ**

```
① (afterCreatedAt == nil) != (afterId == "") → 400 invalid_pagination
② canComment(自分, postId) が false → 403 interaction_forbidden
     ★canView ではなく canComment。投稿は見えてもコメントが読めない場合がある
③ selectCommentsWithAuthor(
     "AND c.post_id=$1 AND ($2::timestamptz IS NULL OR (c.created_at,c.id)>($2,$3))
      ORDER BY c.created_at, c.id LIMIT 51")
④ hasMore = (件数 > 50) → 先頭50件
```

**レスポンス** `200 OK`

```json
{ "items": [ { "id":"…","author":{…},"body":"…","createdAt":"…" } ], "hasMore": false }
```

**エラー**: 400 `invalid_pagination` / 401 `invalid_session` / 403 `region_mismatch` / 403 `interaction_forbidden`

---

### `DELETE /api/comments/:commentId`

| 項目 | 内容 |
|---|---|
| 処理内容 | コメントを論理削除（コメント投稿者 or 投稿者が実行可能） |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `comment_api.go` |

**処理の流れ**

```
① トランザクション開始
② SELECT id FROM users WHERE id=? FOR NO KEY UPDATE（自分のみ）
③ SELECT c.author_id, p.author_id AS post_author_id
     FROM comments c JOIN posts p ON p.id = c.post_id
     WHERE c.id = ? AND c.deleted_at IS NULL FOR UPDATE OF c
     行なし → 404 interaction_not_found
④ 自分がコメント投稿者でも投稿者でもない → 403 interaction_forbidden
⑤ UPDATE comments SET deleted_at = clock_timestamp() WHERE id = ?
⑥ COMMIT
```

**レスポンス** `204 No Content`

**エラー**: 401 `invalid_session` / 403 `region_mismatch` / 403 `interaction_forbidden` / 404 `interaction_not_found`

> 枠が終了していても削除できる（可視性・枠のチェックなし）。

---

## 2.10 RealMoji

RealMoji は「自撮り写真で返すリアクション」。1投稿につき1ユーザー1つだけが現役で、投稿し直すと古いものに `replaced_at` が入る。

### `POST /api/posts/:postId/realmojis`

| 項目 | 内容 |
|---|---|
| 処理内容 | RealMoji を付ける / 差し替える |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `realmoji_api.go` |

**リクエスト** `multipart/form-data`

| フィールド | 必須 | 制約 |
|---|---|---|
| `image` | **必須** | JPEG。**512×512 以内** |
| `reactionType` | **必須** | `like` / `smile` / `surprise` / `heart` / `laugh` / `instant` |

**処理の流れ**

```
① image 無し → 400 invalid_realmoji
② reactionType が6種以外 → 400 invalid_realmoji
③ 画像を全読み → jpeg.Decode 失敗 → 400 invalid_realmoji
④ 幅 > 512 または 高さ > 512 → 400 invalid_realmoji
⑤ getPost(postId) → 行なし → 403 interaction_forbidden
⑥ 自分が投稿者 → 403 interaction_forbidden   ★自分の投稿には付けられない
⑦ beginRelationshipTx(自分, 投稿者)
     自分==投稿者 → 400 invalid_relationship（⑥で弾かれるので通常到達しない）
     どちらかが非 active → 404 relationship_not_found
     → 2人の users 行を ID 昇順でロック
⑧ canViewPost(自分, postId) が false → 403 interaction_forbidden
⑨ UPDATE realmojis SET replaced_at = now
     WHERE post_id=? AND reactor_id=? AND replaced_at IS NULL AND deleted_at IS NULL
     ← 既存の現役 RealMoji を「置き換え済み」にする
⑩ INSERT image_objects (uuid, 自分, data, 'image/jpeg')   ※created_at は DEFAULT
⑪ INSERT realmojis (uuid, post_id, reactor_id, image_id, reaction_type,
                     created_at = clock_timestamp())
⑫ 読み戻し + getUser(自分) → COMMIT
```

部分ユニークインデックス `realmojis_one_active ON (post_id, reactor_id) WHERE replaced_at IS NULL AND deleted_at IS NULL` が、現役が1つだけであることを DB レベルで保証する。

**レスポンス** `201 Created`

```json
{
  "id": "uuid",
  "reactor": { …relationshipUserResponse… },
  "reactionType": "heart",
  "imageUrl": "/api/images/…?exp=…&sig=…",
  "createdAt": "2026-10-31T03:06:00Z"
}
```

**エラー**: 400 `invalid_realmoji` / 400 `invalid_relationship` / 401 `invalid_session` / 403 `region_mismatch` / 403 `interaction_forbidden` / 404 `relationship_not_found`

---

### `GET /api/posts/:postId/realmojis`

| 項目 | 内容 |
|---|---|
| 処理内容 | 投稿に付いた現役 RealMoji の一覧（新しい順） |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `realmoji_api.go` |

**リクエスト** クエリパラメータ: `beforeCreatedAt` + `beforeId`（セット）

**処理の流れ**

```
① ページネーションのペア不一致 → 400 invalid_pagination
② canViewPost(自分, postId) が false → 403 interaction_forbidden
     ★ここは canView ベース（コメント一覧と違い canComment ではない）
③ selectCurrentRealMojis(
     "AND r.post_id=$1 AND ($2::timestamptz IS NULL OR (r.created_at,r.id)<($2,$3))
      ORDER BY r.created_at DESC, r.id DESC LIMIT 51")
     ※ replaced_at IS NULL AND deleted_at IS NULL、リアクターが active のみ
④ hasMore = (件数 > 50) → 先頭50件
```

**レスポンス** `200 OK`

```json
{ "items": [ { …replaceRealMojiResponse… } ], "hasMore": false }
```

**エラー**: 400 `invalid_pagination` / 401 `invalid_session` / 403 `region_mismatch` / 403 `interaction_forbidden`

---

### `DELETE /api/realmojis/:realMojiId`

| 項目 | 内容 |
|---|---|
| 処理内容 | RealMoji を論理削除（リアクターまたは投稿者が実行可能） |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `realmoji_api.go` |

**処理の流れ**

```
① トランザクション開始
② SELECT id FROM users WHERE id=? FOR NO KEY UPDATE（自分のみ）
③ SELECT r.reactor_id, p.author_id
     FROM realmojis r JOIN posts p ON p.id = r.post_id
     WHERE r.id = ? AND r.deleted_at IS NULL FOR UPDATE OF r
     行なし → 404 interaction_not_found
④ 自分がリアクターでも投稿者でもない → 403 interaction_forbidden
⑤ UPDATE realmojis SET deleted_at = clock_timestamp() WHERE id = ?
⑥ COMMIT
```

**レスポンス** `204 No Content`

**エラー**: 401 `invalid_session` / 403 `region_mismatch` / 403 `interaction_forbidden` / 404 `interaction_not_found`

> 削除すると `image_api.go` の「現役か」チェックに通らなくなるため、その画像 URL は即座に 403 になる（`image_api.go` のコメント: 「削除状態を毎回確認する。即時反映も5秒以内の許容範囲に含まれる」）。

---

## 2.11 画像

### `GET /api/images/:imageId`

| 項目 | 内容 |
|---|---|
| 処理内容 | 署名付きURLで画像バイナリを配信 |
| 認証 | **なし**（署名で保護） |
| リージョン | **不要**（どのサーバでも配信可能） |
| 実装 | `image_api.go`, `image_signature.go` |

**リクエスト**

| 場所 | 名前 | 内容 |
|---|---|---|
| パス | `imageId` | 画像 ID |
| クエリ | `exp` | 失効 UNIX 秒。正準10進表記 |
| クエリ | `sig` | base64url（パディングなし）の 32 バイト |

**処理の流れ**: [1.7 画像と署名付きURL](#17-画像と署名付きurl) の検証手順を参照。

**レスポンス** `200 OK`

- `Content-Type`: `image_objects.media_type`（`image/jpeg` など）
- `Cache-Control: no-store`（**常に設定される。成功・失敗とも**）
- ボディ: 画像の生バイナリ

**エラー**: **すべて 403 `image_forbidden`**（クエリ欠落、期限切れ、署名形式不正、画像なし、所有者が非 active、署名不一致、画像が現役でない）

署名不一致のときのみ `abuse = "invalid_signature"` がログに乗り、`ActionLogMiddleware` のレベルが `debug` から `info` に上がる（＝ `isureal-guard` の BAN カウント対象になる）。

---

## 2.12 通知

### `GET /api/notifications`

| 項目 | 内容 |
|---|---|
| 処理内容 | 新しい投稿枠が開いたことを Server-Sent Events で通知。**1件送って切断するワンショット** |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `notification_api.go` |

**リクエスト**: なし

**処理の流れ**

```
① connectedAt = SELECT clock_timestamp()（DBの現在時刻）
② ヘッダを即送信: Content-Type: text/event-stream, 200 OK → Flush
③ 500ms ごとのループ:
     ├─ getUser(自分)
     │     エラー（削除された等）→ 接続終了
     ├─ user.Region != session.Region → 接続終了   ★リージョン移動で切れる
     ├─ SELECT n.id, n.posting_window_id, w.region, n.created_at
     │    FROM notifications n JOIN posting_windows w ON w.id = n.posting_window_id
     │    WHERE n.user_id = ? AND n.created_at > connectedAt AND w.ended_at IS NULL
     │    ORDER BY n.created_at, n.id LIMIT 1
     ├─ 見つかった → "data: {json}\n\n" を書いて Flush し、★return（接続終了）
     ├─ ErrNoRows 以外のエラー → 接続終了
     └─ ctx.Done() または ticker を待つ
```

**レスポンス** `200 OK`、`Content-Type: text/event-stream`

```
data: {"id":"uuid","postingWindowId":"uuid","region":"asia","createdAt":"2026-10-31T03:00:00Z"}

```

- **接続後に作られた通知**のみ対象（`created_at > connectedAt`）。接続前の通知は配信されない
- **枠が開いているもの**のみ（`w.ended_at IS NULL`）
- 1件送ったら接続を閉じる。クライアントは再接続して待ち受ける必要がある
- エラーは HTTP ステータスで返らない（ヘッダ送信済みのため）。異常時は単に接続が閉じる

**エラー**: 401 `invalid_session` / 403 `region_mismatch`（いずれもヘッダ送信前なので通常の JSON エラー）

> 接続1本あたり 500ms ごとに2クエリ（`getUser` + 通知検索）が走る。

---

## 2.13 関係性

関係性には4種類ある。

| 種類 | テーブル | 性質 |
|---|---|---|
| フレンド | `friendships` | 相互。`(user_low_id, user_high_id)` に正規化（ID の小さい方が low） |
| フレンド申請 | `friend_requests` | `pending` / `accepted` / `rejected` / `cancelled` |
| フォロー | `follows` | 一方向。**`certified_public` ユーザーのみフォロー可** |
| ブロック | `block_relations` | 一方向に記録するが、判定は双方向 |

### 共通の前処理 `beginRelationshipTx`（`relationship.go`）

```
① actor == target、または target == "" → 400 invalid_relationship
② トランザクション開始
③ min(actor,target) → max(actor,target) の順に
     SELECT id FROM users WHERE id=? AND status='active' FOR NO KEY UPDATE
     どちらかが存在しない / 非 active → 404 relationship_not_found
     ※ ID 昇順でロックするのでデッドロックしない
```

---

### `POST /api/friend-requests`

| 項目 | 内容 |
|---|---|
| 処理内容 | フレンド申請を送る。相手から申請が来ていれば即成立 |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `friend_request_api.go` |

**リクエスト** `application/json`

```json
{ "recipientId": "uuid" }
```

**処理の流れ**

```
① beginRelationshipTx(自分, recipientId)
② isBlocked(双方向) → true なら 409 relationship_conflict
③ getFriendIDs(自分) に recipientId が含まれる → 409 relationship_conflict（既に友だち）
④ SELECT id FROM friend_requests WHERE status='pending'
     AND ((requester=自分 AND recipient=相手) OR (requester=相手 AND recipient=自分))
   ├─ 見つかった場合:
   │    ├─ requester == 自分 → 409 relationship_conflict（二重申請）
   │    └─ requester == 相手 → ★相互申請なので即成立
   │         ├─ UPDATE friend_requests SET status='accepted', updated_at=now
   │         └─ INSERT friendships (uuid, min(2者), max(2者), source_request_id)
   └─ 見つからない場合:
        └─ INSERT friend_requests (uuid, 自分, 相手, 'pending', now, now)
⑤ 読み戻して COMMIT
```

**レスポンス** `201 Created`

```json
{ "relationId": "uuid", "status": "pending", "updatedAt": "2026-10-31T03:00:00Z" }
```

相互申請で成立した場合 `status` は `"accepted"`、`relationId` は**相手が作った申請の ID**。

**エラー**: 400 `invalid_relationship`（自分自身・空ID）/ 401 `invalid_session` / 403 `region_mismatch` / 404 `relationship_not_found` / 409 `relationship_conflict`

---

### `POST /api/friend-requests/:requestId/:action`

| 項目 | 内容 |
|---|---|
| 処理内容 | フレンド申請を承認 / 拒否 / 取消 |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `friend_request_api.go` |

**リクエスト** パスパラメータ

| `action` | 遷移後 `status` | 実行できる人 |
|---|---|---|
| `accept` | `accepted` | **受信者**（`recipient_id`） |
| `reject` | `rejected` | **受信者** |
| `cancel` | `cancelled` | **送信者**（`requester_id`） |

**処理の流れ**

```
① action が3種以外 → 403 relationship_action_forbidden
② getFriendRequest(requestId)（トランザクション外）
     行なし → 404 relationship_not_found
③ 実行者の権限チェック
     cancel なのに requester でない、
     または accept/reject なのに recipient でない
     → 403 relationship_action_forbidden
④ beginRelationshipTx(requester, recipient)
⑤ 申請を再取得して status != 'pending' → 403 relationship_action_forbidden
⑥ accept の場合:
     ├─ isBlocked(双方向) → true なら 409 relationship_conflict
     └─ INSERT friendships
⑦ UPDATE friend_requests SET status = <新status>, updated_at = now
⑧ 読み戻して COMMIT
```

**レスポンス** `200 OK`

```json
{ "relationId": "uuid", "status": "accepted", "updatedAt": "…" }
```

**エラー**: 401 `invalid_session` / 403 `region_mismatch` / 403 `relationship_action_forbidden` / 404 `relationship_not_found` / 409 `relationship_conflict`

---

### `GET /api/relationships/:kind`

| 項目 | 内容 |
|---|---|
| 処理内容 | 関係性の一覧を返す（5種） |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `relationship_list_api.go` |

**リクエスト**

パスパラメータ `kind`:

| 値 | 対象 | ベースクエリ |
|---|---|---|
| `friends` | 友だち | `friendships WHERE user_low_id=? OR user_high_id=?` |
| `following` | 自分がフォロー中 | `follows WHERE follower_id=?` |
| `followers` | 自分のフォロワー | `follows WHERE followee_id=?` |
| `incoming-requests` | 受信中の申請 | `friend_requests WHERE recipient_id=? AND status='pending'` |
| `outgoing-requests` | 送信中の申請 | `friend_requests WHERE requester_id=? AND status='pending'` |

クエリパラメータ: `beforeUpdatedAt` + `beforeId`（セット）

**処理の流れ**

```
① ページネーションのペア不一致 → 400 invalid_pagination
② kind が5種以外 → 400 invalid_relationship
③ ベースクエリをサブクエリにして users と JOIN
     SELECT relations.id, relations.updated_at, <userColumns>
     FROM (<ベースクエリ>) relations
     JOIN users u ON u.status='active'
                 AND u.id = CASE WHEN relations.first_id = $1
                                 THEN relations.second_id ELSE relations.first_id END
     WHERE ($2::timestamptz IS NULL OR (relations.updated_at, relations.id) < ($2,$3))
     ORDER BY relations.updated_at DESC, relations.id DESC LIMIT 51
④ accountType != 'certified_public' かつ kind != 'friends' → bio = ""
⑤ status フィールドを決める
     incoming-requests / outgoing-requests → "pending"
     それ以外 → kind の値そのまま
⑥ hasMore = (件数 > 50) → 先頭50件
```

**レスポンス** `200 OK`

```json
{
  "items": [
    {
      "relationId": "uuid",
      "user": { …relationshipUserResponse… },
      "status": "friends",
      "updatedAt": "2026-10-31T03:00:00Z"
    }
  ],
  "hasMore": false
}
```

`status` の値は `friends` / `following` / `followers` / `pending` のいずれか。

**エラー**: 400 `invalid_relationship` / 400 `invalid_pagination` / 401 `invalid_session` / 403 `region_mismatch`

> `block_relations` には `updated_at` が無いため、**ブロック一覧の API は存在しない**。

---

### `DELETE /api/friendships/:userId`

| 項目 | 内容 |
|---|---|
| 処理内容 | フレンド関係を解除 |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `relationship_api.go` |

**処理の流れ**

```
① beginRelationshipTx(自分, userId)
② DELETE FROM friendships
     WHERE user_low_id = min(自分,相手) AND user_high_id = max(自分,相手)
③ RowsAffected == 0 → 409 relationship_conflict
④ COMMIT
```

**レスポンス** `204 No Content`

**エラー**: 400 `invalid_relationship` / 401 `invalid_session` / 403 `region_mismatch` / 404 `relationship_not_found` / 409 `relationship_conflict`

---

### `PUT /api/follows/:userId`

| 項目 | 内容 |
|---|---|
| 処理内容 | 認定公開アカウントをフォローする |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `relationship_api.go` |

**処理の流れ**

```
① beginRelationshipTx(自分, userId)
② getUser(userId)
③ accountType != 'certified_public' → 409 relationship_conflict
     ★一般ユーザーはフォローできない
④ isBlocked(双方向) → true なら 409 relationship_conflict
⑤ 既に follows 行がある → 409 relationship_conflict
⑥ INSERT follows (uuid, follower_id=自分, followee_id=相手)
⑦ COMMIT
```

**レスポンス** `204 No Content`

**エラー**: 400 `invalid_relationship` / 401 `invalid_session` / 403 `region_mismatch` / 404 `relationship_not_found` / 409 `relationship_conflict`

---

### `DELETE /api/follows/:userId`

| 項目 | 内容 |
|---|---|
| 処理内容 | フォローを外す |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `relationship_api.go` |

**処理の流れ**

```
① beginRelationshipTx(自分, userId)
② DELETE FROM follows WHERE follower_id=自分 AND followee_id=相手
③ RowsAffected == 0 → 409 relationship_conflict
④ COMMIT
```

**レスポンス** `204 No Content`

**エラー**: 400 `invalid_relationship` / 401 `invalid_session` / 403 `region_mismatch` / 404 `relationship_not_found` / 409 `relationship_conflict`

---

### `PUT /api/blocks/:userId`

| 項目 | 内容 |
|---|---|
| 処理内容 | ユーザーをブロックし、既存の関係を一括で解消する |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `relationship_api.go` |

**処理の流れ**

```
① beginRelationshipTx(自分, userId)
② 既に自分 → 相手 のブロックがある → 409 relationship_conflict
     ※相手 → 自分 のブロックは妨げにならない（両方向に行を作れる）
③ INSERT block_relations (uuid, blocker_id=自分, blocked_id=相手)
④ DELETE FROM friendships WHERE user_low_id=min AND user_high_id=max
⑤ DELETE FROM follows
     WHERE (follower=自分 AND followee=相手) OR (follower=相手 AND followee=自分)
     ← 双方向のフォローを削除
⑥ UPDATE friend_requests SET status='cancelled', updated_at=now
     WHERE status='pending' AND （どちらの向きでも）
⑦ COMMIT
```

**レスポンス** `204 No Content`

**エラー**: 400 `invalid_relationship` / 401 `invalid_session` / 403 `region_mismatch` / 404 `relationship_not_found` / 409 `relationship_conflict`

**ブロックの効果**

| 対象 | 挙動 |
|---|---|
| 投稿の閲覧 | `canView` が双方向で false → フィードからも消える |
| ユーザー取得 | `GET /api/users/:userId` が 404 |
| ユーザー検索 | `GET /api/users` の結果から除外 |
| フレンド申請 | 409 |
| フォロー | 409 |

---

### `DELETE /api/blocks/:userId`

| 項目 | 内容 |
|---|---|
| 処理内容 | ブロックを解除 |
| 認証 | ユーザー |
| リージョン | **要** |
| 実装 | `relationship_api.go` |

**処理の流れ**

```
① beginRelationshipTx(自分, userId)
② DELETE FROM block_relations WHERE blocker_id=自分 AND blocked_id=相手
③ RowsAffected == 0 → 403 relationship_action_forbidden   ★他と違い 403
④ COMMIT
```

**レスポンス** `204 No Content`

**エラー**: 400 `invalid_relationship` / 401 `invalid_session` / 403 `region_mismatch` / 403 `relationship_action_forbidden` / 404 `relationship_not_found`

> ブロック解除しても、④〜⑥で消えた friendships / follows / friend_requests は**復活しない**。

---

## 2.14 管理者

### `POST /api/admin/auth/login`

| 項目 | 内容 |
|---|---|
| 処理内容 | 管理者ログイン |
| 認証 | なし |
| リージョン | 不要 |
| 実装 | `admin_api.go`, `admin_session.go` |

**リクエスト** `application/json`

```json
{ "username": "admin", "password": "…" }
```

**処理の流れ**

```
① username がパターン不一致 → 401 invalid_credentials
② SELECT id, password_hash FROM administrators WHERE username = ?
     行なし → 401 invalid_credentials
③ hex(SHA256(password)) != password_hash → 401 invalid_credentials
④ CreateAdminSession:
     ├─ INSERT administrator_sessions (uuid, administrator_id, token_hash)
     └─ ★既存セッションの無効化はしない（複数セッション可）
```

**レスポンス** `200 OK`

```json
{ "token": "uuid", "administratorId": "uuid" }
```

**エラー**: 401 `invalid_credentials`

---

### `POST /api/admin/auth/logout`

| 項目 | 内容 |
|---|---|
| 処理内容 | 管理者セッションを無効化 |
| 認証 | 管理者 |
| リージョン | 不要 |
| 実装 | `admin_api.go`, `admin_session.go` |

**処理の流れ**: `UPDATE administrator_sessions SET active=false, invalidated_at=now WHERE administrator_id=? AND token_hash=? AND active=true`（トランザクションなし）

**レスポンス** `204 No Content`

**エラー**: 401 `invalid_administrator_session`

---

### `PUT /api/admin/users/:userId/certification`

| 項目 | 内容 |
|---|---|
| 処理内容 | ユーザーを認定公開アカウント（`certified_public`）にする |
| 認証 | 管理者 |
| リージョン | 不要 |
| 実装 | `admin_user_api.go` |

**処理の流れ**

```
① UPDATE users SET account_type='certified_public', updated_at=now
     WHERE id=? AND status='active' AND account_type='regular'
② RowsAffected == 0 → 409 certification_conflict
     （存在しない・削除済み・既に認定済み のいずれも同じコード）
```

**レスポンス** `204 No Content`

**エラー**: 401 `invalid_administrator_session` / 409 `certification_conflict`

**認定の効果**

| 項目 | 挙動 |
|---|---|
| フォロー | **フォロー可能になる**（`regular` は不可） |
| `bio` の公開 | 検索結果・プロフィール・関係性一覧で `bio` がマスクされなくなる |

---

### `DELETE /api/admin/users/:userId/certification`

| 項目 | 内容 |
|---|---|
| 処理内容 | **常に失敗する**（認定の取り消しは禁止） |
| 認証 | 管理者 |
| リージョン | 不要 |
| 実装 | `admin_user_api.go` |

**処理の流れ**: 無条件に 409 を返すだけ。DB アクセスなし。

**レスポンス**: なし

**エラー**: 409 `certification_irreversible`（常にこれ）/ 401 `invalid_administrator_session`

> ソースのコメント: 「2026-08-26: 認定されたユーザーを戻すことは禁止する」

---

### `POST /api/admin/posting-window`

| 項目 | 内容 |
|---|---|
| 処理内容 | 新しい投稿枠を開き、対象リージョンの全ユーザーに通知を作る |
| 認証 | 管理者 |
| リージョン | 不要（`region` はボディで指定） |
| 実装 | `admin_posting_window_api.go` |

**リクエスト** `application/json`

```json
{ "region": "asia", "localDate": "2026-10-31", "onTimeSeconds": 3600 }
```

| フィールド | 必須 | 内容 |
|---|---|---|
| `region` | **必須** | `america` / `europe` / `asia` |
| `localDate` | **必須** | `2006-01-02` 形式 |
| `onTimeSeconds` | — | 省略時 `86400`（24時間） |

**処理の流れ**

```
① region 不正 または localDate パース失敗 → 400 invalid_posting_window
② seconds = onTimeSeconds ?? 86400
③ トランザクション開始
④ LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE
     ← 通知の一括 INSERT とユーザー登録・リージョン変更を直列化するため
⑤ SELECT EXISTS(SELECT 1 FROM posting_windows
                WHERE region=? AND local_date >= ?)
     → true なら 409 posting_window_conflict
     ★「指定日以降の枠が既にある」＝過去に戻れない
⑥ startsAt = SELECT clock_timestamp()
⑦ UPDATE posting_windows SET ended_at = startsAt
     WHERE region=? AND ended_at IS NULL        ← 旧枠を閉じる
⑧ INSERT posting_windows (uuid, region, local_date,
                           starts_at = startsAt,
                           on_time_deadline = startsAt + seconds秒)
⑨ INSERT INTO notifications (id, user_id, posting_window_id, created_at)
     SELECT gen_random_uuid()::text, id, <windowID>, startsAt
     FROM users WHERE region = ? AND status = 'active'
     ← 対象リージョンの全 active ユーザー分を一括 INSERT
⑩ 読み戻して COMMIT
```

**レスポンス** `201 Created`

```json
{
  "id": "uuid",
  "region": "asia",
  "localDate": "2026-10-31",
  "startsAt": "2026-10-31T03:00:00Z",
  "onTimeDeadline": "2026-10-31T04:00:00Z"
}
```

**エラー**: 400 `invalid_posting_window` / 401 `invalid_administrator_session` / 409 `posting_window_conflict`

**この API が引き起こす連鎖**

```
旧枠を閉じる  →  旧枠の投稿が canView で false になる（他人からは見えなくなる）
             →  旧枠の投稿は PATCH できなくなる（post_not_editable）
             →  旧枠の投稿へのコメントが canComment で false になる
新枠を開く    →  全ユーザーの canPost が true に戻る（削除回数 < 2 の範囲で）
             →  HasPostedToday が false に戻る（＝再び「投稿しないと見えない」状態）
通知を作る    →  SSE 接続中のクライアントに配信される
```

---

# 第3部 付録

## 3.1 エンドポイント一覧

| # | メソッド | パス | 認証 | Region | ログ上の action 名 | 成功 |
|---|---|---|---|---|---|---|
| 1 | POST | `/api/initialize` | なし | — | `initialize` | 200 |
| 2 | POST | `/api/auth/register` | なし | — | `register` | 201 |
| 3 | POST | `/api/auth/login` | なし | — | `login` | 200 |
| 4 | POST | `/api/auth/logout` | ユーザー | — | `logout` | 204 |
| 5 | GET | `/api/users` | ユーザー | 要 | `search_users` | 200 |
| 6 | GET | `/api/users/:userId` | ユーザー | 要 | `get_user` | 200 |
| 7 | GET | `/api/me/export` | ユーザー | 要 | `export_posts` | 200 (zip) |
| 8 | GET | `/api/me` | ユーザー | 要 | `get_me` | 200 |
| 9 | PATCH | `/api/me` | ユーザー | — | `update_me` | 200 |
| 10 | PUT | `/api/me/profile-image` | ユーザー | — | `put_profile_image` | 204 |
| 11 | DELETE | `/api/me/profile-image` | ユーザー | — | `delete_profile_image` | 204 |
| 12 | POST | `/api/me/account-deletion` | ユーザー | 要 | `delete_account` | 204 |
| 13 | POST | `/api/posts` | ユーザー | 要 | `create_post` | 201 |
| 14 | GET | `/api/posting-window/current` | ユーザー | 要 | `get_current_posting_window` | 200 |
| 15 | GET | `/api/posts/me/current` | ユーザー | 要 | `get_my_current_post` | 200 |
| 16 | GET | `/api/posts/:postId` | ユーザー | 要 | `get_post` | 200 |
| 17 | PATCH | `/api/posts/:postId` | ユーザー | 要 | `update_post` | 200 |
| 18 | DELETE | `/api/posts/:postId` | ユーザー | 要 | `delete_post` | 204 |
| 19 | GET | `/api/feeds/:feed` | ユーザー | 要 | `get_feed` | 200 |
| 20 | GET | `/api/memories` | ユーザー | 要 | `get_memories` | 200 |
| 21 | GET | `/api/memories/:date` | ユーザー | 要 | `get_memories_by_date` | 200 |
| 22 | POST | `/api/posts/:postId/comments` | ユーザー | 要 | `create_comment` | 201 |
| 23 | GET | `/api/posts/:postId/comments` | ユーザー | 要 | `get_comments` | 200 |
| 24 | DELETE | `/api/comments/:commentId` | ユーザー | 要 | `delete_comment` | 204 |
| 25 | POST | `/api/posts/:postId/realmojis` | ユーザー | 要 | `replace_real_moji` | 201 |
| 26 | GET | `/api/posts/:postId/realmojis` | ユーザー | 要 | `get_real_mojis` | 200 |
| 27 | DELETE | `/api/realmojis/:realMojiId` | ユーザー | 要 | `delete_real_moji` | 204 |
| 28 | GET | `/api/images/:imageId` | **なし** | — | `get_image` | 200 (bin) |
| 29 | GET | `/api/notifications` | ユーザー | 要 | `get_notifications` | 200 (SSE) |
| 30 | POST | `/api/friend-requests` | ユーザー | 要 | `create_friend_request` | 201 |
| 31 | POST | `/api/friend-requests/:requestId/:action` | ユーザー | 要 | `transition_friend_request` | 200 |
| 32 | GET | `/api/relationships/:kind` | ユーザー | 要 | `get_relationships` | 200 |
| 33 | DELETE | `/api/friendships/:userId` | ユーザー | 要 | `delete_friendship` | 204 |
| 34 | PUT | `/api/follows/:userId` | ユーザー | 要 | `follow` | 204 |
| 35 | DELETE | `/api/follows/:userId` | ユーザー | 要 | `unfollow` | 204 |
| 36 | PUT | `/api/blocks/:userId` | ユーザー | 要 | `block` | 204 |
| 37 | DELETE | `/api/blocks/:userId` | ユーザー | 要 | `unblock` | 204 |
| 38 | POST | `/api/admin/auth/login` | なし | — | `admin_login` | 200 |
| 39 | POST | `/api/admin/auth/logout` | 管理者 | — | `admin_logout` | 204 |
| 40 | PUT | `/api/admin/users/:userId/certification` | 管理者 | — | `certify_user` | 204 |
| 41 | DELETE | `/api/admin/users/:userId/certification` | 管理者 | — | `delete_user_certification` | （常に409） |
| 42 | POST | `/api/admin/posting-window` | 管理者 | — | `open_posting_window` | 201 |

## 3.2 エラーコード索引

| コード | HTTP | 発生箇所 |
|---|---|---|
| `ip_banned` | 403 | 全エンドポイント（`IPBanMiddleware`） |
| `invalid_session` | 401 | 認証必須の全エンドポイント |
| `invalid_administrator_session` | 401 | 管理者認証必須のエンドポイント |
| `region_mismatch` | 403 | `RegionMiddleware` 付きの全エンドポイント |
| `invalid_registration` | 400 | `POST /api/auth/register` |
| `username_taken` | 409 | `POST /api/auth/register` |
| `invalid_credentials` | 401 | `POST /api/auth/login`, `POST /api/admin/auth/login` |
| `account_not_found` | 404 | `GET/PATCH /api/me`, プロフィール画像, アカウント削除 |
| `invalid_profile` | 400 | `PATCH /api/me` |
| `invalid_profile_image` | 400 | `PUT /api/me/profile-image` |
| `unsupported_media_type` | 415 | `PUT /api/me/profile-image` |
| `invalid_post` | 400 | `POST /api/posts`, `PATCH /api/posts/:postId`, **`GET /api/feeds/:feed`（feed名不正）** |
| `post_slot_unavailable` | 409 | `POST /api/posts`, `DELETE /api/posts/:postId` |
| `post_not_found` | 404 | `GET /api/posts/*`（権限不足も含む）, `PATCH/DELETE /api/posts/:postId` |
| `post_not_editable` | 403 | `PATCH/DELETE /api/posts/:postId`（他人の投稿・枠終了） |
| `invalid_pagination` | 400 | カーソルを持つ全一覧系 |
| `invalid_memory_period` | 400 | `GET /api/memories`, `GET /api/memories/:date` |
| `invalid_interaction` | 400 | `POST /api/posts/:postId/comments`（body 長） |
| `invalid_realmoji` | 400 | `POST /api/posts/:postId/realmojis` |
| `interaction_forbidden` | 403 | コメント / RealMoji の作成・一覧・削除 |
| `interaction_not_found` | 404 | `DELETE /api/comments/:commentId`, `DELETE /api/realmojis/:realMojiId` |
| `invalid_relationship` | 400 | 関係性系（自分自身・空ID）, `GET /api/users`（q不正）, `GET /api/relationships/:kind`（kind不正） |
| `relationship_not_found` | 404 | 関係性系（相手が存在しない・非active）, `GET /api/users/:userId` |
| `relationship_conflict` | 409 | 関係性系（重複・ブロック・対象外） |
| `relationship_action_forbidden` | 403 | 申請の遷移（権限・状態）, `DELETE /api/blocks/:userId` |
| `image_forbidden` | 403 | `GET /api/images/:imageId`（全失敗ケース） |
| `certification_conflict` | 409 | `PUT /api/admin/users/:userId/certification` |
| `certification_irreversible` | 409 | `DELETE /api/admin/users/:userId/certification`（常に） |
| `invalid_posting_window` | 400 | `POST /api/admin/posting-window` |
| `posting_window_conflict` | 409 | `POST /api/admin/posting-window` |

## 3.3 ファイル対応表

| ファイル | 行数 | 役割 |
|---|---|---|
| `cmd/isureal/main.go` | 11 | エントリポイント。`:8080` で listen |
| `server.go` | 91 | **全ルーティング定義 + DB初期化 + ミドルウェア登録** |
| `config.go` | 46 | 環境変数の読み込み、DSN 構築 |
| `region.go` | 22 | `RegionMiddleware` |
| `api_error.go` | 47 | `APIError` 型と `APIErrorHandler` |
| `action_log.go` | 111 | アクションログ（`actionNames` が全ルートの一覧） |
| `ip_ban.go` | 81 | `ip_bans` の 200ms キャッシュ、`X-Client-IP` 抽出 |
| `session.go` | 162 | ユーザーセッションの検証・発行・無効化 |
| `admin_session.go` | 115 | 管理者セッション |
| `user.go` | 65 | `User` 型、バリデーション関数、`userColumns` |
| `user_api.go` | 237 | register / login / logout / getMe / updateMe |
| `user_read_api.go` | 121 | ユーザー検索 / ユーザー取得 |
| `profile_image_api.go` | 97 | プロフィール画像の設定・削除 |
| `account_deletion_api.go` | 39 | アカウント論理削除 |
| `export_api.go` | 70 | 全投稿の ZIP エクスポート |
| `posting_window.go` | 23 | `PostingWindow` 型、取得関数 |
| `posting_window_api.go` | 60 | 開催中の枠と投稿状況 |
| `post.go` | 230 | **`viewerContext` / `canView` / 友だち・ブロック取得** |
| `post_api.go` | 375 | 投稿の作成・取得・更新・削除 |
| `feed_api.go` | 202 | 4種フィード、レコメンドスコア |
| `memory_api.go` | 146 | メモリー（過去の自分の投稿） |
| `comment.go` | 73 | `Comment` 型、`canComment` |
| `comment_api.go` | 172 | コメントの作成・一覧・削除 |
| `realmoji.go` | 47 | `RealMoji` 型、現役 RealMoji の取得 |
| `realmoji_api.go` | 203 | RealMoji の作成・一覧・削除 |
| `image_signature.go` | 42 | 署名付きURL生成（HMAC 1000回反復） |
| `image_api.go` | 98 | 画像配信と署名検証 |
| `notification_api.go` | 82 | SSE 通知（ワンショット） |
| `relationship.go` | 73 | `beginRelationshipTx` / `isBlocked` / 共通ヘルパ |
| `relationship_api.go` | 183 | フレンド解除 / フォロー / ブロック |
| `relationship_list_api.go` | 102 | 関係性一覧（5種） |
| `friend_request_api.go` | 164 | フレンド申請の作成・遷移 |
| `admin_api.go` | 69 | 管理者ログイン / ログアウト |
| `admin_user_api.go` | 39 | 認定公開アカウントの付与・（禁止された）解除 |
| `admin_posting_window_api.go` | 88 | 投稿枠のオープン |
| `initialize.go` | 32 | `/api/initialize` |

## 3.4 実装上の注意点・引っかかりどころ

仕様として把握しておくべき「素直でない」挙動を集約する。すべてソースから読み取った事実。

### ステータスコード / エラーコードの揺れ

| 箇所 | 挙動 |
|---|---|
| `GET /api/feeds/:feed` の feed 名不正 | コードは `invalid_post`（`invalid_feed` ではない） |
| `GET /api/posts/:postId` の権限不足 | **404** `post_not_found`（403 ではない） |
| コメント / RealMoji 作成時の投稿なし | **403** `interaction_forbidden`（404 ではない） |
| `DELETE /api/blocks/:userId` の対象なし | **403** `relationship_action_forbidden`（他の関係性系は 409） |
| `DELETE /api/me/profile-image` で画像未設定 | **404** `account_not_found`（画像が無いだけでも） |
| `PUT /api/admin/users/:userId/certification` | 存在しない / 削除済み / 既に認定済み すべて 409 `certification_conflict` |

### 権限判定の非対称性

- `GET /api/posts/:postId/comments` は `canComment` を使う → **投稿は見えてもコメントが読めない**ケースがある（`friends_of_friends` 公開の投稿を、友だちの友だちが見る場合）
- `GET /api/posts/:postId/realmojis` は `canView` を使う → こちらは見える
- `DELETE /api/posts/:postId` は枠の終了をチェックしない → 過去の枠の投稿も削除できる（削除回数 < 2 なら）
- `DELETE /api/comments/:commentId` / `DELETE /api/realmojis/:realMojiId` も可視性・枠をチェックしない

### リージョン絡み

- `PATCH /api/me` に `RegionMiddleware` が無いため、**どのサーバからでもリージョンを変更できる**
- `GET /api/posting-window/current` の `LEFT JOIN posting_windows posted_window ON posted_window.local_date = w.local_date` に**リージョン条件が無い**
- `POST /api/posts` の投稿数カウントも `w.local_date` 基準でリージョンを問わない
- `GET /api/feeds/:feed` の候補取得は `w.ended_at IS NULL` のみで、**リージョンを絞っていない**（他リージョンで開催中の枠の投稿も候補に入る）

### 一意性・ロック

- `users.username` は UNIQUE インデックス `users_username` で担保。`register` は `INSERT … ON CONFLICT (username) DO NOTHING` の 1 文で登録する
- `POST /api/admin/posting-window` は `LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE` を取る。`register` の `INSERT` が取る `ROW EXCLUSIVE` と競合するため、通知の一括 INSERT とユーザー登録は直列化される
- 書き込み系ハンドラはほぼ全てが `SELECT id FROM users WHERE id=? FOR NO KEY UPDATE` で自分（と相手）の行をロック → ユーザー単位で直列化
- 2者を扱う場合は必ず `min(id) → max(id)` の順でロック（`beginRelationshipTx`、`createCommentHandler`）

### クエリの重さ

- `GET /api/feeds/:feed`: SQL に `LIMIT` が無く、開催中の枠に属する**全ユーザーの全投稿**を取得して Go 側で絞り込む。`friends-of-friends` では SQL カーソルも無効化される
- `GET /api/users`: `LIMIT` が無く、**全 active ユーザー**を取得して Go 側で部分一致判定
- `GET /api/me/export`: `LIMIT` が無く、全投稿の画像バイナリをメモリ上の ZIP に詰める
- `signedImageURL`: HMAC-SHA256 を **1000 回反復**。レスポンスに含まれる画像URL1本ごとに実行される
- `User.setImageKey()`: `selectPostsWithAuthor` / `selectCommentsWithAuthor` / `selectCurrentRealMojis` が取得行ごとに呼ぶ
- `GET /api/notifications`: 接続1本あたり 500ms 間隔で2クエリ
- `posts` のインデックスは `posts_author_id` と `posts_posting_window_id` のみ（`posted_at` 系の複合インデックスは無い）

### その他

- `POST /api/posts` の投稿枠取得は、`sql.ErrNoRows` のときに**同じクエリをもう一度実行**している（意味を持たないリトライ）
- `session.go` の `CreateLoginSession` は未使用（コメント「2026-09-03: リファクタリング後にこの関数の利用がないようです。消すのはいかがでしょうか」）
- `001-schema.sql` の `DROP TABLE` 文には `app_settings` / `worker_jobs` / `idempotency_records` が並ぶが、これらは CREATE されていない
- `image_objects` は `created_at` による月次パーティション。`2027-07-01` 以降と範囲外は `image_objects_default` に入る
- `POST /api/posts` は `image_objects.created_at` に `postedAt`（Go 側の時刻）を明示するが、RealMoji / プロフィール画像は DB の `DEFAULT clock_timestamp()` に任せる
- `block_relations` に `updated_at` が無いため、ブロック一覧 API が存在しない
- 管理者は削除できない（スキーマのコメント: 「2026-09-13 消すことは想定されていません」）
