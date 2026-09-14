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
		// baseHref приходит из DOM-атрибута — прямое присваивание в a.href
		// пустило бы схему вроде javascript:; принимаем только same-origin.
		var baseURL;
		try {
			baseURL = new URL(baseHref, window.location.origin);
		} catch (e) {
			return;
		}
		if (baseURL.origin !== window.location.origin) {
			return;
		}
		// keysURL приходит из того же DOM-атрибута, что и baseHref — та же проверка.
		try {
			if (new URL(keysURL, window.location.origin).origin !== window.location.origin) {
				return;
			}
		} catch (e) {
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

		// Без period=/start=/end= из адресной строки web.logsAttrKeys не знает
		// окно страницы и откатывается на дефолт.
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

		// focusout/focusin на root, не blur/focus на input: Tab уводит фокус на
		// подсказку внутри списка, а не за пределы виджета.
		root.addEventListener("focusout", function (ev) {
			if (root.contains(ev.relatedTarget)) {
				return;
			}
			hideTimer = window.setTimeout(hide, HIDE_DELAY_MS);
		});
		root.addEventListener("focusin", function () {
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
