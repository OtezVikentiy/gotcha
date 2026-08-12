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
	function fallbackCopy(ta, root) {
		ta.removeAttribute("aria-hidden");
		ta.focus();
		ta.select();
		try { if (document.execCommand("copy")) flashDone(root); } catch (e) {}
		ta.setAttribute("aria-hidden", "true");
		if (window.getSelection) window.getSelection().removeAllRanges();
	}
	function copyText(ta, root) {
		// navigator.clipboard есть только в secure-context; вдобавок writeText может
		// ОТКЛОНИТЬСЯ (нет фокуса/жеста, permissions-policy). Реджект тоже уводим в
		// фолбэк execCommand — иначе кнопка молча ничего не делает (ни копии, ни тоста).
		if (navigator.clipboard && navigator.clipboard.writeText) {
			navigator.clipboard.writeText(ta.value).then(
				function () { flashDone(root); },
				function () { fallbackCopy(ta, root); }
			);
			return;
		}
		fallbackCopy(ta, root);
	}
	document.addEventListener("click", function (ev) {
		var btn = ev.target.closest ? ev.target.closest("[data-copy-format]") : null;
		if (!btn) return;
		var root = btn.closest(".copy-llm");
		var ta = root && root.querySelector("#" + btn.getAttribute("data-copy-target"));
		if (ta) copyText(ta, root);
	});
})();
