package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func jsonLiteral(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func mysqlLiteral(value string) string {
	// Hex literals avoid dependence on NO_BACKSLASH_ESCAPES and connection quoting.
	return fmt.Sprintf("0x%x", []byte(value))
}

func (c databaseConfig) connectionURL(host, user, password string) string {
	u := url.URL{Host: host + ":" + c.port, User: url.UserPassword(user, password), Path: "/" + c.database}
	switch c.engine {
	case "postgresql":
		u.Scheme, u.RawQuery = "postgres", "sslmode=disable"
	case "mysql", "mariadb":
		u.Scheme = "mysql"
	case "mongodb":
		u.Scheme = "mongodb"
		u.RawQuery = "authSource=" + url.QueryEscape(c.database)
		if user == c.admin {
			u.RawQuery = "authSource=admin"
		}
	case "redis":
		u.Scheme = "redis"
	case "sqlserver":
		u.Scheme, u.Path = "sqlserver", ""
		u.RawQuery = "database=" + url.QueryEscape(c.database)
	}
	return u.String()
}

func (l lifecycle) bootstrap() (string, error) {
	var parts []string
	for _, key := range []string{"DATABASE_SCHEMA_FILE", "DATABASE_BOOTSTRAP_FILE"} {
		path := os.Getenv(key)
		if path == "" {
			continue
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join(l.root, path))
		if err != nil {
			return "", err
		}
		root, err := filepath.EvalSymlinks(l.root)
		if err != nil {
			return "", err
		}
		relative, err := filepath.Rel(root, resolved)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return "", errors.New("bootstrap/schema file escapes the application checkout")
		}
		data, err := os.ReadFile(resolved)
		if err != nil {
			return "", err
		}
		text := string(data)
		if l.config.engine == "redis" {
			var commands [][]string
			if err := json.Unmarshal(data, &commands); err != nil {
				return "", fmt.Errorf("Redis bootstrap must contain a JSON array of command arrays: %w", err)
			}
			for _, args := range commands {
				if len(args) == 0 {
					return "", errors.New("Redis bootstrap commands must not be empty")
				}
				for i, arg := range args {
					if strings.Contains(arg, "{{BOOTSTRAP_PASSWORD}}") && os.Getenv("DATABASE_BOOTSTRAP_PASSWORD") == "" {
						return "", errors.New("bootstrap file requires the bootstrap_password secret")
					}
					args[i] = strings.ReplaceAll(arg, "{{BOOTSTRAP_PASSWORD}}", os.Getenv("DATABASE_BOOTSTRAP_PASSWORD"))
				}
				parts = append(parts, redisCommand(args...))
			}
			continue
		}
		if strings.Contains(text, "{{BOOTSTRAP_PASSWORD}}") {
			password := os.Getenv("DATABASE_BOOTSTRAP_PASSWORD")
			if password == "" {
				return "", errors.New("bootstrap file requires the bootstrap_password secret")
			}
			encoded := literal(password)
			switch l.config.engine {
			case "mysql", "mariadb":
				encoded = mysqlLiteral(password)
			case "mongodb":
				encoded = jsonLiteral(password)
			case "sqlserver":
				encoded = "N" + literal(password)
			}
			text = strings.ReplaceAll(text, "{{BOOTSTRAP_PASSWORD}}", encoded)
		}
		parts = append(parts, text)
	}
	if l.config.engine == "redis" {
		return strings.Join(parts, ""), nil
	}
	return strings.Join(parts, "\n"), nil
}

// Credentials and statements travel over stdin, never kubectl's argv or logs.
func (l lifecycle) executeEngine(namespace, statement string) error {
	c := l.config
	var script string
	switch c.engine {
	case "mysql", "mariadb":
		client := "mysql"
		if c.engine == "mariadb" {
			client = "mariadb"
		}
		script = `IFS= read -r MYSQL_PWD; export MYSQL_PWD; exec ` + client + ` --protocol=socket --batch --user=` + c.admin
	case "mongodb":
		statement = `const admin = db.getSiblingDB("admin"); const auth = await admin.auth(` + jsonLiteral(c.admin) + `,` +
			jsonLiteral(os.Getenv("DATABASE_PASSWORD")) + `); if (auth !== true && auth !== 1 && auth?.ok !== 1) { throw new Error("Admin authentication failed"); }
db = db.getSiblingDB(` + jsonLiteral(c.database) + ");\n" + statement
		script = `exec mongosh --quiet --norc --file /dev/stdin`
	case "redis":
		script = `IFS= read -r REDISCLI_AUTH; export REDISCLI_AUTH; exec redis-cli --user ` + c.admin + ` --no-auth-warning --pipe`
	case "sqlserver":
		script = `IFS= read -r SQLCMDPASSWORD; export SQLCMDPASSWORD; exec /opt/mssql-tools18/bin/sqlcmd -S localhost -U ` + c.admin + ` -C -b -r1`
	default:
		return errors.New("unsupported engine execution")
	}
	input := statement
	if c.engine != "mongodb" {
		input = os.Getenv("DATABASE_PASSWORD") + "\n" + statement
	}
	args := []string{"-n", namespace, "exec", "-i", c.workload + "-0"}
	if c.container != "" {
		args = append(args, "-c", c.container)
	}
	args = append(args, "--", "sh", "-ec", script)
	_, err := command("", input, "kubectl", args...)
	return err
}

