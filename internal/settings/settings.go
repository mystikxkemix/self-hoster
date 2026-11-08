// Package settings holds the admin settings edited from the web UI. They are saved
// in a JSON file on the data volume and take precedence over the SELFHOSTER_* env
// vars, which act as defaults.
package settings

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"selfhoster/internal/fileutil"
)

const (
	hashScheme       = "pbkdf2-sha256"
	hashIterations   = 600_000
	minPasswordLen   = 8
	minPollInterval  = 5 * time.Second
	saltBytes        = 16
	derivedKeyLength = 32
)

var (
	userPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	hostPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

	// ErrWrongPassword is returned when the current password does not match.
	ErrWrongPassword = errors.New("current password is incorrect")
)

// Defaults come from the environment and apply until overridden in the UI.
type Defaults struct {
	AdminUser     string
	AdminPassword string
	PollInterval  time.Duration
}

// file is the persisted format. Empty fields fall back to Defaults.
type file struct {
	AdminUser    string `json:"adminUser,omitempty"`
	PasswordHash string `json:"passwordHash,omitempty"`
	PollInterval string `json:"pollInterval,omitempty"`
	LinkHost     string `json:"linkHost,omitempty"`
}

// View is what the UI sees: effective values, never the password.
type View struct {
	AdminUser string `json:"adminUser"`
	// PasswordSource is "env" until the password is changed in the UI, then "settings".
	PasswordSource      string `json:"passwordSource"`
	PollInterval        string `json:"pollInterval"`
	PollIntervalDefault string `json:"pollIntervalDefault"`
	// LinkHost is the host used to open projects that have no public URL ("" = the
	// host the admin UI is opened with).
	LinkHost string `json:"linkHost"`
}

// Update is the editable part of the settings. An empty PollInterval resets it to the default.
type Update struct {
	AdminUser    string `json:"adminUser"`
	PollInterval string `json:"pollInterval"`
	LinkHost     string `json:"linkHost"`
}

// Manager loads, validates and saves the settings. It is safe for concurrent use.
type Manager struct {
	mu       sync.RWMutex
	path     string
	defaults Defaults
	f        file

	// verified caches a digest of the last accepted credentials, so the slow password
	// hash runs once per password instead of on every request (the UI polls).
	verified    [sha256.Size]byte
	hasVerified bool
}

// Open loads the settings from path, starting from the defaults if it does not exist.
func Open(path string, defaults Defaults) (*Manager, error) {
	m := &Manager{path: path, defaults: defaults}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &m.f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return m, nil
}

// HasStoredPassword reports whether the password was set from the UI, in which case
// the env password is no longer required.
func (m *Manager) HasStoredPassword() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.f.PasswordHash != ""
}

// View returns the effective settings.
func (m *Manager) View() View {
	m.mu.RLock()
	defer m.mu.RUnlock()
	source := "env"
	if m.f.PasswordHash != "" {
		source = "settings"
	}
	return View{
		AdminUser:           m.adminUserLocked(),
		PasswordSource:      source,
		PollInterval:        formatDuration(m.pollIntervalLocked()),
		PollIntervalDefault: formatDuration(m.defaults.PollInterval),
		LinkHost:            m.f.LinkHost,
	}
}

// PollInterval returns the effective auto-deploy interval (0 = disabled).
func (m *Manager) PollInterval() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.pollIntervalLocked()
}

// Apply validates and saves an update.
func (m *Manager) Apply(u Update) error {
	u.AdminUser = strings.TrimSpace(u.AdminUser)
	u.PollInterval = strings.TrimSpace(u.PollInterval)
	u.LinkHost = strings.TrimSpace(u.LinkHost)
	if !userPattern.MatchString(u.AdminUser) {
		return errors.New("username must be 1-64 letters, digits, dots, dashes or underscores")
	}
	if u.PollInterval != "" {
		d, err := time.ParseDuration(u.PollInterval)
		if err != nil {
			return errors.New(`auto-deploy interval must be a duration like "30s", "1m" or "5m"`)
		}
		if d != 0 && d < minPollInterval {
			return fmt.Errorf("auto-deploy interval must be 0 (disabled) or at least %s", minPollInterval)
		}
	}
	if u.LinkHost != "" && !hostPattern.MatchString(u.LinkHost) {
		return errors.New("link host must be a host name or IP, without scheme, port or path (e.g. zima.local)")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.f
	next.AdminUser = u.AdminUser
	if u.AdminUser == m.defaults.AdminUser {
		next.AdminUser = ""
	}
	next.PollInterval = u.PollInterval
	next.LinkHost = u.LinkHost
	return m.saveLocked(next)
}

// ChangePassword replaces the admin password after checking the current one.
func (m *Manager) ChangePassword(current, next string) error {
	if len(next) < minPasswordLen {
		return fmt.Errorf("new password must be at least %d characters", minPasswordLen)
	}
	if !m.checkPassword(current) {
		return ErrWrongPassword
	}
	hash, err := hashPassword(next)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	updated := m.f
	updated.PasswordHash = hash
	return m.saveLocked(updated)
}

// Authenticate checks basic-auth credentials against the effective user and password.
func (m *Manager) Authenticate(user, password string) bool {
	m.mu.RLock()
	expectedUser := m.adminUserLocked()
	m.mu.RUnlock()
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(expectedUser)) == 1
	passwordOK := m.checkPassword(password)
	return userOK && passwordOK
}

func (m *Manager) checkPassword(password string) bool {
	m.mu.RLock()
	hash := m.f.PasswordHash
	digest := sha256.Sum256([]byte(hash + "\x00" + password))
	cached := m.hasVerified && subtle.ConstantTimeCompare(digest[:], m.verified[:]) == 1
	envPassword := m.defaults.AdminPassword
	m.mu.RUnlock()

	if hash == "" {
		return envPassword != "" && subtle.ConstantTimeCompare([]byte(password), []byte(envPassword)) == 1
	}
	if cached {
		return true
	}
	if !verifyPassword(hash, password) {
		return false
	}
	m.mu.Lock()
	m.verified, m.hasVerified = digest, true
	m.mu.Unlock()
	return true
}

func (m *Manager) adminUserLocked() string {
	if m.f.AdminUser != "" {
		return m.f.AdminUser
	}
	return m.defaults.AdminUser
}

func (m *Manager) pollIntervalLocked() time.Duration {
	if m.f.PollInterval == "" {
		return m.defaults.PollInterval
	}
	d, err := time.ParseDuration(m.f.PollInterval)
	if err != nil {
		return m.defaults.PollInterval
	}
	return d
}

// saveLocked persists next and makes it current. The caller holds the write lock.
func (m *Manager) saveLocked(next file) error {
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := fileutil.WriteFileAtomic(m.path, data); err != nil {
		return err
	}
	m.f = next
	return nil
}

// hashPassword returns "pbkdf2-sha256$<iterations>$<salt>$<key>" (base64, no padding).
func hashPassword(password string) (string, error) {
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, hashIterations, derivedKeyLength)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return strings.Join([]string{hashScheme, strconv.Itoa(hashIterations), enc.EncodeToString(salt), enc.EncodeToString(key)}, "$"), nil
}

func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != hashScheme {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations <= 0 {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := enc.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// formatDuration prints whole minutes and hours compactly ("1m", "2h"), else Go's format.
func formatDuration(d time.Duration) string {
	switch {
	case d == 0:
		return "0"
	case d%time.Hour == 0:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	case d%time.Minute == 0:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	default:
		return d.String()
	}
}
