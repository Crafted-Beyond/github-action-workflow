package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Opt-in: starts and removes only disposable, labelled containers without host mounts.
func TestDatabaseEnginesIntegration(t *testing.T) {
	if os.Getenv("DATABASE_ENGINE_INTEGRATION") != "1" {
		t.Skip("set DATABASE_ENGINE_INTEGRATION=1 to run disposable Docker engine tests")
	}
	images := map[string]string{
		"postgresql": "postgres:16-alpine", "mysql": "mysql:8.4", "mariadb": "mariadb:11.4",
		"mongodb": "mongo:8.0", "redis": "redis:7.4-alpine", "sqlserver": "mcr.microsoft.com/mssql/server:2022-latest",
	}
	for _, engine := range []string{"postgresql", "mysql", "mariadb", "mongodb", "redis", "sqlserver"} {
		t.Run(engine, func(t *testing.T) {
			if selected := os.Getenv("DATABASE_TEST_ENGINE"); selected != "" && selected != engine {
				t.Skip("different engine selected")
			}
			if engine == "sqlserver" && (runtime.GOARCH != "amd64" || os.Getenv("DATABASE_SQLSERVER_EULA") != "Y") {
				t.Skip("SQL Server integration requires an AMD64 host and explicit DATABASE_SQLSERVER_EULA=Y")
			}
			c := configForTest(t, engine)
			token := make([]byte, 12)
			if _, err := rand.Read(token); err != nil {
				t.Fatal(err)
			}
			suffix := hex.EncodeToString(token)
			name := "cb-dbtest-" + engine + "-" + suffix
			password := "Aa1_" + suffix
			t.Setenv("DATABASE_PASSWORD", password)
			docker := func(args ...string) ([]byte, error) {
				limit := 30 * time.Second
				if args[0] == "run" {
					limit = 4 * time.Minute
				}
				ctx, cancel := context.WithTimeout(context.Background(), limit)
				defer cancel()
				return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
			}
			t.Cleanup(func() {
				out, err := docker("rm", "-f", "-v", name)
				if err != nil && !strings.Contains(string(out), "No such container") {
					t.Errorf("cannot remove disposable container %s: %v: %s", name, err, out)
				}
			})
			args := []string{"run", "--detach", "--name", name, "--label", "com.craftedbeyond.database-test=" + suffix}
			env := map[string]string{}
			switch engine {
			case "postgresql":
				env = map[string]string{"POSTGRES_USER": c.admin, "POSTGRES_DB": c.database, "POSTGRES_PASSWORD": password}
			case "mysql":
				env = map[string]string{"MYSQL_ROOT_PASSWORD": password}
			case "mariadb":
				env = map[string]string{"MARIADB_ROOT_PASSWORD": password}
			case "mongodb":
				env = map[string]string{"MONGO_INITDB_ROOT_USERNAME": c.admin, "MONGO_INITDB_ROOT_PASSWORD": password}
			case "redis":
				env = map[string]string{"REDIS_PASSWORD": password}
			case "sqlserver":
				env = map[string]string{"ACCEPT_EULA": "Y", "MSSQL_PID": "Developer", "MSSQL_SA_PASSWORD": password}
			}
			for key, value := range env {
				t.Setenv(key, value)
				args = append(args, "--env", key)
			}
			image := images[engine]
			if mirror := os.Getenv("DATABASE_TEST_REGISTRY"); mirror != "" && engine != "sqlserver" {
				image = mirror + "/" + image
			}
			args = append(args, image)
			if engine == "redis" {
				args = append(args, "sh", "-ec", `touch /data/users.acl; exec redis-server --requirepass "$REDIS_PASSWORD" --aclfile /data/users.acl`)
			}
			t.Logf("Disposable test container: %s", name)
			if out, err := docker(args...); err != nil {
				t.Fatalf("start %s: %v: %s", name, err, out)
			}
			dir := t.TempDir()
			shim := `#!/usr/bin/env bash
set -eu
shift 2
test "$1" = exec
shift
while [ "$1" != -- ]; do shift; done
shift
exec docker exec -i "$DATABASE_TEST_CONTAINER" "$@" 2>"$DATABASE_TEST_STDERR"
`
			if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(shim), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("DATABASE_TEST_CONTAINER", name)
			stderrPath := filepath.Join(dir, "client-stderr")
			t.Setenv("DATABASE_TEST_STDERR", stderrPath)
			l := lifecycle{config: c, pr: 1}
			execute := func(user, pass, statement string) error {
				identity := l
				identity.config.admin = user
				t.Setenv("DATABASE_PASSWORD", pass)
				if engine == "postgresql" {
					return identity.sql("test", statement)
				}
				if engine == "mongodb" && user != c.admin {
					statement = "db = db.getSiblingDB(" + jsonLiteral(c.database) + ");\n" +
						"const auth = await db.auth(" + jsonLiteral(user) + "," + jsonLiteral(pass) + ");\n" +
						"if (auth !== true && auth !== 1 && auth?.ok !== 1) throw new Error('Auth failed');\n" + statement
					_, err := command("", statement, "docker", "exec", "-i", name, "mongosh", "--quiet", "--norc", "--file", "/dev/stdin")
					return err
				}
				return identity.executeEngine("test", statement)
			}
			ping, schema, read, write := "SELECT 1;", "", "", ""
			switch engine {
			case "postgresql":
				schema = "CREATE TABLE IF NOT EXISTS items(id integer); INSERT INTO items VALUES (1);"
				read, write = "SELECT * FROM items;", "INSERT INTO items VALUES(2);"
			case "mysql", "mariadb", "sqlserver":
				schema = "CREATE TABLE items(id integer); INSERT INTO items VALUES(1);"
				read, write = "SELECT * FROM items;", "INSERT INTO items VALUES(2);"
				if engine == "sqlserver" {
					read, write = "USE [application];\n"+read, "USE [application];\n"+write
				} else {
					read, write = "USE application;\n"+read, "USE application;\n"+write
				}
			case "mongodb":
				ping, schema = "await db.runCommand({ping:1});", "await db.items.insertOne({id:1});"
				read, write = "db.items.findOne();", "db.items.insertOne({id:2});"
			case "redis":
				ping, schema = redisCommand("PING"), redisCommand("SET", "application:item", "1")
				read, write = redisCommand("GET", "application:item"), redisCommand("SET", "application:item", "2")
			}
			deadline := time.Now().Add(3 * time.Minute)
			for execute(c.admin, password, ping) != nil {
				if time.Now().After(deadline) {
					logs, _ := docker("logs", "--tail", "40", name)
					client, _ := os.ReadFile(stderrPath)
					t.Fatalf("engine never became ready: client=%s\n%s", client, logs)
				}
				time.Sleep(2 * time.Second)
			}
			t.Setenv("DATABASE_PASSWORD", password)
			if engine == "postgresql" {
				if err := l.sql("test", schema); err != nil {
					t.Fatal(err)
				}
				// The PostgreSQL lifecycle's existing grant path is exercised separately.
				reader := "CREATE ROLE preview_pr1 LOGIN PASSWORD '" + password + "'; GRANT USAGE ON SCHEMA public TO preview_pr1; GRANT SELECT ON ALL TABLES IN SCHEMA public TO preview_pr1;"
				if err := l.sql("test", reader); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := l.executeEngine("test", l.engineBootstrap(schema)); err != nil {
					t.Fatal(err)
				}
				if err := l.executeEngine("test", l.accessStatements("preview_pr1", password)); err != nil {
					t.Fatal(err)
				}
			}
			if err := execute("preview_pr1", password, read); err != nil {
				client, _ := os.ReadFile(stderrPath)
				t.Fatalf("reader cannot read: %v: %s", err, client)
			}
			if err := execute("preview_pr1", password, write); err == nil {
				t.Fatal("reader unexpectedly wrote data")
			}
			t.Setenv("DATABASE_PASSWORD", password)
			if err := l.revokeReader("test"); err != nil {
				t.Fatalf("reader revocation failed: %v", err)
			}
			if err := execute("preview_pr1", password, read); err == nil {
				t.Fatal("revoked reader can still access the database")
			}
			if engine != "postgresql" {
				t.Setenv("DATABASE_PASSWORD", password)
				t.Setenv("PR_DATABASE_ACCESS", "write")
				if err := l.executeEngine("test", l.accessStatements("preview_pr1_writer", password)); err != nil {
					t.Fatal(err)
				}
				if err := execute("preview_pr1_writer", password, write); err != nil {
					t.Fatalf("isolated writer cannot write: %v", err)
				}
			}
			if engine == "postgresql" {
				t.Log("PostgreSQL bootstrap, reader queries, denied writes and revocation passed")
			} else {
				t.Logf("%s: bootstrap, reader queries, denied writes, revocation and isolated writer checks passed", engine)
			}
		})
	}
}