func (l lifecycle) previewCredential() (string, string, error) {
	user := fmt.Sprintf("preview_pr%d", l.pr)
	if !l.readOnly() {
		user += "_writer"
	}
	connection, err := get("secret", "argocd", l.connectionName())
	if err != nil {
		return "", "", err
	}
	if connection != nil {
		if !l.owned(connection) || connection.Metadata.Labels["craftedbeyond.com/pr"] != fmt.Sprint(l.pr) {
			return "", "", errors.New("connection Secret ownership does not match this PR")
		}
		if raw := connection.Data["DATABASE_ENGINE"]; raw != "" {
			engine, err := base64.StdEncoding.DecodeString(raw)
			if err != nil || string(engine) != l.config.engine {
				return "", "", errors.New("existing connection uses a different database engine")
			}
		}
		password, err := base64.StdEncoding.DecodeString(connection.Data["DATABASE_PASSWORD"])
		if err != nil || len(password) < 24 {
			return "", "", errors.New("existing preview password is invalid")
		}
		return user, string(password), nil
	}
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", "", err
	}
	password := base64.RawURLEncoding.EncodeToString(data)
	if l.config.engine == "sqlserver" {
		password = "Aa1_" + password
	}
	return user, password, nil
}

func (l lifecycle) validateExistingEngine() error {
	connection, err := get("secret", "argocd", l.connectionName())
	if err != nil || connection == nil {
		return err
	}
	if !l.owned(connection) || connection.Metadata.Labels["craftedbeyond.com/pr"] != fmt.Sprint(l.pr) {
		return errors.New("existing connection Secret belongs to another deployment")
	}
	engine := "postgresql" // Legacy PostgreSQL connections predate the engine field.
	if raw := connection.Data["DATABASE_ENGINE"]; raw != "" {
		value, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return err
		}
		engine = string(value)
	}
	if engine != l.config.engine {
		return errors.New("changing the engine of an existing deployment requires an explicit data migration")
	}
	return nil
}

func redisCommand(args ...string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "*%d\r\n", len(args))
	for _, arg := range args {
		fmt.Fprintf(&out, "$%d\r\n%s\r\n", len(arg), arg)
	}
	return out.String()
}

func (l lifecycle) engineBootstrap(schema string) string {
	c := l.config
	switch c.engine {
	case "mysql", "mariadb":
		return "CREATE DATABASE IF NOT EXISTS `" + c.database + "`;\nUSE `" + c.database + "`;\n" + schema
	case "sqlserver":
		return "IF DB_ID(" + literal(c.database) + ") IS NULL EXEC('CREATE DATABASE [" + c.database + "]');\nGO\nUSE [" + c.database + "];\nGO\n" + schema
	default:
		return schema
	}
}

func (l lifecycle) accessStatements(user, password string) string {
	c := l.config
	inactive := fmt.Sprintf("preview_pr%d_writer", l.pr)
	if !l.readOnly() {
		inactive = fmt.Sprintf("preview_pr%d", l.pr)
	}
	switch c.engine {
	case "mysql", "mariadb":
		// Existing accounts are dropped to remove all stale grants, then recreated.
		privileges := "SELECT"
		if !l.readOnly() {
			privileges = "ALL PRIVILEGES"
		}
		return l.mysqlRevoke(inactive) + l.mysqlRevoke(user) +
			fmt.Sprintf("SET SESSION sql_mode='NO_BACKSLASH_ESCAPES';\nCREATE USER '%s'@'%%' IDENTIFIED BY %s;\nGRANT %s ON `%s`.* TO '%s'@'%%';\n",
				user, literal(password), privileges, c.database, user)
	case "mongodb":
		role := "read"
		if !l.readOnly() {
			role = "readWrite"
		}
		return l.mongoRevoke(inactive) +
			"const spec = {pwd:" + jsonLiteral(password) + ",roles:[{role:" + jsonLiteral(role) + ",db:" + jsonLiteral(c.database) + "}]};\n" +
			"if (await db.getUser(" + jsonLiteral(user) + ")) await db.updateUser(" + jsonLiteral(user) + ",spec); else await db.createUser(Object.assign({user:" + jsonLiteral(user) + "},spec));\n" +
			"await admin.runCommand({invalidateUserCache:1});\n"
	case "redis":
		commands := []string{"ACL", "SETUSER", user, "reset", "on", ">" + password, "~application:*", "+ping", "+echo", "+get", "+mget", "+exists", "+ttl", "+pttl", "+type", "+strlen", "+hget", "+hmget", "+hgetall", "+llen", "+lrange", "+scard", "+smembers", "+zcard", "+zrange"}
		if !l.readOnly() {
			commands = append(commands, "+set", "+mset", "+del", "+unlink", "+expire", "+persist", "+incr", "+decr", "+hset", "+hdel", "+lpush", "+rpush", "+lpop", "+rpop", "+sadd", "+srem", "+zadd", "+zrem")
		}
		return redisRevoke(inactive) + redisRevoke(user) + redisCommand(commands...) + redisCommand("ACL", "SAVE")
	case "sqlserver":
		role := "db_datareader"
		if !l.readOnly() {
			role = "db_owner"
		}
		// LOGIN is disabled first; existing sessions are explicitly terminated.
		return l.sqlServerRevoke(inactive) + l.sqlServerRevoke(user) +
			fmt.Sprintf("USE [master];\nCREATE LOGIN [%s] WITH PASSWORD=N%s, CHECK_POLICY=ON;\nUSE [%s];\nCREATE USER [%s] FOR LOGIN [%s];\nALTER ROLE [%s] ADD MEMBER [%s];\n",
				user, literal(password), c.database, user, user, role, user)
	}
	return ""
}

