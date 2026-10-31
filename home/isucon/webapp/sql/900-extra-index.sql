-- index追加用
-- #17 GET /api/images/:imageId の権限チェック (image_api.go)
CREATE INDEX IF NOT EXISTS realmojis_image_id_reactor_id ON realmojis(image_id, reactor_id) WHERE replaced_at IS NULL AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS posts_image_id ON posts(image_id);
-- #31 GET /api/users のユーザー検索 (user_read_api.go searchUsersHandler)
CREATE INDEX IF NOT EXISTS users_active_username_id ON users(username, id) WHERE status = 'active';
-- #30 friendships の user_low_id = ? OR user_high_id = ? 検索 (post.go loadViewerContext / getFriendIDs, feed_api.go recommendationScores, relationship_list_api.go)
CREATE INDEX IF NOT EXISTS friendships_user_high_id ON friendships(user_high_id);
-- #28 POST /api/auth/register のユーザー名の一意性 (user_api.go)
-- ON CONFLICT (username) がこのインデックスに依存するため、コードより先に適用する
CREATE UNIQUE INDEX IF NOT EXISTS users_username ON users(username);
-- #29 pending の友達申請の検索と一覧 (friend_request_api.go, relationship_api.go, relationship_list_api.go)
CREATE INDEX IF NOT EXISTS friend_requests_requester_id_recipient_id_pending ON friend_requests(requester_id, recipient_id) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS friend_requests_recipient_id_requester_id_pending ON friend_requests(recipient_id, requester_id) WHERE status = 'pending';
-- #51 POST /api/admin/posting-window の通知一括INSERT (admin_posting_window_api.go openPostingWindowHandler)
CREATE INDEX IF NOT EXISTS users_active_region ON users(region) WHERE status = 'active';
-- #62 GET /api/users の部分一致を SQL 側で行うための列 (user_read_api.go searchUsersHandler)
-- 値は Go の normalizeSearchText(display_name)。登録・表示名変更時に書き、initialize と起動時に全行を突き合わせて埋める。
-- コードより先に適用する
ALTER TABLE users ADD COLUMN IF NOT EXISTS display_name_search text;
