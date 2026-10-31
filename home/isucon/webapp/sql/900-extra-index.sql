-- index追加用
-- #17 GET /api/images/:imageId の権限チェック (image_api.go)
CREATE INDEX IF NOT EXISTS realmojis_image_id_reactor_id ON realmojis(image_id, reactor_id) WHERE replaced_at IS NULL AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS posts_image_id ON posts(image_id);
-- #29 pending の友達申請の検索と一覧 (friend_request_api.go, relationship_api.go, relationship_list_api.go)
CREATE INDEX IF NOT EXISTS friend_requests_requester_id_recipient_id_pending ON friend_requests(requester_id, recipient_id) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS friend_requests_recipient_id_requester_id_pending ON friend_requests(recipient_id, requester_id) WHERE status = 'pending';
