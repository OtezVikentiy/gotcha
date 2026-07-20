package web

import "os"

// readAppCSS — таблица стилей читается из файла, а не из embed: она отдаётся
// со /static как есть, и тесты проверяют ровно тот текст, который получит
// браузер.
func readAppCSS() (string, error) {
	b, err := os.ReadFile("static/app.css")
	return string(b), err
}
