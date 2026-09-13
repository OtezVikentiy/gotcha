import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { buildWorld, loadScript, dispatchClick, tick } from "./dom.mjs";
import { buildCopyWidget } from "./fixtures.mjs";

var here = path.dirname(fileURLToPath(import.meta.url));
var COPY_JS = path.join(here, "..", "copy.js");

test("copy: успешный Clipboard API показывает и прячет подтверждение", async function () {
	var writeTextCalls = [];
	var { document, sandbox } = buildWorld({
		navigator: { clipboard: { writeText: function (v) { writeTextCalls.push(v); return Promise.resolve(); } } },
	});
	var w = buildCopyWidget(document);
	loadScript(sandbox, COPY_JS);

	dispatchClick(w.button);
	await tick(0);

	assert.deepEqual(writeTextCalls, ["payload"]);
	assert.equal(w.done.hidden, false, "подтверждение должно показаться");

	await tick(1550);
	assert.equal(w.done.hidden, true, "подтверждение должно спрятаться через 1.5с");
});

test("copy: отказ Clipboard API уходит в фолбэк через execCommand", async function () {
	var { document, sandbox } = buildWorld({
		navigator: { clipboard: { writeText: function () { return Promise.reject(new Error("denied")); } } },
	});
	var w = buildCopyWidget(document);
	var execCalls = [];
	document.execCommand = function (cmd) { execCalls.push(cmd); return true; };
	loadScript(sandbox, COPY_JS);

	dispatchClick(w.button);
	await tick(0);

	assert.deepEqual(execCalls, ["copy"]);
	assert.equal(w.done.hidden, false, "фолбэк тоже обязан показать подтверждение");
});

test("copy: без Clipboard API сразу используется execCommand", async function () {
	var { document, sandbox } = buildWorld({ navigator: { clipboard: undefined } });
	var w = buildCopyWidget(document);
	var execCalls = [];
	document.execCommand = function (cmd) { execCalls.push(cmd); return true; };
	loadScript(sandbox, COPY_JS);

	dispatchClick(w.button);

	assert.deepEqual(execCalls, ["copy"]);
	assert.equal(w.done.hidden, false);
});

test("copy: провал execCommand не показывает подтверждение", async function () {
	var { document, sandbox } = buildWorld({ navigator: { clipboard: undefined } });
	var w = buildCopyWidget(document);
	document.execCommand = function () { return false; };
	loadScript(sandbox, COPY_JS);

	dispatchClick(w.button);

	assert.equal(w.done.hidden, true, "неудачное копирование не должно показывать подтверждение");
});

test("copy: клик мимо data-copy-format ничего не делает", function () {
	var { document, sandbox } = buildWorld({ navigator: { clipboard: undefined } });
	var w = buildCopyWidget(document);
	var execCalls = [];
	document.execCommand = function (cmd) { execCalls.push(cmd); return true; };
	loadScript(sandbox, COPY_JS);

	dispatchClick(w.root);

	assert.deepEqual(execCalls, []);
});
