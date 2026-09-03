package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// healthcheckTimeout — сколько ждём ответа своей же ручки готовности.
// Docker убивает проверку по своему таймауту; свой нужен, чтобы отличить
// «сервер не отвечает» от «Docker передумал».
const healthcheckTimeout = 3 * time.Second

// defaultHealthcheckURL — куда стучится проверка по умолчанию. Порт совпадает с
// портом внутри контейнера (GOTCHA_LISTEN_ADDR по умолчанию :8080).
const defaultHealthcheckURL = "http://127.0.0.1:8080/readyz"

// defaultHealthcheckURLFor строит дефолтный URL из GOTCHA_LISTEN_ADDR: оператор,
// сменивший порт, не должен получать второй независимый симптом от той же
// правки (проверка ходила бы в мёртвый :8080 — «unhealthy» при живом
// приложении). Хост всегда 127.0.0.1: проверка ходит к себе, а GOTCHA_LISTEN_ADDR
// вида 0.0.0.0:9000 или [::]:9000 — адрес ПРОСЛУШИВАНИЯ, адресом назначения
// он быть не может. Непарсибельное значение → дефолтный порт: об опечатке в
// GOTCHA_LISTEN_ADDR скажет сам сервер при старте, работа проверки — не маскировать
// это своей ошибкой.
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

// healthcheckRequested разбирает аргументы проверки состояния и возвращает URL.
//
// Проверка сделана подкомандой самого бинаря, а не вызовом curl/wget из
// HEALTHCHECK, намеренно: тогда она зависит только от того, что в образе точно
// есть — от самого gotcha. Переход на distroless-базу не сломает её молча.
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

// runHealthcheck запрашивает URL и возвращает код выхода: 0 — готов, 1 — нет.
// Тело ответа печатается в stderr: `docker inspect` показывает вывод последних
// проверок, и «not ready» с перечнем компонентов там полезнее пустоты.
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
