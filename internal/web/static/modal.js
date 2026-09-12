(function () {
	"use strict";

	var opener = null;

	function modalFromHash() {
		var hash = window.location.hash;
		if (!hash || hash.length < 2) {
			return null;
		}
		var el = document.getElementById(hash.slice(1));
		return el && el.classList.contains("modal") ? el : null;
	}

	function focusHeading(modal) {
		var h = modal.querySelector(".modal-card > .card-header");
		if (h) {
			h.focus();
		}
	}

	function openModal() {
		return modalFromHash() || document.querySelector(".modal.modal--open");
	}

	// Заголовок (tabindex=-1) и элементы, скрытые CSS (getClientRects пуст),
	// в список не попадают.
	function focusables(modal) {
		var card = modal.querySelector(".modal-card") || modal;
		var all = card.querySelectorAll(
			'a[href], button, input, select, textarea, [tabindex]'
		);
		var out = [];
		for (var i = 0; i < all.length; i++) {
			var el = all[i];
			if (el.disabled || el.tabIndex < 0 || el.type === "hidden") {
				continue;
			}
			if (el.getClientRects().length === 0) {
				continue;
			}
			out.push(el);
		}
		return out;
	}

	// Фон не изолирован ([inert] нет, aria-modal намеренно не выставлен, см.
	// modalShell) — без ручного цикла Tab уходит на элементы позади диалога.
	function trapTab(ev) {
		var modal = openModal();
		if (!modal) {
			return;
		}
		var list = focusables(modal);
		if (list.length === 0) {
			ev.preventDefault();
			return;
		}
		var first = list[0];
		var last = list[list.length - 1];
		var active = document.activeElement;
		var inside = modal.contains(active);
		if (ev.shiftKey) {
			if (!inside || active === first || active === modal.querySelector(".modal-card > .card-header")) {
				ev.preventDefault();
				last.focus();
			}
		} else if (!inside || active === last) {
			ev.preventDefault();
			first.focus();
		}
	}

	function closeAnchor() {
		var targeted = modalFromHash();
		if (targeted) {
			return targeted.id + "-close";
		}
		var served = document.querySelector(".modal.modal--open");
		return served ? served.id + "-close" : "";
	}

	// Открыватель запоминается на click (capture), до навигации по якорю —
	// к моменту hashchange фокус уже увёден на body.
	document.addEventListener("click", function (ev) {
		var a = ev.target.closest ? ev.target.closest('a[href^="#"]') : null;
		if (!a) {
			return;
		}
		var target = document.getElementById(a.getAttribute("href").slice(1));
		if (target && target.classList.contains("modal")) {
			opener = a;
		}
	}, true);

	window.addEventListener("hashchange", function () {
		var m = modalFromHash();
		if (m) {
			focusHeading(m);
		} else if (opener) {
			opener.focus();
			opener = null;
		}
	});

	document.addEventListener("keydown", function (ev) {
		if (ev.defaultPrevented) {
			return;
		}
		if (ev.key === "Tab") {
			trapTab(ev);
			return;
		}
		if (ev.key !== "Escape") {
			return;
		}
		var anchor = closeAnchor();
		if (!anchor) {
			return;
		}
		// .modal-dismiss:target перебивает и :target, и серверное открытие.
		window.location.hash = anchor;
	});

	var served = document.querySelector(".modal.modal--open");
	if (served) {
		focusHeading(served);
	}
})();
