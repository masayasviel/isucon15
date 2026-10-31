-- session.go: getSessionFromHeader が認証付き全リクエストで token_hash を引くが、
-- login_sessions には id の PK しか無い。
-- ログインで行が増えるほど全API が線形に遅くなると考えるため追加。
CREATE INDEX IF NOT EXISTS login_sessions_token_hash ON login_sessions (token_hash);
