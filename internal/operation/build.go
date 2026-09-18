package operation

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	identifierPattern       = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	varNamePattern          = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	manifestRevisionPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// Build validates a request and renders its CLI command. Alongside the
// command it returns the resolved server snapshot used to build it, so
// admission can persist the target identity it acted on (A14).
func Build(req Request, resolve Resolver, projectResolvers ...ProjectResolver) (Command, Server, string, error) {
	var resolveProject ProjectResolver
	if len(projectResolvers) > 0 {
		resolveProject = projectResolvers[0]
	}
	switch req.Kind {
	case KindDeploy:
		return BuildDeploy(req, resolve)
	case KindRollback:
		return BuildRollback(req, resolve)
	case KindRemove:
		return BuildRemove(req, resolve)
	case KindTemplateInstall:
		return BuildTemplateInstall(req, resolve, varStdinSupport())
	case KindAppLifecycle:
		return BuildAppLifecycle(req, resolve)
	case KindMaintenance:
		return BuildMaintenance(req, resolve)
	case KindManifestApply, KindManifestPlan, KindManifestValidate:
		return BuildManifest(req, resolve, resolveProject)
	default:
		return Command{}, Server{}, "", fmt.Errorf("unsupported operation kind %q", req.Kind)
	}
}

// varStdinSupport reports whether the installed teploy CLI accepts
// --var-stdin. The manager injects the real probe (which shells out to
// `teploy template install --help`); the package-level default keeps Build
// usable standalone (tests, recovery tools) with the legacy argv path.
var varStdinSupport = func() bool { return false }

// SetVarStdinSupport installs the CLI --var-stdin capability probe used by
// Build. Wired by the server package at startup.
func SetVarStdinSupport(probe func() bool) {
	if probe != nil {
		varStdinSupport = probe
	}
}

func BuildDeploy(req Request, resolve Resolver) (Command, Server, string, error) {
	if req.Mode != "" && req.Mode != "ad-hoc" {
		return Command{}, Server{}, "", fmt.Errorf("deploy mode must be ad-hoc")
	}
	srv, err := resolveRequest(req, resolve, true)
	if err != nil {
		return Command{}, Server{}, "", err
	}
	if req.Port < 0 || req.Port > 65535 {
		return Command{}, Server{}, "", fmt.Errorf("port must be between 0 and 65535")
	}
	args := []string{"deploy", srv.Name, "--app", req.App}
	args = addUser(args, srv.User)
	if req.Image != "" {
		args = append(args, "--image", req.Image)
	}
	if req.Domain != "" {
		args = append(args, "--domain", req.Domain)
	}
	if req.Port > 0 {
		args = append(args, "--port", strconv.Itoa(req.Port))
	}
	return Command{Args: args, Timeout: 30 * time.Minute}, srv, appTarget(req), nil
}

func BuildManifest(req Request, resolve Resolver, resolveProject ProjectResolver) (Command, Server, string, error) {
	srv, err := resolveRequest(req, resolve, true)
	if err != nil {
		return Command{}, Server{}, "", err
	}
	if req.Mode != "dash-managed" && req.Mode != "git-managed" {
		return Command{}, Server{}, "", fmt.Errorf("manifest mode must be dash-managed or git-managed")
	}
	if !manifestRevisionPattern.MatchString(req.ManifestRevision) {
		return Command{}, Server{}, "", fmt.Errorf("invalid manifest revision")
	}
	if resolveProject == nil {
		return Command{}, Server{}, "", fmt.Errorf("manifest project resolver is unavailable")
	}
	projectDir, err := resolveProject(req.Server, req.App, req.ManifestRevision)
	if err != nil {
		return Command{}, Server{}, "", err
	}
	if projectDir == "" {
		return Command{}, Server{}, "", fmt.Errorf("manifest project directory is unavailable")
	}
	var action string
	var timeout time.Duration
	switch req.Kind {
	case KindManifestApply:
		action, timeout = "deploy", 30*time.Minute
	case KindManifestPlan:
		action, timeout = "plan", 10*time.Minute
	case KindManifestValidate:
		action, timeout = "validate", 10*time.Minute
	}
	args := []string{"--project-dir", projectDir, action, "--host", srv.Host}
	args = addUser(args, srv.User)
	if req.Kind != KindManifestApply {
		args = append(args, "--json")
	}
	return Command{Args: args, Timeout: timeout}, srv, appTarget(req), nil
}

func BuildRollback(req Request, resolve Resolver) (Command, Server, string, error) {
	srv, err := resolveRequest(req, resolve, true)
	if err != nil {
		return Command{}, Server{}, "", err
	}
	args := []string{"rollback", "--host", srv.Host, "--app", req.App}
	return Command{Args: addUser(args, srv.User), Timeout: 10 * time.Minute}, srv, appTarget(req), nil
}

func BuildRemove(req Request, resolve Resolver) (Command, Server, string, error) {
	srv, err := resolveRequest(req, resolve, true)
	if err != nil {
		return Command{}, Server{}, "", err
	}
	args := []string{"remove", "--yes", "--json", "--host", srv.Host, "--app", req.App}
	if req.Purge {
		args = append(args, "--purge")
	}
	if req.Redirect != "" {
		args = append(args, "--redirect", req.Redirect)
	}
	return Command{Args: addUser(args, srv.User), Timeout: 10 * time.Minute}, srv, appTarget(req), nil

}

