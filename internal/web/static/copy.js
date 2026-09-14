/* navigator.clipboard работает только в secure-context (https/localhost);
 * на bare-HTTP — фолбэк execCommand по выделенной textarea. */
(function () {
	"use strict";
	function flash(root, selector, ms) {
		var m = root.querySelector(selector);
		if (!m) return;
		m.hidden = false;
		setTimeout(function () { m.hidden = true; }, ms);
	}
	function flashDone(root) { flash(root, "[data-copy-done]", 1500); }
	function flashFailed(root) { flash(root, "[data-copy-failed]", 4000); }
	function fallbackCopy(ta, root) {
		ta.removeAttribute("aria-hidden");
		// iOS Safari игнорирует select()/setSelectionRange на readonly textarea —
		// снимаем атрибут на время копирования, иначе execCommand("copy") видит пустое выделение.
		var wasReadOnly = ta.hasAttribute("readonly");
		ta.removeAttribute("readonly");
		ta.focus();
		ta.select();
		var ok = false;
		try { ok = document.execCommand("copy"); } catch (e) {}
		if (wasReadOnly) ta.setAttribute("readonly", "");
		ta.setAttribute("aria-hidden", "true");
		if (window.getSelection) window.getSelection().removeAllRanges();
		if (ok) flashDone(root); else flashFailed(root);
	}
	function copyText(ta, root) {
		// writeText может отклониться без фокуса/жеста или по permissions-policy —
		// реджект уходит в fallbackCopy, иначе кнопка молча ничего не делает.
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