func redisRevoke(user string) string {
	return redisCommand("ACL", "SETUSER", user, "reset", "off") +
		redisCommand("CLIENT", "KILL", "USER", user, "SKIPME", "yes") + redisCommand("ACL", "DELUSER", user)
}

func (l lifecycle) mysqlRevoke(user string) string {
	return fmt.Sprintf("USE `%s`;\n", l.config.database) + fmt.Sprintf(`DROP USER IF EXISTS '%[1]s'@'%%';
DROP PROCEDURE IF EXISTS workflow_revoke_%[1]s;
DELIMITER //
CREATE PROCEDURE workflow_revoke_%[1]s()
BEGIN
  DECLARE done INT DEFAULT 0;
  DECLARE connection_id BIGINT;
  DECLARE sessions CURSOR FOR SELECT ID FROM information_schema.PROCESSLIST WHERE USER='%[1]s';
  DECLARE CONTINUE HANDLER FOR NOT FOUND SET done=1;
  DECLARE CONTINUE HANDLER FOR 1094 BEGIN END;
  OPEN sessions;
  session_loop: LOOP
    FETCH sessions INTO connection_id;
    IF done THEN LEAVE session_loop; END IF;
    SET @kill=CONCAT('KILL CONNECTION ',connection_id);
    PREPARE stmt FROM @kill;
    EXECUTE stmt;
    DEALLOCATE PREPARE stmt;
  END LOOP;
  CLOSE sessions;
END//
DELIMITER ;
CALL workflow_revoke_%[1]s();
DROP PROCEDURE workflow_revoke_%[1]s;
`, user)
}

func (l lifecycle) mongoRevoke(user string) string {
	return "if (await db.getUser(" + jsonLiteral(user) + ")) await db.dropUser(" + jsonLiteral(user) + ");\n" +
		"await admin.runCommand({killAllSessionsByPattern:[{users:[{user:" + jsonLiteral(user) +
		",db:" + jsonLiteral(l.config.database) + "}]}]});\n" +
		"await admin.runCommand({invalidateUserCache:1});\n"
}

func (l lifecycle) sqlServerRevoke(user string) string {
	return fmt.Sprintf(`USE [master];
IF SUSER_ID(N'%s') IS NOT NULL ALTER LOGIN [%s] DISABLE;
DECLARE @kill nvarchar(max)=N'';
SELECT @kill=@kill+N'KILL '+CAST(session_id AS nvarchar(12))+N';' FROM sys.dm_exec_sessions WHERE login_name=N'%s';
EXEC sp_executesql @kill;
USE [%s];
IF USER_ID(N'%s') IS NOT NULL DROP USER [%s];
USE [master];
IF SUSER_ID(N'%s') IS NOT NULL DROP LOGIN [%s];
GO
`, user, user, user, l.config.database, user, user, user, user)
}

func (l lifecycle) provisionEngine(namespace string, temporary bool) error {
	c := l.config
	if temporary || l.pr == 0 {
		schema, err := l.bootstrap()
		if err != nil {
			return err
		}
		statement := l.engineBootstrap(schema)
		if statement != "" {
			if err := l.executeEngine(namespace, statement); err != nil {
				return errors.New("database bootstrap failed; backend lock retained (inspect database server logs)")
			}
		}
	}
	user, password := c.admin, os.Getenv("DATABASE_PASSWORD")
	if l.pr > 0 {
		var err error
		user, password, err = l.previewCredential()
		if err != nil {
			return err
		}
		if err := l.executeEngine(namespace, l.accessStatements(user, password)); err != nil {
			return errors.New("preview database access provisioning failed; backend lock retained")
		}
	}
	return l.connection(namespace, c.service+"."+namespace+".svc.cluster.local", user, password, temporary)
}

func (l lifecycle) revokeEngineReader(namespace string) error {
	user := fmt.Sprintf("preview_pr%d", l.pr)
	var statement string
	switch l.config.engine {
	case "mysql", "mariadb":
		statement = l.mysqlRevoke(user)
	case "mongodb":
		statement = l.mongoRevoke(user)
	case "redis":
		statement = redisRevoke(user) + redisCommand("ACL", "SAVE")
	case "sqlserver":
		statement = l.sqlServerRevoke(user)
	default:
		return errors.New("unsupported engine revocation")
	}
	return l.executeEngine(namespace, statement)
}
