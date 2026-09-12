package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// свой таймаут отдельно от таймаута Docker: отличает «сервер не отвечает» от «Docker передумал»
const healthcheckTimeout = 3 * time.Second

// порт совпадает со значением GOTCHA_LISTEN_ADDR по умолчанию (:8080)
const defaultHealthcheckURL = "http://127.0.0.1:8080/readyz"

// хост всегда 127.0.0.1 — GOTCHA_LISTEN_ADDR это адрес прослушивания, не назначения;
// непарсимое значение даёт дефолтный порт, об ошибке в адресе скажет сам сервер при старте
func defaultHealthcheckURLFor(getenv func(string) string) string {
	addr := getenv("GOTCHA_LISTEN_ADDR")
	if addr == "" {
		return defaultHealthcheckURL
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return defaultHealthcheckURL
	}
	return "http://127.0.0.1:" + port + "/readyz"
}

// подкоманда бинаря, а не curl/wget: не ломается молча при переходе на distroless-образ
func healthcheckRequested(args []string, getenv func(string) string) (url string, ok bool) {
	url = defaultHealthcheckURLFor(getenv)
	for i, a := range args {
		switch {
		case a == "--healthcheck" || a == "healthcheck":
			ok = true
		case strings.HasPrefix(a, "--healthcheck-url="):
			url = strings.TrimPrefix(a, "--healthcheck-url=")
		case a == "--healthcheck-url" && i+1 < len(args):
			url = args[i+1]
		}
	}
	return url, ok
}

// тело ответа в stderr: `docker inspect` показывает вывод последних проверок
func runHealthcheck(url string) int {
	client := &http.Client{Timeout: healthcheckTimeout}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: %s: %s\n", resp.Status, strings.TrimSpace(string(buf[:n])))
		return 1
	}
	return 0
}
