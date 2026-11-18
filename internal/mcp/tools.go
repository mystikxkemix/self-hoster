package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"selfhoster/internal/store"
	"selfhoster/internal/supervisor"
)

const (
	defaultLogLines = 100
	maxLogLines     = 1000
)

// userinfoPattern matches credentials embedded in a URL (e.g. a GitHub token).
var userinfoPattern = regexp.MustCompile(`://[^/@\s]+@`)

// redactedUserinfo replaces repo credentials in tool output.
const redactedUserinfo = "://***@"

// tool is one MCP tool: its descriptor and the function running it.
type tool struct {
	name        string
	title       string
	description string
	schema      map[string]any
	readOnly    bool
	destructive bool
	idempotent  bool
	run         func(args json.RawMessage) (any, error)
}

func (t tool) descriptor() map[string]any {
	return map[string]any{
		"name":        t.name,
		"title":       t.title,
		"description": t.description,
		"inputSchema": t.schema,
		"annotations": map[string]any{
			"title":           t.title,
			"readOnlyHint":    t.readOnly,
			"destructiveHint": t.destructive,
			"idempotentHint":  t.idempotent,
			"openWorldHint":   false,
		},
	}
}

// projectOut is a project as the model sees it: no env values, no repo credentials.
type projectOut struct {
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	Repo       string            `json:"repo"`
	Branch     string            `json:"branch,omitempty"`
	BuildCmd   string            `json:"buildCmd,omitempty"`
	StartCmd   string            `json:"startCmd,omitempty"`
	Port       int               `json:"port"`
	StaticDir  string            `json:"staticDir,omitempty"`
	SPA        bool              `json:"spa,omitempty"`
	URL        string            `json:"url,omitempty"`
	AutoDeploy bool              `json:"autoDeploy"`
	Enabled    bool              `json:"enabled"`
	EnvKeys    []string          `json:"envKeys"`
	DataDir    string            `json:"dataDir"`
	Status     supervisor.Status `json:"status"`
}

// projectSummary is the compact form returned by list_projects.
type projectSummary struct {
	Name      string           `json:"name"`
	Type      string           `json:"type"`
	Port      int              `json:"port"`
	URL       string           `json:"url,omitempty"`
	Enabled   bool             `json:"enabled"`
	State     supervisor.State `json:"state"`
	Deploying bool             `json:"deploying"`
	Commit    string           `json:"commit,omitempty"`
	LastError string           `json:"lastError,omitempty"`
}

// Tool argument types. Pointer fields are optional: nil keeps the current value.
type (
	nameArgs struct {
		Name string `json:"name"`
	}
	logsArgs struct {
		Name  string `json:"name"`
		Lines int    `json:"lines"`
	}
	createArgs struct {
		Name       string            `json:"name"`
		Type       string            `json:"type"`
		Repo       string            `json:"repo"`
		Branch     string            `json:"branch"`
		BuildCmd   string            `json:"buildCmd"`
		StartCmd   string            `json:"startCmd"`
		Port       int               `json:"port"`
		StaticDir  string            `json:"staticDir"`
		SPA        bool              `json:"spa"`
		URL        string            `json:"url"`
		AutoDeploy *bool             `json:"autoDeploy"`
		Env        map[string]string `json:"env"`
	}
	updateArgs struct {
		Name       string  `json:"name"`
		Type       *string `json:"type"`
		Repo       *string `json:"repo"`
		Branch     *string `json:"branch"`
		BuildCmd   *string `json:"buildCmd"`
		StartCmd   *string `json:"startCmd"`
		Port       *int    `json:"port"`
		StaticDir  *string `json:"staticDir"`
		SPA        *bool   `json:"spa"`
		URL        *string `json:"url"`
		AutoDeploy *bool   `json:"autoDeploy"`
	}
	envArgs struct {
		Name  string            `json:"name"`
		Set   map[string]string `json:"set"`
		Unset []string          `json:"unset"`
	}
)

