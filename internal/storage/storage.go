// Package storage 是蓬山的 SQLite 持久层：设备、配对会话、审计、元信息四表。
// 连接 modernc.org/sqlite（纯 Go 免 cgo）；token 类敏感值只存 sha256。
package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Device 是已配对设备记录。
type Device struct {
	DeviceID  string    `json:"device_id"`
	Label     string    `json:"label"`
	Platform  string    `json:"platform"`
	PubKey    []byte    `json:"pub_key"` // X25519 32B
	PairedAt  time.Time `json:"paired_at"`
	LastSeen  time.Time `json:"last_seen"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// PairingSession 是一次性配对会话（QR/短码 claim 的凭据）。
type PairingSession struct {
	SessionID string     `json:"session_id"`
	Code      string     `json:"code"`
	ExpiresAt time.Time  `json:"expires_at"`
	Scopes    []string   `json:"scopes,omitempty"`
	Note      string     `json:"note,omitempty"`
	ClaimedBy *string    `json:"claimed_by,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// AuditEvent 是审计行。
type AuditEvent struct {
	ID        int64           `json:"id"`
	At        time.Time       `json:"at"`
	ActorType string          `json:"actor_type"`
	ActorID   string          `json:"actor_id,omitempty"`
	Event     string          `json:"event"`
	Detail    json.RawMessage `json:"detail,omitempty"`
}

// Store 包装一个 SQLite 连接（串行访问：PRAGMA synchronous=NORMAL, journal=WAL）。
type Store struct {
	db *sql.DB
}

// Open 打开（不存在则创建）并迁移到最新 schema。
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite 单写者，限制连接数防 SQLITE_BUSY。
	db.SetMaxOpenConns(4)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关库。
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(schema)
	return err
}

const schema = `
CREATE TABLE IF NOT EXISTS devices (
  device_id  TEXT PRIMARY KEY,
  label      TEXT NOT NULL,
  platform   TEXT NOT NULL,
  pub_key    BLOB NOT NULL,
  paired_at  INTEGER NOT NULL,
  last_seen  INTEGER NOT NULL,
  revoked_at INTEGER
);
CREATE TABLE IF NOT EXISTS pairing_sessions (
  session_id TEXT PRIMARY KEY,
  code       TEXT NOT NULL,
  expires_at INTEGER NOT NULL,
  scopes     TEXT,
  note       TEXT,
  claimed_by TEXT,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_pairing_code ON pairing_sessions(code);
CREATE TABLE IF NOT EXISTS audit_events (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  at        INTEGER NOT NULL,
  actor_type TEXT NOT NULL,
  actor_id  TEXT,
  event     TEXT NOT NULL,
  detail    TEXT
);
CREATE TABLE IF NOT EXISTS meta (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
);
`

// --- devices ---

// ErrDeviceNotFound / ErrDeviceRevoked 供调用方区分。
var (
	ErrDeviceNotFound  = errors.New("storage: device not found")
	ErrDeviceRevoked   = errors.New("storage: device revoked")
)

// PutDevice 写入或覆盖设备（claim 时调用）。
func (s *Store) PutDevice(d Device) error {
	_, err := s.db.Exec(
		`INSERT INTO devices(device_id,label,platform,pub_key,paired_at,last_seen,revoked_at)
		 VALUES(?,?,?,?,?,?,NULL)
		 ON CONFLICT(device_id) DO UPDATE SET label=excluded.label, platform=excluded.platform,
		   pub_key=excluded.pub_key, paired_at=excluded.paired_at, last_seen=excluded.last_seen,
		   revoked_at=NULL`,
		d.DeviceID, d.Label, d.Platform, d.PubKey, d.PairedAt.Unix(), d.LastSeen.Unix(),
	)
	return err
}

func scanDevice(row interface{ Scan(...any) error }) (*Device, error) {
	var d Device
	var pairedAt, lastSeen int64
	var revokedAt sql.NullInt64
	if err := row.Scan(&d.DeviceID, &d.Label, &d.Platform, &d.PubKey, &pairedAt, &lastSeen, &revokedAt); err != nil {
		return nil, err
	}
	d.PairedAt = time.Unix(pairedAt, 0)
	d.LastSeen = time.Unix(lastSeen, 0)
	if revokedAt.Valid && revokedAt.Int64 > 0 {
		t := time.Unix(revokedAt.Int64, 0)
		d.RevokedAt = &t
	}
	return &d, nil
}

const deviceCols = `device_id,label,platform,pub_key,paired_at,last_seen,revoked_at`

// GetDevice 按 ID 查；不存在/已吊销分别返回 ErrDeviceNotFound / ErrDeviceRevoked。
func (s *Store) GetDevice(id string) (*Device, error) {
	row := s.db.QueryRow(`SELECT `+deviceCols+` FROM devices WHERE device_id = ?`, id)
	d, err := scanDevice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDeviceNotFound
	}
	if err != nil {
		return nil, err
	}
	if d.RevokedAt != nil {
		return nil, ErrDeviceRevoked
	}
	return d, nil
}

