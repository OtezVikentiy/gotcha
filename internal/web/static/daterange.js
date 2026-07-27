/* daterange.js — прогрессивное улучшение селектора окна времени (.time-range).
 *
 * Без JS control работает как есть: <select> пресетов + два нативных
 * <input type="datetime-local"> (начало/конец) + кнопка «Применить». Этот скрипт
 * НЕ обязателен: если он не загрузился или отключён, нативные поля полностью
 * функциональны (фолбэк, доступность, боты).
 *
 * С JS: нативные части прячутся (остаются в DOM как источник значений для
 * сабмита формы), а вместо них — одна кнопка-триггер и попап с пресетами слева
 * и календарём на два месяца справа: клик — начало интервала, второй клик —
 * конец, «Применить» пишет значения в скрытые нативные поля и сабмитит форму.
 *
 * CSP: строгий (default-src 'self', без unsafe-inline). Поэтому — внешний файл
 * с того же origin, слушатели через addEventListener, БЕЗ inline-обработчиков и
 * БЕЗ inline-стилей (вся стилистика — классами в app.css).
 */
(function () {
	"use strict";

	var WEEKDAYS = ["Пн", "Вт", "Ср", "Чт", "Пт", "Сб", "Вс"];
	var MONTHS = [
		"январь", "февраль", "март", "апрель", "май", "июнь",
		"июль", "август", "сентябрь", "октябрь", "ноябрь", "декабрь",
	];

	function pad(n) { return (n < 10 ? "0" : "") + n; }

	// Значение для <input type="datetime-local"> (минутная точность, без зоны).
	function localValue(d) {
		return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) +
			"T" + pad(d.getHours()) + ":" + pad(d.getMinutes());
	}

	// Разбор значения datetime-local обратно в Date (локальное время).
	function parseLocal(v) {
		var m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})/.exec(v || "");
		if (!m) return null;
		return new Date(+m[1], +m[2] - 1, +m[3], +m[4], +m[5]);
	}

	function ymd(d) { return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()); }
	function sameDay(a, b) { return a && b && ymd(a) === ymd(b); }
	function startOfDay(d) { return new Date(d.getFullYear(), d.getMonth(), d.getDate(), 0, 0); }
	function endOfDay(d) { return new Date(d.getFullYear(), d.getMonth(), d.getDate(), 23, 59); }
	function addMonths(d, n) { return new Date(d.getFullYear(), d.getMonth() + n, 1); }
	function fmtHuman(d) { return pad(d.getDate()) + "." + pad(d.getMonth() + 1) + "." + d.getFullYear(); }

	// Создать элемент с классом и (опц.) текстом. Атрибуты — объектом.
	function el(tag, cls, text, attrs) {
		var e = document.createElement(tag);
		if (cls) e.className = cls;
		if (text != null) e.textContent = text;
		if (attrs) { for (var k in attrs) if (attrs.hasOwnProperty(k)) e.setAttribute(k, attrs[k]); }
		return e;
	}

	// icon — та же иконка Lucide, что и серверный @icon: <use> на символ из
	// общего спрайта (iconSprite отрендерена в документе). Строим в SVG-
	// namespace через createElementNS (без innerHTML): name всегда наш
	// литерал, но так надёжнее и без XSS-поверхности вовсе.
	var SVG_NS = "http://www.w3.org/2000/svg";
	function icon(name) {
		var svg = document.createElementNS(SVG_NS, "svg");
		svg.setAttribute("class", "ic");
		svg.setAttribute("aria-hidden", "true");
		var use = document.createElementNS(SVG_NS, "use");
		use.setAttribute("href", "#i-" + name);
		svg.appendChild(use);
		return svg;
	}

	function enhance(root) {
		if (root.dataset.drReady) return;
		var select = root.querySelector('select[name="period"]');
		var startIn = root.querySelector('input[name="start"]');
		var endIn = root.querySelector('input[name="end"]');
		var form = root.closest("form");
		if (!select || !startIn || !endIn || !form) return;
		root.dataset.drReady = "1";
		root.classList.add("dr-enhanced");

		// Текущее состояние из уже отрендеренного сервером control:
		//  - активный произвольный диапазон переносится скрытыми cstart/cend;
		//  - иначе активен пресет (выбранная опция, кроме "custom").
		var cStart = root.querySelector('input[name="cstart"]');
		var cEnd = root.querySelector('input[name="cend"]');
		var isCustom = !!(cStart && cEnd);
		var initStart = isCustom ? parseLocal(cStart.value) : null;
		var initEnd = isCustom ? parseLocal(cEnd.value) : null;

		// Пресеты — из опций <select>, кроме "custom".
		var presets = [];
		Array.prototype.forEach.call(select.options, function (o) {
			if (o.value !== "custom") presets.push({ value: o.value, label: o.textContent });
		});

		var trigger = el("button", "dr-trigger", null, { type: "button", "aria-haspopup": "dialog", "aria-expanded": "false", "aria-label": "Выбрать окно времени" });
		var triggerLabel = el("span", "dr-trigger-label");
		trigger.appendChild(icon("calendar"));
		trigger.appendChild(triggerLabel);
		trigger.appendChild(icon("chevron-down"));

		function currentLabel() {
			if (isCustom && initStart && initEnd) return fmtHuman(initStart) + " – " + fmtHuman(initEnd);
			var sel = select.options[select.selectedIndex];
			return sel ? sel.textContent : "";
		}
		triggerLabel.textContent = currentLabel();

		root.insertBefore(trigger, root.firstChild);

		// --- Попап ---
		var popup = el("div", "dr-popup", null, { role: "dialog", "aria-label": "Выбор диапазона" });
		popup.hidden = true;

		// Пресеты (слева).
		var presetCol = el("div", "dr-presets");
		presets.forEach(function (p) {
			var b = el("button", "dr-preset", p.label, { type: "button", "data-v": p.value });
			b.addEventListener("click", function () {
				select.value = p.value;
				// Пресет — не произвольный диапазон: чистим поля дат, чтобы
				// сработала ветка пресета в parseTimeRange.
				startIn.value = "";
				endIn.value = "";
				form.submit();
			});
			presetCol.appendChild(b);
		});

		// Календарь (справа).
		var calWrap = el("div", "dr-cal");
		var calHead = el("div", "dr-cal-head");
		var prevBtn = el("button", "dr-nav", null, { type: "button", "aria-label": "Предыдущий месяц" });
		var nextBtn = el("button", "dr-nav", null, { type: "button", "aria-label": "Следующий месяц" });
		prevBtn.appendChild(icon("chevron-left"));
		nextBtn.appendChild(icon("chevron-right"));
		var monthsWrap = el("div", "dr-months");
		calHead.appendChild(prevBtn);
		calHead.appendChild(monthsWrap);
		calHead.appendChild(nextBtn);
		calWrap.appendChild(calHead);

		var footer = el("div", "dr-foot");
		var rangeText = el("span", "dr-range-text");
		var applyBtn = el("button", "dr-apply btn btn-primary", "Применить", { type: "button" });
		var cancelBtn = el("button", "dr-cancel btn btn-ghost", "Отмена", { type: "button" });
		footer.appendChild(rangeText);
		footer.appendChild(cancelBtn);
		footer.appendChild(applyBtn);

		calWrap.appendChild(footer);

		popup.appendChild(presetCol);
		popup.appendChild(calWrap);
		root.appendChild(popup);

		// Состояние выбора.
		var now = new Date();
		var selStart = initStart ? startOfDay(initStart) : null;
		var selEnd = initEnd ? startOfDay(initEnd) : null;
		var view = addMonths(selEnd || now, -1); // левый месяц — прошлый относительно конца

		function updateFooter() {
			if (selStart && selEnd) {
				rangeText.textContent = fmtHuman(selStart) + " – " + fmtHuman(selEnd);
				applyBtn.disabled = false;
			} else if (selStart) {
				rangeText.textContent = fmtHuman(selStart) + " – …";
				applyBtn.disabled = true;
			} else {
				rangeText.textContent = "Выберите начало и конец";
				applyBtn.disabled = true;
			}
		}

		function inRange(d) {
			if (!selStart || !selEnd) return false;
			return d >= selStart && d <= selEnd;
		}

		function renderMonth(base) {
			var grid = el("div", "dr-month");
			var title = el("div", "dr-month-title", MONTHS[base.getMonth()] + " " + base.getFullYear());
			grid.appendChild(title);
			var head = el("div", "dr-week dr-week-head");
			WEEKDAYS.forEach(function (w) { head.appendChild(el("span", "dr-wd", w)); });
			grid.appendChild(head);

			var first = new Date(base.getFullYear(), base.getMonth(), 1);
			// ISO: понедельник = 0.
			var lead = (first.getDay() + 6) % 7;
			var daysInMonth = new Date(base.getFullYear(), base.getMonth() + 1, 0).getDate();
			var cells = el("div", "dr-days");
			var i;
			for (i = 0; i < lead; i++) cells.appendChild(el("span", "dr-day dr-empty"));
			for (i = 1; i <= daysInMonth; i++) {
				var d = new Date(base.getFullYear(), base.getMonth(), i);
				var cls = "dr-day";
				var future = d > now;
				if (future) cls += " dr-future";
				if (inRange(d)) cls += " dr-in";
				if (sameDay(d, selStart)) cls += " dr-start";
				if (sameDay(d, selEnd)) cls += " dr-end";
				if (sameDay(d, now)) cls += " dr-today";
				var selected = sameDay(d, selStart) || sameDay(d, selEnd) || inRange(d);
				var cell = el("button", cls, String(i), {
					type: "button",
					"aria-label": i + " " + MONTHS[d.getMonth()] + " " + d.getFullYear(),
					"aria-pressed": selected ? "true" : "false",
				});
				if (future) {
					cell.disabled = true;
				} else {
					(function (day) {
						cell.addEventListener("click", function () { pickDay(day); });
					})(startOfDay(d));
				}
				cells.appendChild(cell);
			}
			grid.appendChild(cells);
			return grid;
		}

		function renderCal() {
			monthsWrap.textContent = "";
			monthsWrap.appendChild(renderMonth(view));
			monthsWrap.appendChild(renderMonth(addMonths(view, 1)));
			// Не пускаем «вперёд» за текущий месяц.
			var nextMonthStart = addMonths(view, 2);
			nextBtn.disabled = nextMonthStart > new Date(now.getFullYear(), now.getMonth() + 1, 1);
			updateFooter();
		}

		function pickDay(day) {
			if (!selStart || (selStart && selEnd)) {
				selStart = day; selEnd = null;
			} else if (day < selStart) {
				selStart = day;
			} else {
				selEnd = day;
			}
			renderCal();
		}

		prevBtn.addEventListener("click", function () { view = addMonths(view, -1); renderCal(); });
		nextBtn.addEventListener("click", function () { view = addMonths(view, 1); renderCal(); });

		applyBtn.addEventListener("click", function () {
			if (!selStart || !selEnd) return;
			var s = startOfDay(selStart);
			var e = endOfDay(selEnd);
			if (e > now) e = now;
			startIn.value = localValue(s);
			endIn.value = localValue(e);
			// period не важен: заполненные start/end включают произвольный
			// диапазон (см. parseTimeRange), но для наглядности ставим "custom".
			select.value = "custom";
			form.submit();
		});

		function close() {
			popup.hidden = true;
			trigger.setAttribute("aria-expanded", "false");
			document.removeEventListener("click", onDocClick, true);
			document.removeEventListener("keydown", onKey, true);
		}
		function open() {
			renderCal();
			popup.classList.remove("dr-popup--right");
			popup.hidden = false;
			trigger.setAttribute("aria-expanded", "true");
			// Прижать к правому краю триггера, если двухмесячный попап иначе
			// уезжает за правую границу вьюпорта (фильтр-ряд бывает у края).
			var r = popup.getBoundingClientRect();
			if (r.right > document.documentElement.clientWidth - 4) {
				popup.classList.add("dr-popup--right");
			}
			// Фокус — в календарь (клавиатурник не должен пробиваться табом
			// через весь попап): выбранное начало, иначе сегодня, иначе
			// первый доступный день.
			var f = popup.querySelector(".dr-start") ||
				popup.querySelector(".dr-today:not(.dr-future)") ||
				popup.querySelector(".dr-day:not(.dr-future):not(.dr-empty)");
			if (f) f.focus();
			document.addEventListener("click", onDocClick, true);
			document.addEventListener("keydown", onKey, true);
		}
		function onDocClick(ev) { if (!root.contains(ev.target)) close(); }
		function onKey(ev) { if (ev.key === "Escape") { close(); trigger.focus(); } }
		cancelBtn.addEventListener("click", close);

		trigger.addEventListener("click", function () {
			if (popup.hidden) open(); else close();
		});
	}

	function init() {
		var nodes = document.querySelectorAll(".time-range");
		Array.prototype.forEach.call(nodes, enhance);
	}

	if (document.readyState === "loading") {
		document.addEventListener("DOMContentLoaded", init);
	} else {
		init();
	}
})();
