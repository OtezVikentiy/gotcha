package trace

import (
	"strings"
	"testing"
)

func TestNormalizeSQL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "числовой литерал",
			in:   "SELECT * FROM users WHERE id = 42",
			want: "SELECT * FROM users WHERE id = ?",
		},
		{
			name: "другой числовой литерал даёт ту же форму",
			in:   "SELECT * FROM users WHERE id = 1337",
			want: "SELECT * FROM users WHERE id = ?",
		},
		{
			name: "строковый литерал",
			in:   "SELECT * FROM users WHERE email = 'foo@example.com'",
			want: "SELECT * FROM users WHERE email = ?",
		},
		{
			name: "строковый литерал с экранированной кавычкой",
			in:   "SELECT * FROM users WHERE name = 'O''Brien'",
			want: "SELECT * FROM users WHERE name = ?",
		},
		{
			name: "строковый литерал с обратным слешем",
			in:   `SELECT * FROM users WHERE name = 'it\'s me'`,
			want: "SELECT * FROM users WHERE name = ?",
		},
		{
			name: "точка с запятой внутри литерала не рвёт запрос",
			in:   "SELECT * FROM t WHERE s = 'a;b' AND id = 7",
			want: "SELECT * FROM t WHERE s = ? AND id = ?",
		},
		{
			name: "IN со списком чисел",
			in:   "SELECT * FROM users WHERE id IN (1, 2, 3)",
			want: "SELECT * FROM users WHERE id IN (?)",
		},
		{
			name: "IN со списком другой длины даёт ту же форму",
			in:   "SELECT * FROM users WHERE id IN (4,5)",
			want: "SELECT * FROM users WHERE id IN (?)",
		},
		{
			name: "IN со списком строк",
			in:   "SELECT * FROM users WHERE role IN ('admin', 'user')",
			want: "SELECT * FROM users WHERE role IN (?)",
		},
		{
			name: "in в нижнем регистре: регистр ключевого слова сохраняется",
			in:   "select * from users where id in (1,2,3)",
			want: "select * from users where id in (?)",
		},
		{
			name: "NOT IN",
			in:   "SELECT * FROM users WHERE id NOT IN (1, 2)",
			want: "SELECT * FROM users WHERE id NOT IN (?)",
		},
		{
			name: "IN с подзапросом не схлопывается",
			in:   "SELECT * FROM users WHERE id IN (SELECT user_id FROM bans)",
			want: "SELECT * FROM users WHERE id IN (SELECT user_id FROM bans)",
		},
		{
			name: "IN с одним плейсхолдером",
			in:   "SELECT * FROM users WHERE id IN (?)",
			want: "SELECT * FROM users WHERE id IN (?)",
		},
		{
			name: "join не путается с in",
			in:   "SELECT * FROM a JOIN b ON a.id = b.a_id WHERE a.x = 1",
			want: "SELECT * FROM a JOIN b ON a.id = b.a_id WHERE a.x = ?",
		},
		{
			name: "уже параметризованный запрос с $1 не трогаем",
			in:   "SELECT * FROM users WHERE id = $1 AND org_id = $2",
			want: "SELECT * FROM users WHERE id = $1 AND org_id = $2",
		},
		{
			name: "уже параметризованный запрос с ? не трогаем",
			in:   "SELECT * FROM users WHERE id = ? AND org_id = ?",
			want: "SELECT * FROM users WHERE id = ? AND org_id = ?",
		},
		{
			name: "именованные плейсхолдеры не трогаем",
			in:   "SELECT * FROM users WHERE id = :id AND name = :name",
			want: "SELECT * FROM users WHERE id = :id AND name = :name",
		},
		{
			name: "каст :: не ломается",
			in:   "SELECT id::text FROM users WHERE id = 1",
			want: "SELECT id::text FROM users WHERE id = ?",
		},
		{
			name: "многострочный запрос со схлопыванием пробелов",
			in:   "SELECT *\n  FROM   users\n\tWHERE id = 42\n",
			want: "SELECT * FROM users WHERE id = ?",
		},
		{
			name: "строчный комментарий выбрасывается",
			in:   "SELECT * FROM users -- комментарий Doctrine\nWHERE id = 42",
			want: "SELECT * FROM users WHERE id = ?",
		},
		{
			name: "блочный комментарий выбрасывается",
			in:   "SELECT /* trace=abc */ * FROM users WHERE id = 42",
			want: "SELECT * FROM users WHERE id = ?",
		},
		{
			name: "незакрытый блочный комментарий не зацикливается",
			in:   "SELECT * FROM users /* хвост",
			want: "SELECT * FROM users",
		},
		{
			name: "незакрытый литерал не зацикливается",
			in:   "SELECT * FROM users WHERE name = 'foo",
			want: "SELECT * FROM users WHERE name = ?",
		},
		{
			name: "кавычки-идентификаторы сохраняются",
			in:   `SELECT "user"."id" FROM "user" WHERE "user"."id" = 42`,
			want: `SELECT "user"."id" FROM "user" WHERE "user"."id" = ?`,
		},
		{
			name: "обратные кавычки MySQL сохраняются",
			in:   "SELECT `id` FROM `users` WHERE `id` = 42",
			want: "SELECT `id` FROM `users` WHERE `id` = ?",
		},
		{
			name: "цифры в имени таблицы не литерал",
			in:   "SELECT * FROM users2 WHERE id = 42",
			want: "SELECT * FROM users2 WHERE id = ?",
		},
		{
			name: "float и экспонента",
			in:   "SELECT * FROM m WHERE v > 3.14 AND w < 1.2e-5",
			want: "SELECT * FROM m WHERE v > ? AND w < ?",
		},
		{
			name: "INSERT с литералами",
			in:   "INSERT INTO users (name, age) VALUES ('bob', 30)",
			want: "INSERT INTO users (name, age) VALUES (?, ?)",
		},
		{
			name: "юникод в литерале",
			in:   "SELECT * FROM users WHERE name = 'Пётр' AND id = 5",
			want: "SELECT * FROM users WHERE name = ? AND id = ?",
		},
		{
			name: "юникод в идентификаторе сохраняется",
			in:   "SELECT имя FROM люди WHERE id = 5",
			want: "SELECT имя FROM люди WHERE id = ?",
		},
		{
			name: "E-строка Postgres",
			in:   `SELECT * FROM t WHERE s = E'a\nb' AND id = 1`,
			want: "SELECT * FROM t WHERE s = ? AND id = ?",
		},
		{
			name: "пустая строка",
			in:   "",
			want: "",
		},
		{
			name: "только пробелы",
			in:   "   \n\t ",
			want: "",
		},
		{
			name: "не-SQL мусор не мангается",
			in:   "не запрос вовсе",
			want: "не запрос вовсе",
		},
		{
			name: "NULL остаётся ключевым словом",
			in:   "SELECT * FROM users WHERE deleted_at IS NULL",
			want: "SELECT * FROM users WHERE deleted_at IS NULL",
		},
		{
			// standard_conforming_strings=on (дефолт Postgres): 'C:\' — ПОЛНЫЙ
			// литерал, обратный слеш в нём обычный символ. Если съесть закрывающую
			// кавычку, сканер пересинхронизируется на следующей и вывалит наружу
			// содержимое соседнего литерала.
			name: "backslash в конце литерала (Postgres) не съедает следующий литерал",
			in:   `SELECT * FROM f WHERE path = 'C:\' AND name = 'alice' AND id = 1`,
			want: "SELECT * FROM f WHERE path = ? AND name = ? AND id = ?",
		},
		{
			name: "оператор #> (JSON path Postgres) не комментарий",
			in:   "SELECT data #> '{a,b}' FROM t WHERE id = 1",
			want: "SELECT data #> ? FROM t WHERE id = ?",
		},
		{
			name: "оператор #>> не комментарий",
			in:   "SELECT data #>> '{a,b}' FROM t WHERE id = 2",
			want: "SELECT data #>> ? FROM t WHERE id = ?",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeSQL(tt.in)
			if got != tt.want {
				t.Fatalf("NormalizeSQL(%q)\n got: %q\nwant: %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestNormalizeSQLIsStable — нормализация детерминирована и идемпотентна:
// повторный прогон по уже нормализованному запросу ничего не меняет (иначе
// фингерпринт N+1 плыл бы между репликами).
func TestNormalizeSQLIsStable(t *testing.T) {
	queries := []string{
		"SELECT * FROM users WHERE id = 42",
		"SELECT * FROM users WHERE id IN (1,2,3)",
		"INSERT INTO users (name, age) VALUES ('bob', 30)",
		"SELECT * FROM users WHERE id = $1",
		"",
	}
	for _, q := range queries {
		once := NormalizeSQL(q)
		if twice := NormalizeSQL(once); twice != once {
			t.Fatalf("не идемпотентно для %q: %q -> %q", q, once, twice)
		}
	}
}

// TestNormalizeSQLGroupsNPlusOne — тот самый случай, ради которого всё
// затевалось: N запросов с разными литералами должны схлопнуться в одну форму.
func TestNormalizeSQLGroupsNPlusOne(t *testing.T) {
	var forms []string
	for _, q := range []string{
		"SELECT * FROM posts WHERE user_id = 1",
		"SELECT * FROM posts WHERE user_id = 2",
		"SELECT * FROM posts WHERE user_id = 999999",
	} {
		forms = append(forms, NormalizeSQL(q))
	}
	for _, f := range forms[1:] {
		if f != forms[0] {
			t.Fatalf("формы разошлись: %q != %q", f, forms[0])
		}
	}
}

// TestNormalizeSQLNeverLeaksLiterals — самое важное свойство нормализации:
// содержимое литерала (значение ПОЛЬЗОВАТЕЛЯ) не имеет права попасть в вывод.
// Вывод уезжает в title проблемы и в её фингерпринт: утечка означает и PII в
// хранилище, и склейку разных запросов в одну проблему.
func TestNormalizeSQLNeverLeaksLiterals(t *testing.T) {
	cases := []string{
		`SELECT * FROM f WHERE path = 'C:\' AND name = 'alice' AND id = 1`,
		`SELECT * FROM f WHERE path = 'C:\\' AND name = 'alice'`,
		`SELECT * FROM f WHERE a = 'x\' AND b = 'alice' AND c = 'y\' AND d = 'bob'`,
		`INSERT INTO t (a, b) VALUES ('ends with backslash \', 'alice')`,
		`SELECT * FROM t WHERE s = E'a\'b' AND name = 'alice'`,
	}
	for _, q := range cases {
		got := NormalizeSQL(q)
		if strings.Contains(got, "alice") || strings.Contains(got, "bob") {
			t.Errorf("значение утекло в нормализованный запрос:\n  in: %s\n out: %s", q, got)
		}
	}
}

// TestNormalizeSQLKeepsDifferentQueriesApart — обратная сторона группировки:
// разные по смыслу запросы НЕ должны схлопнуться в одну форму (иначе одна
// проблема поглотит другую и в title попадёт чужой запрос).
func TestNormalizeSQLKeepsDifferentQueriesApart(t *testing.T) {
	queries := []string{
		"SELECT * FROM users WHERE id = 1",
		"SELECT * FROM orders WHERE id = 1",
		"SELECT * FROM users WHERE email = 'a@b.c'",
		"UPDATE users SET name = 'x' WHERE id = 1",
		"DELETE FROM users WHERE id = 1",
		"INSERT INTO users (name) VALUES ('x')",
		"SELECT * FROM users WHERE id = 1 AND org_id = 2",
		"SELECT data #> '{a}' FROM t WHERE id = 1",
		"SELECT data #>> '{a}' FROM t WHERE id = 1",
		`SELECT * FROM f WHERE path = 'C:\' AND name = 'alice' AND id = 1`,
		"SELECT * FROM f WHERE path = 'x' AND id = 1",
	}
	seen := map[string]string{}
	for _, q := range queries {
		form := NormalizeSQL(q)
		if prev, dup := seen[form]; dup {
			t.Errorf("разные запросы схлопнулись в одну форму %q:\n  %s\n  %s", form, prev, q)
		}
		seen[form] = q
	}
}

func TestNormalizeCacheKey(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"redis GET с числовым ключом", "GET user:42", "GET user:?"},
		{"redis GET с другим ключом даёт ту же форму", "GET user:1337", "GET user:?"},
		{"HGETALL с буквенно-цифровым ключом", "HGETALL session:abc123", "HGETALL session:?"},
		{"вложенные сегменты ключа", "GET user:42:posts:7", "GET user:?:posts:?"},
		// Последний сегмент составного ключа — значение, даже если в нём нет цифр:
		// иначе `user:jsmith` и `user:mbrown` остаются разными формами и N+1 по
		// буквенным идентификаторам не виден. Цена — `config:global` тоже
		// схлопывается (см. isValueSegment).
		{"буквенный идентификатор маскируется", "GET user:jsmith", "GET user:?"},
		{"другой буквенный идентификатор даёт ту же форму", "GET user:mbrown", "GET user:?"},
		{"статический составной ключ тоже схлопывается", "GET config:global", "GET config:?"},
		{"односегментный статический ключ остаётся", "GET config", "GET config"},
		{"uuid в ключе", "DEL cart:7c9e6679-7425-40de-944b-e07fc1f90ae7", "DEL cart:?"},
		{"числовой аргумент", "EXPIRE session:abc123 3600", "EXPIRE session:? ?"},
		{"команда без аргументов", "PING", "PING"},
		{"пустая строка", "", ""},
		{"MGET с несколькими ключами", "MGET user:1 user:2", "MGET user:? user:?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeCacheKey(tt.in); got != tt.want {
				t.Fatalf("NormalizeCacheKey(%q)\n got: %q\nwant: %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNormalizeURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"числовой сегмент", "/users/42", "/users/{id}"},
		{"несколько числовых сегментов", "/users/42/posts/7", "/users/{id}/posts/{id}"},
		{"uuid-сегмент", "/orders/7c9e6679-7425-40de-944b-e07fc1f90ae7", "/orders/{id}"},
		{"uuid в верхнем регистре", "/orders/7C9E6679-7425-40DE-944B-E07FC1F90AE7", "/orders/{id}"},
		{"query-строка отбрасывается", "/users?page=2", "/users"},
		{"query-строка со значениями отбрасывается", "/users/42?page=2&token=abc", "/users/{id}"},
		{"фрагмент отбрасывается", "/users/42#top", "/users/{id}"},
		{"хост сохраняется", "https://api.example.com/v2/users/42", "https://api.example.com/v2/users/{id}"},
		{"хост с портом и query", "http://localhost:8080/api/items/9?x=1", "http://localhost:8080/api/items/{id}"},
		{"хост без пути", "https://api.example.com", "https://api.example.com"},
		{"хост со слешем", "https://api.example.com/", "https://api.example.com/"},
		{"метод в описании сохраняется", "GET https://api.example.com/users/42", "GET https://api.example.com/users/{id}"},
		{"метод и относительный путь", "POST /users/42/comments", "POST /users/{id}/comments"},
		{"версия API не число", "/v2/users", "/v2/users"},
		{"не трогаем нечисловые сегменты", "/users/me/settings", "/users/me/settings"},
		{"хвостовой слеш сохраняется", "/users/42/", "/users/{id}/"},
		{"пустая строка", "", ""},
		{"только пробелы", "   ", ""},
		{"мусор не падает", "://///", "://///"},
		{"корень", "/", "/"},
		{"смешанный сегмент не число", "/files/42abc", "/files/42abc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeURL(tt.in)
			if got != tt.want {
				t.Fatalf("NormalizeURL(%q)\n got: %q\nwant: %q", tt.in, got, tt.want)
			}
		})
	}
}

// Маскирование id-подобных сегментов: без него приложение с ObjectID/хешами в
// путях даёт НОВЫЙ фингерпринт на каждый запрос — perf_issues растёт без предела.
// Обратная сторона теста не менее важна: обычные слова (`/articles`,
// `/hello-world`, `/v2`) маскировать нельзя, иначе схлопнутся разные эндпойнты.
func TestNormalizeURLMasksIDLikeSegments(t *testing.T) {
	masked := []struct{ name, in, want string }{
		{"mongo objectid", "/orders/5f8d0d55b54764421b7156c9", "/orders/{id}"},
		{"objectid в верхнем регистре", "/orders/5F8D0D55B54764421B7156C9", "/orders/{id}"},
		{"sha1-хеш", "/commits/da39a3ee5e6b4b0d3255bfef95601890afd80709", "/commits/{id}"},
		{"hex-токен 16 символов", "/s/0123456789abcdef", "/s/{id}"},
		{"буквы с цифрами длиной 8+", "/u/user1234567", "/u/{id}"},
		{"слаг с длинным числовым хвостом", "/p/post-1234567", "/p/{id}"},
		{"метод + objectid", "GET /orders/5f8d0d55b54764421b7156c9", "GET /orders/{id}"},
	}
	for _, tt := range masked {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeURL(tt.in); got != tt.want {
				t.Fatalf("NormalizeURL(%q)\n got: %q\nwant: %q", tt.in, got, tt.want)
			}
		})
	}

	kept := []struct{ name, in string }{
		{"слаг статьи", "/articles/hello-world"},
		{"слаг с буквенным суффиксом", "/articles/hello-world-a"},
		{"обычные слова", "/checkout/confirm"},
		{"версия api", "/v2/users/me"},
		{"короткое слово с цифрой", "/files/42abc"},
		{"длинное слово без цифр", "/administration/settings"},
		{"хост без схемы", "api2.example.com/health"},
		{"имя файла с точкой", "/static/app-2024.css"},
	}
	for _, tt := range kept {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeURL(tt.in); got != tt.in {
				t.Fatalf("NormalizeURL(%q) = %q: обычный сегмент не должен маскироваться", tt.in, got)
			}
		})
	}
}

