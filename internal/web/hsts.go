package web

import "strconv"

// max-age=0 — законное значение, не «выключено»: браузер, однажды получивший большой
// max-age, держит pin год; снять его может только реально отправленный max-age=0.
func HSTSHeaderValue(enabled bool, maxAgeSeconds int, includeSubDomains, preload bool) string {
	if !enabled || maxAgeSeconds < 0 {
		return ""
	}
	v := "max-age=" + strconv.Itoa(maxAgeSeconds)
	if includeSubDomains {
		v += "; includeSubDomains"
	}
	if preload {
		v += "; preload"
	}
	return v
}
