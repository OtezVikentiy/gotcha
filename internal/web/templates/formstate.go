package templates

type FormState map[string]string

func (f FormState) Get(name, fallback string) string {
	if f == nil {
		return fallback
	}
	if v, ok := f[name]; ok {
		return v
	}
	return fallback
}

// двойное подчёркивание не совпадёт с именем поля: карта собирается
// обработчиком поимённо, а не копированием r.Form целиком.
const formModalKey = "__modal"

func (f FormState) Open(id string) FormState {
	if f == nil {
		f = FormState{}
	}
	f[formModalKey] = id
	return f
}

func (f FormState) Opens(id string) bool { return f[formModalKey] == id }

func (f FormState) Has() bool {
	n := len(f)
	if _, ok := f[formModalKey]; ok {
		n--
	}
	return n > 0
}

func (f FormState) Selected(name, value, fallback string) bool {
	return f.Get(name, fallback) == value
}

// снятый чекбокс не попадает в r.Form вовсе (в отличие от radio, который
// приходит пустой строкой) — здесь проверяется наличие ключа, не значение.
func (f FormState) Checked(name string, fallback bool) bool {
	if f == nil {
		return fallback
	}
	if _, ok := f[name]; ok {
		return true
	}
	return fallback
}
