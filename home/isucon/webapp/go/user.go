package isureal

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"
)

type User struct {
	ID             string  `db:"id" json:"id"`
	Username       string  `db:"username" json:"username"`
	SecretKeyHash  string  `db:"secret_key_hash" json:"-"`
	DisplayName    string  `db:"display_name" json:"displayName"`
	Bio            string  `db:"bio" json:"bio"`
	ProfileImageID *string `db:"profile_image_id" json:"-"`
	Region         string  `db:"region" json:"region"`
	AccountType    string  `db:"account_type" json:"accountType"`
	Status         string  `db:"status" json:"status"`
	ImageKey       []byte  `db:"-" json:"-"`
}

var usernamePattern = regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)

func validUserText(value string, minimum, maximum int) bool {
	length := utf8.RuneCountInString(value)
	return utf8.ValidString(value) && length >= minimum && length <= maximum
}

func validRegion(region string) bool {
	switch region {
	case "america", "europe", "asia":
		return true
	default:
		return false
	}
}

var userColumns = []string{"id", "username", "secret_key_hash", "display_name", "bio", "profile_image_id", "region", "account_type", "status"}

func userColumnsAs(alias, prefix string) string {
	columns := make([]string, len(userColumns))
	for i, column := range userColumns {
		columns[i] = fmt.Sprintf(`%s.%s AS "%s.%s"`, alias, column, prefix, column)
	}
	return strings.Join(columns, ", ")
}

// userJSONObject は alias のユーザー行を userColumns と同じキーを持つ JSON オブジェクトにする SQL 式を返す。
// 結果は userJSON に復元できる。
func userJSONObject(alias string) string {
	pairs := make([]string, len(userColumns))
	for i, column := range userColumns {
		pairs[i] = fmt.Sprintf(`'%s', %s.%s`, column, alias, column)
	}
	return "json_build_object(" + strings.Join(pairs, ", ") + ")"
}

// userJSON は userJSONObject で生成した JSON を受け取るための構造体。
type userJSON struct {
	ID             string  `json:"id"`
	Username       string  `json:"username"`
	SecretKeyHash  string  `json:"secret_key_hash"`
	DisplayName    string  `json:"display_name"`
	Bio            string  `json:"bio"`
	ProfileImageID *string `json:"profile_image_id"`
	Region         string  `json:"region"`
	AccountType    string  `json:"account_type"`
	Status         string  `json:"status"`
}

func (row userJSON) user() (User, error) {
	user := User{
		ID: row.ID, Username: row.Username, SecretKeyHash: row.SecretKeyHash, DisplayName: row.DisplayName,
		Bio: row.Bio, ProfileImageID: row.ProfileImageID, Region: row.Region, AccountType: row.AccountType, Status: row.Status,
	}
	return user, user.setImageKey()
}

func (user *User) setImageKey() error {
	key, err := imageSigningKey(user.SecretKeyHash)
	user.ImageKey = key
	return err
}

func getUser(ctx context.Context, db sqlx.QueryerContext, userID string) (User, error) {
	var user User
	err := sqlx.GetContext(ctx, db, &user, `SELECT `+strings.Join(userColumns, ", ")+` FROM users WHERE id = $1 AND status = 'active'`, userID)
	if err != nil {
		return user, err
	}
	return user, user.setImageKey()
}

func lockUser(ctx context.Context, db sqlx.QueryerContext, userID string) (User, error) {
	var user User
	err := sqlx.GetContext(ctx, db, &user, `SELECT `+strings.Join(userColumns, ", ")+` FROM users WHERE id = $1 AND status = 'active' FOR NO KEY UPDATE`, userID)
	if err != nil {
		return user, err
	}
	return user, user.setImageKey()
}
