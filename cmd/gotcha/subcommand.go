package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/db"
)

// Подкоманда — первый аргумент без ведущего "-" (--version/--healthcheck остаются флагами,
// разбираются раньше в main()). Новая подкоманда — новая запись в этой карте.
var subcommands = map[string]func(args []string, getenv func(string) string) int{
	"set-password": runSetPassword,
}

func dispatchSubcommand(args []string, getenv func(string) string) (code int, handled bool) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return 0, false
	}
	fn, ok := subcommands[args[0]]
	if !ok {
		return 0, false
	}
	return fn(args[1:], getenv), true
}

const setPasswordTimeout = 15 * time.Second

func runSetPassword(args []string, getenv func(string) string) int {
	email, err := parseSetPasswordArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "set-password:", err)
		return 2
	}
	password, err := readPassword(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "set-password: reading password from stdin:", err)
		return 2
	}

	// Тот же загрузчик, что у сервера, без своей копии разбора DSN; args не передаём —
	// set-password не знает про --mode/--migrate-* и не должно их парсить.
	cfg, err := loadConfigWithLogging(getenv, os.Environ, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "set-password:", err)
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), setPasswordTimeout)
	defer cancel()

	pool, err := db.NewPostgres(ctx, cfg.PostgresDSN)
	if err != nil {
		fmt.Fprintln(os.Stderr, "set-password: connect to postgres:", err)
		return 1
	}
	defer pool.Close()

	authSvc := auth.NewService(pool)
	if err := authSvc.AdminSetPassword(ctx, email, password); err != nil {
		switch {
		case errors.Is(err, auth.ErrUserNotFound):
			fmt.Fprintf(os.Stderr, "set-password: no user with email %q\n", email)
		case errors.Is(err, auth.ErrWeakPassword):
			fmt.Fprintln(os.Stderr, "set-password:", err)
		default:
			fmt.Fprintln(os.Stderr, "set-password: failed:", err)
		}
		return 1
	}
	fmt.Println("set-password: password updated, all existing sessions for this user were terminated")
	return 0
}

func parseSetPasswordArgs(args []string) (email string, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "--email="):
			email = strings.TrimPrefix(a, "--email=")
		case a == "--email" && i+1 < len(args):
			i++
			email = args[i]
		case a == "--password" || strings.HasPrefix(a, "--password="):
			return "", errors.New("the password is never a command-line argument (shell history, ps); " +
				"provide it on stdin")
		default:
			return "", fmt.Errorf("unrecognized argument %q (usage: set-password --email=<address>, password on stdin)", a)
		}
	}
	if email == "" {
		return "", errors.New("--email is required (usage: set-password --email=<address>, password on stdin)")
	}
	return email, nil
}

// Пароль — только через stdin: аргумент оседает в истории оболочки и ps, переменная
// окружения читаема из /proc/<pid>/environ любым процессом того же UID.
func readPassword(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", errors.New("empty password")
	}
	return line, nil
}
