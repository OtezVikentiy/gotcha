import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { buildWorld, loadScript, fireEvent, tick } from "./dom.mjs";
import { buildExportForm } from "./fixtures.mjs";

var here = path.dirname(fileURLToPath(import.meta.url));
var EXPORT_JS = path.join(here, "..", "export.js");

function resp(opts) {
	var body = opts.text;
	var rest = Object.assign({}, opts);
	delete rest.text;
	return Object.assign({ ok: true, redirected: false, text: function () { return Promise.resolve(body); } }, rest);
}

function submitForm(form) {
	return fireEvent(form, "submit", {});
}

test("export: успешный JSON без усечения скачивает файл и не трогает подсказку", async function () {
	var { document, sandbox } = buildWorld({
		fetch: function () { return Promise.resolve(resp({ text: JSON.stringify({ truncated: false }) })); },
	});
	var f = buildExportForm(document);
	loadScript(sandbox, EXPORT_JS);

	var ev = submitForm(f.form);
	assert.equal(ev.defaultPrevented, true, "перехват формы обязан отменить нативную отправку");
	await tick(10);

	assert.equal(f.hint.hidden, true, "подсказка про усечение не нужна");
	assert.equal(f.form.dataset.exportNative, undefined, "успешный путь не должен переключать на нативную отправку");
});

test("export: usечённый ответ показывает подсказку с числами", async function () {
	var payload = { truncated: true, counts: { logs: { returned: 100, total: 500 } } };
	var { document, sandbox } = buildWorld({
		fetch: function () { return Promise.resolve(resp({ text: JSON.stringify(payload) })); },
	});
	var f = buildExportForm(document);
	loadScript(sandbox, EXPORT_JS);

	submitForm(f.form);
	await tick(10);

	assert.equal(f.hint.hidden, false);
	assert.match(f.hint.textContent, /logs: 100\/500/);
});

test("export: редирект (истёкшая сессия) уходит в нативную отправку", async function () {
	var { document, sandbox } = buildWorld({
		fetch: function () { return Promise.resolve(resp({ redirected: true, text: "<html>login</html>" })); },
	});
	var f = buildExportForm(document);
	var submitted = 0;
	loadScript(sandbox, EXPORT_JS);
	f.form.submit = function () { submitted++; };

	submitForm(f.form);
	await tick(10);

	assert.equal(f.form.dataset.exportNative, "1");
	assert.equal(submitted, 1);
});

test("export: ошибка сервера (не JSON) уходит в нативную отправку", async function () {
	var { document, sandbox } = buildWorld({
		fetch: function () { return Promise.resolve(resp({ ok: false, text: "internal error" })); },
	});
	var f = buildExportForm(document);
	var submitted = 0;
	loadScript(sandbox, EXPORT_JS);
	f.form.submit = function () { submitted++; };

	submitForm(f.form);
	await tick(10);

	assert.equal(submitted, 1);
});

test("export: сетевая ошибка fetch тоже уходит в нативную отправку", async function () {
	var { document, sandbox } = buildWorld({
		fetch: function () { return Promise.reject(new Error("network down")); },
	});
	var f = buildExportForm(document);
	var submitted = 0;
	loadScript(sandbox, EXPORT_JS);
	f.form.submit = function () { submitted++; };

	submitForm(f.form);
	await tick(10);

	assert.equal(f.form.dataset.exportNative, "1");
	assert.equal(submitted, 1);
});

test("export: форма, уже переключённая на нативную отправку, не перехватывается повторно", function () {
	var fetchCalls = 0;
	var { document, sandbox } = buildWorld({
		fetch: function () { fetchCalls++; return Promise.resolve(resp({ text: "{}" })); },
	});
	var f = buildExportForm(document);
	f.form.dataset.exportNative = "1";
	loadScript(sandbox, EXPORT_JS);

	var ev = submitForm(f.form);

	assert.equal(ev.defaultPrevented, false, "нативной отправке не мешаем");
	assert.equal(fetchCalls, 0);
});
