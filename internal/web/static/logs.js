/* logs.js — прогрессивное улучшение: typeahead поиска ключа атрибута на
 * экране логов (задача 6, C2, §6 спеки). Без JS поле — обычный текстовый
 * инпут (data-attr-typeahead-input), у которого без скрипта нет действия:
 * базовая страница логов не ломается, просто не подсказывает.
 *
 * На ввод (дебаунс ~200мс) бьёт fetch по data-attr-keys-url?q=<prefix>
 * (web.logsAttrKeys) и рисует выпадашку найденных ключей со счётчиками. К
 * запросу добавляются текущие period=/start=/end= из window.location
 * (правка ревью UX Important #3) — иначе автокомплит искал ключи в своём
 * фиксированном окне, а не в том, что выбрано в фильтре текущей страницы, и
 * подсказывал ключи, которых в видимой выборке нет. Клик по подсказке —
 * обычная ссылка на data-attr-base-href с добавленным facet=<key>: раскрывает
 * тот же сайдбар-фасет, что и клик по ключу из самого сайдбара (см.
 * logAttrKeyFacetURL в logs.templ), в том числе для ключей ВНЕ топ-N
 * сайдбара (carry-fix задачи 6, см. NewAttrFacets).
 *
 * CSP строгий: внешний файл, никакого inline. */
(function () {
	"use strict";

	var DEBOUNCE_MS = 200;
	var HIDE_DELAY_MS = 150; // даёт клику по подсказке случиться раньше blur (см. ниже)

	function initTypeahead(root) {
		var input = root.querySelector("[data-attr-typeahead-input]");
		var list = root.querySelector("[data-attr-typeahead-list]");
		var keysURL = root.getAttribute("data-attr-keys-url");
		var baseHref = root.getAttribute("data-attr-base-href");
		if (!input || !list || !keysURL || !baseHref) {
			return;
		}
		// Санитизация baseHref (CodeQL #20, js/xss-through-dom): значение приходит
		// из DOM-атрибута, и прямое присваивание в a.href позволило бы схему вроде
		// javascript:, окажись атрибут под контролем атакующего. Нормализуем через
		// URL API относительно текущего origin и принимаем только same-origin;
		// дальше ссылки собираются как pathname+search (см. facetHref) — относительный
		// путь, в котором чужая схема невозможна по построению.
		var baseURL;
		try {
			baseURL = new URL(baseHref, window.location.origin);
		} catch (e) {
			return;
		}
		if (baseURL.origin !== window.location.origin) {
			return;
		}

		var debounceTimer = null;
		var hideTimer = null;
		var controller = null;

		function hide() {
			list.hidden = true;
			list.innerHTML = "";
		}

		function facetHref(key) {
			var u = new URL(baseURL);
			u.searchParams.set("facet", key);
			return u.pathname + u.search;
		}

		// windowRangeQuery — текущее окно фильтра страницы (period=/start=/end=
		// из адресной строки), дописывается к fetch за ключами: без этого
		// web.logsAttrKeys не может узнать, какое окно выбрано на странице, и
		// откатывается на дефолт (см. её докблок).
		function windowRangeQuery() {
			var params = new URLSearchParams(window.location.search);
			var q = "";
			["period", "start", "end"].forEach(function (name) {
				var v = params.get(name);
				if (v) {
					q += "&" + name + "=" + encodeURIComponent(v);
				}
			});
			return q;
		}

		function render(items) {
			list.innerHTML = "";
			if (!items || !items.length) {
				hide();
				return;
			}
			items.forEach(function (item) {
				if (!item || typeof item.key !== "string") {
					return;
				}
				var li = document.createElement("li");
				var a = document.createElement("a");
				a.href = facetHref(item.key);
				a.className = "logs-attr-suggestion";
				var keyEl = document.createElement("span");
				keyEl.textContent = item.key;
				a.appendChild(keyEl);
				if (typeof item.count === "number") {
					var countEl = document.createElement("span");
					countEl.className = "logs-attr-suggestion-count";
					countEl.textContent = String(item.count);
					a.appendChild(countEl);
				}
				li.appendChild(a);
				list.appendChild(li);
			});
			list.hidden = list.children.length === 0;
		}

		function query(prefix) {
			if (controller) {
				controller.abort();
			}
			controller = window.AbortController ? new AbortController() : null;
			var opts = { headers: { Accept: "application/json" } };
			if (controller) {
				opts.signal = controller.signal;
			}
			fetch(keysURL + "?q=" + encodeURIComponent(prefix) + windowRangeQuery(), opts)
				.then(function (r) {
					if (!r.ok) {
						throw new Error("attr-keys: bad status " + r.status);
					}
					return r.json();
				})
				.then(render)
				.catch(function (e) {
					if (e && e.name === "AbortError") {
						return;
					}
					hide();
				});
		}

		input.addEventListener("input", function () {
			var val = input.value.trim();
			window.clearTimeout(debounceTimer);
			if (!val) {
				hide();
				return;
			}
			debounceTimer = window.setTimeout(function () {
				query(val);
			}, DEBOUNCE_MS);
		});

		// blur срабатывает раньше, чем click по подсказке (мышь: mousedown
		// уводит фокус ДО click); задержка даёт click отработать (переход по
		// обычной <a href>), пока список ещё в DOM.
		input.addEventListener("blur", function () {
			hideTimer = window.setTimeout(hide, HIDE_DELAY_MS);
		});
		input.addEventListener("focus", function () {
			window.clearTimeout(hideTimer);
		});

		input.addEventListener("keydown", function (ev) {
			if (ev.key === "Escape") {
				hide();
			}
		});
	}

	var roots = document.querySelectorAll("[data-attr-typeahead]");
	for (var i = 0; i < roots.length; i++) {
		initTypeahead(roots[i]);
	}
})();
