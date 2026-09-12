-- backward-compatible: yes (новая таблица)
--
-- Гейт схемы отказывает при базе впереди бинаря (защита от тихого даунгрейда), но без
-- этого признака откат релиза после наката миграций невозможен — бинарь не стартует.
-- Признак едет в PG вместе со схемой: читает его любой бинарь, включая откатившийся.
CREATE TABLE schema_compat (
    target              text   NOT NULL CHECK (target IN ('pg', 'ch')),
    version             bigint NOT NULL,
    backward_compatible boolean NOT NULL,
    applied_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (target, version)
);
