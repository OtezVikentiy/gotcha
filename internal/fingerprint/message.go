package fingerprint

import "regexp"

var (
	reUUID = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	reHex  = regexp.MustCompile(`\b(0[xX][0-9a-fA-F]+|[0-9a-fA-F]{8,})\b`)
	reNum  = regexp.MustCompile(`\b\d+`)
)

// NormalizeMessage заменяет динамические части сообщения плейсхолдерами,
// чтобы «user 123 not found» и «user 456 not found» попадали в одну группу.
func NormalizeMessage(s string) string {
	s = reUUID.ReplaceAllString(s, "<uuid>")
	s = reHex.ReplaceAllString(s, "<hex>")
	s = reNum.ReplaceAllString(s, "<num>")
	return s
}
