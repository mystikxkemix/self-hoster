package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var defaults = Defaults{AdminUser: "admin", AdminPassword: "env-password", PollInterval: time.Minute}

func openTemp(t *testing.T) (*Manager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	m, err := Open(path, defaults)
	if err != nil {
		t.Fatal(err)
	}
	return m, path
}

func TestDefaultsApplyWithoutFile(t *testing.T) {
	m, path := openTemp(t)
	v := m.View()
	if v.AdminUser != "admin" || v.PasswordSource != "env" || v.PollInterval != "1m" || v.LinkHost != "" {
		t.Fatalf("View() = %+v", v)
	}
	if !m.Authenticate("admin", "env-password") || m.Authenticate("admin", "wrong") || m.Authenticate("root", "env-password") {
		t.Fatal("env credentials not applied")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("settings file written without any change")
	}
}

func TestApplyValidation(t *testing.T) {
	tests := []struct {
		name    string
		update  Update
		wantErr string
	}{
		{"valid", Update{AdminUser: "kemix", PollInterval: "30s", LinkHost: "zima.local"}, ""},
		{"ip link host", Update{AdminUser: "admin", LinkHost: "192.168.1.20"}, ""},
		{"disabled polling", Update{AdminUser: "admin", PollInterval: "0"}, ""},
		{"empty user", Update{AdminUser: " "}, "username"},
		{"user with space", Update{AdminUser: "my admin"}, "username"},
		{"bad duration", Update{AdminUser: "admin", PollInterval: "every minute"}, "duration"},
		{"too frequent", Update{AdminUser: "admin", PollInterval: "1s"}, "at least 5s"},
		{"host with scheme", Update{AdminUser: "admin", LinkHost: "http://zima.local"}, "link host"},
		{"host with port", Update{AdminUser: "admin", LinkHost: "zima.local:8080"}, "link host"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := openTemp(t)
			err := m.Apply(tc.update)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
			if m.View().AdminUser != "admin" {
				t.Fatal("invalid update was applied")
			}
		})
	}
}

func TestApplyPersistsAndChangesEffectiveValues(t *testing.T) {
	m, path := openTemp(t)
	if err := m.Apply(Update{AdminUser: "kemix", PollInterval: "5m", LinkHost: "zima.local"}); err != nil {
		t.Fatal(err)
	}
	if m.PollInterval() != 5*time.Minute {
		t.Fatalf("PollInterval = %s", m.PollInterval())
	}
	if m.Authenticate("admin", "env-password") || !m.Authenticate("kemix", "env-password") {
		t.Fatal("new username not applied")
	}

	reopened, err := Open(path, defaults)
	if err != nil {
		t.Fatal(err)
	}
	v := reopened.View()
	if v.AdminUser != "kemix" || v.PollInterval != "5m" || v.PollIntervalDefault != "1m" || v.LinkHost != "zima.local" {
		t.Fatalf("after reopen: %+v", v)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("settings mode = %o, want 600", info.Mode().Perm())
	}

	// An empty interval goes back to the env default.
	if err := reopened.Apply(Update{AdminUser: "kemix"}); err != nil {
		t.Fatal(err)
	}
	if reopened.PollInterval() != time.Minute {
		t.Fatalf("PollInterval = %s, want the default", reopened.PollInterval())
	}
}

func TestChangePassword(t *testing.T) {
	m, path := openTemp(t)
	if err := m.ChangePassword("wrong", "new-password-1"); err != ErrWrongPassword {
		t.Fatalf("wrong current password: %v", err)
	}
	if err := m.ChangePassword("env-password", "short"); err == nil || !strings.Contains(err.Error(), "at least 8") {
		t.Fatalf("short password: %v", err)
	}
	if err := m.ChangePassword("env-password", "new-password-1"); err != nil {
		t.Fatal(err)
	}
	if m.Authenticate("admin", "env-password") {
		t.Fatal("env password still accepted after a change")
	}
	if !m.Authenticate("admin", "new-password-1") || !m.Authenticate("admin", "new-password-1") {
		t.Fatal("new password rejected (first check or cached check)")
	}
	if m.View().PasswordSource != "settings" || !m.HasStoredPassword() {
		t.Fatal("password source not updated")
	}

	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "new-password-1") || !strings.Contains(string(data), "pbkdf2-sha256$") {
		t.Fatalf("password not stored as a hash: %s", data)
	}

	// Survives a restart, and no longer needs the env password.
	reopened, err := Open(path, Defaults{AdminUser: "admin", PollInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Authenticate("admin", "new-password-1") {
		t.Fatal("stored password not accepted after reopen")
	}
}

func TestCachedPasswordIsInvalidatedByChange(t *testing.T) {
	m, _ := openTemp(t)
	if err := m.ChangePassword("env-password", "first-password"); err != nil {
		t.Fatal(err)
	}
	if !m.Authenticate("admin", "first-password") {
		t.Fatal("first password rejected")
	}
	if err := m.ChangePassword("first-password", "second-password"); err != nil {
		t.Fatal(err)
	}
	if m.Authenticate("admin", "first-password") {
		t.Fatal("old password still accepted from the cache")
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	for _, h := range []string{"", "plain", "md5$1$a$b", "pbkdf2-sha256$x$a$b", "pbkdf2-sha256$10$!!$b"} {
		if verifyPassword(h, "anything") {
			t.Errorf("verifyPassword(%q) = true", h)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "0", time.Minute: "1m", 2 * time.Hour: "2h", 90 * time.Second: "1m30s"} {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%s) = %q, want %q", d, got, want)
		}
	}
}
