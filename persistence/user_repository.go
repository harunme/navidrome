package persistence

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	. "github.com/Masterminds/squirrel"
	"github.com/deluan/rest"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/id"
	"github.com/navidrome/navidrome/utils"
	"github.com/navidrome/navidrome/utils/slice"
	"github.com/pocketbase/dbx"
)

type userRepository struct {
	sqlRepository
}

type dbUser struct {
	*model.User   `structs:",flatten"`
	LibrariesJSON string `structs:"-" json:"-"`
}

func (u *dbUser) PostScan() error {
	if u.LibrariesJSON != "" {
		if err := json.Unmarshal([]byte(u.LibrariesJSON), &u.User.Libraries); err != nil {
			return fmt.Errorf("parsing user libraries from db: %w", err)
		}
	}
	return nil
}

type dbUsers []dbUser

func (us dbUsers) toModels() model.Users {
	return slice.Map(us, func(u dbUser) model.User { return *u.User })
}

var (
	once   sync.Once
	encKey []byte
)

func NewUserRepository(ctx context.Context, db dbx.Builder) model.UserRepository {
	r := &userRepository{}
	r.ctx = ctx
	r.db = db
	r.tableName = "user"
	r.registerModel(&model.User{}, map[string]filterFunc{
		"password": invalidFilter(ctx),
		"name":     r.withTableName(startsWithFilter),
	})
	once.Do(func() {
		_ = r.initPasswordEncryptionKey()
	})
	return r
}

// selectUserWithLibraries returns a SelectBuilder that includes library information
func (r *userRepository) selectUserWithLibraries(options ...model.QueryOptions) SelectBuilder {
	return r.newSelect(options...).
		Columns(`user.*`,
			`COALESCE(json_group_array(json_object(
				'id', library.id,
				'name', library.name,
				'path', library.path,
				'remote_path', library.remote_path,
				'last_scan_at', library.last_scan_at,
				'last_scan_started_at', library.last_scan_started_at,
				'full_scan_in_progress', library.full_scan_in_progress,
				'updated_at', library.updated_at,
				'created_at', library.created_at
			)) FILTER (WHERE library.id IS NOT NULL), '[]') AS libraries_json`).
		LeftJoin("user_library ul ON user.id = ul.user_id").
		LeftJoin("library ON ul.library_id = library.id").
		GroupBy("user.id")
}

func (r *userRepository) CountAll(qo ...model.QueryOptions) (int64, error) {
	return r.count(Select(), qo...)
}

func (r *userRepository) Get(id string) (*model.User, error) {
	sel := r.selectUserWithLibraries().Where(Eq{"user.id": id})
	var res dbUser
	err := r.queryOne(sel, &res)
	if err != nil {
		return nil, err
	}
	return res.User, nil
}

func (r *userRepository) GetAll(options ...model.QueryOptions) (model.Users, error) {
	sel := r.selectUserWithLibraries(options...)
	var res dbUsers
	err := r.queryAll(sel, &res)
	if err != nil {
		return nil, err
	}
	return res.toModels(), nil
}

func (r *userRepository) Put(u *model.User) error {
	if u.ID == "" {
		u.ID = id.NewRandom()
	}
	u.UpdatedAt = time.Now()
	if u.NewPassword != "" {
		_ = r.encryptPassword(u)
	}
	values, err := toSQLArgs(*u)
	if err != nil {
		return fmt.Errorf("error converting user to SQL args: %w", err)
	}
	delete(values, "current_password")

	// Save/update the user
	update := Update(r.tableName).Where(Eq{"id": u.ID}).SetMap(values)
	count, err := r.executeSQL(update)
	if err != nil {
		return err
	}

	isNewUser := count == 0
	if isNewUser {
		values["created_at"] = time.Now()
		insert := Insert(r.tableName).SetMap(values)
		_, err = r.executeSQL(insert)
		if err != nil {
			return err
		}
	}

	// Auto-assign all libraries to admin users in a single SQL operation
	if u.IsAdmin {
		sql := Expr(
			"INSERT OR IGNORE INTO user_library (user_id, library_id) SELECT ?, id FROM library",
			u.ID,
		)
		if _, err := r.executeSQL(sql); err != nil {
			return fmt.Errorf("failed to assign all libraries to admin user: %w", err)
		}
	} else if isNewUser { // Only for new regular users
		// Auto-assign default libraries to new regular users
		sql := Expr(
			"INSERT OR IGNORE INTO user_library (user_id, library_id) SELECT ?, id FROM library WHERE default_new_users = true",
			u.ID,
		)
		if _, err := r.executeSQL(sql); err != nil {
			return fmt.Errorf("failed to assign default libraries to new user: %w", err)
		}
	}

	return nil
}

