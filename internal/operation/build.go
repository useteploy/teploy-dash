package operation

import (
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

func Build(req Request, resolve Resolver, projectResolvers ...ProjectResolver) (Command, string, error) {
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
		return BuildTemplateInstall(req, resolve)
	case KindAppLifecycle:
		return BuildAppLifecycle(req, resolve)
	case KindMaintenance:
		return BuildMaintenance(req, resolve)
	case KindManifestApply, KindManifestPlan, KindManifestValidate:
		return BuildManifest(req, resolve, resolveProject)
	default:
		return Command{}, "", fmt.Errorf("unsupported operation kind %q", req.Kind)
	}
}

func BuildDeploy(req Request, resolve Resolver) (Command, string, error) {
	if req.Mode != "" && req.Mode != "ad-hoc" {
		return Command{}, "", fmt.Errorf("deploy mode must be ad-hoc")
	}
	srv, err := resolveRequest(req, resolve, true)
	if err != nil {
		return Command{}, "", err
	}
	if req.Port < 0 || req.Port > 65535 {
		return Command{}, "", fmt.Errorf("port must be between 0 and 65535")
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
	return Command{Args: args, Timeout: 30 * time.Minute}, appTarget(req), nil
}

func BuildManifest(req Request, resolve Resolver, resolveProject ProjectResolver) (Command, string, error) {
	srv, err := resolveRequest(req, resolve, true)
	if err != nil {
		return Command{}, "", err
	}
	if req.Mode != "dash-managed" && req.Mode != "git-managed" {
		return Command{}, "", fmt.Errorf("manifest mode must be dash-managed or git-managed")
	}
	if !manifestRevisionPattern.MatchString(req.ManifestRevision) {
		return Command{}, "", fmt.Errorf("invalid manifest revision")
	}
	if resolveProject == nil {
		return Command{}, "", fmt.Errorf("manifest project resolver is unavailable")
	}
	projectDir, err := resolveProject(req.Server, req.App, req.ManifestRevision)
	if err != nil {
		return Command{}, "", err
	}
	if projectDir == "" {
		return Command{}, "", fmt.Errorf("manifest project directory is unavailable")
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
	return Command{Args: args, Timeout: timeout}, appTarget(req), nil
}

func BuildRollback(req Request, resolve Resolver) (Command, string, error) {
	srv, err := resolveRequest(req, resolve, true)
	if err != nil {
		return Command{}, "", err
	}
	args := []string{"rollback", "--host", srv.Host, "--app", req.App}
	return Command{Args: addUser(args, srv.User), Timeout: 10 * time.Minute}, appTarget(req), nil
}

func BuildRemove(req Request, resolve Resolver) (Command, string, error) {
	srv, err := resolveRequest(req, resolve, true)
	if err != nil {
		return Command{}, "", err
	}
	args := []string{"remove", "--yes", "--json", "--host", srv.Host, "--app", req.App}
	if req.Purge {
		args = append(args, "--purge")
	}
	if req.Redirect != "" {
		args = append(args, "--redirect", req.Redirect)
	}
	return Command{Args: addUser(args, srv.User), Timeout: 10 * time.Minute}, appTarget(req), nil

}

func BuildTemplateInstall(req Request, resolve Resolver) (Command, string, error) {
	srv, err := resolveRequest(req, resolve, false)
	if err != nil {
		return Command{}, "", err
	}
	if !validIdentifier(req.Template) {
		return Command{}, "", fmt.Errorf("invalid template")
	}
	if req.Domain == "" {
		return Command{}, "", fmt.Errorf("domain is required")
	}
	args := []string{"template", "install", req.Template, "--domain", req.Domain, "--server", srv.Name}
	keys := make([]string, 0, len(req.Vars))
	for key := range req.Vars {
		if !varNamePattern.MatchString(key) {
			return Command{}, "", fmt.Errorf("invalid template variable %q", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	secrets := make([]string, 0, len(keys))
	for _, key := range keys {
		value := req.Vars[key]
		args = append(args, "--var", key+"="+value)
		if value != "" {
			secrets = append(secrets, value)
		}
	}
	return Command{Args: args, Timeout: 30 * time.Minute, Secrets: secrets}, "server:" + req.Server + "/template:" + req.Template, nil
}

func BuildAppLifecycle(req Request, resolve Resolver) (Command, string, error) {
	srv, err := resolveRequest(req, resolve, true)
	if err != nil {
		return Command{}, "", err
	}
	switch req.Action {
	case "start", "stop", "restart", "lock", "unlock":
	default:
		return Command{}, "", fmt.Errorf("unsupported app lifecycle action %q", req.Action)
	}
	args := []string{req.Action, "--host", srv.Host, "--app", req.App}
	return Command{Args: addUser(args, srv.User), Timeout: 5 * time.Minute}, appTarget(req), nil
}

func BuildMaintenance(req Request, resolve Resolver) (Command, string, error) {
	srv, err := resolveRequest(req, resolve, true)
	if err != nil {
		return Command{}, "", err
	}
	if req.Action != "on" && req.Action != "off" {
		return Command{}, "", fmt.Errorf("maintenance action must be on or off")
	}
	args := []string{"maintenance", req.Action, "--host", srv.Host, "--app", req.App}
	return Command{Args: addUser(args, srv.User), Timeout: 5 * time.Minute}, appTarget(req), nil
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

func redactedRequest(req Request) Request {
	if len(req.Vars) == 0 {
		return req
	}
	clean := req
	clean.Vars = make(map[string]string, len(req.Vars))
	for key := range req.Vars {
		clean.Vars[key] = "[REDACTED]"
	}
	return clean
}

func Redact(value string, secrets []string) string {
	sorted := append([]string(nil), secrets...)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, secret := range sorted {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return value
}
