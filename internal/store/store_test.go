package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validProject() Project {
	return Project{Name: "my-api", Type: TypeService, Repo: "https://example.com/r.git", StartCmd: "node index.js", Port: 4000}
}

func openTemp(t *testing.T, opts Options) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	s, err := Open(path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, path
}

func TestPutValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Project)
		wantErr string
	}{
		{"valid service", func(*Project) {}, ""},
		{"type defaults to service", func(p *Project) { p.Type = "" }, ""},
		{"valid static", func(p *Project) { p.Type, p.StartCmd, p.StaticDir = TypeStatic, "", "dist" }, ""},
		{"uppercase name", func(p *Project) { p.Name = "MyApi" }, "name must be"},
		{"name with slash", func(p *Project) { p.Name = "a/b" }, "name must be"},
		{"missing repo", func(p *Project) { p.Repo = "  " }, "repo is required"},
		{"service without start", func(p *Project) { p.StartCmd = " " }, "start command is required"},
		{"static without folder", func(p *Project) { p.Type, p.StaticDir = TypeStatic, "" }, "static folder is required"},
		{"static folder escapes repo", func(p *Project) { p.Type, p.StaticDir = TypeStatic, "../etc" }, "relative path inside"},
		{"static folder absolute", func(p *Project) { p.Type, p.StaticDir = TypeStatic, "/etc" }, "relative path inside"},
		{"unknown type", func(p *Project) { p.Type = "docker" }, "type must be"},
		{"port too high", func(p *Project) { p.Port = 70000 }, "between 1 and 65535"},
		{"reserved admin port", func(p *Project) { p.Port = 8080 }, "reserved for self-hoster"},
		{"port outside published range", func(p *Project) { p.Port = 5000 }, "published range"},
		{"invalid env key", func(p *Project) { p.Env = map[string]string{"BAD-KEY": "x"} }, "invalid environment variable"},
		{"PORT in env", func(p *Project) { p.Env = map[string]string{"PORT": "1"} }, "PORT is set from the project port"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openTemp(t, Options{ReservedPorts: []int{8080}, MinPort: 4000, MaxPort: 4019})
			p := validProject()
			tc.mutate(&p)
			err := s.Put(p)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
			if _, getErr := s.Get(p.Name); getErr == nil {
				t.Fatal("invalid project was stored")
			}
		})
	}
}

func TestPutDefaultsTypeToService(t *testing.T) {
	s, _ := openTemp(t, Options{})
	p := validProject()
	p.Type = ""
	if err := s.Put(p); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(p.Name)
	if got.Type != TypeService {
		t.Fatalf("Type = %q, want %q", got.Type, TypeService)
	}
}

func TestPutRejectsDuplicatePortButAllowsSameProject(t *testing.T) {
	s, _ := openTemp(t, Options{})
	a := validProject()
	if err := s.Put(a); err != nil {
		t.Fatal(err)
	}
	// Updating the same project on its own port is fine.
	a.Branch = "main"
	if err := s.Put(a); err != nil {
		t.Fatalf("update on same port: %v", err)
	}
	b := validProject()
	b.Name = "other"
	if err := s.Put(b); err == nil || !strings.Contains(err.Error(), `already used by "my-api"`) {
		t.Fatalf("error = %v, want port conflict", err)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	s, path := openTemp(t, Options{})
	p := validProject()
	p.Env = map[string]string{"MONGO_URL": "mongodb://u:p@host/db"}
	other := validProject()
	other.Name, other.Port = "another", 4001
	for _, project := range []Project{p, other} {
		if err := s.Put(project); err != nil {
			t.Fatal(err)
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("config mode = %o, want 600 (it holds secrets)", mode)
	}

	reopened, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	list := reopened.List()
	if len(list) != 2 || list[0].Name != "another" || list[1].Name != "my-api" {
		t.Fatalf("List() = %+v, want [another my-api] sorted by name", list)
	}
	if list[1].Env["MONGO_URL"] != "mongodb://u:p@host/db" {
		t.Fatalf("env not persisted: %+v", list[1].Env)
	}
}

func TestDelete(t *testing.T) {
	s, path := openTemp(t, Options{})
	if err := s.Put(validProject()); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("my-api"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("my-api"); err != ErrNotFound {
		t.Fatalf("second Delete = %v, want ErrNotFound", err)
	}
	reopened, _ := Open(path, Options{})
	if len(reopened.List()) != 0 {
		t.Fatal("deleted project still persisted")
	}
}

func TestPutRollsBackWhenSaveFails(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "missing-dir", "config.json"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(validProject()); err == nil {
		t.Fatal("expected a save error when the directory does not exist")
	}
	if _, err := s.Get("my-api"); err != ErrNotFound {
		t.Fatal("project kept in memory after a failed save")
	}
}

func TestOpenRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, Options{}); err == nil {
		t.Fatal("expected a parse error")
	}
}
