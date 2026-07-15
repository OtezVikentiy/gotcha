package oauth

// Registry — включённые на инсталляции провайдеры в порядке объявления
// (порядок кнопок на /login). Собирается в cmd/gotcha/main.go из Config.
type Registry struct {
	order  []Provider
	byName map[string]Provider
}

// NewRegistry строит реестр; дубликат Name — ошибка сборки (panic): такого не
// должно случаться при корректной конфигурации.
func NewRegistry(providers ...Provider) *Registry {
	r := &Registry{byName: make(map[string]Provider, len(providers))}
	for _, p := range providers {
		if _, dup := r.byName[p.Name()]; dup {
			panic("oauth: duplicate provider " + p.Name())
		}
		r.byName[p.Name()] = p
		r.order = append(r.order, p)
	}
	return r
}

func (r *Registry) Get(name string) (Provider, bool) {
	if r == nil {
		return nil, false
	}
	p, ok := r.byName[name]
	return p, ok
}

func (r *Registry) List() []Provider {
	if r == nil {
		return nil
	}
	return r.order
}

func (r *Registry) Empty() bool { return r == nil || len(r.order) == 0 }
