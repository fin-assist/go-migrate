# Изменения

Формат — [Keep a Changelog](https://keepachangelog.com/ru/1.1.0/), выпуск — [docs/releasing.md](docs/releasing.md). Каждый PR добавляет запись в Unreleased.

## [Unreleased]

### Несовместимо

- FIN-278: `Force` принимает только версию, у которой есть файл в источнике, или `0`/`-1` как «ничего не применено» (`0` остаётся настоящей версией при файле версии 0); раньше `force <source> 0` и версия без файла записывались в version table, после чего каждый `Up` молча ничего не применял. `Validate` отклоняет две записи одной числовой версии разной ширины (`1_a.up.sql` и `01_a.up.sql`), раньше они падали только при открытии источника. `Up`/`Down` теперь отказывают, если применённой версии нет среди файлов: при откате приложения на образ N после применения миграции N+1 migrate Job образа N завершится ошибкой (на v0.1.x — no-op); откатывайте без migrate Job либо сначала `Down` образом N+1, `Force` из образа N к версии N+1 невозможен; не выставляйте при откате меньшую версию (`force 4`), если схема не откатывалась, иначе следующая раскатка N+1 повторит миграцию. Действует, когда образ N уже на go-migrate ≥ v0.2.0; откат на образ с v0.1.x ведёт себя по-прежнему. Это касается и источника без SQL-файлов с записанной версией: `Up`/`Down` отказывают, `Versions` показывает версию. Сервисам код менять не нужно: их источники дублей не содержат; операторский `force` с версией без файла теперь возвращает ошибку, version table с 0 после прежнего `force 0` восстанавливается повторным `force 0`.

### Исправлено

- FIN-278: `Up` и `Down` возвращают ошибку, если применённой версии нет среди файлов источника (переименованный или удалённый применённый файл, разрыв нумерации), вместо ложного успеха «migrations up to date» и пропуска следующих файлов; регрессия пошагового `Steps(1)` относительно прежнего runner шаблона.

## [v0.1.1] — 2026-10-25

### Исправлено

- FIN-262: CheckUpDownUp находит down-файл по числовой версии без фиксированной ширины, включая выпущенные библиотеки с именами 001_*.sql. Обработка irreversible и отказ при отсутствии/неоднозначности down-файла сохранены; SQL и теги опубликованных миграций не меняются.

## [v0.1.0] — 2026-10-25

### Добавлено

- FIN-254: подготовлен первый выпуск go-migrate v0.1.0; публикация тега и GitHub Release выполняется владельцем после merge release PR.

- FIN-251: общий PostgreSQL migration runner с независимыми version tables, последовательными источниками, advisory queue, DDL/statement/wait timeout, dirty/force/down/empty-source поведением; публичный migratetest с явным adminDSN, отдельной тестовой БД и up/down/up. Unit без сети, реальные PG18 Integration и внешний consumer API; независимые statements gates80 сохраняются, публичный harness включён в denominator.

### Изменено

- FIN-260: disabled Renovate controller завершается до credentials/config/event/API. Непривилегированный workflow gate допускает Environment только при точном lowercase true; явный --bootstrap независимо требует credential. Enabled guards, runtime Go API и независимые coverage gates сохранены; внешний bootstrap/protections выполняются владельцем отдельно.

- FIN-251: отмена во время инициализации пустого источника сохраняется в Up/Versions/CheckUpDownUp, реальные ошибки инициализации не перетираются. PG regression с двумя файлами и ожидающим конкурентным Down проверяет непрерывную queue lock между native Steps.

- FIN-251: публичный Validate возвращает ошибку для nil FS до файлового/сетевого IO вместо panic; добавлены Unit и внешний consumer API regression.

- FIN-251: обычный текст ошибок и логи не раскрывают migration SQL/DSN/error.Detail. Нативные SQL/engine diagnostics сохраняются для errors.Is/As через безопасный private wrapper; malformed DSN возвращает ErrInvalidDSN без небезопасного ParseError в цепочке. Raw engine Printf logging отключён, структурированные события runner сохранены.

- FIN-251: DefaultConfig использует discard logger вместо глобального slog.Default; сервис передаёт Logger явно. Закрытие engine/драйвера завершается до освобождения migration queue. Переход между файлами выполняется native Steps под непрерывной общей queue lock без race-prone GracefulStop; сохраняются ErrShortLimit и dirty/clean state. Отмена после начала файла больше не возвращает ложный nil: после безопасной остановки возвращается context error, реальная SQL-ошибка сохраняется. Ошибка инициализации version table также закрывает физическое подключение, которое upstream WithInstance не возвращает в sql.DB; нормальное закрытие драйвера не меняется. Starter migration metadata удалены; собственных runtime SQL у runner нет.
