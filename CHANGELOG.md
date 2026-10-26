# Изменения

Формат — [Keep a Changelog](https://keepachangelog.com/ru/1.1.0/), версии и выпуск — [docs/releasing.md](docs/releasing.md). Каждый PR библиотеки, включая документацию, тесты и CI, добавляет строку в «Unreleased»; исключение — полностью ботские PR Renovate только с зависимостями, их сводку по `go.mod` добавляет PR выпуска. При выпуске раздел получает номер версии и дату.

## [Unreleased]

### Несовместимо

- FIN-278: `Force` принимает только версию, у которой есть файл в источнике, или `0`/`-1` как «ничего не применено» (`0` остаётся настоящей версией при файле версии 0); раньше `force <source> 0` и версия без файла записывались в version table, после чего каждый `Up` молча ничего не применял. `Validate` отклоняет две записи одной числовой версии разной ширины (`1_a.up.sql` и `01_a.up.sql`), раньше они падали только при открытии источника. `Up`/`Down` теперь отказывают, если применённой версии нет среди файлов: при откате приложения на образ N после применения миграции N+1 migrate Job образа N завершится ошибкой (на v0.1.x — no-op); откатывайте без migrate Job либо сначала `Down` образом N+1, `Force` из образа N к версии N+1 невозможен; не выставляйте при откате меньшую версию (`force 4`), если схема не откатывалась, иначе следующая раскатка N+1 повторит миграцию. Действует, когда образ N уже на go-migrate ≥ v0.2.0; откат на образ с v0.1.x ведёт себя по-прежнему. Это касается и источника без SQL-файлов с записанной версией: `Up`/`Down` отказывают без подсказки `force` (из такой сборки он не помогает — `Down` сборкой с файлом или пропуск запуска миграций; для dirty-версии, в том числе записи `(-1, dirty)` после неудачного отката первой миграции, — проверка схемы и `Force` сборкой с файлом на версию, которой соответствует фактическая схема), `Versions` показывает версию. Сервисам код менять не нужно: их источники дублей не содержат; операторский `force` с версией без файла теперь возвращает ошибку, version table с 0 после прежнего `force 0` восстанавливается повторным `force 0`.

### Исправлено

- FIN-264: проверка версии в CI сохраняет сравнение с предыдущим выпуском при публикации тега во время gorelease; чужие и изменившиеся теги отклоняются. Runtime и публичный API не меняются.

- FIN-314: `Versions` для непустого источника показывает запись `(-1, dirty)`, которую golang-migrate оставляет после неудачного отката первой миграции, как `{Version: 0, Dirty: true}`; раньше флаг терялся и отчёт был «ничего не применено, чисто», хотя `Up`/`Down` отказывали с `ErrDirty`. Версия читается из драйвера, а не через `Migrate.Version()`; `CheckUpDownUp` на таком источнике теперь сразу отказывает как на dirty-версии. Совместимо: меняется только отчёт о dirty-состоянии, сервисам ничего делать не нужно.
- FIN-278: `Up` и `Down` возвращают ошибку, если применённой версии нет среди файлов источника (переименованный или удалённый применённый файл, разрыв нумерации), вместо ложного успеха «migrations up to date» и пропуска следующих файлов; регрессия пошагового `Steps(1)` относительно прежнего runner шаблона.

### Изменено

- FIN-341: правила lint о процессном состоянии приведены к ядру go-library-template (FIN-322): секции `forbidigo` и `depguard` в `.golangci.yml` и `scripts/test_lint_policy.py` совпадают с шаблоном. Добавлены запреты `slog.With` и `os.Exit`; сняты правила вне ядра: `grpclog` и `grpclog/glogger`, `prometheus.Unregister`, `http.DefaultTransport`/`DefaultServeMux`/`PostForm`, `otel.Tracer`/`Meter`/`Handle` и `otel/log/global`, `os.UserHomeDir`/`UserConfigDir`/`UserCacheDir`, `os.TempDir`/`CreateTemp`/`MkdirTemp`, `(*os.File).Chdir`, `runtime.SetDefaultGOMAXPROCS`, `syscall`/`golang.org/x/sys`; принцип в целом проверяется на ревью. Runtime и экспортируемый API не менялись.
- FIN-308: `forbidigo` и `depguard` перенесены из go-library-template (FIN-294): запрещены оставшиеся пути к глобальному состоянию — стандартные логгеры, глобальный реестр метрик, `http.DefaultClient` и функции пакета `http`, глобальные провайдеры `otel`, `os/signal`, окружение (`os`, `syscall`, `golang.org/x/sys`), функции пакета `flag`, стандартные потоки, `os.TempDir`/`CreateTemp`/`MkdirTemp`, настройка runtime и смена рабочего каталога; пустой импорт `expvar`, `net/http/pprof` и др. `scripts/test_lint_policy.py` прогоняет закреплённый линтер на пробном модуле; после `go mod tidy` пробы версии из `go.mod` должны совпасть точно, а закреплённые вручную — не ниже закреплённых (по semver), чтобы обновление Renovate в `go.mod` не ломало пробу. Нарушений в коде библиотеки не найдено. Runtime и экспортируемый API не менялись.
- FIN-308: CI не оставляет токен checkout в jobs `lint`, `api` и `migrations`, имя базовой ветки передаётся в скрипт `migrations` через окружение (FIN-301); `scripts/test_workflow_hardening.py` разбирает workflow через PyYAML и запрещает `${{` в `run` и checkout без `persist-credentials: false`. Runtime и экспортируемый API не менялись.
- FIN-309: `docs/releasing.md` и вступление CHANGELOG требуют строку в `[Unreleased]` от каждого PR, включая документацию, тесты и CI (кроме полностью ботских PR Renovate только с зависимостями — их сводку по `go.mod` вносит PR выпуска); заголовок PR выпуска — с ключом FIN (FIN-307). Runtime и экспортируемый API не менялись.
- FIN-279: integration/API тесты закрытия подключений runner ждут выхода backend PostgreSQL с ограниченным сроком вместо однократного чтения pg_stat_activity, которое гонялось с асинхронным закрытием; настоящая утечка по-прежнему проваливает тест; на время ожидания GC отключён, чтобы финализатор утёкшего подключения не закрыл сокет и не скрыл утечку. Только тесты, runtime и API не меняются.

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
