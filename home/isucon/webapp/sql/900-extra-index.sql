-- index追加用
-- #17 GET /api/images/:imageId の権限チェック (image_api.go)
CREATE INDEX IF NOT EXISTS realmojis_image_id_reactor_id ON realmojis(image_id, reactor_id) WHERE replaced_at IS NULL AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS posts_image_id ON posts(image_id);
-- #26 loadViewerContext と getFriendIDs / getBlockedUserIDs の逆方向の検索 (post.go)
CREATE INDEX IF NOT EXISTS friendships_user_high_id ON friendships(user_high_id);
CREATE INDEX IF NOT EXISTS block_relations_blocked_id ON block_relations(blocked_id);
