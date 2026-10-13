package main

import (
	"database/sql"
	"time"

	_ "modernc.org/sqlite"
)

// User is a Forgejo user who logged in via OAuth. Tokens are decrypted;
// only the encrypted blobs are persisted.
type User struct {
	ID           int64
	ForgejoID    int64
	Username     string
	AccessToken  string
	RefreshToken string
	TokenExpiry  time.Time
}

// Site is one published repository.
type Site struct {
	ID          int64
	UserID      int64
	Owner       string
	Repo        string
	Branch      string
	Subdir      string
	SPAFallback bool
	UpdatedAt   time.Time
}

// Session is a login session identified by a random cookie value.
type Session struct {
	ID        string
	UserID    int64
	CSRFToken string
	ExpiresAt time.Time
}

type Store struct {
	db       *sql.DB
	tokenKey []byte
}

func openStore(path string, tokenKey []byte) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, tokenKey: tokenKey}
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY,
			forgejo_user_id INTEGER UNIQUE NOT NULL,
			username TEXT NOT NULL,
			access_token BLOB NOT NULL,
			refresh_token BLOB NOT NULL,
			token_expiry INTEGER NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			csrf_token TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS oauth_states (
			state TEXT PRIMARY KEY,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS sites (
			id INTEGER PRIMARY KEY,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			owner TEXT NOT NULL,
			repo TEXT NOT NULL,
			branch TEXT NOT NULL DEFAULT 'main',
			subdir TEXT NOT NULL DEFAULT '',
			spa_fallback INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL,
			UNIQUE(owner, repo)
		)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) close() error { return s.db.Close() }

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// upsertUser creates or updates a user from a fresh OAuth token grant.
func (s *Store) upsertUser(forgejoID int64, username, accessToken, refreshToken string, expiry time.Time) (*User, error) {
	encAccess, err := encrypt(s.tokenKey, []byte(accessToken))
	if err != nil {
		return nil, err
	}
	encRefresh, err := encrypt(s.tokenKey, []byte(refreshToken))
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	_, err = s.db.Exec(`INSERT INTO users (forgejo_user_id, username, access_token, refresh_token, token_expiry, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(forgejo_user_id) DO UPDATE SET
			username=excluded.username,
			access_token=excluded.access_token,
			refresh_token=excluded.refresh_token,
			token_expiry=excluded.token_expiry`,
		forgejoID, username, encAccess, encRefresh, expiry.Unix(), now)
	if err != nil {
		return nil, err
	}
	return s.getUserByForgejoID(forgejoID)
}

func (s *Store) getUserByForgejoID(forgejoID int64) (*User, error) {
	return s.scanUser(s.db.QueryRow(`SELECT id, forgejo_user_id, username, access_token, refresh_token, token_expiry
		FROM users WHERE forgejo_user_id = ?`, forgejoID))
}

func (s *Store) getUserByID(id int64) (*User, error) {
	return s.scanUser(s.db.QueryRow(`SELECT id, forgejo_user_id, username, access_token, refresh_token, token_expiry
		FROM users WHERE id = ?`, id))
}

func (s *Store) scanUser(row *sql.Row) (*User, error) {
	var u User
	var encAccess, encRefresh []byte
	var expiryUnix int64
	if err := row.Scan(&u.ID, &u.ForgejoID, &u.Username, &encAccess, &encRefresh, &expiryUnix); err != nil {
		return nil, err
	}
	access, err := decrypt(s.tokenKey, encAccess)
	if err != nil {
		return nil, err
	}
	refresh, err := decrypt(s.tokenKey, encRefresh)
	if err != nil {
		return nil, err
	}
	u.AccessToken = string(access)
	u.RefreshToken = string(refresh)
	u.TokenExpiry = time.Unix(expiryUnix, 0)
	return &u, nil
}

