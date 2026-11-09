package main

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"openfabrica/config"
	"openfabrica/db"
	"openfabrica/db/models"
	"openfabrica/internal/backup"
	"openfabrica/internal/singleinstance"
	"openfabrica/security"
)

// runSubcommand handles the maintenance subcommands (backup, restore, reset-admin). It reports
// whether args named one and, if so, the process exit code. They run without
// starting the server, so they work on a stopped instance.
func runSubcommand(args []string) (handled bool, code int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "backup":
		return true, cmdBackup(args[1:])
	case "restore":
		return true, cmdRestore(args[1:])
	case "reset-admin":
		return true, cmdResetAdmin(args[1:])
	case "eval":
		return true, cmdEval(args[1:])
	case "selfdestruct":
		return true, cmdSelfDestruct(args[1:])
	}
	return false, 0
}

// splitSubArgs separates --config from the remaining arguments of a subcommand.
func splitSubArgs(args []string) (configPath string, rest []string, err error) {
	configPath = config.DefaultConfigPath
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-c" || a == "--config":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("%s requires a path argument", a)
			}
			i++
			configPath = args[i]
		case strings.HasPrefix(a, "--config="):
			configPath = strings.TrimPrefix(a, "--config=")
		default:
			rest = append(rest, a)
		}
	}
	return configPath, rest, nil
}

func cmdBackup(args []string) int {
	configPath, rest, err := splitSubArgs(args)
	if err != nil || len(rest) > 1 {
		fmt.Fprintln(os.Stderr, "usage: openfabrica backup [file.zip] [--config <path>]")
		return 2
	}
	cfg, err := config.LoadFrom(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	dest := fmt.Sprintf("openfabrica-backup-%s.zip", time.Now().Format("20060102-150405"))
	if len(rest) == 1 {
		dest = rest[0]
	}
	if _, err := os.Stat(dest); err == nil {
		fmt.Fprintf(os.Stderr, "error: %s already exists\n", dest)
		return 1
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	m, err := backup.Create(out, backup.Options{DataDir: cfg.DataDir, DBPath: cfg.DBPath, Version: Version, Skip: dest})
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dest)
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	abs, _ := filepath.Abs(dest)
	fmt.Fprintf(os.Stdout, "Backed up %d project(s), %d file(s) to %s\n", len(m.Projects), m.Files, abs)
	fmt.Fprintln(os.Stdout, "The archive holds your encryption and signing keys. Store it as carefully as the data directory.")
	return 0
}

func cmdRestore(args []string) int {
	configPath, rest, err := splitSubArgs(args)
	force := false
	var file string
	for _, a := range rest {
		switch a {
		case "--force":
			force = true
		default:
			if file != "" {
				err = fmt.Errorf("unexpected argument %q", a)
			}
			file = a
		}
	}
	if err != nil || file == "" {
		fmt.Fprintln(os.Stderr, "usage: openfabrica restore <file.zip> [--force] [--config <path>]")
		return 2
	}
	// Restoring replaces files a running server holds open.
	if err := singleinstance.Acquire("OpenFabrica-SingleInstance"); err != nil {
		fmt.Fprintln(os.Stderr, "error: Open Fabrica is running. Stop it before restoring.")
		return 1
	}
	cfg, err := config.LoadFrom(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	f, err := os.Open(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	m, err := backup.Restore(f, info.Size(), backup.Options{DataDir: cfg.DataDir, DBPath: cfg.DBPath}, force)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "Restored %d project(s) from a backup made %s by version %s.\n", len(m.Projects), m.CreatedAt.Format(time.RFC3339), m.Version)
	return 0
}

// passwordAlphabet leaves out look-alike characters (0/O, 1/l/I) so a generated
// password can be read off a terminal and typed.
const passwordAlphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// generatePassword returns n random characters from passwordAlphabet.
func generatePassword(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = passwordAlphabet[int(b)%len(passwordAlphabet)]
	}
	return string(out), nil
}

// resetAdminPassword sets a new random password for username, or for the only
// admin when username is empty. Changing a password bumps the user's token
// version, so every session issued before the reset stops working.
func resetAdminPassword(sdb *sql.DB, username string) (*models.User, string, error) {
	var user *models.User
	if username != "" {
		u, err := models.GetUserByUsername(sdb, username)
		if err != nil || u == nil {
			return nil, "", fmt.Errorf("no user named %q", username)
		}
		user = u
	} else {
		users, err := models.ListUsers(sdb)
		if err != nil {
			return nil, "", err
		}
		var admins []models.User
		for _, u := range users {
			if u.Role == "admin" {
				admins = append(admins, u)
			}
		}
		switch len(admins) {
		case 0:
			return nil, "", errors.New("there is no admin user yet; open the admin page to run first-time setup")
		case 1:
			user = &admins[0]
		default:
			names := make([]string, len(admins))
			for i, a := range admins {
				names[i] = a.Username
			}
			return nil, "", fmt.Errorf("more than one admin exists (%s); name the user to reset", strings.Join(names, ", "))
		}
	}
	password, err := generatePassword(20)
	if err != nil {
		return nil, "", err
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return nil, "", err
	}
	if err := models.UpdateUserPassword(sdb, user.ID, hash); err != nil {
		return nil, "", err
	}
	return user, password, nil
}

func cmdResetAdmin(args []string) int {
	configPath, rest, err := splitSubArgs(args)
	if err != nil || len(rest) > 1 {
		fmt.Fprintln(os.Stderr, "usage: openfabrica reset-admin [username] [--config <path>]")
		return 2
	}
	cfg, err := config.LoadFrom(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if _, err := os.Stat(cfg.DBPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: no database at %s; nothing to reset\n", cfg.DBPath)
		return 1
	}
	database, err := db.Open(cfg.DBPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer func() { _ = database.Close() }()
	username := ""
	if len(rest) == 1 {
		username = rest[0]
	}
	user, password, err := resetAdminPassword(database.DB, username)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "Password for %q reset. New password (shown once): %s\n", user.Username, password)
	fmt.Fprintln(os.Stdout, "Every existing session for that user was signed out. Change the password after signing in.")
	return 0
}