func TestNormalizeURLIsStable(t *testing.T) {
	for _, u := range []string{"/users/42", "https://api.example.com/v2/users/42?p=1", "", "GET /a/1"} {
		once := NormalizeURL(u)
		if twice := NormalizeURL(once); twice != once {
			t.Fatalf("не идемпотентно для %q: %q -> %q", u, once, twice)
		}
	}
}

func TestNormalizeDescription(t *testing.T) {
	tests := []struct {
		name string
		op   string
		in   string
		want string
	}{
		{"db.sql.query -> SQL", "db.sql.query", "SELECT * FROM users WHERE id = 42", "SELECT * FROM users WHERE id = ?"},
		{"db -> SQL", "db", "SELECT * FROM users WHERE id IN (1,2,3)", "SELECT * FROM users WHERE id IN (?)"},
		{"db.redis -> маскирование ключа", "db.redis", "GET user:42", "GET user:?"},
		{"db.redis: разные ключи дают одну форму", "db.redis", "HGETALL session:abc123", "HGETALL session:?"},
		{"db.memcached тоже не SQL", "db.memcached", "get item:99", "get item:?"},
		{"регистр op не важен", "DB.SQL.QUERY", "SELECT 1", "SELECT ?"},
		{"http.client -> URL", "http.client", "GET https://api.example.com/users/42?x=1", "GET https://api.example.com/users/{id}"},
		{"http.server -> URL", "http.server", "/users/42", "/users/{id}"},
		{"template.render -> как есть", "template.render", "index.html.twig", "index.html.twig"},
		{"template.render с числом не трогаем", "template.render", "widget 42 render", "widget 42 render"},
		{"пустой op", "", "что-то", "что-то"},
		{"пустое описание", "db.sql.query", "", ""},
		{"пробелы по краям обрезаются", "cache.get", "  key  ", "key"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeDescription(tt.op, tt.in)
			if got != tt.want {
				t.Fatalf("NormalizeDescription(%q, %q)\n got: %q\nwant: %q", tt.op, tt.in, got, tt.want)
			}
		})
	}
}

