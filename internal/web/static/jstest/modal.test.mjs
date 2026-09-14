import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { buildWorld, loadScript, dispatchKeydown, dispatchClick, tick } from "./dom.mjs";
import { buildModal, buildOpenerLink } from "./fixtures.mjs";

var here = path.dirname(fileURLToPath(import.meta.url));
var MODAL_JS = path.join(here, "..", "modal.js");

// K37: закрытие крестиком снимает показ через :target, но класс modal--open
// раньше не снимался — openModal() находил invisible-модалку и глушил весь Tab.
test("modal: закрытие снятого с показа modal--open освобождает Tab", async function () {
	var { document, window, sandbox } = buildWorld();
	var m = buildModal(document, "m1", { open: true });
	loadScript(sandbox, MODAL_JS);

	assert.equal(document.activeElement, m.header, "при загрузке серверная модалка получает фокус на заголовок");

	// Пользователь кликает крестик — браузер переходит на "#m1-close", CSS прячет модалку.
	window.location.hash = "m1-close";
	await tick(5);
	m.modal._visible = false; // визуальный эффект CSS-правила .modal-dismiss:target + .modal--open

	var ev = dispatchKeydown(document, "Tab");
	assert.equal(ev.defaultPrevented, false, "Tab не должен гаситься после закрытия модалки");
	assert.equal(m.modal.classList.contains("modal--open"), false, "класс modal--open обязан сняться при закрытии");
});

// K136: :target может быть только один. Открытие второй модалки по ссылке должно
// снимать modal--open у первой — иначе обе остаются видимыми одновременно.
test("modal: открытие модалки по ссылке снимает modal--open у ранее открытой", async function () {
	var { document, window, sandbox } = buildWorld();
	var a = buildModal(document, "a", { open: true });
	var b = buildModal(document, "b", { open: false });
	loadScript(sandbox, MODAL_JS);

	window.location.hash = "b";
	await tick(5);

	assert.equal(a.modal.classList.contains("modal--open"), false, "первая модалка не должна остаться помеченной открытой");
	assert.equal(document.activeElement, b.header, "фокус обязан перейти на заголовок второй модалки");
});

test("modal: пока модалка реально открыта, Tab на пустом списке всё ещё ловится (контрольный случай)", async function () {
	var { document, window, sandbox } = buildWorld();
	var m = buildModal(document, "m1", { open: true });
	loadScript(sandbox, MODAL_JS);

	var ev = dispatchKeydown(document, "Tab");
	assert.equal(ev.defaultPrevented, false, "фокусируемые элементы есть — обычный Tab не перехватывается");
});

test("modal: Escape закрывает открытую по ссылке модалку и возвращает фокус открывателю", async function () {
	var { document, window, sandbox } = buildWorld();
	var m = buildModal(document, "m1", { open: false });
	var opener = buildOpenerLink(document, "m1");
	loadScript(sandbox, MODAL_JS);

	dispatchClick(opener);

	window.location.hash = "m1";
	await tick(5);
	assert.equal(document.activeElement, m.header, "открытие по хэшу фокусирует заголовок");

	var ev = dispatchKeydown(document, "Escape");
	assert.equal(window.location.hash, "#m1-close", "Escape переводит на якорь закрытия");
	await tick(5);
	assert.equal(document.activeElement, opener, "после закрытия фокус возвращается на открывателя");
});
