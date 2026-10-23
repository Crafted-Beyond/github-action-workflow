package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type databaseConfig struct {
	engine, database, admin, workload, service, pvc, secret, passwordKey, container, port string
}

func setting(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func loadConfig() (databaseConfig, error) {
	engine := setting("DATABASE_ENGINE", "postgresql")
	c := databaseConfig{engine: engine, database: setting("DATABASE_NAME", "application")}
	switch engine {
	case "postgresql":
		c.admin, c.workload, c.port, c.passwordKey = "application_owner", "postgres", "5432", "POSTGRES_PASSWORD"
	case "mysql", "mariadb":
		c.admin, c.workload, c.port, c.passwordKey = "root", engine, "3306", "MYSQL_ROOT_PASSWORD"
		if engine == "mariadb" {
			c.passwordKey = "MARIADB_ROOT_PASSWORD"
		}
	case "mongodb":
		c.admin, c.workload, c.port, c.passwordKey = "root", "mongodb", "27017", "MONGO_INITDB_ROOT_PASSWORD"
	case "redis":
		c.admin, c.workload, c.port, c.passwordKey = "default", "redis", "6379", "REDIS_PASSWORD"
		if c.database != "application" && c.database != "0" {
			return c, errors.New("Redis uses database 0; key isolation uses the application: prefix")
		}
		c.database = "0"
	case "sqlserver":
		c.admin, c.workload, c.port, c.passwordKey = "sa", "sqlserver", "1433", "MSSQL_SA_PASSWORD"
	default:
		return c, fmt.Errorf("unsupported DATABASE_ENGINE %q", engine)
	}
	c.admin = setting("DATABASE_ADMIN_USERNAME", c.admin)
	c.workload = setting("DATABASE_WORKLOAD", c.workload)
	c.service = setting("DATABASE_SERVICE", c.workload)
	c.pvc = setting("DATABASE_PVC", c.workload+"-data")
	c.secret = setting("DATABASE_ADMIN_SECRET", c.workload+"-admin")
	c.passwordKey = setting("DATABASE_PASSWORD_KEY", c.passwordKey)
	c.container = os.Getenv("DATABASE_CONTAINER")
	for _, v := range []string{c.database, c.admin} {
		if !(c.engine == "redis" && v == "0") && !regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,62}$`).MatchString(v) {
			return c, errors.New("database and admin names must be simple identifiers (Redis database 0 is supported)")
		}
	}
	for _, v := range []string{c.workload, c.service, c.pvc, c.secret} {
		if len(v) > 63 || !regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`).MatchString(v) {
			return c, errors.New("database Kubernetes names must be DNS labels of at most 63 characters")
		}
	}
	if c.container != "" && !regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`).MatchString(c.container) {
		return c, errors.New("invalid database container name")
	}
	if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(c.passwordKey) ||
		!regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(setting("DATABASE_PASSWORD_VARIABLE", "database_password")) {
		return c, errors.New("invalid database password key or Terraform variable name")
	}
	for _, key := range []string{"DATABASE_DIRECTORY", "API_DIRECTORY", "DATABASE_VAR_FILE", "API_VAR_FILE", "DATABASE_SCHEMA_FILE", "DATABASE_BOOTSTRAP_FILE"} {
		value := os.Getenv(key)
		if value != "" && (filepath.IsAbs(value) || filepath.Clean(value) == ".." ||
			strings.HasPrefix(filepath.Clean(value), ".."+string(filepath.Separator))) {
			return c, fmt.Errorf("%s must stay within its checkout/root", key)
		}
	}
	return c, nil
}

func (l lifecycle) databaseDirectory() string {
	return filepath.Join(l.root, setting("DATABASE_DIRECTORY", "infra/database"))
}

func (l lifecycle) apiDirectory() string {
	return filepath.Join(l.root, setting("API_DIRECTORY", "infra/api"))
}

func releaseBranch() string { return setting("RELEASE_BRANCH", "main") }

func (l lifecycle) validateDirectory(directory string) error {
	root, err := filepath.EvalSymlinks(l.root)
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("Terraform root escapes the application checkout")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("Terraform root is not a directory")
	}
	return nil
}
