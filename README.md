# go-migrate

Общий PostgreSQL migration runner «Понятных финансов» ([FIN-251](https://finassist.youtrack.cloud/issue/FIN-251), эпик FIN-248). Выделен из `internal/adapter/postgres/migrate` шаблона сервисов: один источник SQL и отдельная version table для каждой библиотеки и сервиса. Библиотека не содержит собственных runtime-миграций, CLI, конфигурации окружения или контейнеров.

## Подключение

Импорт `github.com/fin-assist/go-migrate` имеет имя пакета `migrate`. До первого выпуска сервисы подключаются отдельными PR; этот PR не меняет сервисы. После публикации использовать фиксированный тег библиотеки.

```go
sources := []migrate.Source{
    {Name: outbox.MigrationsName, Table: outbox.MigrationsTable, FS: outbox.Migrations},
    {Name: "service", Table: "schema_migrations", FS: serviceMigrations},
}
cfg := migrate.DefaultConfig(primaryDSN)
cfg.Logger = logger // explicit service-owned *slog.Logger
if err := migrate.Up(ctx, cfg, sources); err != nil {
    return err
}
```

`Source.FS` содержит пары `NNN_name.up.sql` / `NNN_name.down.sql` в корне. Имена источников и version tables обязательны и уникальны; SQL-имя таблицы задаётся доверенной сборкой приложения. `Validate` проверяет файлы без БД (две записи одной числовой версии, например `1_a.up.sql` и `01_a.up.sql`, отклоняются), `Find` выбирает источник по имени и сообщает известные имена при ошибке. Источники применяются в переданном порядке: библиотеки перед сервисом. Никакого runtime-поиска зависимостей нет.

`DefaultConfig` задаёт DDL `lock_timeout=5s`, `statement_timeout=15m`, ожидание migration queue `15m`. По умолчанию logger пишет в discard; `Logger=nil` также безопасен. Это намеренное отличие от прежнего `slog.Default`: сервис передаёт свой logger явно, глобальный logger не читается.

## Выполнение и восстановление

- `Up(ctx, cfg, sources)` применяет только ожидающие файлы, повторный запуск — no-op. Если применённой (чистой) версии из version table нет среди файлов источника (применённый файл переименован или удалён, разрыв нумерации), `Up` и `Down` возвращают ошибку с номером версии, а не ложный успех (FIN-278). Это касается и отката образа приложения: если релиз N+1 применил `000005`, а приложение откатили на образ N с файлами 1..4, migrate Job образа N завершится ошибкой «applied version 5 … has no migration file» (на v0.1.x он проходил как no-op). Варианты оператора: откатывать приложение без запуска migrate Job образа N (схема N+1 совместима с кодом N по правилу двух шагов) либо сначала выполнить `Down` образом N+1 и затем раскатить образ N; `Force` к версии 5 из образа N невозможен — у него нет этого файла. **Не выставляйте при откате образа меньшую версию (`force <source> 4`), если схема фактически не откатывалась:** следующая раскатка N+1 снова выполнит `000005`. Новое поведение действует, когда образ N уже использует go-migrate ≥ v0.2.0; первый откат на образ с v0.1.x ведёт себя по-прежнему. Источник без SQL-файлов (у образа N его миграций ещё нет) тоже не исключение: если его version table содержит версию, `Up` и `Down` возвращают ошибку «applied version N … has no migration file» без подсказки `force` (в такой сборке `Force` не помогает: выполните `Down` сборкой, где файл есть, или пропустите запуск миграций; если версия dirty, `Down` откажет и там — проверьте схему и выполните `Force` сборкой с файлом; то же для записи `(-1, dirty)`, которую golang-migrate оставляет после неудачного отката первой миграции: `Up`/`Down` отказывают с «source X is dirty with no applied version», `Versions` показывает `{0, Dirty: true}`), а `Versions` показывает записанную версию; без записанной версии пустой источник по-прежнему пропускается. Версию для `Force` выбирайте по фактической схеме: неудачный файл обычно откатывается целиком, а упавший down мог не выполнить ни одной команды — тогда схема всё ещё соответствует применённой версии, и `force 0` без ручного отката приведёт к «relation already exists» при следующем `Up`; после такого отката первой миграции подходит и `force 1` сборкой с файлом. Если version table содержит 0 после прежнего `force <source> 0`, восстановление при пустой схеме — снова `force <source> 0`. Первый отказ останавливает весь список; автоматического rollback уже успешно применённых источников нет.
- `Versions` возвращает `{Source, Version, Dirty}` в порядке источников; версия до первого применения и у пустого источника без записанной версии — 0; для пустого источника с записанной версией (её применила более новая сборка) `Versions` возвращает эту версию. Пустой источник всё равно требует доступной БД и создаёт свою version table.
- `Down(ctx, cfg, source, steps)` требует положительное число шагов; rollback — отдельное операторское действие. Если доступных шагов меньше запрошенного, сохраняется native `migrate/v4.ErrShortLimit` с количеством недостающих шагов; rollback из пустого состояния возвращает `os.ErrNotExist`. Down-файл с `-- irreversible` содержит SQL, который сам отказывает в rollback; runner не обходит этот отказ.
- `Force(ctx, cfg, source, version)` меняет только metadata, не SQL. Принимается версия, у которой есть файл в источнике, либо `0`/`-1` — «ничего не применено» (`0` — настоящая версия, только если в источнике есть файл версии 0); другая версия отклоняется до подключения к БД. После ошибки проверить фактическую схему вручную, устранить причину и только затем сбросить dirty flag нужной версией. Никакого автоматического Force нет.

Runner владеет отдельными подключениями, которые сам открывает и закрывает. Он не принимает транзакцию сервиса. Перед golang-migrate advisory lock захватывается отдельная queue connection по ключу `fin-assist/migrate/<table>` без session lock/statement timeout: конкурентный запуск ждёт `WaitTimeout`, а не короткий DDL timeout. Выполняющий SQL connection получает DDL/statement timeout. Queue освобождается после закрытия engine; cleanup имеет отдельный лимит10 секунд. При ошибке инициализации version table закрывается также физическое подключение, не возвращённое upstream WithInstance в sql.DB. Tracking ограничен открытием собственного pool до результата инициализации; подключения вызывающего кода не принимаются и не закрываются. После успеха tracking отключается, закрытие остаётся штатным driver.Close.

Отмена context прерывает ожидание очереди. Runner вызывает native `Steps(1)` / `Steps(-1)` по одному файлу и проверяет context между вызовами; native driver lock переснимается между файлами, общая queue connection удерживается весь источник. Сторонние клиенты, обходящие этот runner и его очередь, не поддерживаются как конкурентный deploy path. Канал GracefulStop не используется: в golang-migrate v4.20.1 он содержит подтверждённую race. Во время миграции остановка выполняется на безопасной границе файлов; текущий SQL ограничен `StatementTimeout`, context не обрывает его посередине. Отмена не откатывает предыдущие файлы. После безопасной остановки возвращается context error, даже если golang-migrate сообщил nil; реальная ошибка SQL не перетирается. Это явное исправление прежнего ложного успешного результата при отмене. Парсинг DSN и ошибки подключения возвращаются вызывающему коду; не публикуйте эти служебные ошибки клиентам API и не логируйте DSN.

Ошибки имеют безопасный stage-only Error(): SQL, error.Detail и DSN в обычный текст/логи не попадают. Некорректный DSN возвращает `ErrInvalidDSN`, небезопасный ParseError не сохраняется. Остальные engine/DB errors доступны программно через `errors.Is`/`errors.As`; native `database.Error` хранит `OrigErr` для SQLSTATE и других диагностик. Явно извлечённые диагностики могут содержать SQL и секреты: их нельзя логировать или передавать клиентам. Raw native Printf logging отключён; структурированные source/from/to события остаются.

## Проверка SQL в сервисе

`migratetest` — публичный test harness; его обычный Go-код учитывается во всех трёх coverage-профилях. Он не выбирает сервер по env, не запускает Docker, не хранит singleton. Test entrypoint сервиса сам выбирает PostgreSQL18 (например, existing `internal/pgtest` / `TEST_DATABASE_URL`) и передаёт adminDSN явно:

```go
func TestIntegrationMigrations(t *testing.T) {
    adminDSN := pgtest.DSN(t) // server selection belongs to the caller
    dsn := migratetest.DSN(t, adminDSN)
    migratetest.CheckUpDownUp(t, dsn, migrate.Source{
        Name: "service", Table: "schema_migrations", FS: serviceMigrations,
    })
}
```

`DSN(t, adminDSN)` создаёт случайно названную отдельную БД и регистрирует `DROP DATABASE ... WITH (FORCE)` через `t.Cleanup`. Пользователь БД из adminDSN должен иметь `CREATEDB`; adminDSN может быть URL или pgx keyword DSN. Создание и cleanup ограничены10 секундами; ошибки setup/cleanup проваливают тест. Helper не удаляет admin database.

`CheckUpDownUp(t, dsn, source)` получает **пустую БД вызывающего кода** и не создаёт/удаляет её. Он применяет каждый файл, выполняет down, проверяет возврат к предыдущей версии и повторяет up. Down с начальным `-- irreversible` не запускается, проход продолжается вперёд; в конце dirty state недопустим. Версия down-файла сравнивается численно независимо от ширины (`001_name.down.sql` и `000001_name.down.sql` допустимы); для одной версии должен существовать ровно один down-файл. Отсутствующие и неоднозначные файлы отклоняются. Context ожидания/переходов и statement timeout —2 минуты каждый; уже выполняющийся SQL завершается до своей границы/statement timeout, поэтому отмена не является жёстким wall-clock kill процесса. Родительская отмена context сохраняется. `migrate.CheckUpDownUp(ctx, cfg, source)` — тот же проход с возвращаемой ошибкой вместо `testing.TB`, для собственных test harness. Caller также владеет cleanup этой БД.

## Проверки и выпуск

```bash
TEST_DATABASE_URL='postgres://postgres@localhost:5432/postgres?sslmode=disable' make test
make lint
make cover
make api
```

Без явно заданного локального PostgreSQL только внутренний `internal/pgtest` запускает testcontainers PostgreSQL18; недоступность БД при `CI=true` — ошибка. Unit не обращается к сети/БД: проверяет success/error/cleanup через маленькие private consumer IO interfaces. Integration проверяет реальные PG18 SQL/locks/dirty/rollback, API — внешний Go-потребитель публичного runner и harness с PG18.

Каждый из Unit, Integration, API независимо требует80% **Go statements**, одинаковый denominator всех production packages; только точный `<module>/internal/pgtest` исключён по решению владельца. Публичный `migratetest` включён. Suite остаётся `./...`, тесты pgtest исполняются. Пустые/skip/fail suites, пустые/header-only профили отвергаются; исключение N/A принадлежит только оригинальному `fin-assist/go-library-template` и этой библиотеке не доступно. Закреплён go-test-coverage/v2 v2.20.1, принято его округление79,95%→80,0%; combined только информационный. Подробнее: [docs/testing.md](docs/testing.md).

CI сохраняет required job `go`, а также `lint`, `api` (gorelease) и `migrations`. SQL fixtures находятся только в `testdata/`; workflow `migrations` защищает runtime SQL `migrations/*.sql`, которых у runner нет. [CHANGELOG.md](CHANGELOG.md), [AGENTS.md](AGENTS.md), [выпуск](docs/releasing.md), [Renovate](docs/renovate-policy.md).

FIN-260: при unset/false/TRUE/True controller успешно завершается без credentials, чтения config/event и любых API-запросов. Только точное case-sensitive `true` допускает привилегированный job: shell gate без permissions, secrets, checkout и Environment выдаёт константу true/false. CLI повторяет early-off до token/config/event/API; явный `--bootstrap` отдельно требует RENOVATE_POLICY_TOKEN и не включает автоматизацию. Enabled guards сохранены. Собственные presets/bootstrap и их resolver proof обязательны до merge own-ref config; фактические настройки, защиты, credentials и активацию выполняет владелец отдельно, этот код их не меняет.

Проверка `api` устойчива к публикации тега во время CI: release-коммит сравнивается
с предыдущим выпуском, удалённый тег проверяется по commit до и после `gorelease`.
Подробности и границы исключения — [docs/releasing.md](docs/releasing.md#проверка-версии-в-ci-fin-264).
