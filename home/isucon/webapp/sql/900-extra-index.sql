-- index追加用
-- #17 GET /api/images/:imageId の権限チェック (image_api.go)
CREATE INDEX IF NOT EXISTS realmojis_image_id_reactor_id ON realmojis(image_id, reactor_id) WHERE replaced_at IS NULL AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS posts_image_id ON posts(image_id);
-- #28 POST /api/auth/register のユーザー名の一意性 (user_api.go)
-- ON CONFLICT (username) がこのインデックスに依存するため、コードより先に適用する
CREATE UNIQUE INDEX IF NOT EXISTS users_username ON users(username);
