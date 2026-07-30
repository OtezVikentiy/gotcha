package docs

// SlugifyForTest открывает разбор заголовка тестам пакета docs_test: якорь —
// часть публичного поведения (он попадает в адресную строку и в чужие ссылки),
// поэтому проверяется, а не остаётся деталью реализации.
func SlugifyForTest(s string) string { return slugify(s) }
