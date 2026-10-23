package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type object struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Data map[string]string `json:"data"`
}

type lifecycle struct {
	root, backend, repository, name, run string
	pr                                   int
	sourceSHA                            string
	config                               databaseConfig
}

func (l lifecycle) readOnly() bool {
	return l.pr > 0 && os.Getenv("PR_DATABASE_ACCESS") != "write"
}

func command(directory, input, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = directory
	cmd.Stdin = strings.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if name == "git" || name == "tofu" {
			return nil, fmt.Errorf("%s failed: %w\n%s", name, err, stderr.String())
		}
		// SQL and Secret validation errors can echo their credential-bearing input.
		return nil, fmt.Errorf("%s failed: %w; inspect cluster events and backend ownership before retrying", name, err)
	}
	return out, nil
}

func get(kind, namespace, name string) (*object, error) {
	data, err := command("", "", "kubectl", "-n", namespace, "get", kind, name, "--ignore-not-found", "-o", "json")
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return nil, err
	}
	var result object
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (l lifecycle) owned(value *object) bool {
	return value != nil && value.Metadata.Labels["craftedbeyond.com/repository"] == strings.ReplaceAll(l.repository, "/", ".")
}

func (l lifecycle) connectionName() string {
	if l.pr > 0 {
		return fmt.Sprintf("%s-pr%d-database", l.name, l.pr)
	}
	return l.name + "-database"
}

func (l lifecycle) checkLegacyDatabase() error {
	namespace := l.name
	if l.pr > 0 {
		namespace = fmt.Sprintf("%s-pr%d", l.name, l.pr)
	}
	legacy, err := get("pvc", namespace, namespace+"-postgres-data")
	if err != nil {
		return err
	}
	if legacy != nil {
		return errors.New("legacy application-owned PostgreSQL PVC exists; back up and migrate its data/state before enabling the separate database lifecycle")
	}
	return nil
}

func (l lifecycle) statePath(environment string) string {
	return filepath.Join("combine", l.repository, environment)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func (l lifecycle) commit(message string) error {
	for _, identity := range [][2]string{
		{"user.name", "github-actions[bot]"},
		{"user.email", "41898282+github-actions[bot]@users.noreply.github.com"},
	} {
		if _, err := command(l.backend, "", "git", "config", "--local", identity[0], identity[1]); err != nil {
			return err
		}
	}
	if _, err := command(l.backend, "", "git", "add", "-f", "--", "combine/"+l.repository); err != nil {
		return err
	}
	if _, err := command(l.backend, "", "git", "diff", "--cached", "--quiet"); err == nil {
		return nil
	} else {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return err
		}
	}
	if _, err := command(l.backend, "", "git", "-c", "user.name=github-actions[bot]", "-c", "user.email=41898282+github-actions[bot]@users.noreply.github.com", "commit", "-m", message); err != nil {
		return err
	}
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := command(l.backend, "", "git", "push", "origin", "HEAD:"+setting("STATE_REF", "main")); err == nil {
			return nil
		}
		if _, err := command(l.backend, "", "git", "pull", "--rebase", "origin", setting("STATE_REF", "main")); err != nil {
			return errors.New("backend push/rebase failed; preserve local state and recover before unlocking")
		}
	}
	return errors.New("backend push failed; preserve local state and recover before unlocking")
}

