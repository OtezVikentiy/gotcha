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

	// Локаль берём из <html lang> (её ставит layout.templ по i18n). Месяцы, дни
	// недели и формат дат локализуются через Intl — без хардкода языка. Кнопки и
	// aria-подписи приходят из data-l-* на .time-range (см. timeRangeFields).
	var LANG = (document.documentElement.getAttribute("lang") || "ru");
	var fmtMonthTitle = new Intl.DateTimeFormat(LANG, { month: "long", year: "numeric" });
	var fmtDayAria = new Intl.DateTimeFormat(LANG, { day: "numeric", month: "long", year: "numeric" });
	var fmtDate = new Intl.DateTimeFormat(LANG, { day: "2-digit", month: "2-digit", year: "numeric" });

	// Короткие имена дней недели в ISO-порядке (Пн…Вс). 2024-01-01 — понедельник,
	// поэтому семь дней от него дают нужный порядок в любой локали.
	var WEEKDAYS = (function () {
		var f = new Intl.DateTimeFormat(LANG, { weekday: "short" }), out = [];
		for (var i = 0; i < 7; i++) out.push(f.format(new Date(2024, 0, 1 + i)));
		return out;
	})();

	var drUid = 0; // уникализирует id попапа для aria-controls (несколько .time-range)

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
	function fmtHuman(d) { return fmtDate.format(d); }

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

		// Локализованные подписи из data-l-* (fallback — RU, если атрибут снят).
		var L = {
			apply: root.dataset.lApply || "Применить",
			cancel: root.dataset.lCancel || "Отмена",
			hint: root.dataset.lHint || "Выберите начало и конец",
			trigger: root.dataset.lTrigger || "Выбрать окно времени",
			dialog: root.dataset.lDialog || "Выбор диапазона",
			prev: root.dataset.lPrev || "Предыдущий месяц",
			next: root.dataset.lNext || "Следующий месяц",
		};

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

		var popupId = "dr-popup-" + (++drUid);
		var trigger = el("button", "dr-trigger", null, { type: "button", "aria-haspopup": "dialog", "aria-expanded": "false", "aria-controls": popupId, "aria-label": L.trigger });
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
		// Accessible name должен СОДЕРЖАТЬ видимый текст (WCAG 2.5.3 Label in
		// Name): иначе SR/голосовое управление не знают текущее окно. Компонуем
		// «Выбрать окно времени: 24 ч».
		trigger.setAttribute("aria-label", L.trigger + ": " + currentLabel());

		root.insertBefore(trigger, root.firstChild);

		// --- Попап ---
		var popup = el("div", "dr-popup", null, { role: "dialog", "aria-modal": "true", "aria-label": L.dialog, id: popupId });
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
		var prevBtn = el("button", "dr-nav", null, { type: "button", "aria-label": L.prev });
		var nextBtn = el("button", "dr-nav", null, { type: "button", "aria-label": L.next });
		prevBtn.appendChild(icon("chevron-left"));
		nextBtn.appendChild(icon("chevron-right"));
		var monthsWrap = el("div", "dr-months");
		calHead.appendChild(prevBtn);
		calHead.appendChild(monthsWrap);
		calHead.appendChild(nextBtn);
		calWrap.appendChild(calHead);

		var footer = el("div", "dr-foot");
		var rangeText = el("span", "dr-range-text");
		var applyBtn = el("button", "dr-apply btn btn-primary", L.apply, { type: "button" });
		var cancelBtn = el("button", "dr-cancel btn btn-ghost", L.cancel, { type: "button" });
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
		var focusDay = null; // день с tabindex=0 (roving); им управляют стрелки клавиатуры

		function updateFooter() {
			if (selStart && selEnd) {
				rangeText.textContent = fmtHuman(selStart) + " – " + fmtHuman(selEnd);
				applyBtn.disabled = false;
			} else if (selStart) {
				rangeText.textContent = fmtHuman(selStart) + " – …";
				applyBtn.disabled = true;
			} else {
				rangeText.textContent = L.hint;
				applyBtn.disabled = true;
			}
		}

		function inRange(d) {
			if (!selStart || !selEnd) return false;
			return d >= selStart && d <= selEnd;
		}

		function renderMonth(base) {
			var grid = el("div", "dr-month");
			var title = el("div", "dr-month-title", fmtMonthTitle.format(base));
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
				// Roving tabindex: таббельна ровно одна ячейка (focusDay) — так в
				// сетку один tab-стоп, а не 60+; перемещение внутри — стрелками.
				var isFocus = focusDay && sameDay(d, focusDay);
				var cell = el("button", cls, String(i), {
					type: "button",
					"data-ymd": ymd(d),
					tabindex: (isFocus && !future) ? "0" : "-1",
					"aria-label": fmtDayAria.format(d),
					"aria-pressed": selected ? "true" : "false",
				});
				if (future) {
					cell.disabled = true;
				} else {
					(function (day) {
						cell.addEventListener("click", function () { focusDay = day; pickDay(day); });
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

		// Вернуть фокус на ячейку focusDay после пересборки сетки: renderCal
		// стирает DOM, иначе фокус клавиатурника падал бы на <body> при каждом
		// выборе дня или сдвиге стрелкой.
		function focusFocusDay() {
			if (!focusDay) return;
			var c = monthsWrap.querySelector('[data-ymd="' + ymd(focusDay) + '"]');
			if (c) c.focus();
		}

		// Держим focusDay в пределах двух видимых месяцев, сдвигая view.
		function ensureVisible(d) {
			var leftStart = new Date(view.getFullYear(), view.getMonth(), 1);
			var rightEnd = new Date(view.getFullYear(), view.getMonth() + 2, 0);
			if (d < leftStart) view = new Date(d.getFullYear(), d.getMonth(), 1);
			else if (d > rightEnd) view = new Date(d.getFullYear(), d.getMonth() - 1, 1);
		}

		// Обратное к ensureVisible: после смены view кнопками месяца затягиваем
		// focusDay в новые видимые месяцы. Иначе tabindex="0" стоит на ячейке,
		// которой в сетке уже нет, и вся сетка становится недостижимой с
		// клавиатуры (roving-tabindex оставляет ровно одну таббельную ячейку).
		function clampFocusToView() {
			var leftStart = new Date(view.getFullYear(), view.getMonth(), 1);
			var rightEnd = new Date(view.getFullYear(), view.getMonth() + 2, 0);
			if (!focusDay || focusDay < leftStart) focusDay = leftStart;
			else if (focusDay > rightEnd) focusDay = rightEnd;
			if (focusDay > now) focusDay = startOfDay(now); // не в будущее
		}

		// Перенести фокус клавиатуры на день nd (не в будущее), сдвинув месяцы.
		function moveFocus(nd) {
			if (nd > now) nd = now;
			focusDay = startOfDay(nd);
			ensureVisible(focusDay);
			renderCal();
			focusFocusDay();
		}

		function pickDay(day) {
			if (!selStart || (selStart && selEnd)) {
				selStart = day; selEnd = null;
			} else if (day < selStart) {
				selStart = day;
			} else {
				selEnd = day;
			}
			focusDay = day;
			renderCal();
			focusFocusDay();
		}

		// Навигация по сетке дней с клавиатуры (WAI-ARIA date-grid): стрелки — на
		// день/неделю, Home/End — на края недели, PageUp/Down — на ±месяц. Enter/
		// Space обрабатывает сама кнопка-ячейка (нативный click), поэтому здесь их
		// не трогаем, чтобы выбор не срабатывал дважды.
		monthsWrap.addEventListener("keydown", function (ev) {
			if (!focusDay) return;
			var f = focusDay, nd, wd = (f.getDay() + 6) % 7; // 0 = понедельник
			switch (ev.key) {
				case "ArrowLeft": nd = new Date(f.getFullYear(), f.getMonth(), f.getDate() - 1); break;
				case "ArrowRight": nd = new Date(f.getFullYear(), f.getMonth(), f.getDate() + 1); break;
				case "ArrowUp": nd = new Date(f.getFullYear(), f.getMonth(), f.getDate() - 7); break;
				case "ArrowDown": nd = new Date(f.getFullYear(), f.getMonth(), f.getDate() + 7); break;
				case "Home": nd = new Date(f.getFullYear(), f.getMonth(), f.getDate() - wd); break;
				case "End": nd = new Date(f.getFullYear(), f.getMonth(), f.getDate() + (6 - wd)); break;
				case "PageUp": nd = new Date(f.getFullYear(), f.getMonth() - 1, f.getDate()); break;
				case "PageDown": nd = new Date(f.getFullYear(), f.getMonth() + 1, f.getDate()); break;
				default: return;
			}
			ev.preventDefault();
			moveFocus(nd);
		});

		prevBtn.addEventListener("click", function () { view = addMonths(view, -1); clampFocusToView(); renderCal(); });
		// Если «вперёд» сам себя отключил (дошли до текущего месяца), фокус с
		// disabled-кнопки перекидываем в сетку, а не роняем на <body>.
		nextBtn.addEventListener("click", function () { view = addMonths(view, 1); clampFocusToView(); renderCal(); if (nextBtn.disabled) focusFocusDay(); });

		applyBtn.addEventListener("click", function () {
			if (!selStart || !selEnd) return;
			var s = startOfDay(selStart);
			var e = endOfDay(selEnd);
			// Симметрично концу: pickDay уже не допускает будущих дней (кнопки
			// disabled), так что на практике сюда не долетает, но не полагаемся
			// на это молча — начало тоже не должно уйти в будущее.
			if (s > now) s = now;
			if (e > now) e = now;
			startIn.value = localValue(s);
			endIn.value = localValue(e);
			// period не важен: заполненные start/end включают произвольный
			// диапазон (см. parseTimeRange), но для наглядности ставим "custom".
			select.value = "custom";
			form.submit();
		});

		// Снимок выбора на момент открытия — чтобы закрытие без «Применить»
		// (Отмена/Esc/клик мимо) откатывало недособранный диапазон, а не
		// оставляло «01.07.2026 – …» до следующего открытия.
		var snapStart = null, snapEnd = null;

		// refocus=true возвращает фокус на триггер (закрытие по Esc/Отмена/клику
		// по триггеру); при закрытии кликом мимо фокус остаётся там, куда кликнули.
		// Любое закрытие БЕЗ применения откатывает выбор к снимку open().
		function close(refocus) {
			selStart = snapStart;
			selEnd = snapEnd;
			popup.hidden = true;
			trigger.setAttribute("aria-expanded", "false");
			document.removeEventListener("click", onDocClick, true);
			document.removeEventListener("keydown", onKey, true);
			if (refocus) trigger.focus();
		}
		function open() {
			snapStart = selStart;
			snapEnd = selEnd;
			// Начальная таббельная ячейка: выбранное начало, иначе сегодня; не в
			// будущем и обязательно в пределах видимых месяцев (иначе в сетке не
			// было бы ячейки с tabindex=0).
			focusDay = startOfDay(selStart || now);
			if (focusDay > now) focusDay = startOfDay(now);
			ensureVisible(focusDay);
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
			focusFocusDay();
			document.addEventListener("click", onDocClick, true);
			document.addEventListener("keydown", onKey, true);
		}
		function onDocClick(ev) { if (!root.contains(ev.target)) close(false); }
		// popupFocusables — таббельные элементы попапа по порядку (сетка даёт одну
		// ячейку через roving tabindex). Нужен для удержания фокуса (aria-modal).
		function popupFocusables() {
			return Array.prototype.filter.call(
				popup.querySelectorAll('button:not([disabled]):not([tabindex="-1"]), a[href], [tabindex="0"]'),
				function (e) { return e.offsetParent !== null; });
		}
		function onKey(ev) {
			if (ev.key === "Escape") { close(true); return; }
			if (ev.key !== "Tab") return;
			// Фокус-ловушка: попап модальный (aria-modal), Tab не должен уводить в
			// страницу за открытым диалогом — заворачиваем по кругу.
			var f = popupFocusables();
			if (!f.length) return;
			var first = f[0], last = f[f.length - 1];
			if (ev.shiftKey && document.activeElement === first) { ev.preventDefault(); last.focus(); }
			else if (!ev.shiftKey && document.activeElement === last) { ev.preventDefault(); first.focus(); }
		}
		cancelBtn.addEventListener("click", function () { close(true); });

		trigger.addEventListener("click", function () {
			if (popup.hidden) open(); else close(true);
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

/* Сообщения о результате действия: крестик закрывает сразу.
 *
 * Прогрессивное улучшение. Без скрипта сообщение уходит само через шесть секунд
 * по CSS-анимации, а кнопка скрыта — мёртвая кнопка хуже её отсутствия. Класс
 * js на <html> её показывает.
 *
 * Отдельным слушателем на document, а не на каждой плашке: сообщение рисуется
 * один раз за загрузку страницы, и делегирование дешевле поиска элементов. */
(function () {
	"use strict";
	document.documentElement.classList.add("js");
	document.addEventListener("click", function (e) {
		var btn = e.target.closest && e.target.closest(".flash-close");
		if (!btn) return;
		var box = btn.closest(".flash");
		if (box && box.parentNode) box.parentNode.removeChild(box);
	});
})();
