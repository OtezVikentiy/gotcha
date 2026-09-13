import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { buildWorld, loadScript, dispatchClick, dispatchKeydown } from "./dom.mjs";
import { buildTimeRange } from "./fixtures.mjs";

var here = path.dirname(fileURLToPath(import.meta.url));
var DATERANGE_JS = path.join(here, "..", "daterange.js");

function load(document, sandbox) {
	document.documentElement.setAttribute("lang", "ru");
	loadScript(sandbox, DATERANGE_JS);
}

test("daterange: класс js навешивается сразу, флеш закрывается по кнопке", function () {
	var { document, sandbox } = buildWorld();
	var box = document.createElement("div");
	box.classList.add("flash");
	var closeBtn = document.createElement("button");
	closeBtn.classList.add("flash-close");
	box.appendChild(closeBtn);
	document.body.appendChild(box);

	load(document, sandbox);

	assert.equal(document.documentElement.classList.contains("js"), true);
	dispatchClick(closeBtn);
	assert.equal(box.parentNode, null, "флеш обязан удалиться из DOM по клику на крестик");
});

test("daterange: триггер получает подпись текущего пресета", function () {
	var { document, sandbox } = buildWorld();
	var tr = buildTimeRange(document, { selectedIndex: 1 });
	load(document, sandbox);

	var trigger = tr.root.querySelector(".dr-trigger");
	assert.ok(trigger, "enhance() обязан вставить кнопку-триггер");
	var label = trigger.querySelector(".dr-trigger-label");
	assert.equal(label.textContent, "Последние 24 часа");
});

test("daterange: сетка недель начинается с понедельника (ISO), как формирует Intl", function () {
	var { document, sandbox } = buildWorld();
	var tr = buildTimeRange(document);
	load(document, sandbox);

	var trigger = tr.root.querySelector(".dr-trigger");
	dispatchClick(trigger);

	var months = tr.root.querySelectorAll(".dr-month");
	assert.equal(months.length, 2, "попап рисует два месяца");

	var head = months[0].querySelector(".dr-week-head");
	var wd = head.querySelectorAll(".dr-wd");
	assert.equal(wd.length, 7);
	var mondayLabel = new Intl.DateTimeFormat("ru", { weekday: "short" }).format(new Date(2024, 0, 1));
	assert.equal(wd[0].textContent, mondayLabel, "первый столбец обязан быть понедельником");

	// Число ведущих пустых ячеек обязано сдвигать 1-е число месяца под его
	// настоящий день недели (ISO, понедельник=0) — иначе сетка съезжает.
	var days = months[0].querySelector(".dr-days");
	var leadingEmpty = 0;
	for (var i = 0; i < days.children.length; i++) {
		if (days.children[i].classList.contains("dr-empty")) { leadingEmpty++; } else { break; }
	}
	// Первый отрисованный месяц — предыдущий относительно "сейчас" (addMonths(now, -1)).
	var now = new Date();
	var firstOfMonth = new Date(now.getFullYear(), now.getMonth() - 1, 1);
	var wantLead = (firstOfMonth.getDay() + 6) % 7;
	assert.equal(leadingEmpty, wantLead, "1-е число обязано попасть в свой столбец недели");
});

test("daterange: выбор диапазона из двух дней и Применить заполняют скрытые поля", function () {
	var { document, sandbox } = buildWorld();
	var tr = buildTimeRange(document);
	load(document, sandbox);

	var trigger = tr.root.querySelector(".dr-trigger");
	dispatchClick(trigger);

	var monthsWrap = tr.root.querySelector(".dr-months");
	var dayButtons = function () { return monthsWrap.children[0].querySelectorAll("button"); };

	dispatchClick(dayButtons()[0]);
	var applyBtn = tr.root.querySelector(".dr-apply");
	assert.equal(applyBtn.disabled, true, "после одной даты Применить ещё недоступно");

	dispatchClick(dayButtons()[5]);
	assert.equal(applyBtn.disabled, false, "после второй даты Применить обязано стать доступным");

	var submitted = 0;
	tr.form.submit = function () { submitted++; };
	dispatchClick(applyBtn);

	assert.match(tr.start.value, /^\d{4}-\d{2}-\d{2}T00:00$/);
	assert.match(tr.end.value, /^\d{4}-\d{2}-\d{2}T23:59$/);
	assert.equal(tr.select.value, "custom");
	assert.equal(submitted, 1);
});

test("daterange: клик по пресету чистит поля дат и отправляет форму сразу", function () {
	var { document, sandbox } = buildWorld();
	var tr = buildTimeRange(document, { custom: { start: "2026-01-01T00:00", end: "2026-01-02T00:00" } });
	tr.start.value = "2026-01-01T00:00";
	tr.end.value = "2026-01-02T23:59";
	load(document, sandbox);

	var trigger = tr.root.querySelector(".dr-trigger");
	dispatchClick(trigger);
	var presetBtn = tr.root.querySelector('.dr-preset[data-v="24h"]');
	assert.ok(presetBtn);

	var submitted = 0;
	tr.form.submit = function () { submitted++; };
	dispatchClick(presetBtn);

	assert.equal(tr.select.value, "24h");
	assert.equal(tr.start.value, "");
	assert.equal(tr.end.value, "");
	assert.equal(submitted, 1);
});

test("daterange: Tab внутри попапа заворачивается по кругу (фокус-ловушка)", function () {
	var { document, sandbox } = buildWorld();
	var tr = buildTimeRange(document);
	load(document, sandbox);

	var trigger = tr.root.querySelector(".dr-trigger");
	dispatchClick(trigger);

	var firstPreset = tr.root.querySelector(".dr-preset");
	var cancelBtn = tr.root.querySelector(".dr-cancel");
	var applyBtn = tr.root.querySelector(".dr-apply");
	assert.equal(applyBtn.disabled, true, "до выбора дат Применить недоступно и выпадает из ловушки");

	cancelBtn.focus();
	var forward = dispatchKeydown(document, "Tab");
	assert.equal(forward.defaultPrevented, true, "Tab с последнего элемента обязан завернуться на первый");
	assert.equal(document.activeElement, firstPreset);

	firstPreset.focus();
	var backward = dispatchKeydown(document, "Tab", { shiftKey: true });
	assert.equal(backward.defaultPrevented, true, "Shift+Tab с первого элемента обязан завернуться на последний");
	assert.equal(document.activeElement, cancelBtn);
});

test("daterange: закрытие попапа снимает ловушку — Tab снаружи больше не перехватывается", function () {
	var { document, sandbox } = buildWorld();
	var tr = buildTimeRange(document);
	load(document, sandbox);

	var trigger = tr.root.querySelector(".dr-trigger");
	dispatchClick(trigger);
	dispatchKeydown(document, "Escape");

	var afterClose = dispatchKeydown(document, "Tab");
	assert.equal(afterClose.defaultPrevented, false, "после закрытия попапа Tab обязан ходить по странице свободно");
});

test("daterange: Escape закрывает попап и возвращает фокус триггеру", function () {
	var { document, sandbox } = buildWorld();
	var tr = buildTimeRange(document);
	load(document, sandbox);

	var trigger = tr.root.querySelector(".dr-trigger");
	dispatchClick(trigger);
	var popup = tr.root.querySelector(".dr-popup");
	assert.equal(popup.hidden, false);

	dispatchKeydown(document, "Escape");
	assert.equal(popup.hidden, true);
	assert.equal(document.activeElement, trigger);
});