func (r *userRepository) FindFirstAdmin() (*model.User, error) {
	sel := r.selectUserWithLibraries(model.QueryOptions{Sort: "updated_at", Max: 1}).Where(Eq{"user.is_admin": true})
	var usr dbUser
	err := r.queryOne(sel, &usr)
	if err != nil {
		return nil, err
	}
	return usr.User, nil
}

func (r *userRepository) FindByUsername(username string) (*model.User, error) {
	sel := r.selectUserWithLibraries().Where(Expr("user.user_name = ? COLLATE NOCASE", username))
	var usr dbUser
	err := r.queryOne(sel, &usr)
	if err != nil {
		return nil, err
	}
	return usr.User, nil
}

func (r *userRepository) FindByUsernameWithPassword(username string) (*model.User, error) {
	usr, err := r.FindByUsername(username)
	if err != nil {
		return nil, err
	}
	_ = r.decryptPassword(usr)
	return usr, nil
}

func (r *userRepository) UpdateLastLoginAt(id string) error {
	upd := Update(r.tableName).Where(Eq{"id": id}).Set("last_login_at", time.Now())
	_, err := r.executeSQL(upd)
	return err
}

func (r *userRepository) UpdateLastAccessAt(id string) error {
	now := time.Now()
	upd := Update(r.tableName).Where(Eq{"id": id}).Set("last_access_at", now)
	_, err := r.executeSQL(upd)
	return err
}

func (r *userRepository) Count(options ...rest.QueryOptions) (int64, error) {
	usr := loggedUser(r.ctx)
	if !usr.IsAdmin {
		return 0, rest.ErrPermissionDenied
	}
	return r.CountAll(r.parseRestOptions(r.ctx, options...))
}

func (r *userRepository) Read(id string) (any, error) {
	usr := loggedUser(r.ctx)
	if !usr.IsAdmin && usr.ID != id {
		return nil, rest.ErrPermissionDenied
	}
	usr, err := r.Get(id)
	if errors.Is(err, model.ErrNotFound) {
		return nil, rest.ErrNotFound
	}
	return usr, err
}

func (r *userRepository) ReadAll(options ...rest.QueryOptions) (any, error) {
	usr := loggedUser(r.ctx)
	if !usr.IsAdmin {
		return nil, rest.ErrPermissionDenied
	}
	return r.GetAll(r.parseRestOptions(r.ctx, options...))
}

func (r *userRepository) EntityName() string {
	return "user"
}

func (r *userRepository) NewInstance() any {
	return &model.User{}
}

func (r *userRepository) Save(entity any) (string, error) {
	usr := loggedUser(r.ctx)
	if !usr.IsAdmin {
		return "", rest.ErrPermissionDenied
	}
	u := entity.(*model.User)
	if err := validateUsernameUnique(r, u); err != nil {
		return "", err
	}
	err := r.Put(u)
	if err != nil {
		return "", err
	}
	return u.ID, err
}

func (r *userRepository) Update(id string, entity any, _ ...string) error {
	u := entity.(*model.User)
	u.ID = id
	usr := loggedUser(r.ctx)
	if !usr.IsAdmin && usr.ID != u.ID {
		return rest.ErrPermissionDenied
	}
	if !usr.IsAdmin {
		if !conf.Server.EnableUserEditing {
			return rest.ErrPermissionDenied
		}
		u.IsAdmin = false
		u.UserName = usr.UserName
	}

	// Decrypt the user's existing password before validating. This is required otherwise the existing password entered by the user will never match.
	if err := r.decryptPassword(usr); err != nil {
		return err
	}
	if err := validatePasswordChange(u, usr); err != nil {
		return err
	}
	if err := validateUsernameUnique(r, u); err != nil {
		return err
	}
	err := r.Put(u)
	if errors.Is(err, model.ErrNotFound) {
		return rest.ErrNotFound
	}
	return err
}

func validatePasswordChange(newUser *model.User, logged *model.User) error {
	err := &rest.ValidationError{Errors: map[string]string{}}
	if logged.IsAdmin && newUser.ID != logged.ID {
		return nil
	}
	if newUser.NewPassword == "" {
		if newUser.CurrentPassword == "" {
			return nil
		}
		err.Errors["password"] = "ra.validation.required"
	}

	if !strings.HasPrefix(logged.Password, consts.PasswordAutogenPrefix) {
		if newUser.CurrentPassword == "" {
			err.Errors["currentPassword"] = "ra.validation.required"
		}
		if newUser.CurrentPassword != logged.Password {
			err.Errors["currentPassword"] = "ra.validation.passwordDoesNotMatch"
		}
	}
	if len(err.Errors) > 0 {
		return err
	}
	return nil
}

