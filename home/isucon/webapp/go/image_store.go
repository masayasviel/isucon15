package isureal

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// 画像の実体のキャッシュ。DB が正本で、ファイルが無ければ DB から補完する。
const (
	imageDir                = "/home/isucon/images"
	imageInternalPathPrefix = "/images-internal/"
	imageTempFilePrefix     = ".tmp-"
)

func imageFileName(imageID string) (string, bool) {
	id, err := uuid.Parse(imageID)
	if err != nil || id.String() != imageID {
		return "", false
	}
	return imageID, true
}

func imageFileExists(name string) bool {
	_, err := os.Stat(filepath.Join(imageDir, name))
	return err == nil
}

// 書き込み途中のファイルを配信しないよう、一時ファイルから rename する。
func writeImageFile(imageID string, data []byte) error {
	name, ok := imageFileName(imageID)
	if !ok {
		return nil
	}
	f, err := os.CreateTemp(imageDir, imageTempFilePrefix+name+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(imageDir, name))
}

// 画像の実体をファイルから読み、ファイルが無いものだけ DB からまとめて取得してファイルへ書き出す。
// DB にも無い画像は結果に含めない。
func loadImageData(ctx context.Context, db sqlx.QueryerContext, imageIDs []string) (map[string][]byte, error) {
	images := make(map[string][]byte, len(imageIDs))
	missing := make([]string, 0)
	for _, id := range imageIDs {
		if _, ok := images[id]; ok {
			continue
		}
		if name, ok := imageFileName(id); ok {
			if data, err := os.ReadFile(filepath.Join(imageDir, name)); err == nil {
				images[id] = data
				continue
			}
		}
		missing = append(missing, id)
	}
	if len(missing) == 0 {
		return images, nil
	}
	query, args, err := sqlx.In(`SELECT id, data FROM image_objects WHERE id IN (?)`, missing)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID   string `db:"id"`
		Data []byte `db:"data"`
	}
	if err := sqlx.SelectContext(ctx, db, &rows, sqlx.Rebind(sqlx.DOLLAR, query), args...); err != nil {
		return nil, err
	}
	for _, row := range rows {
		images[row.ID] = row.Data
	}
	// ファイルの書き出しは応答に不要なので、リクエストを待たせない。
	go func() {
		for _, row := range rows {
			_ = writeImageFile(row.ID, row.Data)
		}
	}()
	return images, nil
}

const imagePrefetchBatchSize = 100

// 次の画像を、ファイルが無いものだけ少しずつ DB から取得して書き出す。
//   - 自リージョンの利用者の投稿画像（エクスポートと Memories で読む）
//   - その投稿に付いた現役の RealMoji の画像
//   - 全リージョンの active ユーザーのプロフィール画像（他リージョンの利用者のものも画面に出る）
func prefetchImages(db *sqlx.DB, region string) {
	ctx := context.Background()
	startedAt := time.Now()
	var ids []string
	if err := db.SelectContext(ctx, &ids, `
  SELECT p.image_id FROM posts p JOIN users u ON u.id = p.author_id
  WHERE u.region = $1 AND p.deleted_at IS NULL
  UNION
  SELECT r.image_id FROM realmojis r
  JOIN posts p ON p.id = r.post_id AND p.deleted_at IS NULL
  JOIN users u ON u.id = p.author_id
  WHERE u.region = $1 AND r.replaced_at IS NULL AND r.deleted_at IS NULL
  UNION
  SELECT profile_image_id FROM users
  WHERE status = 'active' AND profile_image_id IS NOT NULL
 `, region); err != nil {
		slog.Error("failed to list images to prefetch", "error", err)
		return
	}
	missing := make([]string, 0, len(ids))
	for _, id := range ids {
		if name, ok := imageFileName(id); ok && !imageFileExists(name) {
			missing = append(missing, id)
		}
	}
	slog.Info("image prefetch started", "images", len(missing))
	for start := 0; start < len(missing); start += imagePrefetchBatchSize {
		end := min(start+imagePrefetchBatchSize, len(missing))
		if _, err := loadImageData(ctx, db, missing[start:end]); err != nil {
			slog.Error("failed to prefetch images", "error", err, "finished", start)
			return
		}
	}
	slog.Info("image prefetch finished", "images", len(missing), "elapsed", time.Since(startedAt))
}

// DB に存在しない画像のファイルを消す。
// ファイルは DB への書き込み後に作られるため、ReadDir の後に ID を取得すれば作成中の画像を消さない。
func pruneImageFiles(ctx context.Context, db *sqlx.DB) error {
	entries, err := os.ReadDir(imageDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var ids []string
	if err := db.SelectContext(ctx, &ids, `SELECT id FROM image_objects`); err != nil {
		return err
	}
	keep := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		keep[id] = struct{}{}
	}
	for _, e := range entries {
		if _, ok := keep[e.Name()]; ok || strings.HasPrefix(e.Name(), imageTempFilePrefix) {
			continue
		}
		os.Remove(filepath.Join(imageDir, e.Name()))
	}
	return nil
}

// initialize は1台にしか届かないため、各サーバが reset.sql で更新される世代番号を監視し、
// 変化したら自サーバのファイルを初期化する。起動直後にも1回実行する。
func watchImageFileGeneration(db *sqlx.DB) {
	ctx := context.Background()
	var last int64 = -1
	for {
		var generation int64
		if err := db.GetContext(ctx, &generation, `SELECT generation FROM image_file_generation WHERE id = 1`); err == nil && generation != last {
			if pruneImageFiles(ctx, db) == nil {
				last = generation
			}
		}
		time.Sleep(time.Second)
	}
}
