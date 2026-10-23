package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func configForTest(t *testing.T, engine string) databaseConfig {
	t.Helper()
	t.Setenv("DATABASE_ENGINE", engine)
	c, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}

	return c
}

func TestMongoScriptWithAsyncClient(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node is required for the optional mongosh JavaScript contract check")
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "stdin")
	tool := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\ncat > \"$DATABASE_TEST_INPUT\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DATABASE_TEST_INPUT", input)
	t.Setenv("DATABASE_PASSWORD", "Aa1_example-only-password")
	l := lifecycle{config: configForTest(t, "mongodb"), pr: 1}
	if err := l.executeEngine("test", l.accessStatements("preview_pr1", "Aa1_preview-password")); err != nil {
		t.Fatal(err)
	}
	program := `
	const fs = require("fs");
	const source = fs.readFileSync(process.env.DATABASE_TEST_INPUT, "utf8");
	const calls = [];
	const client = {
	  auth: async () => ({ok:1}),
	  getSiblingDB: () => client,
	  getUser: async () => null,
	  dropUser: async () => { throw new Error("must not drop an absent user"); },
	  createUser: async spec => calls.push(spec),
	  updateUser: async () => { throw new Error("must create the absent user"); },
	  runCommand: async command => ({ok:1})
	};
	const AsyncFunction = Object.getPrototypeOf(async function(){}).constructor;
	new AsyncFunction("db", source)(client).then(() => {
	  if (calls.length !== 1 || calls[0].roles[0].role !== "read") {
	    throw new Error("incorrect MongoDB preview grants");
	  }
	}).catch(error => { console.error(error); process.exit(1); });
	`
	cmd := exec.Command("node", "-")
	cmd.Stdin = strings.NewReader(program)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("async mongosh contract failed: %v: %s", err, out)
	}
}

func TestEngineConfigurationAndConnections(t *testing.T) {
	for engine, port := range map[string]string{
		"postgresql": "5432", "mysql": "3306", "mariadb": "3306",
		"mongodb": "27017", "redis": "6379", "sqlserver": "1433",
	} {
		t.Run(engine, func(t *testing.T) {
			c := configForTest(t, engine)
			if c.port != port || c.pvc != c.workload+"-data" || c.secret != c.workload+"-admin" {
				t.Fatalf("invalid engine defaults: %+v", c)
			}
			connection := c.connectionURL("db.namespace.svc.cluster.local", "preview_pr1", "p@ss:/word")
			if !strings.Contains(connection, ":"+port) || strings.Contains(connection, "p@ss:/word") {
				t.Fatalf("incorrect or unescaped connection URL: %s", connection)
			}
			if engine == "mongodb" && !strings.Contains(connection, "authSource=application") {
				t.Fatal(connection)
			}
		})
	}
}

func TestRejectInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"DATABASE_ENGINE", "unknown"}, {"DATABASE_ADMIN_USERNAME", "root; DROP DATABASE"},
		{"DATABASE_DIRECTORY", "../other"}, {"API_DIRECTORY", "/tmp/other"},
		{"DATABASE_SCHEMA_FILE", "../../secret"}, {"DATABASE_WORKLOAD", "bad/workload"},
		{"DATABASE_PASSWORD_VARIABLE", "password\ninjection"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if _, err := loadConfig(); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestEngineAccessIsolation(t *testing.T) {
	for _, engine := range []string{"mysql", "mariadb", "mongodb", "redis", "sqlserver"} {
		t.Run(engine, func(t *testing.T) {
			c := configForTest(t, engine)
			l := lifecycle{config: c, pr: 17}
			reader := l.accessStatements("preview_pr17", "Aa1_password")
			t.Setenv("PR_DATABASE_ACCESS", "write")
			writer := l.accessStatements("preview_pr17_writer", "Aa1_password")
			if reader == "" || writer == "" || reader == writer {
				t.Fatal("missing engine-specific reader/writer distinction")
			}
			for _, text := range []string{reader, writer} {
				if !strings.Contains(text, "preview_pr17") || !strings.Contains(text, "preview_pr17_writer") {
					t.Fatal("mode switching must revoke the inactive identity")
				}
			}
			switch engine {
			case "mysql", "mariadb":
				if !strings.Contains(reader, "GRANT SELECT ON") || strings.Contains(reader, "GRANT ALL") ||
					!strings.Contains(writer, "GRANT ALL PRIVILEGES ON `application`.*") ||
					!strings.Contains(reader, "KILL CONNECTION") {
					t.Fatal("incorrect MySQL grants or missing session revocation")
				}
			case "mongodb":
				if !strings.Contains(reader, `role:"read"`) || !strings.Contains(writer, `role:"readWrite"`) ||
					!strings.Contains(reader, "killAllSessionsByPattern") {
					t.Fatal("incorrect MongoDB roles/session revocation")
				}
			case "redis":
				if strings.Contains(reader, "+set\r\n") || !strings.Contains(writer, "+set\r\n") ||
					strings.Contains(writer, "+@all") || !strings.Contains(reader, "~application:*") ||
					!strings.Contains(reader, "+echo\r\n") {
					t.Fatal("Redis ACLs must be restricted to application keys and explicit commands")
				}
			case "sqlserver":
				if !strings.Contains(reader, "[db_datareader]") || strings.Contains(reader, "[db_owner]") ||
					!strings.Contains(writer, "[db_owner]") || !strings.Contains(reader, "KILL ") {
					t.Fatal("incorrect SQL Server roles/session revocation")
				}
			}
		})
	}
}

func TestBootstrapBelongsToApplication(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "bootstrap.sql")
	if err := os.WriteFile(file, []byte("SELECT {{BOOTSTRAP_PASSWORD}};"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_BOOTSTRAP_FILE", "bootstrap.sql")
	t.Setenv("DATABASE_BOOTSTRAP_PASSWORD", "a'b\\c")
	for _, engine := range []string{"postgresql", "mysql", "mariadb", "mongodb", "redis", "sqlserver"} {
		t.Run(engine, func(t *testing.T) {
			c := configForTest(t, engine)
			l := lifecycle{root: dir, config: c}
			source := "SELECT {{BOOTSTRAP_PASSWORD}};"
			if engine == "redis" {
				source = `[["SET","application:bootstrap","{{BOOTSTRAP_PASSWORD}}"]]`
			}
			if err := os.WriteFile(file, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			text, err := l.bootstrap()
			if err != nil || strings.Contains(text, "{{BOOTSTRAP_PASSWORD}}") || strings.Contains(text, "administrators") {
				t.Fatalf("bootstrap failed: %s, %v", text, err)
			}
		})
	}
	outside := filepath.Join(t.TempDir(), "outside.sql")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape.sql")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_BOOTSTRAP_FILE", "escape.sql")
	if _, err := (lifecycle{root: dir}).bootstrap(); err == nil {
		t.Fatal("bootstrap symlink escaped source checkout")
	}
}
