package isureal

import (
	"context"
	"errors"
	"io/fs"
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