func (s *Store) updateUserTokens(id int64, accessToken, refreshToken string, expiry time.Time) error {
	encAccess, err := encrypt(s.tokenKey, []byte(accessToken))
	if err != nil {
		return err
	}
	encRefresh, err := encrypt(s.tokenKey, []byte(refreshToken))
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE users SET access_token=?, refresh_token=?, token_expiry=? WHERE id=?`,
		encAccess, encRefresh, expiry.Unix(), id)
	return err
}

var sessionTTL = 30 * 24 * time.Hour

func (s *Store) createSession(userID int64) (*Session, error) {
	id, err := randHex(32)
	if err != nil {
		return nil, err
	}
	csrf, err := randHex(32)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	sess := &Session{ID: id, UserID: userID, CSRFToken: csrf, ExpiresAt: now.Add(sessionTTL)}
	_, err = s.db.Exec(`INSERT INTO sessions (id, user_id, csrf_token, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`, id, userID, csrf, now.Unix(), sess.ExpiresAt.Unix())
	if err != nil {
		return nil, err
	}
	return sess, nil
}

func (s *Store) getSession(id string) (*Session, error) {
	var sess Session
	var expiresUnix int64
	err := s.db.QueryRow(`SELECT id, user_id, csrf_token, expires_at FROM sessions WHERE id=?`, id).
		Scan(&sess.ID, &sess.UserID, &sess.CSRFToken, &expiresUnix)
	if err != nil {
		return nil, err
	}
	sess.ExpiresAt = time.Unix(expiresUnix, 0)
	if time.Now().After(sess.ExpiresAt) {
		s.db.Exec(`DELETE FROM sessions WHERE id=?`, id)
		return nil, sql.ErrNoRows
	}
	return &sess, nil
}

func (s *Store) deleteSession(id string) {
	s.db.Exec(`DELETE FROM sessions WHERE id=?`, id)
}

func (s *Store) saveOAuthState(state string, ttl time.Duration) error {
	_, err := s.db.Exec(`INSERT INTO oauth_states (state, created_at) VALUES (?, ?)`,
		state, time.Now().Add(ttl).Unix())
	return err
}

// consumeOAuthState validates a state value once and deletes it.
func (s *Store) consumeOAuthState(state string) bool {
	var expUnix int64
	err := s.db.QueryRow(`SELECT created_at FROM oauth_states WHERE state=?`, state).Scan(&expUnix)
	s.db.Exec(`DELETE FROM oauth_states WHERE state=?`, state)
	return err == nil && time.Now().Unix() < expUnix
}

func (s *Store) listSitesByUser(userID int64) ([]Site, error) {
	rows, err := s.db.Query(`SELECT id, user_id, owner, repo, branch, subdir, spa_fallback, updated_at
		FROM sites WHERE user_id=? ORDER BY owner, repo`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSites(rows)
}

func (s *Store) getSite(owner, repo string) (*Site, error) {
	row := s.db.QueryRow(`SELECT id, user_id, owner, repo, branch, subdir, spa_fallback, updated_at
		FROM sites WHERE owner=? AND repo=?`, owner, repo)
	return scanSite(row)
}

func (s *Store) getSiteByID(id int64) (*Site, error) {
	row := s.db.QueryRow(`SELECT id, user_id, owner, repo, branch, subdir, spa_fallback, updated_at
		FROM sites WHERE id=?`, id)
	return scanSite(row)
}

func scanSite(row *sql.Row) (*Site, error) {
	var st Site
	var spa, updatedUnix int64
	if err := row.Scan(&st.ID, &st.UserID, &st.Owner, &st.Repo, &st.Branch, &st.Subdir, &spa, &updatedUnix); err != nil {
		return nil, err
	}
	st.SPAFallback = spa != 0
	st.UpdatedAt = time.Unix(updatedUnix, 0)
	return &st, nil
}

func scanSites(rows *sql.Rows) ([]Site, error) {
	var out []Site
	for rows.Next() {
		var st Site
		var spa, updatedUnix int64
		if err := rows.Scan(&st.ID, &st.UserID, &st.Owner, &st.Repo, &st.Branch, &st.Subdir, &spa, &updatedUnix); err != nil {
			return nil, err
		}
		st.SPAFallback = spa != 0
		st.UpdatedAt = time.Unix(updatedUnix, 0)
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *Store) createSite(userID int64, owner, repo, branch string) (*Site, error) {
	res, err := s.db.Exec(`INSERT INTO sites (user_id, owner, repo, branch, subdir, spa_fallback, updated_at)
		VALUES (?, ?, ?, ?, '', 0, ?)`, userID, owner, repo, branch, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.getSiteByID(id)
}

func (s *Store) updateSite(id int64, branch, subdir string, spa bool) error {
	_, err := s.db.Exec(`UPDATE sites SET branch=?, subdir=?, spa_fallback=?, updated_at=? WHERE id=?`,
		branch, subdir, boolToInt(spa), time.Now().Unix(), id)
	return err
}

func (s *Store) deleteSite(id int64) error {
	_, err := s.db.Exec(`DELETE FROM sites WHERE id=?`, id)
	return err
}

func (s *Store) cleanupExpired() error {
	now := time.Now().Unix()
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, now); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM oauth_states WHERE created_at < ?`, now)
	return err
}