func (s *Server) toolset() []tool {
	return []tool{
		{
			name: "list_projects", title: "List projects", readOnly: true, idempotent: true,
			description: "List every hosted project with its port, run state, deployed commit and last error, plus the allowed port range for new projects.",
			schema:      objectSchema(nil),
			run:         s.listProjects,
		},
		{
			name: "get_project", title: "Get project", readOnly: true, idempotent: true,
			description: "Get one project's full configuration (commands, branch, port, env variable names) and runtime status.",
			schema:      objectSchema(map[string]any{"name": nameProp}, "name"),
			run:         withArgs(s.getProject),
		},
		{
			name: "project_logs", title: "Project logs", readOnly: true, idempotent: true,
			description: "Read the latest log lines of a project: build output, process stdout/stderr and self-hoster messages.",
			schema: objectSchema(map[string]any{
				"name":  nameProp,
				"lines": map[string]any{"type": "integer", "minimum": 1, "maximum": maxLogLines, "description": fmt.Sprintf("Number of lines from the end (default %d).", defaultLogLines)},
			}, "name"),
			run: withArgs(s.projectLogs),
		},
		{
			name: "deploy_project", title: "Deploy project",
			description: "Fetch the branch, build, and (re)start the project in the background. If the build fails the previous version keeps running. Marks the project as enabled.",
			schema:      objectSchema(map[string]any{"name": nameProp}, "name"),
			run:         withArgs(s.deployProject),
		},
		{
			name: "start_project", title: "Start project", idempotent: true,
			description: "Start an already deployed project without rebuilding it.",
			schema:      objectSchema(map[string]any{"name": nameProp}, "name"),
			run:         withArgs(s.startProject),
		},
		{
			name: "stop_project", title: "Stop project", destructive: true, idempotent: true,
			description: "Stop a project. It stays stopped across restarts and is not auto-deployed until started or deployed again.",
			schema:      objectSchema(map[string]any{"name": nameProp}, "name"),
			run:         withArgs(s.stopProject),
		},
		{
			name: "restart_project", title: "Restart project",
			description: "Restart a deployed project's process without rebuilding it.",
			schema:      objectSchema(map[string]any{"name": nameProp}, "name"),
			run:         withArgs(s.restartProject),
		},
		{
			name: "create_project", title: "Create project",
			description: "Register a new project. It is created stopped: call deploy_project afterwards. A service needs startCmd and must listen on $PORT; a static site needs staticDir (its build output folder).",
			schema:      objectSchema(projectProps(true), "name", "repo", "port"),
			run:         withArgs(s.createProject),
		},
		{
			name: "update_project", title: "Update project",
			description: "Change a project's settings. Only the fields given are changed. A running project restarts with the new settings; call deploy_project to rebuild after changing the repo, branch or build command.",
			schema:      objectSchema(projectProps(false), "name"),
			run:         withArgs(s.updateProject),
		},
		{
			name: "set_env", title: "Set environment variables",
			description: "Add, change or remove a project's environment variables. Variables not mentioned are kept. A running project restarts so the change applies; static sites need deploy_project (env is used at build time).",
			schema: objectSchema(map[string]any{
				"name":  nameProp,
				"set":   map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "Variables to add or change, e.g. {\"MONGO_URL\": \"mongodb://...\"}."},
				"unset": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Variable names to remove."},
			}, "name"),
			run: withArgs(s.setEnv),
		},
	}
}

var nameProp = map[string]any{"type": "string", "description": "Project name (lowercase letters, digits, dashes)."}

// projectProps describes a project's editable fields. On create, defaults are documented.
func projectProps(create bool) map[string]any {
	props := map[string]any{
		"name":       nameProp,
		"type":       map[string]any{"type": "string", "enum": []string{store.TypeService, store.TypeStatic}, "description": "service: a long-running process. static: a website served from its build folder."},
		"repo":       map[string]any{"type": "string", "description": "Git URL (https or ssh). On update, omit it to keep the current URL and its credentials."},
		"branch":     map[string]any{"type": "string", "description": "Branch to deploy (empty = the remote's default branch)."},
		"buildCmd":   map[string]any{"type": "string", "description": "Shell command run in the checkout before starting, e.g. \"npm ci && npm run build\"."},
		"startCmd":   map[string]any{"type": "string", "description": "Service only: shell command starting the process, e.g. \"node dist/index.js\"."},
		"port":       map[string]any{"type": "integer", "description": "Port the project listens on (passed as $PORT). Must be free and in the allowed range (see list_projects)."},
		"staticDir":  map[string]any{"type": "string", "description": "Static only: build output folder relative to the repo root, e.g. \"dist\"."},
		"spa":        map[string]any{"type": "boolean", "description": "Static only: serve index.html for unknown paths (client-side routing)."},
		"url":        map[string]any{"type": "string", "description": "Public URL where the project is reachable, e.g. https://app.example.com (informational)."},
		"autoDeploy": map[string]any{"type": "boolean", "description": "Redeploy automatically when a new commit lands on the branch."},
	}
	if create {
		props["type"].(map[string]any)["default"] = store.TypeService
		props["autoDeploy"].(map[string]any)["default"] = true
		props["env"] = map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "Environment variables. PORT and DATA_DIR are set automatically."}
	}
	return props
}

func objectSchema(props map[string]any, required ...string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// withArgs decodes the tool arguments strictly, so a misspelled field is an error
// instead of being silently ignored.
func withArgs[T any](fn func(T) (any, error)) func(json.RawMessage) (any, error) {
	return func(raw json.RawMessage) (any, error) {
		var args T
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&args); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		return fn(args)
	}
}

func (s *Server) listProjects(json.RawMessage) (any, error) {
	projects := s.store.List()
	out := make([]projectSummary, 0, len(projects))
	for _, p := range projects {
		st := s.sup.Status(p.Name)
		out = append(out, projectSummary{
			Name: p.Name, Type: p.Type, Port: p.Port, URL: p.URL, Enabled: p.Enabled,
			State: st.State, Deploying: st.Deploying, Commit: st.Commit, LastError: st.LastError,
		})
	}
	ports := s.ports
	if ports == "" {
		ports = "any"
	}
	return map[string]any{"projects": out, "allowedPorts": ports}, nil
}