// GetDevicePubKey 只取公钥（握手热路径，少两列）。
func (s *Store) GetDevicePubKey(id string) ([]byte, error) {
	var pub []byte
	var revokedAt int64
	err := s.db.QueryRow(`SELECT pub_key, COALESCE(revoked_at,0) FROM devices WHERE device_id = ?`, id).
		Scan(&pub, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDeviceNotFound
	}
	if err != nil {
		return nil, err
	}
	if revokedAt > 0 {
		return nil, ErrDeviceRevoked
	}
	return pub, nil
}

// ListDevices 列出未吊销设备。
func (s *Store) ListDevices() ([]Device, error) {
	rows, err := s.db.Query(`SELECT ` + deviceCols + ` FROM devices WHERE revoked_at IS NULL ORDER BY paired_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// RevokeDevice 软吊销。
func (s *Store) RevokeDevice(id string, at time.Time) error {
	res, err := s.db.Exec(`UPDATE devices SET revoked_at = ? WHERE device_id = ? AND revoked_at IS NULL`, at.Unix(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrDeviceNotFound
	}
	return nil
}

// RevokeAllDevices 全量吊销（unpair）。
func (s *Store) RevokeAllDevices(at time.Time) (int64, error) {
	res, err := s.db.Exec(`UPDATE devices SET revoked_at = ? WHERE revoked_at IS NULL`, at.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// TouchDevice 更新 last_seen。
func (s *Store) TouchDevice(id string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE devices SET last_seen = ? WHERE device_id = ?`, at.Unix(), id)
	return err
}

// --- pairing sessions ---

// ErrPairingNotFound / ErrPairingExpired / ErrPairingClaimed。
var (
	ErrPairingNotFound = errors.New("storage: pairing session not found")
	ErrPairingExpired  = errors.New("storage: pairing session expired")
	ErrPairingClaimed  = errors.New("storage: pairing session already claimed")
)

// CreatePairingSession 建会话（code 由上层生成并保证唯一）。
func (s *Store) CreatePairingSession(p PairingSession) error {
	scopes, _ := json.Marshal(p.Scopes)
	_, err := s.db.Exec(
		`INSERT INTO pairing_sessions(session_id,code,expires_at,scopes,note,claimed_by,created_at)
		 VALUES(?,?,?,?,?,NULL,?)`,
		p.SessionID, p.Code, p.ExpiresAt.Unix(), string(scopes), p.Note, p.CreatedAt.Unix(),
	)
	return err
}

// ClaimPairingSession 原子 claim：过期/已 claim 返回对应错误；成功置 claimed_by。
func (s *Store) ClaimPairingSession(sessionID, code string, deviceID string, at time.Time) (*PairingSession, error) {
	res, err := s.db.Exec(
		`UPDATE pairing_sessions SET claimed_by = ?
		 WHERE session_id = ? AND code = ? AND claimed_by IS NULL AND expires_at > ?`,
		deviceID, sessionID, code, at.Unix(),
	)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// 区分原因：不存在/过期/已 claim。
		var exp int64
		var claimed *string
		err := s.db.QueryRow(`SELECT expires_at, claimed_by FROM pairing_sessions WHERE session_id = ?`, sessionID).
			Scan(&exp, &claimed)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPairingNotFound
		}
		if err != nil {
			return nil, err
		}
		if claimed != nil {
			return nil, ErrPairingClaimed
		}
		if at.Unix() >= exp {
			return nil, ErrPairingExpired
		}
		return nil, errors.New("storage: code mismatch")
	}
	return s.getPairingSession(sessionID)
}

func (s *Store) getPairingSession(sessionID string) (*PairingSession, error) {
	var p PairingSession
	var exp, created int64
	var scopes string
	var claimed *string
	err := s.db.QueryRow(
		`SELECT session_id,code,expires_at,scopes,note,claimed_by,created_at FROM pairing_sessions WHERE session_id = ?`,
		sessionID,
	).Scan(&p.SessionID, &p.Code, &exp, &scopes, &p.Note, &claimed, &created)
	if err != nil {
		return nil, err
	}
	p.ExpiresAt = time.Unix(exp, 0)
	p.CreatedAt = time.Unix(created, 0)
	if scopes != "" {
		_ = json.Unmarshal([]byte(scopes), &p.Scopes)
	}
	if claimed != nil {
		p.ClaimedBy = claimed
	}
	return &p, nil
}

// CountPendingPairings 数未 claim 未过期会话（并发上限用）。
func (s *Store) CountPendingPairings(at time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM pairing_sessions WHERE claimed_by IS NULL AND expires_at > ?`, at.Unix()).
		Scan(&n)
	return n, err
}

// --- audit ---

// AppendAudit 写审计行。
func (s *Store) AppendAudit(at time.Time, actorType, actorID, event string, detail any) error {
	var d string
	if detail != nil {
		b, err := json.Marshal(detail)
		if err != nil {
			return err
		}
		d = string(b)
	}
	_, err := s.db.Exec(
		`INSERT INTO audit_events(at,actor_type,actor_id,event,detail) VALUES(?,?,?,?,?)`,
		at.Unix(), actorType, actorID, event, d,
	)
	return err
}

// --- meta ---

// SetMeta / GetMeta 存取 k/v（如 connect_signing_secret 预留）。
func (s *Store) SetMeta(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO meta(k,v) VALUES(?,?)
		ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

func (s *Store) GetMeta(k string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM meta WHERE k = ?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}