func BuildTemplateInstall(req Request, resolve Resolver, varStdin bool) (Command, Server, string, error) {
	srv, err := resolveRequest(req, resolve, false)
	if err != nil {
		return Command{}, Server{}, "", err
	}
	if !validIdentifier(req.Template) {
		return Command{}, Server{}, "", fmt.Errorf("invalid template")
	}
	if req.Domain == "" {
		return Command{}, Server{}, "", fmt.Errorf("domain is required")
	}
	args := []string{"template", "install", req.Template, "--domain", req.Domain, "--server", srv.Name}
	keys := make([]string, 0, len(req.Vars))
	for key := range req.Vars {
		if !varNamePattern.MatchString(key) {
			return Command{}, Server{}, "", fmt.Errorf("invalid template variable %q", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	secrets := make([]string, 0, len(keys))
	var stdinVars []byte
	if varStdin && len(keys) > 0 {
		// Secret values travel on the CLI's stdin (--var-stdin reads a JSON
		// object), never on the argv where every local process can read them
		// (A11 / UPSTREAM-1 adoption).
		payload := make(map[string]string, len(keys))
		for _, key := range keys {
			payload[key] = req.Vars[key]
			if req.Vars[key] != "" {
				secrets = append(secrets, req.Vars[key])
			}
		}
		stdinVars, err = json.Marshal(payload)
		if err != nil {
			return Command{}, Server{}, "", err
		}
		args = append(args, "--var-stdin")
	} else {
		for _, key := range keys {
			value := req.Vars[key]
			args = append(args, "--var", key+"="+value)
			if value != "" {
				secrets = append(secrets, value)
			}
		}
	}
	command := Command{Args: args, Timeout: 30 * time.Minute, Secrets: secrets}
	if stdinVars != nil {
		command.Stdin = string(stdinVars)
	}
	return command, srv, "server:" + req.Server + "/template:" + req.Template, nil
}

func BuildAppLifecycle(req Request, resolve Resolver) (Command, Server, string, error) {
	srv, err := resolveRequest(req, resolve, true)
	if err != nil {
		return Command{}, Server{}, "", err
	}
	switch req.Action {
	case "start", "stop", "restart", "lock", "unlock":
	default:
		return Command{}, Server{}, "", fmt.Errorf("unsupported app lifecycle action %q", req.Action)
	}
	args := []string{req.Action, "--host", srv.Host, "--app", req.App}
	return Command{Args: addUser(args, srv.User), Timeout: 5 * time.Minute}, srv, appTarget(req), nil
}

func BuildMaintenance(req Request, resolve Resolver) (Command, Server, string, error) {
	srv, err := resolveRequest(req, resolve, true)
	if err != nil {
		return Command{}, Server{}, "", err
	}
	if req.Action != "on" && req.Action != "off" {
		return Command{}, Server{}, "", fmt.Errorf("maintenance action must be on or off")
	}
	args := []string{"maintenance", req.Action, "--host", srv.Host, "--app", req.App}
	return Command{Args: addUser(args, srv.User), Timeout: 5 * time.Minute}, srv, appTarget(req), nil
}

func resolveRequest(req Request, resolve Resolver, appRequired bool) (Server, error) {
	if !validIdentifier(req.Server) {
		return Server{}, fmt.Errorf("invalid server")
	}
	if appRequired && !validIdentifier(req.App) {
		return Server{}, fmt.Errorf("invalid app")
	}
	if resolve == nil {
		return Server{}, fmt.Errorf("server resolver is unavailable")
	}
	srv, err := resolve(req.Server)
	if err != nil {
		return Server{}, err
	}
	if srv.Name == "" {
		srv.Name = req.Server
	}
	if srv.Host == "" {
		srv.Host = srv.Name
	}
	return srv, nil
}

func validIdentifier(value string) bool {
	return value != "." && value != ".." && !strings.HasPrefix(value, "-") && identifierPattern.MatchString(value)
}

func addUser(args []string, user string) []string {
	if user != "" && user != "root" {
		return append(args, "--user", user)
	}
	return args
}

func appTarget(req Request) string {
	return "server:" + req.Server + "/app:" + req.App
}

// redactedRequest returns the request shape persisted in the operation
// record. Nonempty variable values are replaced with [REDACTED]; EMPTY values
// are preserved as empty — the redacted record must stay replayable for
// requests whose variables are all empty, and a literal "[REDACTED]" on
// replay would deploy a nonsense value (A10).
func redactedRequest(req Request) Request {
	if len(req.Vars) == 0 {
		return req
	}
	clean := req
	clean.Vars = make(map[string]string, len(req.Vars))
	for key, value := range req.Vars {
		if value == "" {
			clean.Vars[key] = ""
		} else {
			clean.Vars[key] = "[REDACTED]"
		}
	}
	return clean
}

// Redact removes known secret values from a string. Line-based log redaction
// never sees a multiline value as one line, so in addition to the full values
// every newline/CR-delimited COMPONENT of a secret is replaced too — this
// intentionally may over-redact (A11).
func Redact(value string, secrets []string) string {
	terms := redactionTerms(secrets)
	for _, secret := range terms {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return value
}

// redactionTerms expands secret values with their whitespace-split components
// so a value spanning multiple log lines is still scrubbed per line.
func redactionTerms(secrets []string) []string {
	terms := make([]string, 0, len(secrets)*2)
	seen := make(map[string]bool, len(secrets)*2)
	add := func(t string) {
		if t != "" && !seen[t] {
			seen[t] = true
			terms = append(terms, t)
		}
	}
	for _, secret := range secrets {
		add(secret)
		for _, part := range strings.FieldsFunc(secret, func(r rune) bool {
			return r == '\n' || r == '\r'
		}) {
			add(part)
		}
	}
	sort.Slice(terms, func(i, j int) bool { return len(terms[i]) > len(terms[j]) })
	return terms
}