func (s *Server) getProject(args nameArgs) (any, error) {
	p, err := s.store.Get(args.Name)
	if err != nil {
		return nil, err
	}
	return s.view(p), nil
}

func (s *Server) projectLogs(args logsArgs) (any, error) {
	p, err := s.store.Get(args.Name)
	if err != nil {
		return nil, err
	}
	n := args.Lines
	if n <= 0 {
		n = defaultLogLines
	}
	lines := s.sup.Logs(p.Name)
	if len(lines) > min(n, maxLogLines) {
		lines = lines[len(lines)-min(n, maxLogLines):]
	}
	if len(lines) == 0 {
		return "(no logs yet)", nil
	}
	return redact(strings.Join(lines, "\n")), nil
}

func (s *Server) deployProject(args nameArgs) (any, error) {
	return s.lifecycle(s.projects.Deploy, args.Name)
}

func (s *Server) startProject(args nameArgs) (any, error) {
	return s.lifecycle(s.projects.Start, args.Name)
}

func (s *Server) stopProject(args nameArgs) (any, error) {
	return s.lifecycle(s.projects.Stop, args.Name)
}

func (s *Server) restartProject(args nameArgs) (any, error) {
	return s.lifecycle(s.projects.Restart, args.Name)
}

// lifecycle runs a project action and returns the project's view.
func (s *Server) lifecycle(op func(string) (store.Project, error), name string) (any, error) {
	p, err := op(name)
	if err != nil {
		return nil, err
	}
	return s.view(p), nil
}

func (s *Server) createProject(args createArgs) (any, error) {
	if err := checkRepo(args.Repo); err != nil {
		return nil, err
	}
	autoDeploy := true
	if args.AutoDeploy != nil {
		autoDeploy = *args.AutoDeploy
	}
	p := store.Project{
		Name: args.Name, Type: args.Type, Repo: args.Repo, Branch: args.Branch,
		BuildCmd: args.BuildCmd, StartCmd: args.StartCmd, Port: args.Port,
		StaticDir: args.StaticDir, SPA: args.SPA, URL: args.URL, AutoDeploy: autoDeploy,
		Env: args.Env,
	}
	p, err := s.projects.Create(p)
	if err != nil {
		return nil, err
	}
	return s.view(p), nil
}

func (s *Server) updateProject(args updateArgs) (any, error) {
	p, err := s.store.Get(args.Name)
	if err != nil {
		return nil, err
	}
	if args.Repo != nil {
		if err := checkRepo(*args.Repo); err != nil {
			return nil, err
		}
	}
	setIf(&p.Type, args.Type)
	setIf(&p.Repo, args.Repo)
	setIf(&p.Branch, args.Branch)
	setIf(&p.BuildCmd, args.BuildCmd)
	setIf(&p.StartCmd, args.StartCmd)
	setIf(&p.Port, args.Port)
	setIf(&p.StaticDir, args.StaticDir)
	setIf(&p.SPA, args.SPA)
	setIf(&p.URL, args.URL)
	setIf(&p.AutoDeploy, args.AutoDeploy)
	return s.save(p)
}

func (s *Server) setEnv(args envArgs) (any, error) {
	p, err := s.store.Get(args.Name)
	if err != nil {
		return nil, err
	}
	if len(args.Set) == 0 && len(args.Unset) == 0 {
		return nil, errors.New("nothing to change: give set and/or unset")
	}
	env := maps.Clone(p.Env)
	if env == nil {
		env = map[string]string{}
	}
	for _, key := range args.Unset {
		delete(env, key)
	}
	maps.Copy(env, args.Set)
	p.Env = env
	return s.save(p)
}

// save persists an edited project; a running one restarts so the change applies.
func (s *Server) save(p store.Project) (any, error) {
	p, err := s.projects.Save(p)
	if err != nil {
		return nil, err
	}
	return s.view(p), nil
}

func (s *Server) view(p store.Project) projectOut {
	keys := slices.Sorted(maps.Keys(p.Env))
	if keys == nil {
		keys = []string{}
	}
	return projectOut{
		Name: p.Name, Type: p.Type, Repo: redact(p.Repo), Branch: p.Branch,
		BuildCmd: p.BuildCmd, StartCmd: p.StartCmd, Port: p.Port, StaticDir: p.StaticDir,
		SPA: p.SPA, URL: p.URL, AutoDeploy: p.AutoDeploy, Enabled: p.Enabled,
		EnvKeys: keys, DataDir: s.sup.AppDir(p.Name), Status: s.sup.Status(p.Name),
	}
}

// checkRepo rejects a repo URL copied from redacted output, which would replace the
// real token with "***".
func checkRepo(repo string) error {
	if strings.Contains(repo, redactedUserinfo) {
		return errors.New("repo contains a redacted credential (***): omit repo to keep the current URL")
	}
	return nil
}

func redact(text string) string {
	return userinfoPattern.ReplaceAllString(text, redactedUserinfo)
}

func setIf[T any](dst *T, value *T) {
	if value != nil {
		*dst = *value
	}
}
