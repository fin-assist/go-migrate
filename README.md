# go-library-template

Шаблон репозитория Go-библиотеки «Понятных финансов» ([FIN-118](https://finassist.youtrack.cloud/issue/FIN-118)). Из него создаются `go-idempotency` ([FIN-78](https://finassist.youtrack.cloud/issue/FIN-78)), `go-outbox` ([FIN-109](https://finassist.youtrack.cloud/issue/FIN-109)), `go-tombstone` ([FIN-110](https://finassist.youtrack.cloud/issue/FIN-110)), `go-identity` ([FIN-119](https://finassist.youtrack.cloud/issue/FIN-119)) и следующие. Сервисы — из [`fin-assist/go-service-template`](https://github.com/fin-assist/go-service-template), контракты и `finerr` — в [`fin-assist/contracts`](https://github.com/fin-assist/contracts).

- [docs/releasing.md](docs/releasing.md) — версии, что несовместимо, CHANGELOG, выпуск, миграции, Renovate.
- [CHANGELOG.md](CHANGELOG.md) — изменения по версиям.
- [AGENTS.md](AGENTS.md) — карта и правила для агентов, Code Review Rules.

## Автоматизация обновлений зависимостей

[Политика Renovate](docs/renovate-policy.md) (FIN-211): одна актуальная release/main, автомерж полностью ботских PR после обязательного CI, один повтор красного CI и закрытие с явным повтором через Dependency Dashboard. AndroidX/Compose остаются на ручной проверке. Исходные группировки, ограничения версий и расписания — `renovate-base.json`; generated target и quarantine — ветка `renovate-policy`. Активация требует отдельного токена и строгой защиты веток; по умолчанию выключена.

## Новая библиотека из шаблона

1. Создать репозиторий `fin-assist/go-<назначение>` из шаблона (GitHub → «Use this template») или влить шаблон в уже созданный.
2. Заменить модуль и пакет: `go mod edit -module github.com/fin-assist/go-<назначение>`, пути импорта в Go-файлах, `package library` → `package <назначение>` в корневых файлах, `MigrationsName` в `migrations.go`. Не заменять имя исходного репозитория в `scripts/check-suite.sh`: это ограничитель исключения N/A, который новая библиотека не наследует. При локальном клонировании сменить `origin` на новый репозиторий перед проверками; в Actions проверяется `GITHUB_REPOSITORY`.
3. Библиотека без таблиц удаляет `migrations.go`, `migrations_test.go` и каталог `migrations/`; без gRPC — ничего не добавляет.
4. Если библиотека строит ошибки для клиентов — `go get github.com/fin-assist/contracts/gen/go@<версия>` и конструкторы `finerr`.
5. Переписать первый абзац этого README, раздел «Внедрение» ниже и первый абзац `AGENTS.md`.
6. Секреты репозитория: `CLAUDE_CODE_OAUTH_TOKEN` (ревью `@claude`), `GO_MODULES_TOKEN` при приватных модулях. Claude GitHub App, Codex и Renovate стоят на организации.
7. Первый выпуск — `v0.1.0` по [docs/releasing.md](docs/releasing.md).

Перед первым запуском Renovate заменить **оба** `local>` ref в `renovate.json` на новый `fin-assist/<repo>`. GitHub template не копирует служебную ветку: создать собственную `renovate-policy` по [инструкции bootstrap](docs/renovate-policy.md#подготовка-и-включение), затем защитить её и настроить `controllerLogin`, обязательные checks и Environment `renovate-policy` с запуском только из `main` и доступ к organization secret `RENOVATE_POLICY_TOKEN` (выбранный владельцем вариант; новому репозиторию явно проверить доступность секрета). Проверить, что оба presets разрешаются из нового репозитория, а не из шаблона. До проверки оставить `RENOVATE_POLICY_ENABLED` выключенным; секреты, environments и branch protection из шаблона не наследуются.

## Раскладка

```
doc.go               package documentation: what the library does and what it needs from the service
migrations.go        Migrations, MigrationsName, MigrationsTable for the service's migration runner
migrations/          SQL migrations of the library, embedded (NNN_name.up.sql / .down.sql)
internal/pgtest/     PostgreSQL for tests and the up -> down -> up check, same rules as the service runner
scripts/             independent suite guards, external coverage policy and CHANGELOG checks
docs/releasing.md    versions and releases
```

Код библиотеки — в корневом пакете (импорт `github.com/fin-assist/go-outbox` даёт `outbox.Publish`); подпакеты — когда у частей разные потребители (`identity/grpcserver` и `identity/httpmw`), внутренние детали — в `internal/`.

## Внедрение

Раздел, который библиотека переписывает под себя: всё, что сервису нужно сделать, чтобы её подключить, по шагам и с кодом. Общий порядок:

1. `go get github.com/fin-assist/go-<назначение>@vX.Y.Z`.
2. Миграции — источник в `MigrationSources()` (`internal/app/wire.go` сервиса), до источника сервиса:

   ```go
   {Name: outbox.MigrationsName, Table: outbox.MigrationsTable, FS: outbox.Migrations},
   ```

3. Сборка в `wire()`: конструктор библиотеки получает от сервиса всё, что ей нужно, — транзакцию через интерфейс `Tx(ctx) (pgx.Tx, bool)` (его реализует `postgres.DB` шаблона), `*slog.Logger`, `prometheus.Registerer`, часы, настройки. Своих глобальных объектов и переменных окружения у библиотеки нет.
4. Интерцепторы, задания (`worker.Job`), обработчики — куда и в каком порядке.
5. Чек-лист для нового метода, таблицы или эндпоинта сервиса, если библиотека его требует.
6. Метрики и алерты, которые появляются у сервиса.

## Проверки

CI (`.github/workflows/ci.yml`):

| Job | Что |
|---|---|
| `go` | tidy/vet, race inventory; независимые unit/integration/API statements-профили и total80 (N/A только у исходного пустого шаблона); проверки скриптов/CHANGELOG; отчёты и artifacts |
| `lint` | golangci-lint (`.golangci.yml`): `forbidigo` против обхода `finerr` и против глобальных логгера, реестра метрик и переменных окружения; `depguard` против фреймворков |
| `api` | `gorelease`: изменения экспортируемого API против последнего тега и предлагаемая версия; на PR выпуска — проверка номера из `CHANGELOG.md` |
| `migrations` (PR) | существующие миграции не изменены, squawk по новым |

Тег `vX.Y.Z` запускает `.github/workflows/release.yml`: проверка тега и GitHub Release с текстом из `CHANGELOG.md`. Повторный запуск сохраняет существующий Release; актуальную проверку старого тега можно запустить вручную из `main` по [инструкции](docs/releasing.md#выпуск).

Локально:

```bash
make test          # Docker for testcontainers, or TEST_DATABASE_URL=postgres://... to use your PostgreSQL
make lint
make cover         # independent unit/integration/API profiles and total80 gates
make api           # API changes against the latest tag
make release-check VERSION=v0.2.0
```

FIN-243: каждый из unit/integration/API независимо требует80% statements; combined не заменяет уровни. Модули `github.com/fin-assist/*` берутся из git, минуя прокси (`GOPRIVATE`, задан в `Makefile` и CI).


### Покрытие FIN-243

`make collect-unit`, `make collect-integration`, `make collect-api` собирают независимые профили штатным Go coverage. `make check-unit`, `make check-integration`, `make check-api` проверяют каждый профиль закреплённым go-test-coverage/v2 v2.20.1 с total80. Принимается округление79,95% до80,0%; `make cover-combined` строит дополнительное объединение штатными средствами инструмента.

Все три набора выполняют `./...` и инструментируют одинаковые пакеты из `go list -race ./...`, исключая только точный пакет `<module>/internal/pgtest` по решению владельца 25.10.2026. Остальные обычные `.go` helpers и generated production code учитываются; тесты pgtest продолжают исполняться. Имена TestUnit/TestIntegration/TestAPI и FuzzUnit/FuzzIntegration/FuzzAPI проверяются race inventory до сборщика; исполняемые Example входят в API. Unit не использует БД/сеть, integration использует реальные зависимости, API проверяет публичный Go-контракт от лица потребителя. API не означает новый HTTP/gRPC сервис.

Пустые/пропущенные/упавшие наборы и отсутствующие/пустые/повреждённые профили не проходят guard; недоступный PostgreSQL при CI=true — ошибка. Обязательный job `go` падает при любой ошибке одного из трёх уровней, даже если combined выше80. Отчёты и профили сохраняются в CI artifacts; fork PR не пишет комментарии. Собственного расчёта процентов и merge нет.

FIN-247: только исходный `fin-assist/go-library-template` получает **N/A**, когда успешный suite сформировал точный профиль `mode: atomic` без блоков. Три набора по-прежнему обязаны выполнить тесты без skips/failures; lint, миграции и прочие проверки сохраняются. Идентичность проверяется по `GITHUB_REPOSITORY` в GitHub Actions, локально — по точному `origin` исходного шаблона. При появлении первого production statement автоматически включается штатный порог80 каждого уровня, в том числе при нуле покрытых statements. Combined получает N/A только при трёх валидных N/A профилях; смешанные пустые/непустые профили отклоняются. Созданные из шаблона библиотеки отклоняют пустой профиль даже до смены module path. `go-service-template` и остальные библиотеки сохраняют строгие правила.

[docs/testing.md](docs/testing.md) описывает уровни и границы; актуальные проценты и невыполненные критерии указаны в PR.