func (l lifecycle) lock(path string) error {
	file := filepath.Join(l.backend, path, "deployment-lock.json")
	if _, err := os.Stat(file); err == nil {
		return fmt.Errorf("backend lock exists at %s; reconcile its owning run and state before removing it", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := writeJSON(file, map[string]string{"run": l.run, "repository": l.repository, "path": path, "pr": strconv.Itoa(l.pr)}); err != nil {
		return err
	}
	return l.commit("Lock " + path + " run " + l.run)
}

func (l lifecycle) unlock(path string) error {
	file := filepath.Join(l.backend, path, "deployment-lock.json")
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var owner map[string]string
	if err := json.Unmarshal(data, &owner); err != nil {
		return err
	}
	if owner["run"] != l.run {
		return errors.New("backend lock is owned by another run")
	}
	if err := os.Remove(file); err != nil {
		return err
	}
	return l.commit("Unlock " + path + " run " + l.run)
}

func copyFile(from, to string) error {
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0700); err != nil {
		return err
	}
	return os.WriteFile(to, data, 0600)
}

func (l lifecycle) hasResources(path string) (bool, error) {
	if _, err := os.Stat(filepath.Join(l.backend, path, "deployment-lock.json")); err == nil {
		return false, fmt.Errorf("backend lock remains at %s; recover before cleanup", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	data, err := os.ReadFile(filepath.Join(l.backend, path, "terraform.tfstate"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var state struct {
		Version   int               `json:"version"`
		Resources []json.RawMessage `json:"resources"`
	}
	if err := json.Unmarshal(data, &state); err != nil || state.Version == 0 {
		return false, errors.New("saved Terraform state is invalid")
	}
	return len(state.Resources) > 0, nil
}

func (l lifecycle) terraform(directory, path string, destroy bool) error {
	return l.terraformMode(directory, path, destroy, false)
}

func (l lifecycle) terraformMode(directory, path string, destroy, planOnly bool) error {
	if err := l.validateDirectory(directory); err != nil {
		return err
	}
	script := os.Getenv("DATABASE_REPORT_SCRIPT")
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("shared Terraform reporting scripts are required: %w", err)
	}
	if planOnly {
		if _, err := l.hasResources(path); err != nil {
			return err
		}
	} else {
		if err := l.lock(path); err != nil {
			return err
		}
	}
	fmt.Printf("Using isolated backend state %s/terraform.tfstate.\n", path)
	state := filepath.Join(l.backend, path, "terraform.tfstate")
	if _, err := os.Stat(state); err == nil {
		if err := copyFile(state, filepath.Join(directory, "terraform.tfstate")); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else if destroy {
		return errors.New("refusing cleanup without saved Terraform state; recover ownership before retrying")
	}
	varFile := os.Getenv("DATABASE_VAR_FILE")
	stack := "database"
	if directory == l.apiDirectory() {
		varFile = setting("API_VAR_FILE", "environments/prod.tfvars")
		stack = "api-cleanup"
	}
	report := filepath.Join(os.Getenv("RUNNER_TEMP"), stack+"-plan")
	runPhase := func(phase string) error {
		cmd := exec.Command("bash", script, phase, path, report, varFile, strconv.FormatBool(destroy))
		cmd.Dir = directory
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("Terraform %s failed for %s: %w", phase, path, err)
		}
		return nil
	}
	if err := runPhase("plan"); err != nil {
		return err
	}
	if planOnly {
		return appendSummary("\n**Plan only:** no apply, backend writes, SQL provisioning, connection Secret updates, or Argo refresh was performed.\n")
	}
	applyErr := runPhase("apply")
	workingState := filepath.Join(directory, "terraform.tfstate")
	if err := copyFile(workingState, state); err != nil {
		return fmt.Errorf("state missing after apply; retaining backend lock: %w", err)
	}
	if _, err := os.Stat(workingState + ".backup"); err == nil {
		if err := copyFile(workingState+".backup", state+".backup"); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := l.commit("Save " + path + " run " + l.run); err != nil {
		return err
	}
	fmt.Printf("Saved backend state at %s/terraform.tfstate.\n", path)
	summary, err := os.OpenFile(report+".md", os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := fmt.Fprintln(summary, "\nBackend state saved, including any partial apply. Provisioning/cleanup retains its lock until all remaining operations succeed.")
	if err := errors.Join(writeErr, summary.Close()); err != nil {
		return err
	}
	if applyErr != nil {
		return applyErr
	}
	// Keep ownership through SQL provisioning or credential revocation.
	return nil
}

func (l lifecycle) databaseVars(namespace string, temporary bool) error {
	if err := l.validateDirectory(l.databaseDirectory()); err != nil {
		return err
	}
	return writeJSON(filepath.Join(l.databaseDirectory(), "lifecycle.auto.tfvars.json"), map[string]any{
		"kubeconfig_path": os.Getenv("KUBECONFIG"), "namespace": namespace,
		"repository": l.repository, "temporary": temporary,
		setting("DATABASE_PASSWORD_VARIABLE", "database_password"): os.Getenv("DATABASE_PASSWORD"),
	})
}

func literal(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

func (l lifecycle) sql(namespace, statement string) error {
	statement = strings.ReplaceAll(statement, "FOR ROLE application_owner ", "FOR ROLE \""+l.config.admin+"\" ")
	statement = strings.ReplaceAll(statement, "ON DATABASE application ", "ON DATABASE \""+l.config.database+"\" ")
	args := []string{"-n", namespace, "exec", "-i", l.config.workload + "-0"}
	if l.config.container != "" {
		args = append(args, "-c", l.config.container)
	}
	args = append(args, "--", "psql", "-X", "-U", l.config.admin, "-d", l.config.database)
	_, err := command("", "\\set ON_ERROR_STOP on\n"+statement, "kubectl", args...)
	return err
}

func (l lifecycle) provision(namespace string, temporary bool) error {
	if l.pr > 0 && !l.readOnly() && !temporary {
		return errors.New("writable PR access requires an isolated temporary database")
	}
	if err := l.waitDatabaseApplication(namespace); err != nil {
		return err
	}
	if l.config.engine != "postgresql" {
		return l.provisionEngine(namespace, temporary)
	}
	if temporary || l.pr == 0 {
		schema, err := l.bootstrap()
		if err != nil {
			return err
		}
		statement := "BEGIN;\n" + schema + "\nCOMMIT;\n"
		if err := l.sql(namespace, statement); err != nil {
			return errors.New("database schema/admin provisioning failed; backend lock retained")
		}
	}
	host := l.config.service + "." + namespace + ".svc.cluster.local"
	username, password := l.config.admin, os.Getenv("DATABASE_PASSWORD")
	if l.pr > 0 {
		var err error
		username, password, err = l.previewCredential()
		if err != nil {
			return err
		}
		statement := fmt.Sprintf(`BEGIN;
SELECT format('CREATE ROLE %%I LOGIN PASSWORD %%L NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS', %s, %s)
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = %s) \gexec
ALTER ROLE %s WITH LOGIN PASSWORD %s NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
ALTER ROLE %s SET default_transaction_read_only = on;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
REVOKE TEMPORARY ON DATABASE application FROM PUBLIC;
GRANT CONNECT ON DATABASE application TO %s;
GRANT USAGE ON SCHEMA public TO %s;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO %s;
ALTER DEFAULT PRIVILEGES FOR ROLE application_owner IN SCHEMA public GRANT SELECT ON TABLES TO %s;
COMMIT;
`, literal(username), literal(password), literal(username), username, literal(password), username, username, username, username, username)
		if err := l.sql(namespace, statement); err != nil {
			return errors.New("read-only role provisioning failed; backend lock retained")
		}
		if temporary {
			reader, writer := fmt.Sprintf("preview_pr%d", l.pr), fmt.Sprintf("preview_pr%d_writer", l.pr)
			inactive := writer
			if !l.readOnly() {
				inactive = reader
				statement := fmt.Sprintf(`BEGIN;
ALTER ROLE %s SET default_transaction_read_only = off;
GRANT USAGE, CREATE ON SCHEMA public TO %s;
GRANT TEMPORARY ON DATABASE application TO %s;
SELECT format('ALTER TABLE %%I.%%I OWNER TO %%I', schemaname, tablename, %s)
FROM pg_tables WHERE schemaname = 'public' \gexec
COMMIT;
`, writer, writer, writer, literal(writer))
				if err := l.sql(namespace, statement); err != nil {
					return errors.New("isolated PR writer provisioning failed; backend lock retained")
				}
			} else {
				statement := fmt.Sprintf(`SELECT format('ALTER DEFAULT PRIVILEGES FOR ROLE %%I IN SCHEMA public GRANT SELECT ON TABLES TO %%I', %s, %s)
WHERE EXISTS (SELECT FROM pg_roles WHERE rolname = %s) \gexec
`, literal(writer), literal(reader), literal(writer))
				if err := l.sql(namespace, statement); err != nil {
					return errors.New("isolated PR reader grants failed; backend lock retained")
				}
			}
			statement := fmt.Sprintf(`SELECT format('ALTER ROLE %%I NOLOGIN', %s)
WHERE EXISTS (SELECT FROM pg_roles WHERE rolname = %s) \gexec
SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = %s;
`, literal(inactive), literal(inactive), literal(inactive))
			if err := l.sql(namespace, statement); err != nil {
				return errors.New("previous PR access mode could not be disabled; backend lock retained")
			}
		}
	}
	return l.connection(namespace, host, username, password, temporary)
}

func (l lifecycle) waitDatabaseApplication(name string) error {
	fmt.Printf("Waiting for database Argo Application %s to reconcile and become healthy (up to 10 minutes).\n", name)
	start := time.Now().UTC().Truncate(time.Second)
	if _, err := command("", "", "kubectl", "-n", "argocd", "annotate", "application", name,
		"argocd.argoproj.io/refresh=hard", "--overwrite"); err != nil {
		return err
	}
	for attempt := 0; attempt < 120; attempt++ {
		data, err := command("", "", "kubectl", "-n", "argocd", "get", "application", name, "-o", "json")
		if err != nil {
			return err
		}
		var app struct {
			Spec   struct{ Source map[string]any }
			Status struct {
				ReconciledAt time.Time
				Health       struct{ Status string }
				Sync         struct {
					Status     string
					ComparedTo struct{ Source map[string]any }
				}
				Conditions []struct{ Type, Message string }
			}
		}
		if err := json.Unmarshal(data, &app); err != nil {
			return err
		}
		for _, condition := range app.Status.Conditions {
			if condition.Type == "InvalidSpecError" || condition.Type == "ComparisonError" {
				return fmt.Errorf("database Argo Application %s reports %s; inspect its conditions before retrying", name, condition.Type)
			}
		}
		if !app.Status.ReconciledAt.Before(start) && app.Status.Sync.Status == "Synced" &&
			app.Status.Health.Status == "Healthy" && databaseSourcesEqual(app.Spec.Source, app.Status.Sync.ComparedTo.Source) {
			_, err := command("", "", "kubectl", "-n", name, "rollout", "status", "statefulset/"+l.config.workload, "--timeout=60s")
			return err
		}
		if attempt%6 == 0 {
			fmt.Printf("Database %s readiness: sync=%s, health=%s, reconciled=%s (elapsed %s).\n",
				name, app.Status.Sync.Status, app.Status.Health.Status, app.Status.ReconciledAt.Format(time.RFC3339),
				time.Since(start).Truncate(time.Second))
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("database Argo Application %s did not become synced and healthy within 10 minutes; backend lock retained", name)
}

func (l lifecycle) connection(namespace, host, username, password string, temporary bool) error {
	existing, err := get("secret", "argocd", l.connectionName())
	if err != nil {
		return err
	}
	if existing != nil && (!l.owned(existing) || existing.Metadata.Labels["craftedbeyond.com/pr"] != strconv.Itoa(l.pr)) {
		return errors.New("connection Secret ownership does not match this deployment")
	}
	connection := map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]any{
			"name": l.connectionName(), "namespace": "argocd",
			"labels": map[string]string{"craftedbeyond.com/repository": strings.ReplaceAll(l.repository, "/", "."), "craftedbeyond.com/pr": strconv.Itoa(l.pr)},
		},
		"stringData": map[string]string{
			"DATABASE_HOST": host, "DATABASE_NAME": l.config.database, "DATABASE_USER": username,
			"DATABASE_PASSWORD": password, "DATABASE_READ_ONLY": strconv.FormatBool(l.readOnly()),
			"DATABASE_NAMESPACE": namespace, "DATABASE_TEMPORARY": strconv.FormatBool(temporary),
			"DATABASE_ENGINE": l.config.engine, "DATABASE_PORT": l.config.port,
			"DATABASE_URL": l.config.connectionURL(host, username, password),
		},
	}
	data, err := json.Marshal(connection)
	if err != nil {
		return err
	}
	_, err = command("", string(data), "kubectl", "apply", "--server-side", "--force-conflicts", "--field-manager=database-workflow", "-f", "-")
	return err
}

func (l lifecycle) prepare() error {
	if err := l.authorize(); err != nil {
		return err
	}
	if l.pr > 0 {
		head, err := command(l.root, "", "git", "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		if err := validateDatabaseSource(os.Getenv("DATABASE_SOURCE_SHA"), strings.TrimSpace(string(head)), l.sourceSHA); err != nil {
			return err
		}
	}
	if err := l.checkLegacyDatabase(); err != nil {
		return err
	}
	if err := l.validateExistingEngine(); err != nil {
		return err
	}
	namespace, temporary := l.name+"-db", false
	if l.pr > 0 {
		preview := fmt.Sprintf("%s-pr%d-db", l.name, l.pr)
		existing, err := get("namespace", "", preview)
		if err != nil {
			return err
		}
		main, err := get("namespace", "", namespace)
		if err != nil {
			return err
		}
		if !l.readOnly() || existing != nil || main == nil {
			namespace, temporary = preview, true
		}
	}
	existing, err := get("namespace", "", namespace)
	if err != nil {
		return err
	}
	if existing != nil && !l.owned(existing) {
		return errors.New("database namespace exists without matching ownership; refusing adoption")
	}
	reuseOnly := l.pr > 0 && os.Getenv("PR_DATABASE_CREATE") == "false"
	if reuseOnly {
		if existing == nil {
			return fmt.Errorf("PR_DATABASE_CREATE=false: required database namespace %s does not exist; provision it separately or enable creation", namespace)
		}
		for _, resource := range []struct{ kind, name string }{
			{"statefulset", l.config.workload}, {"pvc", l.config.pvc}, {"service", l.config.service},
		} {
			value, err := get(resource.kind, namespace, resource.name)
			if err != nil {
				return err
			}
			if value == nil {
				return fmt.Errorf("PR_DATABASE_CREATE=false: existing database is incomplete (%s/%s missing in %s); refusing to create it", resource.kind, resource.name, namespace)
			}
		}
	}
	if existing != nil && (temporary || l.pr == 0) {
		admin, err := get("secret", namespace, l.config.secret)
		if err != nil {
			return err
		}
		if admin == nil {
			return errors.New("existing database is missing its admin Secret; recover before proceeding")
		}
		password, err := base64.StdEncoding.DecodeString(admin.Data[l.config.passwordKey])
		if err != nil {
			return err
		}
		if string(password) != os.Getenv("DATABASE_PASSWORD") {
			return errors.New("database password differs from the source Secret; rotate the database role explicitly before updating infrastructure")
		}
	}
	path := l.statePath("db")
	if temporary {
		path = l.statePath(fmt.Sprintf("pr%d-db", l.pr))
	}
	if l.pr > 0 && (!temporary || reuseOnly) {
		// A reader may use an existing server, but must never apply its infrastructure.
		text := fmt.Sprintf("## Database Terraform: `%s`\n\n**Plan/apply skipped:** reusing the existing database in `%s`; infrastructure changes are disabled for this access mode.\n", path, namespace)
		if err := os.WriteFile(filepath.Join(os.Getenv("RUNNER_TEMP"), "database-plan.md"), []byte(text), 0600); err != nil {
			return err
		}
		if err := appendSummary(text); err != nil {
			return err
		}
		if err := l.lock(path); err != nil {
			return err
		}
	} else {
		if existing != nil {
			if _, err := os.Stat(filepath.Join(l.backend, path, "terraform.tfstate")); err != nil {
				return errors.New("existing database has no accessible backend state; recover/import before proceeding")
			}
		}
		if err := l.databaseVars(namespace, temporary); err != nil {
			return err
		}
		if err := l.terraform(l.databaseDirectory(), path, false); err != nil {
			return err
		}
	}
	if temporary && !l.readOnly() {
		prior, err := get("secret", "argocd", l.connectionName())
		if err != nil {
			return err
		}
		if prior != nil {
			if !l.owned(prior) {
				return errors.New("previous connection Secret belongs to another owner")
			}
			priorNamespace, err := base64.StdEncoding.DecodeString(prior.Data["DATABASE_NAMESPACE"])
			if err != nil {
				return err
			}
			if string(priorNamespace) == l.name+"-db" {
				mainPath := l.statePath("db")
				if err := l.lock(mainPath); err != nil {
					return err
				}
				if err := l.revokeReader(l.name + "-db"); err != nil {
					return err
				}
				if err := l.unlock(mainPath); err != nil {
					return err
				}
			}
		}
	}
	if err := l.provision(namespace, temporary); err != nil {
		return err
	}
	fmt.Printf("Database connection ready: namespace=%s, temporary=%t, read-only=%t.\n", namespace, temporary, l.readOnly())
	return l.unlock(path)
}

func (l lifecycle) plan() error {
	if l.pr != 0 {
		return errors.New("database plan-only operation requires a manual release-branch run, not a PR")
	}
	if err := l.authorize(); err != nil {
		return err
	}
	if err := l.checkLegacyDatabase(); err != nil {
		return err
	}
	if err := l.validateExistingEngine(); err != nil {
		return err
	}
	namespace := l.name + "-db"
	existing, err := get("namespace", "", namespace)
	if err != nil {
		return err
	}
	if existing != nil && !l.owned(existing) {
		return errors.New("database namespace exists without matching ownership; refusing to plan adoption")
	}
	path := l.statePath("db")
	if existing != nil {
		if _, err := os.Stat(filepath.Join(l.backend, path, "terraform.tfstate")); err != nil {
			return errors.New("existing database has no accessible backend state; recover/import before planning")
		}
	}
	if err := l.databaseVars(namespace, false); err != nil {
		return err
	}
	return l.terraformMode(l.databaseDirectory(), path, false, true)
}

func appendSummary(text string) error {
	file, err := os.OpenFile(os.Getenv("GITHUB_STEP_SUMMARY"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := io.WriteString(file, text)
	return errors.Join(writeErr, file.Close())
}

func validateDatabaseSource(expected, checkout, head string) error {
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(expected) || checkout != expected || head != expected {
		return errors.New("database checkout or PR head differs from the authorized source commit; post a new /combine-deploy command")
	}
	return nil
}

func (l *lifecycle) authorize() error {
	if l.pr == 0 {
		if os.Getenv("GITHUB_EVENT_NAME") != "workflow_dispatch" || os.Getenv("GITHUB_REF") != "refs/heads/"+releaseBranch() {
			return errors.New("permanent database provisioning requires a manual run on the release branch")
		}
		l.sourceSHA = os.Getenv("GITHUB_SHA")
		if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(l.sourceSHA) {
			return errors.New("manual database provisioning requires a valid workflow commit SHA")
		}
		return nil
	}
	data, err := os.ReadFile(os.Getenv("GITHUB_EVENT_PATH"))
	if err != nil {
		return err
	}
	var event struct {
		Action  string
		Comment struct {
			Body string
			User struct{ Login string }
		}
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return err
	}
	if os.Getenv("GITHUB_EVENT_NAME") != "issue_comment" || event.Action != "created" || event.Comment.Body != "/combine-deploy" || event.Comment.User.Login == "" {
		return errors.New("preview database provisioning requires a new /combine-deploy comment")
	}
	getAPI := func(endpoint string, result any) error {
		request, err := http.NewRequest("GET", "https://api.github.com/repos/"+l.repository+endpoint, nil)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+os.Getenv("GH_TOKEN"))
		client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("GitHub redirects are not allowed")
		}}
		response, err := client.Do(request)
		if err != nil {
			return errors.New("GitHub authorization request failed")
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("GitHub authorization failed with HTTP %d", response.StatusCode)
		}
		return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(result)
	}
	var permission struct{ Permission string }
	if err := getAPI("/collaborators/"+url.PathEscape(event.Comment.User.Login)+"/permission", &permission); err != nil {
		return err
	}
	if permission.Permission != "admin" && permission.Permission != "maintain" && permission.Permission != "write" {
		return errors.New("only repository writers may provision preview database access")
	}
	var pr struct {
		State string
		Head  struct {
			SHA  string
			Repo struct {
				FullName string `json:"full_name"`
			}
		}
		Base   struct{ Ref string }
		Labels []struct{ Name string }
	}
	if err := getAPI(fmt.Sprintf("/pulls/%d", l.pr), &pr); err != nil {
		return err
	}
	combine, releases := false, 0
	for _, label := range pr.Labels {
		combine = combine || label.Name == "combine-deploy"
		if label.Name == "major" || label.Name == "minor" || label.Name == "patch" {
			releases++
		}
	}
	if pr.State != "open" || pr.Head.Repo.FullName != l.repository || pr.Base.Ref != releaseBranch() || !combine || releases != 1 {
		return errors.New("preview requires an open same-repository PR targeting the release branch, combine-deploy and one release label")
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(pr.Head.SHA) {
		return errors.New("GitHub returned an invalid PR source commit")
	}
	l.sourceSHA = pr.Head.SHA
	return nil
}

func (l lifecycle) cleanup() error {
	eventData, err := os.ReadFile(os.Getenv("GITHUB_EVENT_PATH"))
	if err != nil {
		return err
	}
	var event struct {
		Action      string
		PullRequest struct {
			Number int
			Merged bool
			Base   struct{ Ref string }
			Head   struct {
				Repo struct {
					FullName string `json:"full_name"`
				}
			}
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(eventData, &event); err != nil {
		return err
	}
	if os.Getenv("GITHUB_EVENT_NAME") != "pull_request" || event.Action != "closed" ||
		!event.PullRequest.Merged || l.pr < 1 || event.PullRequest.Number != l.pr ||
		event.PullRequest.Base.Ref != releaseBranch() ||
		event.PullRequest.Head.Repo.FullName != l.repository {
		return errors.New("cleanup requires a merged same-repository PR")
	}
	connection, err := get("secret", "argocd", l.connectionName())
	if err != nil {
		return err
	}
	appPath := l.statePath(fmt.Sprintf("pr%d", l.pr))
	_, stateErr := os.Stat(filepath.Join(l.backend, appPath, "terraform.tfstate"))
	if connection == nil {
		hasApplication, err := l.hasResources(appPath)
		if err != nil {
			return err
		}
		if !hasApplication {
			temporaryPath := l.statePath(fmt.Sprintf("pr%d-db", l.pr))
			hasDatabase, err := l.hasResources(temporaryPath)
			if err != nil {
				return err
			}
			if hasDatabase {
				return errors.New("temporary database state exists without a connection Secret; recover its provisioning before cleanup")
			}
			app, err := get("namespace", "", fmt.Sprintf("%s-pr%d", l.name, l.pr))
			if err != nil {
				return err
			}
			db, err := get("namespace", "", fmt.Sprintf("%s-pr%d-db", l.name, l.pr))
			if err != nil {
				return err
			}
			if app != nil || db != nil {
				return errors.New("PR resources exist without saved connection/state; recover ownership before cleanup")
			}
			fmt.Println("No database connection or application state exists for this PR; nothing to clean.")
			return nil
		}
		return errors.New("application state exists without its database connection; recover configuration before cleanup")
	}
	if !l.owned(connection) || connection.Metadata.Labels["craftedbeyond.com/pr"] != strconv.Itoa(l.pr) {
		return errors.New("refusing cleanup of a connection Secret without matching PR ownership")
	}
	if err := l.validateExistingEngine(); err != nil {
		return err
	}
	if err := l.checkLegacyDatabase(); err != nil {
		return err
	}
	app, err := get("namespace", "", fmt.Sprintf("%s-pr%d", l.name, l.pr))
	if err != nil {
		return err
	}
	if app != nil && (app.Metadata.Labels["app.kubernetes.io/managed-by"] != "combine-workflow" ||
		app.Metadata.Labels["craftedbeyond.com/environment"] != fmt.Sprintf("pr%d", l.pr)) {
		return errors.New("PR namespace ownership does not match cleanup")
	}
	value := func(key string) (string, error) {
		data, err := base64.StdEncoding.DecodeString(connection.Data[key])
		return string(data), err
	}
	namespace, err := value("DATABASE_NAMESPACE")
	if err != nil {
		return err
	}
	temporary, err := value("DATABASE_TEMPORARY")
	if err != nil || (temporary != "true" && temporary != "false") {
		return errors.New("invalid database ownership mode")
	}
	expected := l.name + "-db"
	dbPath := l.statePath("db")
	if temporary == "true" {
		expected = fmt.Sprintf("%s-pr%d-db", l.name, l.pr)
		dbPath = l.statePath(fmt.Sprintf("pr%d-db", l.pr))
	}
	if namespace != expected {
		return errors.New("database namespace does not match this repository/PR")
	}
	db, err := get("namespace", "", namespace)
	if err != nil {
		return err
	}
	if !l.owned(db) || db.Metadata.Labels["craftedbeyond.com/temporary"] != temporary {
		return errors.New("database namespace ownership does not match cleanup")
	}
	if !errors.Is(stateErr, os.ErrNotExist) {
		if stateErr != nil {
			return stateErr
		}
		runtime := map[string]any{
			"repository": l.repository, "environment": fmt.Sprintf("pr%d", l.pr),
			"pr_number": l.pr, "preview": true, "sha": os.Getenv("GITHUB_SHA"),
			"registry": "docker.io", "image_repository": "docker.io/" + os.Getenv("REGISTRY_USERNAME") + "/" + l.name,
			"image_tag": "cleanup",
		}
		if err := l.validateDirectory(l.apiDirectory()); err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(l.apiDirectory(), "lifecycle.auto.tfvars.json"), map[string]any{"combine_runtime": runtime}); err != nil {
			return err
		}
		if err := l.terraform(l.apiDirectory(), appPath, true); err != nil {
			return err
		}
		if err := l.unlock(appPath); err != nil {
			return err
		}
	} else {
		app, err := get("namespace", "", fmt.Sprintf("%s-pr%d", l.name, l.pr))
		if err != nil {
			return err
		}
		if app != nil {
			return errors.New("PR namespace exists without saved application state; recover/import before cleanup")
		}
	}
	if temporary == "true" {
		if err := l.databaseVars(namespace, true); err != nil {
			return err
		}
		if err := l.terraform(l.databaseDirectory(), dbPath, true); err != nil {
			return err
		}
	} else {
		if err := l.lock(dbPath); err != nil {
			return err
		}
		if err := l.revokeReader(namespace); err != nil {
			return err
		}
	}
	if _, err := command("", "", "kubectl", "-n", "argocd", "delete", "secret", l.connectionName()); err != nil {
		return err
	}
	return l.unlock(dbPath)
}

func (l lifecycle) revokeReader(namespace string) error {
	if l.config.engine != "postgresql" {
		return l.revokeEngineReader(namespace)
	}
	role := fmt.Sprintf("preview_pr%d", l.pr)
	statement := fmt.Sprintf(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = %s;
DO $block$
BEGIN
IF EXISTS (SELECT FROM pg_roles WHERE rolname = %s) THEN
  REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM %s;
  ALTER DEFAULT PRIVILEGES FOR ROLE application_owner IN SCHEMA public REVOKE SELECT ON TABLES FROM %s;
  REVOKE USAGE ON SCHEMA public FROM %s;
  REVOKE CONNECT ON DATABASE application FROM %s;
  DROP ROLE %s;
END IF;
END;
$block$;
`, literal(role), literal(role), role, role, role, role, role)
	if err := l.sql(namespace, statement); err != nil {
		return errors.New("PR reader revocation failed; backend lock retained for recovery")
	}
	return nil
}

func run() error {
	if len(os.Args) != 2 || (os.Args[1] != "resolve" && os.Args[1] != "prepare" && os.Args[1] != "cleanup" && os.Args[1] != "plan") {
		return errors.New("usage: database resolve|plan|prepare|cleanup")
	}
	repository := os.Getenv("GITHUB_REPOSITORY")
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[a-z0-9][a-z0-9-]*$`).MatchString(repository) {
		return errors.New("invalid repository identity")
	}
	pr, err := strconv.Atoi(os.Getenv("PR_NUMBER"))
	if err != nil || pr < 0 {
		return errors.New("invalid PR number")
	}
	if len(os.Getenv("DATABASE_PASSWORD")) < 24 ||
		strings.ContainsAny(os.Getenv("DATABASE_PASSWORD")+os.Getenv("DATABASE_BOOTSTRAP_PASSWORD"), "\r\n\x00") {
		return errors.New("database provisioning requires DATABASE_PASSWORD >=24 characters, without newlines or NUL")
	}
	config, err := loadConfig()
	if err != nil {
		return err
	}
	workspace := os.Getenv("GITHUB_WORKSPACE")
	l := lifecycle{
		root: filepath.Join(workspace, "source"), backend: filepath.Join(workspace, ".database-state"),
		repository: repository, name: strings.Split(repository, "/")[1], run: os.Getenv("GITHUB_RUN_ID"), pr: pr, config: config,
	}
	if os.Args[1] == "resolve" {
		if l.run == "" || workspace == "" {
			return errors.New("runner identity and workspace are required")
		}
		if err := l.authorize(); err != nil {
			return err
		}
		output, err := os.OpenFile(os.Getenv("GITHUB_OUTPUT"), os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, writeErr := fmt.Fprintln(output, "sha="+l.sourceSHA)
		return errors.Join(writeErr, output.Close())
	}
	if l.run == "" || workspace == "" || os.Getenv("KUBECONFIG") == "" {
		return errors.New("runner identity, workspace and kubeconfig are required")
	}
	if os.Args[1] == "cleanup" {
		return l.cleanup()
	}
	if os.Args[1] == "plan" {
		return l.plan()
	}
	if mode := os.Getenv("PR_DATABASE_ACCESS"); mode != "" && mode != "read" && mode != "write" {
		return errors.New("PR_DATABASE_ACCESS must be read or write")
	}
	if create := os.Getenv("PR_DATABASE_CREATE"); create != "" && create != "true" && create != "false" {
		return errors.New("PR_DATABASE_CREATE must be true or false")
	}
	return l.prepare()
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Database lifecycle failed:", err)
		os.Exit(1)
	}
}