// TestNormalizeDescriptionCaps — результат каппится: описание уезжает в
// фингерпринт и в title проблемы, раздувать их нельзя.
func TestNormalizeDescriptionCaps(t *testing.T) {
	long := "SELECT " + strings.Repeat("col_name, ", 5000) + "x FROM t"
	got := NormalizeDescription("db.sql.query", long)
	if n := len([]rune(got)); n != maxNormalizedDescription {
		t.Fatalf("длина результата = %d, want %d", n, maxNormalizedDescription)
	}

	longURL := "https://example.com/" + strings.Repeat("segment/", 5000)
	got = NormalizeDescription("http.client", longURL)
	if n := len([]rune(got)); n > maxNormalizedDescription {
		t.Fatalf("длина результата = %d, want <= %d", n, maxNormalizedDescription)
	}
}

// TestNormalizeNoPanic — вход враждебен: нормализаторы не должны падать ни на
// чём, что приедет из SDK.
func TestNormalizeNoPanic(t *testing.T) {
	inputs := []string{
		"", " ", "'", `"`, "`", "--", "/*", "*/", "$", "$$", ":", "::", "?", "(", ")",
		"'''", `\`, "0x", "1e", "1e+", ".", "..", "SELECT 'a", "IN (", "in()", "in ( , )",
		"\x00\x01\x02", "Ω≈ç√∫", strings.Repeat("(", 1000), strings.Repeat("'", 1000),
		strings.Repeat("/*", 500), "%%%", "//////", "http://", "?a=1", "#", "{}",
	}
	for _, in := range inputs {
		for _, op := range []string{"db.sql.query", "http.client", "template.render", ""} {
			_ = NormalizeSQL(in)
			_ = NormalizeURL(in)
			_ = NormalizeDescription(op, in)
		}
	}
}
