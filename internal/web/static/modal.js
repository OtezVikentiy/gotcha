// Клавиатурное поведение модалки: Escape, фокус при открытии, возврат фокуса
// при закрытии.
//
// Модалка построена на CSS :target и работает без JS: открытие — переход на
// #id, закрытие — переход на #id-close. Без JS так и остаётся. Улучшения
// прогрессивные (№80, №81):
//  - Escape закрывает открытую модалку (в т.ч. открытую сервером);
//  - при открытии фокус уходит на заголовок диалога (tabindex=-1), при
//    серверном переоткрытии после ошибки валидации — тоже, иначе клавиатура
//    остаётся в начале страницы и модалку «не видит»;
//  - при закрытии фокус возвращается на элемент, с которого открывали
//    (образец — close(refocus) в daterange.js);
//  - Tab/Shift+Tab циклят по фокусируемым элементам внутри открытой модалки:
//    фон не изолирован ([inert] нет, aria-modal намеренно не выставлен — см.
//    modalShell), и без ловушки Tab с последнего поля уходил на десятки
//    фокусируемых элементов позади диалога.
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

	// openModal — открытая модалка: либо та, на которую указывает адресная
	// строка (:target), либо открытая сервером (modal--open).
	function openModal() {
		return modalFromHash() || document.querySelector(".modal.modal--open");
	}

	// focusables — элементы карточки диалога, достижимые по Tab, в порядке
	// DOM. Заголовок (tabindex=-1) и всё, что скрыто CSS (getClientRects
	// пуст), не считаются; фон-якорь .modal-backdrop лежит вне .modal-card.
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

	// trapTab — цикл фокуса: Tab с последнего элемента → на первый, Shift+Tab
	// с первого → на последний; фокус вне диалога (или на заголовке при
	// Shift+Tab, когда перед ним ничего нет) возвращается в цикл.
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

	// closeAnchor находит якорь закрытия для открытой модалки: либо той, на
	// которую указывает адресная строка (:target), либо открытой сервером
	// (класс modal--open — форма вернулась с ошибкой валидации).
	function closeAnchor() {
		var targeted = modalFromHash();
		if (targeted) {
			return targeted.id + "-close";
		}
		var served = document.querySelector(".modal.modal--open");
		return served ? served.id + "-close" : "";
	}

	// Открыватель запоминается на click (capture), ДО навигации по якорю:
	// к моменту hashchange браузер уже обработал фрагмент и увёл фокус с
	// триггера, activeElement там — body. Enter на ссылке тоже даёт click.
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
		// Тот же переход, что делает крестик: правило .modal-dismiss:target
		// перебивает и :target, и открытие с сервера. Возврат фокуса сделает
		// обработчик hashchange выше.
		window.location.hash = anchor;
	});

	// Серверное переоткрытие: страница загрузилась с .modal.modal--open —
	// увести фокус в диалог сразу (opener пуст: возвращать некуда, фокус
	// после закрытия останется на закрывшем элементе).
	var served = document.querySelector(".modal.modal--open");
	if (served) {
		focusHeading(served);
	}
})();
