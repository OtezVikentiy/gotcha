/* copy.js — прогрессивное улучшение: копирование контекста ошибки в буфер.
 * Без JS кнопок нет (их рендерит шаблон рядом с textarea-источниками), базовая
 * страница не ломается. CSP строгий: внешний файл, слушатели через
 * addEventListener. navigator.clipboard есть только в secure-context (https/
 * localhost); на bare-HTTP LAN — фолбэк execCommand по выделенной textarea. */
(function () {
	"use strict";
	function flashDone(root) {
		var m = root.querySelector("[data-copy-done]");
		if (!m) return;
		m.hidden = false;
		setTimeout(function () { m.hidden = true; }, 1500);
	}
	function copyText(ta, root) {
		if (navigator.clipboard && navigator.clipboard.writeText) {
			navigator.clipboard.writeText(ta.value).then(function () { flashDone(root); });
			return;
		}
		ta.removeAttribute("aria-hidden");
		ta.focus();
		ta.select();
		try { if (document.execCommand("copy")) flashDone(root); } catch (e) {}
		ta.setAttribute("aria-hidden", "true");
		if (window.getSelection) window.getSelection().removeAllRanges();
	}
	document.addEventListener("click", function (ev) {
		var btn = ev.target.closest ? ev.target.closest("[data-copy-format]") : null;
		if (!btn) return;
		var root = btn.closest(".copy-llm");
		var ta = root && root.querySelector("#" + btn.getAttribute("data-copy-target"));
		if (ta) copyText(ta, root);
	});
})();
