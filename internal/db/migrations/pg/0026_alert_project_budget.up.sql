-- Пер-проектный потолок уведомлений за окно.
--
-- Троттлинг alert_throttle ключуется парой (issue_id, rule_id), и у НОВОГО
-- issue строки там нет по определению — значит он проходит всегда. Отправитель
-- с уникальным fingerprint на каждое событие получал новый issue на событие и
-- уведомление на событие: тысяча событий в одном конверте = тысяча писем
-- участникам организации и тысяча сообщений в Telegram. Ключ DSN при этом
-- публичный, он лежит в браузерном SDK.
--
-- Строка на проект: окно, счётчик отправленных, счётчик подавленных и исход
-- последнего решения. allowed хранится потому, что решение и учёт обязаны
-- приниматься ОДНИМ оператором (см. claimBudget) — иначе между «проверил
-- бюджет» и «списал» встаёт гонка, ровно та же, ради которой claimThrottle
-- сделан одним INSERT ... ON CONFLICT.
--
-- suppressed НЕ обнуляется при выдаче бюджета: его забирает и сбрасывает
-- отдельная задача, рассылающая сводку «подавлено ещё N» (см. alert.Digester).
-- Так горячий путь остаётся коротким, а сводка — самостоятельной заботой.
CREATE TABLE alert_project_budget (
    project_id   bigint PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    window_start timestamptz NOT NULL DEFAULT now(),
    sent         integer NOT NULL DEFAULT 0,
    suppressed   integer NOT NULL DEFAULT 0,
    allowed      boolean NOT NULL DEFAULT true,
    digest_at    timestamptz
);
