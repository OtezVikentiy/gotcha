package web

import "os"

func readAppCSS() (string, error) {
	b, err := os.ReadFile("static/app.css")
	return string(b), err
}
