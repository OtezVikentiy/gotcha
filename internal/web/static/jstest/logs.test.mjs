import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { buildWorld, loadScript, fireEvent, tick } from "./dom.mjs";
import { buildTypeahead } from "./fixtures.mjs";

var here = path.dirname(fileURLToPath(import.meta.url));
var LOGS_JS = path.join(here, "..", "logs.js");
var DEBOUNCE_MS = 200;
var HIDE_DELAY_MS = 150;

function fakeFetch(items) {
	return function () {
		return Promise.resolve({ ok: true, json: function () { return Promise.resolve(items); } });
	};
}

async function typeAndWaitForSuggestions(input) {
	input.value = "http";
	fireEvent(input, "input", {});
	await tick(DEBOUNCE_MS + 30);
	await tick(0);
}

// K43: Tab доводит фокус до подсказки, но blur поля уже завёл таймер скрытия —
// hide() удаляет только что сфокусированный элемент, и фокус падает на body.
test("logs: подсказка, получившая фокус по Tab, не исчезает и не роняет фокус на body", async function () {
	var { document, sandbox } = buildWorld({ fetch: fakeFetch([{ key: "http.method", count: 3 }]) });
	var ta = buildTypeahead(document);
	loadScript(sandbox, LOGS_JS);

	ta.input.focus();
	await typeAndWaitForSuggestions(ta.input);
	assert.equal(ta.list.children.length, 1, "подсказка должна отрендериться");

	var link = ta.list.children[0].children[0];
	assert.equal(link.tagName, "A");

	link.focus(); // эмулирует Tab: фокус реально переходит на подсказку
	await tick(HIDE_DELAY_MS + 30);

	assert.equal(document.activeElement, link, "фокус обязан остаться на подсказке");
	assert.equal(ta.list.children.length, 1, "список не должен быть очищен, пока фокус внутри виджета");
});

test("logs: фокус, ушедший из виджета целиком, всё равно прячет список", async function () {
	var { document, sandbox } = buildWorld({ fetch: fakeFetch([{ key: "http.method", count: 3 }]) });
	var ta = buildTypeahead(document);
	var outside = document.createElement("button");
	document.body.appendChild(outside);
	loadScript(sandbox, LOGS_JS);

	ta.input.focus();
	await typeAndWaitForSuggestions(ta.input);
	assert.equal(ta.list.hidden, false);

	outside.focus();
	await tick(HIDE_DELAY_MS + 30);

	assert.equal(ta.list.hidden, true, "уход фокуса за пределы виджета обязан скрыть список");
});

test("logs: Escape скрывает список подсказок", async function () {
	var { document, sandbox } = buildWorld({ fetch: fakeFetch([{ key: "http.method", count: 1 }]) });
	var ta = buildTypeahead(document);
	loadScript(sandbox, LOGS_JS);

	ta.input.focus();
	await typeAndWaitForSuggestions(ta.input);
	assert.equal(ta.list.hidden, false);

	fireEvent(ta.input, "keydown", { key: "Escape" });
	assert.equal(ta.list.hidden, true);
});