func validateUsernameUnique(r model.UserRepository, u *model.User) error {
	usr, err := r.FindByUsername(u.UserName)
	if errors.Is(err, model.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if usr.ID != u.ID {
		return &rest.ValidationError{Errors: map[string]string{"userName": "ra.validation.unique"}}
	}
	return nil
}

func (r *userRepository) Delete(id string) error {
	usr := loggedUser(r.ctx)
	if !usr.IsAdmin {
		return rest.ErrPermissionDenied
	}
	err := r.delete(Eq{"id": id})
	if errors.Is(err, model.ErrNotFound) {
		return rest.ErrNotFound
	}
	return err
}

func keyTo32Bytes(input string) []byte {
	data := sha256.Sum256([]byte(input))
	return data[0:]
}

func (r *userRepository) initPasswordEncryptionKey() error {
	encKey = keyTo32Bytes(consts.DefaultEncryptionKey)
	if conf.Server.PasswordEncryptionKey == "" {
		return nil
	}

	key := keyTo32Bytes(conf.Server.PasswordEncryptionKey)
	keySum := fmt.Sprintf("%x", sha256.Sum256(key))

	props := NewPropertyRepository(r.ctx, r.db)
	savedKeySum, err := props.Get(consts.PasswordsEncryptedKey)

	// If passwords are already encrypted
	if err == nil {
		if savedKeySum != keySum {
			log.Error("Password Encryption Key changed! Users won't be able to login!")
			return errors.New("passwordEncryptionKey changed")
		}
		encKey = key
		return nil
	}

	// if not, try to re-encrypt all current passwords with new encryption key,
	// assuming they were encrypted with the DefaultEncryptionKey
	sql := r.newSelect().Columns("id", "user_name", "password")
	users := model.Users{}
	err = r.queryAll(sql, &users)
	if err != nil {
		log.Error("Could not encrypt all passwords", err)
		return err
	}
	log.Warn("New PasswordEncryptionKey set. Encrypting all passwords", "numUsers", len(users))
	if err = r.decryptAllPasswords(users); err != nil {
		return err
	}
	encKey = key
	for i := range users {
		u := users[i]
		u.NewPassword = u.Password
		if err := r.encryptPassword(&u); err == nil {
			upd := Update(r.tableName).Set("password", u.NewPassword).Where(Eq{"id": u.ID})
			_, err = r.executeSQL(upd)
			if err != nil {
				log.Error("Password NOT encrypted! This may cause problems!", "user", u.UserName, "id", u.ID, err)
			} else {
				log.Warn("Password encrypted successfully", "user", u.UserName, "id", u.ID)
			}
		}
	}

	err = props.Put(consts.PasswordsEncryptedKey, keySum)
	if err != nil {
		log.Error("Could not flag passwords as encrypted. It will cause login errors", err)
		return err
	}
	return nil
}

// encrypts u.NewPassword
func (r *userRepository) encryptPassword(u *model.User) error {
	encPassword, err := utils.Encrypt(r.ctx, encKey, u.NewPassword)
	if err != nil {
		log.Error(r.ctx, "Error encrypting user's password", "user", u.UserName, err)
		return err
	}
	u.NewPassword = encPassword
	return nil
}

// decrypts u.Password
func (r *userRepository) decryptPassword(u *model.User) error {
	plaintext, err := utils.Decrypt(r.ctx, encKey, u.Password)
	if err != nil {
		log.Error(r.ctx, "Error decrypting user's password", "user", u.UserName, err)
		return err
	}
	u.Password = plaintext
	return nil
}

func (r *userRepository) decryptAllPasswords(users model.Users) error {
	for i := range users {
		if err := r.decryptPassword(&users[i]); err != nil {
			return err
		}
	}
	return nil
}

// Library association methods

func (r *userRepository) GetUserLibraries(userID string) (model.Libraries, error) {
	sel := Select("l.*").
		From("library l").
		Join("user_library ul ON l.id = ul.library_id").
		Where(Eq{"ul.user_id": userID}).
		OrderBy("l.name")

	var res model.Libraries
	err := r.queryAll(sel, &res)
	return res, err
}

func (r *userRepository) SetUserLibraries(userID string, libraryIDs []int) error {
	// Remove existing associations
	delSql := Delete("user_library").Where(Eq{"user_id": userID})
	if _, err := r.executeSQL(delSql); err != nil {
		return err
	}

	// Add new associations
	if len(libraryIDs) > 0 {
		insert := Insert("user_library").Columns("user_id", "library_id")
		for _, libID := range libraryIDs {
			insert = insert.Values(userID, libID)
		}
		_, err := r.executeSQL(insert)
		return err
	}
	return nil
}

var _ model.UserRepository = (*userRepository)(nil)
var _ rest.Repository = (*userRepository)(nil)
var _ rest.Persistable = (*userRepository)(nil)
