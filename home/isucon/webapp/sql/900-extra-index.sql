-- index追加用
-- #17 GET /api/images/:imageId の権限チェック (image_api.go)
CREATE INDEX IF NOT EXISTS realmojis_image_id_reactor_id ON realmojis(image_id, reactor_id) WHERE replaced_at IS NULL AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS posts_image_id ON posts(image_id);
-- #31 GET /api/users のユーザー検索 (user_read_api.go searchUsersHandler)
CREATE INDEX IF NOT EXISTS users_active_username_id ON users(username, id) WHERE status = 'active';
